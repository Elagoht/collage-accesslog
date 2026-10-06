package accesslog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	accesslog "github.com/Elagoht/collage-accesslog"
	"github.com/Elagoht/collage/pkg/collage"
)

// logBuffer is a bytes.Buffer safe for the concurrent writes of a slog handler.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines decodes every JSON line written; a line's values are strings, numbers,
// or nested objects, so they are decoded into the one type that holds them all.
func (b *logBuffer) lines(t *testing.T) []map[string]any { // any: JSON of unknown shape
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any // any: JSON of unknown shape
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any // any: JSON of unknown shape
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		if m["msg"] == "request" {
			out = append(out, m)
		}
	}
	return out
}

type setup struct {
	opts      accesslog.Options
	config    string
	appLogger bool // leave Options.Logger unset and give the application the buffer
	handler   http.Handler
}

func site(t *testing.T, s setup) (http.Handler, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	cfg := &collage.Config{
		Server: collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html":  {Data: []byte(`<p>{{.}}</p>`)},
			"t/ok.html": {Data: []byte(`<p>hello</p>`)},
		}, Root: "t"},
	}
	if s.appLogger {
		cfg.Logger = logger
	} else {
		s.opts.Logger = logger
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	cfg.Plugins = []collage.Plugin{accesslog.New(s.opts)}
	if s.config != "" {
		cfg.PluginConfig = map[string]json.RawMessage{accesslog.Name: json.RawMessage(s.config)}
	}
	app, err := collage.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []*collage.Page{
		collage.NewPage("home").WithContent(collage.NewFragment("home", "ok.html").Build()).WithPath("en", "/").Build(),
		collage.NewPage("id").WithContent(collage.NewFragment("id", "p.html").WithData(collage.Load(
			func(_ context.Context, rc *collage.RenderContext) (template.HTML, error) {
				return template.HTML(accesslog.RequestID(rc.Context())), nil
			})).Build()).WithPath("en", "/id").Build(),
		collage.NewPage("broken").WithContent(collage.NewFragment("broken", "ok.html").Required().WithData(collage.Load(
			func(context.Context, *collage.RenderContext) (string, error) {
				return "", errors.New("down")
			})).Build()).WithPath("en", "/broken").Build(),
		collage.NewPage("health").WithContent(collage.NewFragment("health", "ok.html").Build()).WithPath("en", "/healthz").Build(),
	} {
		if err := app.RegisterPage(page); err != nil {
			t.Fatal(err)
		}
	}
	if s.handler != nil {
		if err := app.Handle("/raw/", s.handler); err != nil {
			t.Fatal(err)
		}
	}
	return app.Handler(), logs
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func get(h http.Handler, path string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	return do(h, r)
}

func TestOneLinePerRequest(t *testing.T) {
	h, logs := site(t, setup{})
	r := httptest.NewRequest(http.MethodGet, "/?secret=1", nil)
	r.RemoteAddr = "203.0.113.9:4000"
	r.Header.Set("User-Agent", "probe/1.0")
	r.Header.Set("Referer", "https://example.org/")
	w := do(h, r)

	lines := logs.lines(t)
	if len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}
	l := lines[0]
	for key, want := range map[string]any{ // any: JSON of unknown shape
		"level":      "INFO",
		"method":     "GET",
		"path":       "/",
		"status":     float64(200),
		"bytes":      float64(w.Body.Len()),
		"ip":         "203.0.113.9",
		"user_agent": "probe/1.0",
		"referer":    "https://example.org/",
		"request_id": w.Header().Get("X-Request-ID"),
	} {
		if l[key] != want {
			t.Errorf("%s = %v, want %v", key, l[key], want)
		}
	}
	if d, ok := l["duration"].(float64); !ok || d <= 0 {
		t.Errorf("duration = %v", l["duration"])
	}
	if strings.Contains(l["path"].(string), "secret") {
		t.Error("the query was logged")
	}
}

func TestServerErrorIsAnError(t *testing.T) {
	h, logs := site(t, setup{})
	get(h, "/broken")
	if l := logs.lines(t); len(l) != 1 || l[0]["level"] != "ERROR" || l[0]["status"] != float64(500) {
		t.Errorf("lines = %v", l)
	}
}

// The application's logger is used when none is given.
func TestApplicationLogger(t *testing.T) {
	h, logs := site(t, setup{appLogger: true})
	get(h, "/")
	if l := logs.lines(t); len(l) != 1 {
		t.Errorf("lines = %v", l)
	}
}

func TestRequestID(t *testing.T) {
	h, logs := site(t, setup{})

	// A sensible id is kept, and reaches the page's data handler.
	w := get(h, "/id", "X-Request-ID", "abc-123")
	if w.Header().Get("X-Request-ID") != "abc-123" || !strings.Contains(w.Body.String(), "<p>abc-123</p>") {
		t.Errorf("kept id: header %q, body %s", w.Header().Get("X-Request-ID"), w.Body.String())
	}

	// A new one is made when there is none, or it is not an id.
	for _, sent := range []string{"", "has space", strings.Repeat("a", 129), "a\"b"} {
		w := get(h, "/id", "X-Request-ID", sent)
		got := w.Header().Get("X-Request-ID")
		if got == sent || len(got) != 32 || !strings.Contains(w.Body.String(), got) {
			t.Errorf("sent %q: got %q, body %s", sent, got, w.Body.String())
		}
	}
	if a, b := get(h, "/").Header().Get("X-Request-ID"), get(h, "/").Header().Get("X-Request-ID"); a == b {
		t.Error("two requests were given one id")
	}
	for _, l := range logs.lines(t) {
		if l["request_id"] == "" {
			t.Errorf("line without an id: %v", l)
		}
	}
	if accesslog.RequestID(context.Background()) != "" {
		t.Error("an id outside a request")
	}
}

func TestSkip(t *testing.T) {
	h, logs := site(t, setup{})
	get(h, "/healthz")
	get(h, "/_collage/anything")
	if l := logs.lines(t); len(l) != 0 {
		t.Errorf("skipped paths logged: %v", l)
	}
	// The id is given all the same.
	if get(h, "/healthz").Header().Get("X-Request-ID") == "" {
		t.Error("a skipped request has no id")
	}

	h, logs = site(t, setup{config: `{"skip": []}`})
	get(h, "/healthz")
	if l := logs.lines(t); len(l) != 1 {
		t.Errorf("an empty skip list should log everything: %v", l)
	}
}

// collage redirects a path with dot segments ahead of all middleware, so the
// redirect is not logged, and /_collage/../ cannot pass as a skipped path.
func TestUncleanPathIsRedirectedFirst(t *testing.T) {
	h, logs := site(t, setup{})
	if rec := get(h, "/_collage/../"); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/" {
		t.Fatalf("/_collage/../ = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if l := logs.lines(t); len(l) != 0 {
		t.Errorf("the redirect was logged: %v", l)
	}
	get(h, "/")
	if l := logs.lines(t); len(l) != 1 || l[0]["path"] != "/" {
		t.Errorf("the clean request: %v", l)
	}
}

// Sampling drops successful responses, never errors.
func TestSample(t *testing.T) {
	h, logs := site(t, setup{opts: accesslog.Options{Sample: 1e-9}})
	for range 50 {
		get(h, "/")
	}
	get(h, "/nowhere")
	get(h, "/broken")
	l := logs.lines(t)
	if len(l) != 2 || l[0]["status"] != float64(404) || l[1]["status"] != float64(500) {
		t.Errorf("lines = %v", l)
	}
}

func TestProxyAwareAddress(t *testing.T) {
	cases := []struct {
		name, remote, xff, want string
		opts                    accesslog.Options
	}{
		{"untrusted headers ignored", "127.0.0.1:1", "198.51.100.7", "127.0.0.1", accesslog.Options{}},
		{"behind a local proxy", "127.0.0.1:1", "198.51.100.7", "198.51.100.7", accesslog.Options{TrustProxy: true}},
		{"a client's forged hop", "10.0.0.2:1", "1.1.1.1, 198.51.100.7, 10.0.0.3", "198.51.100.7", accesslog.Options{TrustProxy: true}},
		{"a public peer is not a proxy", "203.0.113.1:1", "198.51.100.7", "203.0.113.1", accesslog.Options{TrustProxy: true}},
		{"named proxies", "203.0.113.1:1", "198.51.100.7", "198.51.100.7", accesslog.Options{TrustProxy: true, TrustedProxies: []string{"203.0.113.0/24"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, logs := site(t, setup{opts: tc.opts})
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			r.Header.Set("X-Forwarded-For", tc.xff)
			do(h, r)
			if l := logs.lines(t); len(l) != 1 || l[0]["ip"] != tc.want {
				t.Errorf("lines = %v, want ip %s", l, tc.want)
			}
		})
	}
}

func TestMisconfigurationStopsStartup(t *testing.T) {
	for _, config := range []string{
		`{"sample": 1.5}`,
		`{"sample": -0.1}`,
		`{"skip": ["healthz"]}`,
		`{"trustedProxies": ["not-an-address"]}`,
		`{"requestIdHeader": "X Request"}`,
	} {
		h, _ := site(t, setup{config: config})
		if code := get(h, "/").Code; code != http.StatusServiceUnavailable {
			t.Errorf("%s: status %d, want 503", config, code)
		}
	}
}

// A stream and a WebSocket beneath the middleware still reach the connection, and
// the line counts what the stream wrote.
func TestFlushAndHijackPassThrough(t *testing.T) {
	var flushed, hijackable bool
	h, logs := site(t, setup{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("data: 1\n\n"))
		_, isFlusher := w.(http.Flusher)
		flushed = isFlusher && http.NewResponseController(w).Flush() == nil
		_, hijackable = w.(http.Hijacker)
	})})
	get(h, "/raw/events")
	if !flushed || !hijackable {
		t.Errorf("flush %v, hijacker %v", flushed, hijackable)
	}
	if l := logs.lines(t); len(l) != 1 || l[0]["status"] != float64(202) || l[0]["bytes"] != float64(9) {
		t.Errorf("lines = %v", l)
	}
}
