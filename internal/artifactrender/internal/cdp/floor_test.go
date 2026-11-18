package cdp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
)

// A BROWSER BELOW THE FLOOR IS REFUSED, NAMING BOTH VERSIONS: CheckVersion
// reads the major out of Browser.getVersion's product and refuses one below
// browser.MinChromiumMajor with a *VersionError naming the browser's product
// and the floor, passes the floor itself and anything newer, and refuses a
// product whose major cannot be read.
//
// Mutation: refuse at the floor as well (`<=` for `<`), and a browser of the
// floor's major is refused — red.
func TestABrowserBelowTheFloorIsRefusedNamingBothVersions(t *testing.T) {
	t.Parallel()
	floor := browser.MinChromiumMajor
	for _, tc := range []struct {
		product string
		major   int
		refused string // "older", "unreadable", or "" for a pass
	}{
		{fmt.Sprintf("HeadlessChrome/%d.0.8010.52", floor), floor, ""},
		{fmt.Sprintf("Chrome/%d.0.8103.38", floor+4), floor + 4, ""},
		{fmt.Sprintf("HeadlessChrome/%d.0.7977.82", floor-1), floor - 1, "older"},
		{"HeadlessChrome", 0, "unreadable"},
		{"Chromium/x.0.1.2", 0, "unreadable"},
	} {
		t.Run(tc.product, func(t *testing.T) {
			t.Parallel()
			c, p := socketPair(t, nil)
			go func() {
				if r, err := p.recv(); err == nil {
					_ = p.answer(r, map[string]any{"product": tc.product, "protocolVersion": "1.3"})
				}
			}()
			v, err := CheckVersion(bounded(t), c.Browser())
			var older *VersionError
			switch tc.refused {
			case "":
				if err != nil || v.Major != tc.major || v.Product != tc.product {
					t.Fatalf("CheckVersion = %+v, %v; want major %d", v, err, tc.major)
				}
			case "older":
				if !errors.As(err, &older) || older.Major != tc.major ||
					!strings.Contains(err.Error(), tc.product) || !strings.Contains(err.Error(), "major "+strconv.Itoa(floor)) {
					t.Fatalf("CheckVersion refused with %v; want a *VersionError naming %s and major %d", err, tc.product, floor)
				}
			case "unreadable":
				if err == nil || errors.As(err, &older) || !strings.Contains(err.Error(), strconv.Quote(tc.product)) {
					t.Fatalf("CheckVersion = %+v, %v; want a refusal naming the product", v, err)
				}
			}
		})
	}
}
