package iam

import "testing"

// THE ZERO VALUE IS INVALID, AND IT IS THE DANGEROUS ONE.
//
// A bool here would default to false, which reads as "optional" — so a company
// that declared nothing would silently be the company with no second factor,
// and nothing about the configuration would look wrong.
func TestTheZeroSecondFactorIsRefusedRatherThanRead(t *testing.T) {
	t.Parallel()
	var zero SecondFactor
	if zero.Valid() {
		t.Error("the zero value validated, so a company that declared " +
			"nothing has silently chosen one of the two")
	}
	for _, mode := range SecondFactors {
		if !mode.Valid() {
			t.Errorf("%q is in the list and does not validate", mode)
		}
	}
	if SecondFactor("somethingnewer").Valid() {
		t.Error("a value this build has never heard of validated")
	}
}
