package iam

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAnUnknownGrantIsIgnoredNotRejected is the rolling-upgrade arm. A newer
// build writes a capability string this one has never heard of; refusing the
// whole principal over it would log every person in the company out for the
// length of the deploy.
func TestAnUnknownGrantIsIgnoredNotRejected(t *testing.T) {
	const fromTheFuture = Grant("swarm:conscript")

	p := Principal{
		ID:    uuid.New(),
		Login: "jane.doe",
		Kind:  KindPerson,
		Stage: StageActive,
		Grants: []Grant{
			GrantWorkWrite, fromTheFuture, GrantStateRead,
		},
	}

	if err := p.Validate(); err != nil {
		t.Fatalf("Validate = %v; a grant this build cannot read must not refuse the principal", err)
	}
	if !p.Can(GrantWorkWrite) || !p.Can(GrantStateRead) {
		t.Fatal("the grants this build DOES know stopped working next to one it does not")
	}
	if p.Can(fromTheFuture) {
		t.Fatalf("%q was granted — this build cannot open a door it has never heard of", fromTheFuture)
	}
	if got := p.KnownGrants(); !slices.Equal(got, []Grant{GrantWorkWrite, GrantStateRead}) {
		t.Errorf("KnownGrants = %v, want the two this build declares", got)
	}
	if got := p.UnknownGrants(); !slices.Equal(got, []Grant{fromTheFuture}) {
		t.Errorf("UnknownGrants = %v, want [%q] so a log line can name it", got, fromTheFuture)
	}

	// THE CONTROL. Validate has to be able to refuse something, or the
	// assertion above is satisfied by a method that returns nil forever.
	for _, bad := range []struct {
		name string
		p    Principal
	}{
		{"no id", Principal{Login: "jane.doe", Kind: KindPerson, Stage: StageActive}},
		{"unknown kind", Principal{ID: uuid.New(), Kind: Kind("wraith"), Stage: StageActive}},
		{"unknown stage", Principal{ID: uuid.New(), Kind: KindPerson, Login: "jane.doe", Stage: Stage("pending")}},
	} {
		if err := bad.p.Validate(); err == nil {
			t.Errorf("Validate accepted a principal with %s", bad.name)
		} else if !errors.Is(err, ErrInvalidPrincipal) {
			t.Errorf("Validate(%s) = %v, which does not wrap ErrInvalidPrincipal", bad.name, err)
		}
	}
}

// TestValidateHoldsEachKindToItsOwnNamespace is the other half of the package
// doc's collision claim: the grammar is not advice, a principal is refused for
// carrying a name from the wrong space.
func TestValidateHoldsEachKindToItsOwnNamespace(t *testing.T) {
	cases := []struct {
		name string
		p    Principal
		ok   bool
	}{
		{"a dotted person login", Principal{Kind: KindPerson, Login: "jane.doe"}, true},
		{"a person login with no dot", Principal{Kind: KindPerson, Login: "jane"}, false},
		{"a person carrying a machine handle", Principal{Kind: KindPerson, Login: "ci:release"}, false},
		{"a coloned machine handle", Principal{Kind: KindMachine, Login: "ci:release"}, true},
		{"a machine carrying a person login", Principal{Kind: KindMachine, Login: "jane.doe"}, false},
		{"a seat naming its handle", Principal{Kind: KindSeat, Seat: "backend-lead"}, true},
		{"a seat naming nothing", Principal{Kind: KindSeat}, false},
		{"the engine naming its node", Principal{Kind: KindEngine, Login: "node-7"}, true},
		{"the engine naming nothing", Principal{Kind: KindEngine}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.p
			p.ID, p.Stage = uuid.New(), StageActive
			err := p.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate = %v, want accepted", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("Validate accepted a name from the wrong namespace")
			}
		})
	}
}

// TestAZeroReauthDeadlineIsStaleNotEternal protects the reading that fails
// safe. The two are one keystroke apart, and the other one turns a field
// somebody forgot to fill in into a session that outlives the company.
func TestAZeroReauthDeadlineIsStaleNotEternal(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	if (Principal{}).Fresh(now) {
		t.Error("a principal with no re-auth deadline reads as fresh")
	}
	if (Principal{ReauthAt: now.Add(-time.Second)}).Fresh(now) {
		t.Error("a deadline one second in the past reads as fresh")
	}
	if (Principal{ReauthAt: now}).Fresh(now) {
		t.Error("a deadline exactly now reads as fresh — the instant AFTER which it is stale is not still valid at it")
	}
	// The control: something has to come back fresh, or the three
	// assertions above are satisfied by a method that always says stale.
	if !(Principal{ReauthAt: now.Add(time.Hour)}).Fresh(now) {
		t.Error("a deadline an hour out reads as stale, so the checks above prove nothing")
	}
}

// EACH RECENCY READS ITS OWN WINDOW, and the sensitive one is not the ordinary
// one relabelled.
//
// The case that matters is the middle row: a proof forty minutes old is inside
// `step_up` and outside `step_up_sensitive`, so an ordinary gesture proceeds
// and revealing a secret asks again. A method that read ReauthAt for both would
// pass every other row here and reveal secrets on an hour-old proof.
func TestEachRecencyReadsItsOwnWindow(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	proved := func(age time.Duration) Principal {
		at := now.Add(-age)
		return Principal{ReauthAt: at.Add(time.Hour),
			SensitiveReauthAt: at.Add(15 * time.Minute)}
	}
	for _, tc := range []struct {
		name      string
		p         Principal
		any, step bool
		sensitive bool
	}{
		{"a proof a minute old", proved(time.Minute), true, true, true},
		{"a proof forty minutes old", proved(40 * time.Minute), true, true, false},
		{"a proof two hours old", proved(2 * time.Hour), true, false, false},
		{"nothing ever proved", Principal{}, true, false, false},
	} {
		if got := tc.p.Proved(RecencyAny, now); got != tc.any {
			t.Errorf("%s: any = %v, want %v", tc.name, got, tc.any)
		}
		if got := tc.p.Proved(RecencyStepUp, now); got != tc.step {
			t.Errorf("%s: step_up = %v, want %v", tc.name, got, tc.step)
		}
		if got := tc.p.Proved(RecencySensitive, now); got != tc.sensitive {
			t.Errorf("%s: step_up_sensitive = %v, want %v", tc.name, got, tc.sensitive)
		}
	}
	// A RECENCY THIS BUILD CANNOT NAME IS NOT PROVED, however fresh the
	// proof — a newer peer's rule must not open a door here.
	for _, r := range []Recency{"", "step_up_paranoid"} {
		if proved(0).Proved(r, now) {
			t.Errorf("%q reads as proved on a proof taken this instant", r)
		}
		if r.Valid() || r.Demands() {
			t.Errorf("%q reports itself a recency this build knows", r)
		}
	}
	for _, r := range Recencies {
		if !r.Valid() {
			t.Errorf("%q is declared and reports itself invalid", r)
		}
		if got, want := r.Demands(), r != RecencyAny; got != want {
			t.Errorf("%q.Demands() = %v, want %v", r, got, want)
		}
	}
}

// TestOnlyAnActiveStageMayAct pins the allowlist. A denylist would have
// admitted a stage a newer peer invented, which is the direction that cannot
// be walked back.
func TestOnlyAnActiveStageMayAct(t *testing.T) {
	for _, s := range Stages {
		if got, want := s.MayAct(), s == StageActive; got != want {
			t.Errorf("%q.MayAct() = %v, want %v", s, got, want)
		}
	}
	for _, s := range []Stage{"", "enrolled", "ACTIVE", "probation"} {
		if s.MayAct() {
			t.Errorf("%q may act, and this build has never heard of it", s)
		}
	}
}
