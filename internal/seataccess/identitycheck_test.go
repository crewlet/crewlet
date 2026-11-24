package seataccess_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/vendortext"
)

const keysPage = "https://appstoreconnect.apple.com/access/integrations/api"

// A PAGE A STEP NAMES IS LINKED THERE, NEVER REPEATED AS A BUTTON: a sheet
// carries the button only where no step links a page, and always one or the other.
func TestASheetLinksItsPageOnAStepOrAButtonNeverBoth(t *testing.T) {
	t.Parallel()
	linked := []seataccess.GuideStep{{Text: vendortext.Of(vendortext.W("Open "),
		vendortext.UI("Users and Access", "Integrations", "Team Keys").At(keysPage), vendortext.W("."))}}
	plain := []seataccess.GuideStep{{Text: vendortext.Of(vendortext.W("Press "), vendortext.UI("Generate"),
		vendortext.W("."))}}
	button := &seataccess.GuideLink{Label: "Open Team Keys", URL: keysPage}
	for name, tc := range map[string]struct {
		g    seataccess.IdentityGuide
		want string
	}{
		"linked on its step":  {seataccess.IdentityGuide{Steps: linked}, ""},
		"a button, no link":   {seataccess.IdentityGuide{Steps: plain, Link: button}, ""},
		"both":                {seataccess.IdentityGuide{Steps: linked, Link: button}, "repeats it"},
		"neither":             {seataccess.IdentityGuide{Steps: plain}, "no button opens it"},
		"markup in a note":    {seataccess.IdentityGuide{Steps: linked, Note: vendortext.Plain("See {link}.")}, "note"},
		"markup in a hand-in": {seataccess.IdentityGuide{Steps: linked, Back: seataccess.GuideBack{Hint: vendortext.Plain("Press **Add**.")}}, "back hint"},
	} {
		err := tc.g.Check()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: Check() = %v, want nil", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: Check() = %v, want an error naming %q", name, err, tc.want)
		}
	}
}
