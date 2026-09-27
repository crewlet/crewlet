package session_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// tokenValue is a Tier A token's value, as the configuration holds it.
const tokenValue = "a-tier-a-token-for-the-ops-team-01"

// exchanged mints the rig's session again as one exchanged from a Tier A
// token, bound to value, and points the rig's directory at a token subject
// answering to answers.
func (s *signedIn) exchanged(value, answers string) string {
	s.t.Helper()
	cookie, err := s.signer.Mint(session.Mint{
		Lineage: s.lineage, Person: iam.TokenLogin("ops"), Epoch: 0,
		Generation: 1, StartPosition: startPos,
		AbsoluteExpiresAt: s.clock.now().Add(time.Hour),
		Credential:        session.CredentialOf(value),
	})
	if err != nil {
		s.t.Fatalf("mint an exchanged bearer: %v", err)
	}
	s.dir.identity.Person = session.PersonRow{Found: true, Stage: iam.StageActive,
		Login: iam.TokenLogin("ops")}
	s.dir.identity.Session = session.LineageRow{Found: true}
	s.dir.identity.Credential = session.CredentialOf(answers)
	return cookie
}

// A SESSION EXCHANGED FROM A TOKEN IS OVER ONCE THE TOKEN'S VALUE CHANGES.
//
// It answered to the token's NAME: a new value under the same id kept every
// session the old value opened for the rest of its hour, which is the hour a
// leak is answered in. The bearer is bound to the value, so the new one ends
// it — on a node that has not applied the session's row too, where the reads
// would otherwise be served on the bearer alone. THE CONTROL is the same value,
// served on both nodes. Mutation: skip the binding check and the rotated rows
// serve.
func TestAnExchangedSessionEndsWhenItsTokensValueChanges(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		answers string
		want    session.Row
		behind  session.Row
	}{
		{"the value it was exchanged with", tokenValue, session.RowValid, session.RowBehind},
		{"a new value under the same name", "a-rotated-tier-a-token-for-ops-02", session.RowEnded, session.RowEnded},
		{"no value at all", "", session.RowEnded, session.RowEnded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newSignedIn(t)
			rig.cookie = rig.exchanged(tokenValue, tc.answers)
			if got := rig.validate(); got.Row != tc.want {
				t.Errorf("landed on %q (%s), want %q", got.Row, got.Detail, tc.want)
			}
			// A NODE THAT HAS NOT APPLIED THE SESSION'S START.
			rig.dir.identity.Session = session.LineageRow{}
			rig.dir.identity.Applied = startPos - 1
			if got := rig.validate(); got.Row != tc.behind {
				t.Errorf("behind, landed on %q (%s), want %q", got.Row,
					got.Detail, tc.behind)
			}
		})
	}
}

// A PERSON'S SESSION IS NOT A TOKEN'S, IN EITHER DIRECTION.
//
// A bearer with no binding whose subject answers to a configured value was
// minted without the value it stands for, and a bound bearer whose subject
// answers to none names a credential this node does not hold under that name:
// neither is served. The control is the rig's own person, whose bearer carries
// no binding and whose subject answers to none.
func TestABindingAndItsAbsenceMustAgree(t *testing.T) {
	t.Parallel()
	person := newSignedIn(t)
	if got := person.validate(); got.Row != session.RowValid {
		t.Fatalf("the control landed on %q: %s", got.Row, got.Detail)
	}
	person.dir.identity.Credential = session.CredentialOf(tokenValue)
	if got := person.validate(); got.Row != session.RowEnded {
		t.Errorf("an unbound bearer whose subject answers to a value landed "+
			"on %q: %s", got.Row, got.Detail)
	}

	token := newSignedIn(t)
	token.cookie = token.exchanged(tokenValue, tokenValue)
	token.dir.identity.Credential = session.Credential{}
	if got := token.validate(); got.Row != session.RowEnded {
		t.Errorf("a bound bearer whose subject answers to nothing landed on "+
			"%q: %s", got.Row, got.Detail)
	}
}

// THE BINDING SURVIVES A RE-ISSUE AND A KEYRING ROTATION, and is recomputed
// under the key the new bearer is signed with.
//
// A binding keyed under the old key and copied onto a bearer signed under the
// new one would never verify once the old key was dropped, so every exchanged
// session would end at the drop rather than drain onto the new key; and a
// re-issue that dropped it would serve the session whatever value its token
// held from then on.
func TestTheBindingFollowsTheBearerOntoANewKey(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	old, err := session.New(session.Options{
		Material: runtoken.OneKey("k1", "the-previous-key-material"),
		Now:      rig.clock.now,
	})
	if err != nil {
		t.Fatalf("build the old signer: %v", err)
	}
	cookie, err := old.Mint(session.Mint{
		Lineage: rig.lineage, Person: iam.TokenLogin("ops"), Generation: 1,
		StartPosition: startPos, AbsoluteExpiresAt: rig.clock.now().Add(time.Hour),
		Credential: session.CredentialOf(tokenValue),
	})
	if err != nil {
		t.Fatalf("mint under the old key: %v", err)
	}
	rig.exchanged(tokenValue, tokenValue)
	rig.clock.advance(session.ReissueAfter + time.Minute)
	got := rig.signer.Validate(t.Context(), rig.dir, cookie)
	if got.Row != session.RowValid || got.Reissue == "" {
		t.Fatalf("the old key's bearer landed on %q with re-issue %t: %s",
			got.Row, got.Reissue != "", got.Detail)
	}

	// The new key alone, as after the old one is dropped.
	newOnly, err := session.New(session.Options{
		Material: runtoken.OneKey("k2", "the-active-key-material"),
		Now:      rig.clock.now,
	})
	if err != nil {
		t.Fatalf("build the new signer: %v", err)
	}
	if next := newOnly.Validate(t.Context(), rig.dir, got.Reissue); next.Row != session.RowValid {
		t.Errorf("the re-issued bearer landed on %q under the new key alone: %s",
			next.Row, next.Detail)
	}
	rig.dir.identity.Credential = session.CredentialOf("a-rotated-tier-a-token-for-ops-02")
	if next := newOnly.Validate(t.Context(), rig.dir, got.Reissue); next.Row != session.RowEnded {
		t.Errorf("the re-issued bearer outlived its token's new value: %q (%s)",
			next.Row, next.Detail)
	}
}

// THE VALUE NEVER LEAVES THE PROCESS IN A COOKIE OR A LOG LINE.
//
// A cookie is the value most likely to end up somewhere nobody meant it to,
// and a Tier A value is only as strong as whoever wrote it — so the cookie
// carries a MAC under the keyring, never the value or a bare digest of it, and
// the credential an identity holds prints redacted under every verb.
func TestTheValueIsNeverCarriedOrPrinted(t *testing.T) {
	t.Parallel()
	rig := newSignedIn(t)
	cookie := rig.exchanged(tokenValue, tokenValue)
	if strings.Contains(cookie, tokenValue) {
		t.Error("the cookie carries the token's value")
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if printed := fmt.Sprintf(verb, rig.dir.identity); strings.Contains(printed, tokenValue) {
			t.Errorf("an identity printed with %s carries the value: %s", verb, printed)
		}
	}
}
