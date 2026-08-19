package web

import (
	"os"
	"strings"
	"testing"
	"time"
)

const demoCfg = `
checks:
  - name: Nginx Proxy Manager
    group: Core
    type: http
    url: http://198.51.100.52:81
  - name: Forgejo
    group: Core
    type: http
    url: http://198.51.100.27:3000
  - name: Immich
    group: Services
    type: http
    url: http://198.51.100.24:2283
  - name: Vaultwarden
    group: Services
    type: http
    url: https://vault.example.dev
  - name: Website
    group: Public Sites
    type: http
    url: https://example.ru
  - name: RAG (chat backend)
    group: Public Sites
    type: http
    url: https://example.com/api/health
`

// TestRenderDemoPage is a design tool, not an assertion: it writes the page
// with a day of plausible history to /tmp so a change to the layout can be
// looked at in a browser instead of imagined.
//
//	LOOKOUT_RENDER=1 go test ./internal/web -run RenderDemoPage
func TestRenderDemoPage(t *testing.T) {
	if os.Getenv("LOOKOUT_RENDER") == "" {
		t.Skip("set LOOKOUT_RENDER=1 to write /tmp/lookout-page.html")
	}
	m := testMonitor(t, demoCfg)
	now := time.Now()
	day := now.Add(-24 * time.Hour)
	feed(t, m, "Nginx Proxy Manager", strings.Repeat("U", 1440), day, 2*time.Millisecond, 200)
	feed(t, m, "Forgejo", strings.Repeat("U", 1440), day, 4*time.Millisecond, 200)
	feed(t, m, "Immich", strings.Repeat("U", 700)+strings.Repeat("D", 25)+strings.Repeat("U", 715), day, 38*time.Millisecond, 200)
	feed(t, m, "Vaultwarden", strings.Repeat("U", 1440), day, 61*time.Millisecond, 200)
	feed(t, m, "Website", strings.Repeat("U", 1300)+strings.Repeat("UD", 70), day, 74*time.Millisecond, 200)
	feed(t, m, "RAG (chat backend)", strings.Repeat("U", 1420)+strings.Repeat("D", 20), day, 5000*time.Millisecond, 502)

	body := get(t, New(m, "50ae3a3"), "/").Body.String()
	if err := os.WriteFile("/tmp/lookout-page.html", []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	open := strings.Replace(body, `<details class="row down"`, `<details open class="row down"`, 1)
	if err := os.WriteFile("/tmp/lookout-page-open.html", []byte(open), 0o644); err != nil {
		t.Fatal(err)
	}
}
