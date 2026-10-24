package artifacts

import (
	"errors"
	"strings"
	"testing"
)

// EVERY AUDIENCE ROUND-TRIPS THROUGH ITS ONE TEXT FORM, which is what a URL
// segment and a tool argument carry.
func TestAnAudienceRoundTripsThroughItsTextForm(t *testing.T) {
	t.Parallel()
	for _, a := range []Audience{
		{Kind: AudienceSeat, ID: "ada"},
		{Kind: AudienceUnit, ID: "platform"},
		{Kind: AudienceCompany},
		{Kind: AudienceUnit, ID: "key:with:colons"},
	} {
		text := a.String()
		got, err := ParseAudience(text)
		if err != nil || got != a {
			t.Errorf("ParseAudience(%q) = %+v, %v; want %+v", text, got, err, a)
		}
	}
	if got := (Audience{Kind: AudienceCompany}).String(); got != "company" {
		t.Errorf("the company audience renders as %q", got)
	}
}

// AN AUDIENCE THE TEXT FORM COULD NOT HAVE PRODUCED IS REFUSED, and so is a
// kind this build does not share with: a later build's `link` is one.
func TestAnAudienceRefusesWhatNamesNoPrincipalSet(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"", "seat", "seat:", "unit:", "company:", "company:acme", "link:abc",
		"seat:has space", "unit:a/b", "seat:" + strings.Repeat("x", MaxAudienceIDBytes+1),
	} {
		if _, err := ParseAudience(s); !errors.Is(err, ErrInvalidAudience) {
			t.Errorf("ParseAudience(%q) = %v, want ErrInvalidAudience", s, err)
		}
	}
	if err := (Audience{Kind: AudienceCompany, ID: "x"}).Validate(); !errors.Is(err, ErrInvalidAudience) {
		t.Errorf("a company audience carrying an id validated: %v", err)
	}
	if err := (Audience{Kind: "link", ID: "x"}).Validate(); !errors.Is(err, ErrInvalidAudience) {
		t.Errorf("an unknown audience kind validated: %v", err)
	}
}

// A NEW SEAT AUDIENCE IS A SEAT HANDLE, NEVER A LOGIN. `seat:token:maya` was
// written, counted by Maya's Shared listing and admitted by nothing, since the
// authority table matches a seat share against the principal's seat alone.
func TestANewSeatAudienceRefusesALogin(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"token:maya", "maya.ortiz", "ci:release", "Maya", "-maya"} {
		a := Audience{Kind: AudienceSeat, ID: id}
		err := a.ValidateGrant()
		if !errors.Is(err, ErrInvalidAudience) || !strings.Contains(err.Error(), "not a seat handle") {
			t.Errorf("a new seat audience naming %q validated or said %v", id, err)
		}
		if a.Reaches() {
			t.Errorf("a seat audience naming %q is said to reach somebody", id)
		}
	}
	for _, id := range []string{"maya", "founder", "swe-2"} {
		a := Audience{Kind: AudienceSeat, ID: id}
		if err := a.ValidateGrant(); err != nil || !a.Reaches() {
			t.Errorf("the seat %q was refused (%v) or said to reach nobody", id, err)
		}
	}
}

// A SHARE WRITTEN BEFORE THAT RULE STAYS ADDRESSABLE: its text form parses,
// so its owner can withdraw it by URL, rather than the engine holding a record
// it can neither honour nor remove.
func TestAStoredLoginAudienceStillParsesForItsWithdrawal(t *testing.T) {
	t.Parallel()
	a, err := ParseAudience("seat:token:maya")
	if err != nil || a != (Audience{Kind: AudienceSeat, ID: "token:maya"}) {
		t.Fatalf("ParseAudience(seat:token:maya) = %+v, %v", a, err)
	}
	if a.Reaches() {
		t.Error("a login named as a seat is said to reach somebody")
	}
}
