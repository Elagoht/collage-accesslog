package accesslog_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accesslog "github.com/Elagoht/collage-accesslog"
	"github.com/Elagoht/collage/pkg/collage"
	"github.com/Elagoht/collage/pkg/collagetest"
)

// refuser is a plugin whose middleware answers 403 for every path under
// /banned/ without calling the next handler — the shape of elagoht/fail2ban's
// ban check, which its README asks to be listed first.
type refuser struct{}

func (refuser) Name() string                   { return "test/refuser" }
func (refuser) Version() string                { return "0" }
func (refuser) Shutdown(context.Context) error { return nil }
func (refuser) Init(_ context.Context, host collage.Host) error {
	return host.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/banned/") || strings.HasPrefix(r.URL.Path, "/healthz") {
				http.Error(w, "banned", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
}

// unseenSite lists refuser before accesslog and mounts a bare app.Handle at
// /api/ that answers 401 and reports the request id it was served under.
func unseenSite(t *testing.T, opts accesslog.Options) (http.Handler, *logBuffer, *string) {
	t.Helper()
	logs := &logBuffer{}
	opts.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	seenID := new(string)
	site := collagetest.NewSite(t,
		collagetest.WithPlugins(refuser{}, accesslog.New(opts)),
		collagetest.WithSetup(func(app *collage.App) error {
			return app.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				*seenID = accesslog.RequestID(r.Context())
				http.Error(w, "nope", http.StatusUnauthorized)
			}))
		}),
	)
	return site.App.Handler(), logs, seenID
}

// PLUGA-C03, regression. A request refused by a plugin listed before
// accesslog — fail2ban's ban — is logged, once, with its status.
func TestARequestRefusedAheadOfTheMiddlewareIsLogged(t *testing.T) {
	h, logs, _ := unseenSite(t, accesslog.Options{})
	for range 5 {
		r := httptest.NewRequest(http.MethodGet, "/banned/x", nil)
		r.RemoteAddr = "203.0.113.9:5555"
		if code := do(h, r).Code; code != http.StatusForbidden {
			t.Fatalf("setup: status %d", code)
		}
	}
	lines := logs.lines(t)
	if len(lines) != 5 {
		t.Fatalf("%d lines for 5 refused requests: %v", len(lines), lines)
	}
	l := lines[0]
	if l["status"] != float64(http.StatusForbidden) || l["path"] != "/banned/x" || l["ip"] != "203.0.113.9" || l["method"] != "GET" {
		t.Errorf("line = %v", l)
	}
	if id, _ := l["request_id"].(string); len(id) != 32 {
		t.Errorf("request_id = %v", l["request_id"])
	}
	if _, ok := l["bytes"]; ok {
		t.Errorf("bytes = %v: the plugin did not see the body", l["bytes"])
	}
}

// PLUGA-C03, regression. A bare app.Handle is logged too, and its handler
// reads the id the line carries.
func TestABareHandleIsLogged(t *testing.T) {
	h, logs, seenID := unseenSite(t, accesslog.Options{})
	r := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	r.Header.Set("X-Request-ID", "abc-123")
	if code := do(h, r).Code; code != http.StatusUnauthorized {
		t.Fatalf("setup: status %d", code)
	}
	lines := logs.lines(t)
	if len(lines) != 1 || lines[0]["status"] != float64(http.StatusUnauthorized) || lines[0]["path"] != "/api/login" {
		t.Fatalf("lines = %v", lines)
	}
	if lines[0]["request_id"] != "abc-123" || *seenID != "abc-123" {
		t.Errorf("logged id %v, handler's id %q, want abc-123 both", lines[0]["request_id"], *seenID)
	}
}

// A request the middleware sees is logged once, by it, with its bytes.
func TestARequestTheMiddlewareSeesIsLoggedOnce(t *testing.T) {
	h, logs, _ := unseenSite(t, accesslog.Options{})
	w := get(h, "/")
	lines := logs.lines(t)
	if len(lines) != 1 {
		t.Fatalf("%d lines: %v", len(lines), lines)
	}
	if lines[0]["bytes"] != float64(w.Body.Len()) || lines[0]["request_id"] != w.Header().Get("X-Request-ID") {
		t.Errorf("line = %v, header id %q", lines[0], w.Header().Get("X-Request-ID"))
	}
}

// Skip and sampling hold for a request the middleware never saw.
func TestSkipAndSampleHoldAheadOfTheMiddleware(t *testing.T) {
	h, logs, _ := unseenSite(t, accesslog.Options{Sample: 1e-9})
	get(h, "/healthz")
	if l := logs.lines(t); len(l) != 0 {
		t.Errorf("a skipped path was logged: %v", l)
	}
	get(h, "/banned/x")
	if l := logs.lines(t); len(l) != 1 {
		t.Errorf("a 403 is always logged: %v", l)
	}
}
