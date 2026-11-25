package browser

import (
	"strconv"
	"strings"
	"testing"
)

// A BROWSER OLDER THAN THE BUILD THE ENGINE KNEW IS OUTDATED, and no other:
// each of a version's four numbers is compared as a number, the first that
// differs deciding, so 159.0.8154.10 is newer than 159.0.8154.6; a version
// that is not four numbers is an error, never read as either; and
// ChromiumMajor is the first number. Outdated holds a box's version to
// ExpectedChromiumVersion, whichever release the pin names.
//
// Mutation: compare the versions as text, and 159.0.8154.10 reads outdated.
func TestABrowserOlderThanTheBuildTheEngineKnewIsOutdated(t *testing.T) {
	t.Parallel()
	const expected = "159.0.8154.6"
	for version, older := range map[string]bool{
		"157.0.8103.50": true, "158.0.9999.1": true, "159.0.8154.5": true,
		"159.0.8154.6": false, "159.0.8154.10": false, "160.0.1.1": false,
	} {
		got, err := olderThan(version, expected)
		if err != nil || got != older {
			t.Errorf("%s against %s reads outdated %t (%v), want %t", version, expected, got, err, older)
		}
	}
	for _, malformed := range []string{"", "159", "159.0.8154", "159.0.8154.6.1", "159.0.8154.x", "159..8154.6",
		"+159.0.8154.6", "159.0.8154.6 "} {
		if _, err := olderThan(malformed, expected); err == nil {
			t.Errorf("%q is no Chromium version and reads as one", malformed)
		}
		if _, ok := ChromiumMajor(malformed); ok {
			t.Errorf("%q is no Chromium version and has a major", malformed)
		}
	}
	if major, ok := ChromiumMajor("157.0.8103.50"); !ok || major != 157 {
		t.Errorf("ChromiumMajor(157.0.8103.50) = %d, %t, want 157", major, ok)
	}
	numbers := strings.Split(ExpectedChromiumVersion, ".")
	first, err := strconv.Atoi(numbers[0])
	if err != nil {
		t.Fatalf("ExpectedChromiumVersion %q is not four numbers: %v", ExpectedChromiumVersion, err)
	}
	earlier := strconv.Itoa(first-1) + "." + strings.Join(numbers[1:], ".")
	if old, err := Outdated(ExpectedChromiumVersion); err != nil || old {
		t.Errorf("the build the pin expects reads outdated %t (%v)", old, err)
	}
	if old, err := Outdated(earlier); err != nil || !old {
		t.Errorf("%s, a major before the build the pin expects, reads outdated %t (%v)", earlier, old, err)
	}
}
