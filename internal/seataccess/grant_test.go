package seataccess_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// grantsOffer is a lane whose catalogue offers one read and one write
// permission on its zones, and one account write it cannot grant yet.
func grantsOffer() seataccess.Options {
	return seataccess.Options{Lane: seataccess.LaneCloudflare,
		Levels: []seataccess.Level{{ID: "read"}, {ID: "operate"}, {ID: "secure", Administers: true}},
		Grants: []seataccess.GrantEntry{
			{ID: "Origin Read", Name: "Origin Rules: Read", ScopeKind: "zone"},
			{ID: "Origin Write", Name: "Origin Rules: Edit", ScopeKind: "zone", Write: true},
			{ID: "Cache Purge", Name: "Cache Purge: Purge", ScopeKind: "zone", Write: true, InLevel: "operate"},
			{ID: "Workers Scripts Write", Name: "Workers Scripts: Edit", ScopeKind: seataccess.GrantScopeAccount,
				Write: true, Unavailable: "The provisioner token does not hold Workers Scripts: Edit"},
		},
		GrantScope: []string{"example.com"},
	}
}

var grantAt = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

func grant(id, permission string, expires time.Time, scope ...string) seataccess.Grant {
	return seataccess.Grant{ID: id, Permission: permission, Scope: scope, ExpiresAt: expires,
		GrantedBy: "Ada", GrantedAt: grantAt.Add(-time.Hour), Reason: "a change"}
}

// A LAPSED GRANT IS NO GRANT: a plan reads a grant only while it is more than
// [seataccess.LapseLead] from its expiry, each key once at its latest lapse,
// and the loop is armed at the earliest lapse. Mutation: compare against the
// bare expiry and the grant a minute from lapsing is still applied.
func TestALapsingGrantLeavesThePlanBeforeItsExpiry(t *testing.T) {
	t.Parallel()
	grants := []seataccess.Grant{
		grant("g0000000001", "Origin Read", grantAt.Add(30*time.Second)),
		grant("g0000000002", "Origin Write", grantAt.Add(2*time.Hour), "example.com"),
		grant("g0000000003", "Origin Write", grantAt.Add(5*time.Hour), "example.com"),
		grant("g0000000004", "DNS Read", time.Time{}),
	}
	live := seataccess.LiveGrants(grants, grantAt)
	if len(live) != 2 || live[0].Permission != "DNS Read" || live[1].ID != "g0000000003" {
		t.Fatalf("live = %+v, want DNS Read and the later Origin Write", live)
	}
	on := true
	c := &config.Company{Roles: []config.Role{{Name: "SRE", Handle: "sre",
		Integrations: config.RoleIntegrations{Cloudflare: &config.RoleCloudflare{Enabled: &on, Grants: grants}}}}}
	next := seataccess.NextExpiry(c, "cloudflare", grantAt)
	// THE LATER ORIGIN WRITE OUTLASTS THE EARLIER, so its lapse is the one.
	if want := grantAt.Add(5*time.Hour - seataccess.LapseLead); !next.Equal(want) {
		t.Errorf("next expiry = %s, want %s", next, want)
	}
}

// A WRITE GRANT IS A RAISE AS SECURE IS: adding a write permission, or one the
// catalogue cannot vouch for, needs the step-up; a read one, a removal, and a
// write already held at least as long never do. Mutation: drop RaisesGrant
// from NeedsStepUp and the first case passes without a step-up.
func TestAWriteGrantNeedsTheStepUpTheAdministeringLevelDoes(t *testing.T) {
	t.Parallel()
	opts := grantsOffer()
	before := seataccess.Desired{Enabled: true, Level: "read",
		Grants: []seataccess.Grant{grant("g0000000002", "Origin Write", grantAt.Add(4*time.Hour), "example.com")}}
	with := func(gs ...seataccess.Grant) seataccess.Desired {
		d := before
		d.Grants = append(append([]seataccess.Grant{}, before.Grants...), gs...)
		return d
	}
	for name, c := range map[string]struct {
		after seataccess.Desired
		want  bool
	}{
		"a write added":        {with(grant("", "Origin Write", time.Time{}, "example.com")), true},
		"one nobody lists":     {with(grant("", "Billing Write", time.Time{})), true},
		"a read added":         {with(grant("", "Origin Read", time.Time{})), false},
		"a removal":            {seataccess.Desired{Enabled: true, Level: "read"}, false},
		"held, for less long":  {with(grant("", "Origin Write", grantAt.Add(time.Hour), "example.com")), false},
		"held, for longer now": {with(grant("", "Origin Write", grantAt.Add(8*time.Hour), "example.com")), true},
	} {
		if got := seataccess.NeedsStepUp(opts, before, c.after); got != c.want {
			t.Errorf("%s: NeedsStepUp = %v, want %v", name, got, c.want)
		}
	}
}

// A GRANT IS ADDED OR REMOVED, NEVER EDITED: a new one is stamped with an id,
// its author and its time; one named by id must be the one held, field for
// field. Mutation: copy the body's grant for a known id and its author can be
// rewritten.
func TestAGrantIsAddedOrRemovedNeverEdited(t *testing.T) {
	t.Parallel()
	held := grant("g0000000002", "Origin Write", grantAt.Add(4*time.Hour), "example.com")
	before := seataccess.Desired{Enabled: true, Grants: []seataccess.Grant{held}}
	added := seataccess.Grant{Permission: "Origin Read", GrantedBy: "mallory", Reason: "read"}
	got, err := seataccess.StampGrants(before, seataccess.Desired{Enabled: true,
		Grants: []seataccess.Grant{held, added}}, "Ada", grantAt)
	if err != nil {
		t.Fatal(err)
	}
	if n := got.Grants[1]; n.ID == "" || n.GrantedBy != "Ada" || !n.GrantedAt.Equal(grantAt) {
		t.Errorf("added grant = %+v, want stamped by the engine", n)
	}
	forged := held
	forged.GrantedBy = "mallory"
	if _, err := seataccess.StampGrants(before, seataccess.Desired{Enabled: true,
		Grants: []seataccess.Grant{forged}}, "Ada", grantAt); err == nil {
		t.Error("a grant edited in place was stamped")
	}
}

// A GRANT IS CHECKED AGAINST THE LIVE CATALOGUE: a permission it does not
// list, one it cannot grant yet, one the level already holds, an account
// permission given zones, a zone outside the company's, too long a lapse and
// too many grants are refused; a grant kept as it was is never judged again.
func TestAGrantIsCheckedAgainstTheLiveCatalogue(t *testing.T) {
	t.Parallel()
	opts := grantsOffer()
	before := seataccess.Desired{Enabled: true, Level: "operate"}
	for name, g := range map[string]seataccess.Grant{
		"unlisted":      grant("", "Billing Write", time.Time{}),
		"unavailable":   grant("", "Workers Scripts Write", time.Time{}),
		"in the level":  grant("", "Cache Purge", time.Time{}),
		"account zones": {Permission: "Workers Scripts Write", Scope: []string{"example.com"}},
		"outside":       grant("", "Origin Read", time.Time{}, "example.org"),
		"too long":      grant("", "Origin Read", grantAt.Add(200*time.Hour)),
		"lapsed":        grant("", "Origin Read", grantAt.Add(10*time.Second)),
	} {
		after := before
		after.Grants = []seataccess.Grant{g}
		if err := seataccess.CheckGrants(opts, before, after, grantAt); err == nil {
			t.Errorf("%s: %+v was accepted", name, g)
		}
	}
	ok := before
	ok.Grants = []seataccess.Grant{grant("", "Origin Write", grantAt.Add(4*time.Hour), "example.com")}
	if err := seataccess.CheckGrants(opts, before, ok, grantAt); err != nil {
		t.Errorf("a grant the catalogue offers was refused: %v", err)
	}
	kept := seataccess.Desired{Enabled: true, Level: "operate",
		Grants: []seataccess.Grant{grant("g0000000009", "Workers Scripts Write", time.Time{})}}
	if err := seataccess.CheckGrants(opts, kept, kept, grantAt); err != nil {
		t.Errorf("a grant kept as it was was judged again: %v", err)
	}
	many := before
	for range seataccess.MaxGrants + 1 {
		many.Grants = append(many.Grants, grant("", "Origin Read", time.Time{}, "example.com"))
	}
	if err := seataccess.CheckGrants(opts, before, many, grantAt); err != nil {
		t.Errorf("one key granted many times counts once: %v", err)
	}
}

// A GRANT FOR ONE CHANGE IS NEVER PERMANENT NOR PAST ITS BOUND, saved or stored,
// and only an agent's request gives a grant its target. Mutation: drop the
// one-change case from CheckGrant and a permanent grant of it saves.
func TestAGrantForOneChangeIsNeverPermanentNorPastItsBound(t *testing.T) {
	t.Parallel()
	opts := grantsOffer()
	opts.Grants = append(opts.Grants, seataccess.GrantEntry{ID: "Bucket Write", Name: "Bucket Settings: Edit",
		ScopeKind: "zone", Write: true, MaxHours: 4})
	before := seataccess.Desired{Enabled: true, Level: "operate"}
	asked := func(expires time.Time, request, target string) seataccess.Desired {
		d := before
		d.Grants = []seataccess.Grant{{Permission: "Bucket Write", ExpiresAt: expires, Request: request,
			Target: target, Reason: "make it private"}}
		return d
	}
	for name, c := range map[string]struct {
		after seataccess.Desired
		saves bool
	}{
		"an hour":        {asked(grantAt.Add(time.Hour), "aaaaaaaaaa", "user-uploads"), true},
		"its bound":      {asked(grantAt.Add(4*time.Hour), "aaaaaaaaaa", "user-uploads"), true},
		"past its bound": {asked(grantAt.Add(5*time.Hour), "aaaaaaaaaa", "user-uploads"), false},
		"permanently":    {asked(time.Time{}, "aaaaaaaaaa", "user-uploads"), false},
		"a person's":     {asked(grantAt.Add(time.Hour), "", ""), true},
		"a typed target": {asked(grantAt.Add(time.Hour), "", "user-uploads"), false},
	} {
		if err := seataccess.CheckGrants(opts, before, c.after, grantAt); (err == nil) != c.saves {
			t.Errorf("%s: %v, want saved %v", name, err, c.saves)
		}
	}
	if err := seataccess.CheckGrant(opts, "", grant("g0000000001", "Bucket Write", grantAt.Add(3*time.Hour))); err != nil {
		t.Errorf("a stored grant inside its bound was refused: %v", err)
	}
	if err := seataccess.CheckGrant(opts, "", grant("g0000000001", "Bucket Write", grantAt.Add(4*time.Hour))); err == nil {
		t.Error("a stored grant lasting past its bound from when it was granted was applied")
	}
	req := seataccess.GrantRequest{ID: "aaaaaaaaaa", Kind: "cloudflare", Lane: seataccess.LaneCloudflare,
		Handle: "sre", Permission: "Bucket Write", Hours: 1, Target: "user-uploads"}
	if err := req.Validate(); err != nil {
		t.Fatalf("a request naming its target was refused: %v", err)
	}
	if g := req.Grant(false, grantAt); g.Target != "user-uploads" || !g.ExpiresAt.Equal(grantAt.Add(time.Hour)) {
		t.Errorf("the request's grant is %+v, want its target for its hour", g)
	}
	req.Target = "the uploads bucket"
	if err := req.Validate(); err == nil {
		t.Error("a request whose target is a sentence was accepted")
	}
}

// A REQUEST'S OPTIONS CARRY ITS ID AND NOTHING ELSE: both read back as the
// request and its choice, inside a decision's option id, and no key of the
// access catalogue reads as one. Mutation: widen the option pattern and
// `grant-health` reads as a request.
func TestARequestsOptionsCarryItsID(t *testing.T) {
	t.Parallel()
	id := seataccess.NewRequestID()
	for option, permanent := range map[string]bool{
		seataccess.GrantOptionTemporary(id): false, seataccess.GrantOptionPermanent(id): true,
	} {
		got, perm, ok := seataccess.GrantOption(option)
		if !ok || got != id || perm != permanent || len(option) > 32 {
			t.Errorf("%q read back as %q, %v, %v", option, got, perm, ok)
		}
		if !strings.HasPrefix(option, "grant-") {
			t.Errorf("%q is not under the engine's reserved prefix", option)
		}
	}
	for _, key := range advisor.AccessKeys() {
		if _, _, ok := seataccess.GrantOption(string(key)); ok {
			t.Errorf("the access key %q reads as a request's option", key)
		}
	}
	r := seataccess.GrantRequest{ID: id, Permission: "Origin Write", Scope: []string{"example.com"}, Hours: 4,
		Reason: "a rule"}
	if g := r.Grant(false, grantAt); !g.ExpiresAt.Equal(grantAt.Add(4*time.Hour)) || g.Request != id || g.Reason != "a rule" {
		t.Errorf("temporary grant = %+v, want four hours from the choice", g)
	}
	if g := r.Grant(true, grantAt); !g.ExpiresAt.IsZero() {
		t.Errorf("permanent grant = %+v, want no expiry", g)
	}
}

// A REQUEST IS HELD TO WHAT ITS RECORD KEEPS: one stored at
// coord.MaxGrantRequestBytes is written, and one a byte past it, its scope or
// its reason too long, is refused naming what to narrow, before the store is
// asked; the tool that files one refuses it the same way.
//
// Mutation: drop the StoredBytes check from Validate, and the request past its
// bound is accepted.
func TestARequestPastWhatItsRecordKeepsIsRefusedNamingWhatToNarrow(t *testing.T) {
	t.Parallel()
	req := seataccess.GrantRequest{ID: "aaaaaaaaaa", Kind: "cloudflare", Lane: seataccess.LaneCloudflare,
		Handle: "sre", Permission: "Zone Write", Hours: 1, AskedAt: grantAt}
	req.Scope = []string{""}
	req.Scope[0] = strings.Repeat("z", coord.MaxGrantRequestBytes-req.StoredBytes())
	if size := req.StoredBytes(); size != coord.MaxGrantRequestBytes {
		t.Fatalf("premise: the request at its bound is stored as %d bytes, want %d", size, coord.MaxGrantRequestBytes)
	}
	if err := req.Validate(); err != nil {
		t.Errorf("a request at its bound was refused: %v", err)
	}
	req.Scope[0] += "z"
	err := req.Validate()
	if err == nil || !strings.Contains(err.Error(), "fewer scopes") {
		t.Errorf("a request a byte past its bound = %v, want it refused naming what to narrow", err)
	}
}
