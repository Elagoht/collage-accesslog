package accesslog_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func refererLogged(t *testing.T, s setup, referer string) string {
	t.Helper()
	h, logs := site(t, s)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if referer != "" {
		r.Header["Referer"] = []string{referer}
	}
	do(h, r)
	lines := logs.lines(t)
	if len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}
	got, ok := lines[0]["referer"].(string)
	if !ok {
		t.Fatalf("referer = %v, not a string", lines[0]["referer"])
	}
	return got
}

// TestRefererIsTrimmed: by default a referer is logged as its scheme, host and
// path, so a token in its query, fragment or userinfo never reaches the log.
func TestRefererIsTrimmed(t *testing.T) {
	for _, tc := range []struct{ name, referer, want string }{
		{"plain", "https://example.org/", "https://example.org/"},
		{"no path", "https://example.org", "https://example.org"},
		{"query", "https://example.org/reset?token=s3cret", "https://example.org/reset"},
		{"fragment", "https://example.org/cb#access_token=s3cret", "https://example.org/cb"},
		{"empty query", "https://example.org/a?", "https://example.org/a"},
		{"userinfo", "https://user:s3cret@example.org/a", "https://example.org/a"},
		{"port", "http://example.org:8080/a?x=s3cret", "http://example.org:8080/a"},
		{"escaped path", "https://example.org/a%20b/%3Fq?x=s3cret", "https://example.org/a%20b/%3Fq"},
		{"relative", "/a?token=s3cret", "/a"},
		{"opaque", "data:text/html,s3cret", "data:"},
		{"mailto", "mailto:s3cret@example.org", "mailto:"},
		{"bad host", "http://[::1", ""},
		{"bad escape", "https://example.org/%zz?token=s3cret", ""},
		{"control character", "https://example.org/a\x7f?token=s3cret", ""},
		{"none", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := refererLogged(t, setup{}, tc.referer); got != tc.want {
				t.Errorf("referer %q logged as %q, want %q", tc.referer, got, tc.want)
			}
		})
	}
}

// TestFullReferer: fullReferer logs the header exactly as it came, as before.
func TestFullReferer(t *testing.T) {
	for _, referer := range []string{
		"https://user:pw@example.org/reset?token=s3cret#frag",
		"http://[::1",
	} {
		if got := refererLogged(t, setup{config: `{"fullReferer": true}`}, referer); got != referer {
			t.Errorf("referer %q logged as %q", referer, got)
		}
	}
}
