package accesslog_test

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	accesslog "github.com/Elagoht/collage-accesslog"
	"github.com/Elagoht/collage/pkg/collage"
	"github.com/Elagoht/collage/pkg/collagetest"
)

// buildReader reads a finished build, as elagoht/deploy does; with one
// registered, the build asks the handler for every file to capture its headers.
type buildReader struct{ files []collage.BuiltFile }

func (*buildReader) Name() string                             { return "test/buildreader" }
func (*buildReader) Version() string                          { return "0" }
func (*buildReader) Init(context.Context, collage.Host) error { return nil }
func (*buildReader) Shutdown(context.Context) error           { return nil }
func (b *buildReader) OnBuildFinished(_ context.Context, ev *collage.BuildFinishedEvent) error {
	b.files = ev.Files
	return nil
}

// A static build's header capture is not a request anyone served: it writes no
// line, and gets no request id, which would differ between the build's two
// asks and could never be a header of the deployed file.
func TestBuildCaptureIsNotLogged(t *testing.T) {
	logs := &logBuffer{}
	reader := &buildReader{}
	var pages []collage.Registrable
	for _, path := range []string{"/", "/a", "/b"} {
		pages = append(pages, collage.NewPage("p"+path).WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", path).Build())
	}
	site := collagetest.NewSite(t,
		collagetest.WithoutHome(),
		collagetest.WithTemplates(map[string]string{"p.html": `<main>home</main>`}),
		collagetest.WithPlugins(
			accesslog.New(accesslog.Options{Logger: slog.New(slog.NewTextHandler(logs, nil))}),
			reader,
		),
		collagetest.WithRegister(pages...),
	)
	report, err := collage.Build(context.Background(), site.App, collage.BuildOptions{OutDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// A request id differing between the build's two asks would be an
	// unstable-header finding.
	for _, f := range report.Findings {
		t.Errorf("finding: %s %s: %s", f.Rule, f.Path, f.Message)
	}
	captured := 0
	for _, f := range reader.files {
		if !f.Captured {
			continue
		}
		captured++
		if f.Status != http.StatusOK {
			t.Errorf("%s: status %d", f.Path, f.Status)
		}
		if f.Headers.Get("X-Request-ID") != "" {
			t.Errorf("%s: the capture carries a request id: %v", f.Path, f.Headers)
		}
	}
	if captured != 3 {
		t.Fatalf("%d pages captured, want 3", captured)
	}
	logs.mu.Lock()
	out := logs.buf.String()
	logs.mu.Unlock()
	if strings.Contains(out, "msg=request") {
		t.Errorf("the capture was logged:\n%s", out)
	}
}
