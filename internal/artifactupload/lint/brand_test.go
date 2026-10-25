package lint_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// A PAGE OPTS OUT OF THE BRAND ON ITS <html> ELEMENT ALONE: the attribute on
// the first element, whatever comes before it that is not an element, and
// nowhere else, so a stray attribute deep in a page never unbrands it and a
// page cannot opt out by a value that is not "off".
//
// Mutation: read the attribute from any element and the body case opts out.
func TestAPageOptsOutOfTheBrandOnItsHTMLElementAlone(t *testing.T) {
	t.Parallel()
	for page, want := range map[string]bool{
		`<!doctype html><html data-crewlet-brand="off"><body></body></html>`:       true,
		"\xef\xbb\xbf<!-- made by hand -->\n<html lang=en DATA-CREWLET-BRAND=OFF>": true,
		`<html data-crewlet-brand=" off ">`:                                        true,
		`<!doctype html><html><body data-crewlet-brand="off"></body></html>`:       false,
		`<html data-crewlet-brand="on">`:                                           false,
		`<html data-crewlet-brand>`:                                                false,
		`<html>`:                                                                   false,
		`<body data-crewlet-brand="off">`:                                          false,
		`text first <html data-crewlet-brand="off">`:                               false,
		``: false,
	} {
		if got := lint.BrandOff([]byte(page)); got != want {
			t.Errorf("BrandOff(%q) = %v, want %v", page, got, want)
		}
	}
}
