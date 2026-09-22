package credential_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/iam/credential"
)

// THE ZERO VALUE IS INVALID, AND IT IS THE DANGEROUS ONE.
//
// A bool here would default to false, which reads as "optional" — so a company
// that declared nothing would silently be the company with no second factor,
// and nothing about the configuration would look wrong.
func TestTheZeroSecondFactorIsRefusedRatherThanRead(t *testing.T) {
	t.Parallel()
	var zero credential.SecondFactor
	if zero.Valid() {
		t.Error("the zero value validated, so a company that declared " +
			"nothing has silently chosen one of the two")
	}
	for _, mode := range credential.SecondFactors {
		if !mode.Valid() {
			t.Errorf("%q is in the list and does not validate", mode)
		}
	}
	if credential.SecondFactor("somethingnewer").Valid() {
		t.Error("a value this build has never heard of validated")
	}
}

// AN UNKNOWN VALUE REQUIRES A SECOND FACTOR.
//
// The safe direction for "must you prove more" is yes. A denylist would read a
// newer peer's value as optional and quietly drop the requirement for the
// length of a rolling upgrade.
func TestAnUnknownSecondFactorStillRequiresOne(t *testing.T) {
	t.Parallel()
	for mode, want := range map[credential.SecondFactor]bool{
		credential.SecondFactorRequired: true,
		credential.SecondFactorOptional: false,
		"":                              true,
		"somethingnewer":                true,
	} {
		if got := mode.Requires(); got != want {
			t.Errorf("%q requires a second factor %v, want %v", mode, got, want)
		}
	}
}
