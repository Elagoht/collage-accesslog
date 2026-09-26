// Package accesslog is a collage plugin that writes one log line per request.
//
//	app, err := collage.New(&collage.Config{
//		Plugins: []collage.Plugin{accesslog.New(accesslog.Options{})},
//	})
//
// A line carries the method, path, status, bytes written, duration, client
// address, user agent, referer and request id, as slog attributes: text or JSON is
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
	"net/netip"
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
	// Skip are path prefixes never logged. Default ["/_collage/", "/healthz"]:
	// collage's development endpoints, and the health check a load balancer
	// polls every few seconds. An empty list in configuration logs everything.
	Skip []string `json:"skip"`
	// Sample is the fraction of successful (2xx) responses logged, between 0 and
	// 1: 0.1 logs one in ten. Zero, the default, logs every one. A redirect, a
	// client error and a server error are always logged: they are the lines
	// someone goes looking for.
	Sample float64 `json:"sample"`
	// TrustProxy reads the client's address from X-Forwarded-For, or
	// X-Real-IP, when the request came from a trusted proxy. Without it those
	// headers are ignored, since anybody can send them.
	TrustProxy bool `json:"trustProxy"`
	// TrustedProxies are the addresses and CIDR ranges of the proxies in front
	// of the site. Empty trusts loopback and private addresses, which is what a
	// reverse proxy on the same host or network has.
	TrustedProxies []string `json:"trustedProxies"`
	// RequestIDHeader is the header a request id is read from and sent back in.
	// Default "X-Request-ID".
	RequestIDHeader string `json:"requestIdHeader"`
}

// Plugin writes the lines.
type Plugin struct {
	opts    Options
	log     *slog.Logger
	trusted []netip.Prefix
}

// New returns a plugin with opts as its starting point, which the application's
// own configuration is then decoded over.
func New(opts Options) *Plugin { return &Plugin{opts: opts} }

func (p *Plugin) Name() string                   { return Name }
func (p *Plugin) Version() string                { return "0.1.1" }
func (p *Plugin) Shutdown(context.Context) error { return nil }

var _ collage.Plugin = (*Plugin)(nil)

var headerName = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// Init reads the configuration and wraps every request.
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	if p.opts.Skip == nil {
		p.opts.Skip = []string{"/_collage/", "/healthz"}
	}
	if err := host.Config(&p.opts); err != nil {
		return err
	}
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
	for _, s := range o.TrustedProxies {
		prefix, err := netip.ParsePrefix(s)
		if err != nil {
			addr, addrErr := netip.ParseAddr(s)
			if addrErr != nil {
				return fmt.Errorf("accesslog: trusted proxy %q is neither an address nor a CIDR range", s)
			}
			prefix = netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen())
		}
		p.trusted = append(p.trusted, prefix.Masked())
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
			if strings.HasPrefix(r.URL.Path, prefix) {
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
		slog.String("ip", p.clientIP(r)),
		slog.String("user_agent", r.UserAgent()),
		slog.String("referer", r.Referer()),
		slog.String("request_id", id),
	)
}

// clientIP returns the address r came from.
//
// Behind trusted proxies it walks X-Forwarded-For from the right, past every
// proxy the site trusts, and takes the first address that is not one: each proxy
// appends the address it was reached from, so everything to the right of that
// point was written by a proxy the site runs, and everything to the left by the
// client, who can write anything there.
func (p *Plugin) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	if !p.opts.TrustProxy || !p.isTrusted(addr) {
		return addr.String()
	}
	if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
		hops := strings.Split(strings.Join(values, ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
			if err != nil {
				// A trusted proxy writes an address; this was written by the
				// client, so the nearest trusted hop is as far as can be told.
				break
			}
			addr = hop.Unmap()
			if !p.isTrusted(addr) {
				break
			}
		}
		return addr.String()
	}
	if realIP, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
		return realIP.Unmap().String()
	}
	return addr.String()
}

func (p *Plugin) isTrusted(addr netip.Addr) bool {
	if p.trusted == nil {
		return addr.IsLoopback() || addr.IsPrivate()
	}
	for _, prefix := range p.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
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
