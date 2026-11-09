package cdp

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// A BROWSER BELOW THE FLOOR IS REFUSED, NAMING BOTH VERSIONS: CheckVersion
// reads the major out of Browser.getVersion's product and refuses one below
// MinMajor with a *VersionError naming the browser's product and the floor,
// passes the floor itself and anything newer, and refuses a product whose
// major cannot be read.
//
// Mutation: refuse at the floor as well (`<=` for `<`), and a browser of
// MinMajor is refused — red.
func TestABrowserBelowTheFloorIsRefusedNamingBothVersions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		product string
		major   int
		refused string // "older", "unreadable", or "" for a pass
	}{
		{fmt.Sprintf("HeadlessChrome/%d.0.8010.52", MinMajor), MinMajor, ""},
		{fmt.Sprintf("Chrome/%d.0.8103.38", MinMajor+4), MinMajor + 4, ""},
		{fmt.Sprintf("HeadlessChrome/%d.0.7977.82", MinMajor-1), MinMajor - 1, "older"},
		{"HeadlessChrome", 0, "unreadable"},
		{"Chromium/x.0.1.2", 0, "unreadable"},
	} {
		t.Run(tc.product, func(t *testing.T) {
			t.Parallel()
			c, p := pipePair(t, nil)
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
					!strings.Contains(err.Error(), tc.product) || !strings.Contains(err.Error(), "major "+strconv.Itoa(MinMajor)) {
					t.Fatalf("CheckVersion refused with %v; want a *VersionError naming %s and major %d", err, tc.product, MinMajor)
				}
			case "unreadable":
				if err == nil || errors.As(err, &older) || !strings.Contains(err.Error(), strconv.Quote(tc.product)) {
					t.Fatalf("CheckVersion = %+v, %v; want a refusal naming the product", v, err)
				}
			}
		})
	}
}

// THE PINS MEET THE FLOOR: scripts/chromium-pin.env names exactly the newest
// and the floor builds, each a four-number Chrome for Testing version with a
// SHA-256 for each of the four platforms the engine is released for; the
// newest is at or above MinMajor and the floor is of MinMajor exactly, so the
// suite drives both ends of what the engine promises to drive. Raising the
// floor moves the floor pin in the same change.
//
// Mutation: raise MinMajor by one, and the floor pin is of a major below it —
// red.
func TestThePinsMeetTheFloor(t *testing.T) {
	t.Parallel()
	pins := prereqtest.ReadPins(t, filepath.Join(sourcetree.Root(t), "scripts", "chromium-pin.env"))
	var names []string
	for _, pin := range []string{"NEWEST", "FLOOR"} {
		names = append(names, "CHROMIUM_"+pin+"_VERSION")
		for _, platform := range []string{"LINUX64", "LINUX_ARM64", "MAC_X64", "MAC_ARM64"} {
			names = append(names, "CHROMIUM_"+pin+"_SHA256_"+platform)
		}
	}
	if got := slices.Sorted(maps.Keys(pins)); !slices.Equal(got, slices.Sorted(slices.Values(names))) {
		t.Fatalf("scripts/chromium-pin.env names %q, want exactly %q", got, names)
	}
	version := regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
	digest := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for name, value := range pins {
		want := digest
		if strings.HasSuffix(name, "_VERSION") {
			want = version
		}
		if !want.MatchString(value) {
			t.Errorf("%s=%q is not a %s", name, value, map[bool]string{true: "four-number version", false: "SHA-256"}[want == version])
		}
	}
	major := func(name string) int {
		n, err := strconv.Atoi(strings.SplitN(pins[name], ".", 2)[0])
		if err != nil {
			t.Fatalf("%s=%q names no major", name, pins[name])
		}
		return n
	}
	if got := major("CHROMIUM_NEWEST_VERSION"); got < MinMajor {
		t.Errorf("the newest pin is of major %d, below MinMajor %d", got, MinMajor)
	}
	if got := major("CHROMIUM_FLOOR_VERSION"); got != MinMajor {
		t.Errorf("the floor pin is of major %d and MinMajor is %d: a floor nobody is testing — move the floor pin "+
			"to the newest patch of major %d (scripts/chromium-pin-check.sh prints it) with MinMajor", got, MinMajor, MinMajor)
	}
}
