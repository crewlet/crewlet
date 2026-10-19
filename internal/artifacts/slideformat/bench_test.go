package slideformat

import (
	"strings"
	"testing"
	"time"
)

// A LARGE HOSTILE SLIDE IS REFUSED FAST: tens of thousands of elements, or of
// nested spans, cost a bounded read rather than one that grows with the square
// of the file.
func TestALargeSlideIsRefusedFast(t *testing.T) {
	t.Parallel()
	files := func(string) (string, bool) { return "", false }
	for name, body := range map[string]string{
		"siblings": `<section id="a" style="background:#fff">` + strings.Repeat("<p>x</p>", 30000) + "</section>",
		"nested":   `<section id="a" style="background:#fff"><p>` + strings.Repeat("<span>", 30000) + "</p></section>",
		"svg":      `<section id="a" style="background:#fff"><svg aria-label="x">` + strings.Repeat(`<g>`, 30000) + "</svg></section>",
	} {
		start := time.Now()
		_, found := ParseSlide("slides/a.html", "a", []byte(body), files)
		if len(found) == 0 {
			t.Errorf("%s: a %d byte slide was not refused", name, len(body))
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("%s: refusing took %v", name, took)
		}
	}
}
