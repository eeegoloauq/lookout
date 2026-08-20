package web

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eeegoloauq/lookout/internal/demo"
)

// TestRenderDemoPage is a design tool, not an assertion: it writes the page
// with a day of plausible history to /tmp so a change to the layout can be
// looked at in a browser instead of imagined. The data is the same one
// `lookout demo` serves, so what is reviewed here is what ships.
//
//	LOOKOUT_RENDER=1 go test ./internal/web -run RenderDemoPage
func TestRenderDemoPage(t *testing.T) {
	if os.Getenv("LOOKOUT_RENDER") == "" {
		t.Skip("set LOOKOUT_RENDER=1 to write /tmp/lookout-page.html")
	}
	m, err := demo.Monitor(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body := get(t, New(m, "v0.1.0 (0000000) · 20 Aug 2026"), "/").Body.String()
	if err := os.WriteFile("/tmp/lookout-page.html", []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	open := strings.Replace(body, `class="toggle"`, `class="toggle" checked`, 1)
	if err := os.WriteFile("/tmp/lookout-page-open.html", []byte(open), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The demo board is a shipped surface: if it stops rendering, `lookout demo`
// and every screenshot made from it break with it.
func TestDemoBoardRenders(t *testing.T) {
	m, err := demo.Monitor(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body := get(t, New(m, "test"), "/").Body.String()
	for _, want := range []string{"Website API", "Domains", "example.com", "slowest 5%"} {
		if !strings.Contains(body, want) {
			t.Errorf("demo page is missing %q", want)
		}
	}
	if strings.Contains(body, "192.168.") {
		t.Error("demo page leaks a private address")
	}
}
