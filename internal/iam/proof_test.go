package iam

import "testing"

// VALID IS THE TWO VALUES A FILE MAY WRITE, AND NOTHING ELSE.
//
// The zero is not one of them: it is the UNSET setting, which internal/config
// accepts beside the two and reads as required (`APIAuth.SecondFactor`), so
// `optional` is always somebody's explicit choice. What Valid guards is a
// spelling neither of the two — refused, rather than read as either — because
// read as optional it would be a company with no second factor that nothing
// about its configuration said it chose.
func TestValidIsOnlyTheTwoValuesAFileMayWrite(t *testing.T) {
	t.Parallel()
	for _, mode := range SecondFactors {
		if !mode.Valid() {
			t.Errorf("%q is in the list and does not validate", mode)
		}
	}
	var unset SecondFactor
	if unset.Valid() {
		t.Error("the unset value validated, and Valid is the two values a " +
			"file writes — the unset setting is internal/config's to default")
	}
	if SecondFactor("somethingnewer").Valid() {
		t.Error("a value this build has never heard of validated")
	}
}
