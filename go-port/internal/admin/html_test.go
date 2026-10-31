package admin

import (
	"strings"
	"testing"
)

const xss = `<img src=x onerror=1>`

// notes() and errorPage() carry dynamic text (Drive error strings today).
// A string value must be escaped like any other value, as the TS html“
// tag escaped every interpolation.
func TestH_EscapesStringValues(t *testing.T) {
	for name, out := range map[string]string{
		"notes":     notes([]string{xss}, "note").HTML,
		"errorPage": errorPage(502, xss),
		"h":         h(`<p>`, xss, `</p>`).HTML,
	} {
		if strings.Contains(out, "<img") {
			t.Errorf("%s: value not escaped: %s", name, out)
		}
		if !strings.Contains(out, "&lt;img") {
			t.Errorf("%s: escaped value missing: %s", name, out)
		}
	}
}

func TestH_TemplateTextAndSafePassThrough(t *testing.T) {
	got := h(`<p class="a">`, raw(`<b>bold</b>`), `</p>`).HTML
	if got != `<p class="a"><b>bold</b></p>` {
		t.Fatalf("got %s", got)
	}
}
