// Package accesslog is a collage plugin that writes one log line per request.
//
//	app, err := collage.New(&collage.Config{
//		Plugins: []collage.Plugin{accesslog.New(accesslog.Options{})},
//	})
//
// A line carries the method, path, status, bytes written, duration, client
// address, user agent, referer (its scheme, host and path, unless
// Options.FullReferer) and request id, as slog attributes: text or JSON is
// the logger's handler's choice, not the plugin's. The logger is the application's
// own unless Options.Logger names another.
//
// Every request gets an id — the one its X-Request-ID header carried when that is
// a sensible value, a new one otherwise — sent back on the response and put in the
// request's context, so a handler can log with it:
//
//	log.InfoContext(ctx, "charged card", "request_id", accesslog.RequestID(ctx))
package accesslog

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Elagoht/collage/pkg/collage"
)

// Name is the plugin's name, and the key its configuration is found under.
const Name = "elagoht/accesslog"

// Options configures the plugin.
type Options struct {
	// Logger receives the lines. Unset, it is the application's logger. It can
	// only be set from Go.
	Logger *slog.Logger `json:"-"`
	// Skip are path prefixes never logged. Default ["/_collage/", "/healthz", "/readyz"]:
	// collage's development endpoints, and the liveness and readiness checks a
	// load balancer or kubelet polls every few seconds (elagoht/health's paths).
	// An empty list in configuration logs everything. A prefix covers whole
	// segments, with or without a trailing slash: "/healthz" and "/healthz/"
	// both cover /healthz and /healthz/db, and neither covers /healthz-report.
	Skip []string `json:"skip"`
	// Sample is the fraction of successful (2xx) responses logged, between 0 and
	// 1: 0.1 logs one in ten. Zero, the default, logs every one. A redirect, a
	// client error and a server error are always logged: they are the lines
	// someone goes looking for.
	Sample float64 `json:"sample"`
	// RequestIDHeader is the header a request id is read from and sent back in.
	// Default "X-Request-ID".
	RequestIDHeader string `json:"requestIDHeader"`
	// FullReferer logs the Referer header exactly as it came. Without it a
	// referer is logged as its scheme, host and path only — no query, fragment
	// or user info, where a reset token or a session id travels — and one that
	// does not parse as a URL is logged empty.
	FullReferer bool `json:"fullReferer"`
}

// Plugin writes the lines.
type Plugin struct {
	opts Options
	log  *slog.Logger
}

// New returns a plugin with opts as its starting point, which the application's
// own configuration is then decoded over.
func New(opts Options) *Plugin { return &Plugin{opts: opts} }

func (p *Plugin) Name() string                   { return Name }
func (p *Plugin) Version() string                { return "0.2.2" }
func (p *Plugin) Shutdown(context.Context) error { return nil }

var _ collage.Plugin = (*Plugin)(nil)

var headerName = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// Init reads the configuration and wraps every request.
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	if p.opts.Skip == nil {
		p.opts.Skip = []string{"/_collage/", "/healthz", "/readyz"}
	}
	cfg, err := collage.PluginConfig(host, p.opts)
	if err != nil {
		return err
	}
	p.opts = cfg
	o := &p.opts
	if o.Sample < 0 || o.Sample > 1 {
		return fmt.Errorf("accesslog: sample %v must be between 0 and 1", o.Sample)
	}
	if o.Sample == 0 {
		o.Sample = 1
	}
	for _, prefix := range o.Skip {
		if !strings.HasPrefix(prefix, "/") {
			return fmt.Errorf("accesslog: skip prefix %q must begin with /", prefix)
		}
	}
	if o.RequestIDHeader == "" {
		o.RequestIDHeader = "X-Request-ID"
	}
	if !headerName.MatchString(o.RequestIDHeader) {
		return fmt.Errorf("accesslog: %q is not a header name", o.RequestIDHeader)
	}
	p.log = o.Logger
	if p.log == nil {
		p.log = host.Logger()
	}
	return host.Use(p.middleware)
}

type idKey struct{}

// RequestID returns the id of the request ctx belongs to, or "" outside a request
// the plugin wrapped. A data handler reaches it through rc.Context().
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(idKey{}).(string)
	return id
}

// validID accepts what request id schemes in use look like — UUIDs, hex, base64,
// AWS's "Root=1-…" — and nothing that could break a log line or a header.
var validID = regexp.MustCompile(`^[A-Za-z0-9._:;=+/-]{1,128}$`)

func newID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:]) // crypto/rand.Read does not fail on supported platforms
	return hex.EncodeToString(raw[:])
}

func (p *Plugin) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A static build asking for its own files to capture their headers is
		// not a request anyone served: it is not logged, and gets no request
		// id, which differs on every request and is no header of the file.
		if collage.IsCapture(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}
		id := r.Header.Get(p.opts.RequestIDHeader)
		if !validID.MatchString(id) {
			id = newID()
			// A handler forwarding the request onward passes on the id it was
			// given, not the one the client sent that was refused.
			r.Header.Set(p.opts.RequestIDHeader, id)
		}
		w.Header().Set(p.opts.RequestIDHeader, id)
		r = r.WithContext(context.WithValue(r.Context(), idKey{}, id))

		for _, prefix := range p.opts.Skip {
			if under(r.URL.Path, prefix) {
				next.ServeHTTP(w, r)
				return
			}
		}

		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		defer func() {
			// A panic on its way to collage's own recovery becomes a 500 there;
			// the line says so rather than going missing with it.
			if v := recover(); v != nil {
				rec.status = http.StatusInternalServerError
				p.write(r, rec, id, time.Since(start))
				panic(v)
			}
		}()
		next.ServeHTTP(rec, r)
		p.write(r, rec, id, time.Since(start))
	})
}

func (p *Plugin) write(r *http.Request, rec *recorder, id string, d time.Duration) {
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	if status < 300 && p.opts.Sample < 1 && mrand.Float64() >= p.opts.Sample {
		return
	}
	level := slog.LevelInfo
	if status >= 500 {
		level = slog.LevelError
	}
	p.log.LogAttrs(r.Context(), level, "request",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Int64("bytes", rec.bytes),
		slog.Duration("duration", d),
		slog.String("ip", clientIP(r)),
		slog.String("user_agent", r.UserAgent()),
		slog.String("referer", p.referer(r)),
		slog.String("request_id", id),
	)
}

// referer returns r's Referer header as it is logged: whole with FullReferer,
// otherwise trimmed by trimReferer.
func (p *Plugin) referer(r *http.Request) string {
	if p.opts.FullReferer {
		return r.Referer()
	}
	return trimReferer(r.Referer())
}

// trimReferer returns the scheme, host and path of a referer, without its user
// info, query or fragment: a page linking to the site can carry a token in any of
// them, and an access log is read by more people, and kept longer, than the token
// was meant to be. A referer that does not parse is "", never the raw header; an
// opaque one, "data:…" or "mailto:…", keeps only its scheme.
func trimReferer(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Opaque != "" {
		return u.Scheme + ":"
	}
	trimmed := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}
	return trimmed.String()
}

// clientIP returns the address r came from: collage.ClientIP, which believes
// X-Forwarded-For only from the application's Server.TrustedProxies. When it
// cannot name one — a trusted proxy forwarded a hop that is not an address — the
// connection's own address is logged.
func clientIP(r *http.Request) string {
	if addr := collage.ClientIP(r); addr.IsValid() {
		return addr.String()
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// under reports whether urlPath is prefix or below it, by whole segments, a
// trailing slash on prefix or not: "/healthz" and "/healthz/" cover "/healthz"
// and "/healthz/db", not "/healthz-report".
func under(urlPath, prefix string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	return prefix == "" || urlPath == prefix || strings.HasPrefix(urlPath, prefix+"/")
}

// recorder counts what is written through it. Flush and Hijack pass through, and
// Unwrap lets http.ResponseController reach the connection, so an event stream and
// a WebSocket work beneath it.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *recorder) WriteHeader(status int) {
	// A 1xx is informational and followed by the real status.
	if w.status == 0 && status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *recorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *recorder) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil && w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}

func (w *recorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }
