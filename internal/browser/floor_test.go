package browser_test

import (
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE PINS MEET THE FLOOR: scripts/chromium-pin.env names exactly the newest
// and the floor builds, each a four-number Chrome for Testing version with a
// SHA-256 for each of the four platforms the engine is released for; the
// newest is at or above MinChromiumMajor and the floor is of it exactly, so the
// suite certifies both ends of what the engine accepts. Raising the floor
// moves the floor pin in the same change.
//
// Mutation: raise MinChromiumMajor by one, and the floor pin is of a major
// below it — red.
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
	floor := browser.MinChromiumMajor
	if got := major("CHROMIUM_NEWEST_VERSION"); got < floor {
		t.Errorf("the newest pin is of major %d, below MinChromiumMajor %d", got, floor)
	}
	if got := major("CHROMIUM_FLOOR_VERSION"); got != floor {
		t.Errorf("the floor pin is of major %d and MinChromiumMajor is %d: a floor nobody is testing — move the floor "+
			"pin to the newest patch of major %d (scripts/chromium-pin-check.sh prints it) with MinChromiumMajor",
			got, floor, floor)
	}
}
