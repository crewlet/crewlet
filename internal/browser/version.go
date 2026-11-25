package browser

import (
	"fmt"
	"strconv"
	"strings"
)

// ChromiumMajor is a Chromium version's major, the first of its four
// numbers; false for a version that is not four numbers.
func ChromiumMajor(version string) (int, bool) {
	numbers, ok := chromiumNumbers(version)
	if !ok {
		return 0, false
	}
	return numbers[0], true
}

// Outdated reports whether version, a Chromium version as a box's ensure
// reads it, is older than [ExpectedChromiumVersion], the build the pinned
// Playwright expects. It warns and never withholds (the package doc, "A
// seat's browser"); a version that is not four numbers is an error.
func Outdated(version string) (bool, error) {
	return olderThan(version, ExpectedChromiumVersion)
}

// olderThan reports whether version is older than expected, each compared as
// four numbers, never as text: 159.0.8154.10 is newer than 159.0.8154.6.
func olderThan(version, expected string) (bool, error) {
	got, ok := chromiumNumbers(version)
	if !ok {
		return false, fmt.Errorf("browser: %q is no Chromium version: want four numbers, as 159.0.8154.6", version)
	}
	want, ok := chromiumNumbers(expected)
	if !ok {
		return false, fmt.Errorf("browser: the expected Chromium version %q is not four numbers", expected)
	}
	for i := range got {
		if got[i] != want[i] {
			return got[i] < want[i], nil
		}
	}
	return false, nil
}

// chromiumNumbers is version's four numbers, each of ASCII digits alone.
func chromiumNumbers(version string) ([4]int, bool) {
	var out [4]int
	parts := strings.Split(version, ".")
	if len(parts) != len(out) {
		return out, false
	}
	for i, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return out, false
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
