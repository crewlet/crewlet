package code_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE REPOSITORY LIFECYCLE AS TOOLS: what a person, a unit's lead and a seat may
// make, rename, change, archive, delete and restore, each one request decided
// by the lifecycle store in one snapshot — through the whole path, a real
// broker, the shipped applier and the store's own reads back.

// lcPerson is a person acting through a credential bound to their human seat:
// the test chart's founder.
var lcPerson = code.Principal{Kind: code.PrincipalOperator, OperatorID: "pat:founder", Seat: "founder"}

// lcOp mints a caller's operation id, as a tool's call is given one.
func lcOp() string { return statelog.NewOpID(time.Now(), "code.test") }

// unitOwned and seatOwned are a repository's owner.
func unitOwned(ref string) code.Owner { return code.Owner{Kind: code.OwnerUnit, Ref: ref} }
func seatOwned(ref string) code.Owner { return code.Owner{Kind: code.OwnerSeat, Ref: ref} }

// lcCreate is a tool's create of name, owned by owner, of the kind its name
// says.
func lcCreate(actor code.Principal, name string, owner code.Owner) code.LifecycleRequest {
	return code.LifecycleRequest{Kind: code.LifecycleCreate, OpID: lcOp(), Actor: actor, Name: name,
		Create: &code.RepoSettings{ManagedBy: code.ManagedTools, Kind: code.KindOf(name), Owner: owner,
			Visibility: code.VisibilityCompany, Search: code.SearchGrep, DefaultBranch: "main"}}
}

// lcOn is a request of kind on the repository id that carries nothing beyond
// it: an archive, an unarchive, a delete or a restore.
func lcOn(kind code.LifecycleKind, actor code.Principal, id string) code.LifecycleRequest {
	return code.LifecycleRequest{Kind: kind, OpID: lcOp(), Actor: actor, RepoID: id}
}

// lcRename is a rename of the repository id to name.
func lcRename(actor code.Principal, id, name string) code.LifecycleRequest {
	req := lcOn(code.LifecycleRename, actor, id)
	req.Name = name
	return req
}

// lcSettings is a settings change of the repository id.
func lcSettings(actor code.Principal, id string, change code.RepoChange) code.LifecycleRequest {
	req := lcOn(code.LifecycleSettings, actor, id)
	req.Change = &change
	return req
}

// lcProtect sets pattern's rule on the repository id; a nil rule removes it.
func lcProtect(actor code.Principal, id, pattern string, rule *code.Protection) code.LifecycleRequest {
	req := lcOn(code.LifecycleProtect, actor, id)
	req.Protect = &code.ProtectChange{Pattern: pattern, Rule: rule}
	return req
}

// lcGate sets pattern's rule on the repository id, its merge gate changed by
// gate.
func lcGate(actor code.Principal, id, pattern string, rule *code.Protection,
	gate code.GateChange) code.LifecycleRequest {

	req := lcProtect(actor, id, pattern, rule)
	req.Protect.Gate = &gate
	return req
}

// gateTo is the change that leaves a rule's gate g, whatever it held: every
// field named.
func gateTo(g code.MergeGate) code.GateChange {
	return code.GateChange{MergeBy: ptr(g.MergeBy), Approvals: ptr(g.Approvals),
		ApprovalsUnless: ptr(g.ApprovalsUnless), HumanPaths: ptr(g.HumanPaths), Checks: ptr(g.Checks),
		Landing:       &code.LandingChange{Depth: ptr(g.Landing.Depth), Batch: ptr(g.Landing.Batch)},
		OwnersApprove: ptr(g.OwnersApprove), DeleteSource: ptr(g.DeleteSource),
		AgentsEditChecks: ptr(g.AgentsEditChecks)}
}

// ptr is a pointer to v: a field a change names.
func ptr[T any](v T) *T { return &v }

// lcHold is a live turn's hold on the repository id, decided at the read seen.
func lcHold(actor code.Principal, id, turn string, seen statelog.Position) code.LifecycleRequest {
	req := lcOn(code.LifecycleHold, actor, id)
	req.TurnID, req.Seen = turn, seen
	return req
}

// persons is a direct push a person alone may make: the rule a protection
// change in these cases sets.
var persons = &code.Protection{PushRule: code.PushRule{DirectPush: code.DirectPersons}}

// life asks req of n's lifecycle store, failing the case on an error.
func (n *node) life(req code.LifecycleRequest) code.LifecycleResult {
	n.t.Helper()
	res, err := n.store.Lifecycle(n.t.Context(), req)
	if err != nil {
		n.t.Fatalf("%s of %s: %v", req.Kind, cmp.Or(req.Name, req.RepoID), err)
	}
	return res
}

// lands asks req and fails the case unless it applied.
func (n *node) lands(req code.LifecycleRequest) code.LifecycleResult {
	n.t.Helper()
	res := n.life(req)
	if res.Refusal != "" || res.Outcome != statelog.OutcomeApplied {
		n.t.Fatalf("%s of %s by %s: %+v, want it applied", req.Kind, cmp.Or(req.Name, req.RepoID),
			req.Actor.Seat, res)
	}
	return res
}

// made creates name through actor and answers its id.
func (n *node) made(actor code.Principal, name string, owner code.Owner) string {
	n.t.Helper()
	return n.lands(lcCreate(actor, name, owner)).Repo.ID
}

// resolved is the repository name names as this node's rows answer it, and
// the position they were read at.
func (n *node) resolved(name string) code.RepoRef {
	n.t.Helper()
	ref, err := n.reader.Resolve(n.t.Context(), code.ResolveQuery{Name: name, Fresh: stale})
	if err != nil {
		n.t.Fatalf("resolve %s: %v", name, err)
	}
	return ref
}

// A LIFECYCLE REQUEST IS ONE WRITE, whoever asks it again. The same operation
// twice is one record, the repeat answered from the domain's ledger at the first
// copy's position; and with the ledger's row swept — a request retried past the
// ledger's horizon — a create still names the repository its operation derives
// (code.RepoIDOf), finds it holding the name, and is answered from its row with
// no record, rather than refused as a name somebody else took.
//
// Mutation: mint the repository's id (uuid.NewV7) rather than derive it — the
// third create is refused name_taken, red.
func TestALifecycleRequestIsOneWrite(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	req := lcCreate(lcPerson, "eng-app", unitOwned("eng"))
	first := n.lands(req)
	if derived, ok := code.RepoIDOf(req.OpID); !ok || first.Repo.ID != derived {
		t.Fatalf("the create made %s, want the repository its operation derives, %s", first.Repo.ID, derived)
	}
	end := n.end()
	again := n.life(req)
	if again.Outcome != statelog.OutcomeApplied || again.Position != first.Position ||
		again.Repo.ID != first.Repo.ID {
		t.Fatalf("the repeat answered %+v, want the first copy's record at %s", again, first.Position)
	}
	n.exec(`DELETE FROM code_ops WHERE op_id = ?`, req.OpID)
	third := n.life(req)
	if third.Refusal != "" || third.Outcome != statelog.OutcomeApplied || third.Repo.ID != first.Repo.ID {
		t.Fatalf("the create retried past its ledger row answered %+v, want the repository it made", third)
	}
	if got := n.end(); got != end {
		t.Fatalf("the log grew from %d to %d records: a repeated request wrote again", end, got)
	}
}

// A LIFECYCLE REQUEST WHOSE OUTCOME IS UNKNOWN IS ANSWERED SO — an outcome,
// never an error. Its record is appended and the acknowledgement lost: the
// answer says the outcome is unknown and names the operation, so whoever asked
// makes the same request again, which the ledger answers with what landed. As
// an error it crossed the estate's wire as words with no sentinel, and the
// tool that asked a data node for it read the node as broken and told its
// caller that trying again could not help.
//
// Mutation: answer the lost acknowledgement as an error again → red.
func TestALifecycleRequestWhoseOutcomeIsUnknownIsAnsweredSo(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	lossy := newGatedPublisher(n.pub, 1)
	lossy.lose = func(context.Context, statelog.Request) bool { return true }
	s, err := code.NewStore(code.StoreOptions{
		Publisher: lossy, DB: n.db.Reader(), NodeID: n.id,
		Now:       func() time.Time { return harnessAt },
		SeatRules: func() (code.SeatRules, bool) { return code.SeatRules{Roster: n.chart}, true },
		Policy:    func() code.LifecyclePolicy { return n.scratch },
	})
	if err != nil {
		t.Fatalf("build the store: %v", err)
	}
	req := lcCreate(lcPerson, "eng-lost", unitOwned("eng"))
	res, err := s.Lifecycle(t.Context(), req)
	if err != nil || res.Outcome != statelog.OutcomeUnknown || res.Refusal != "" ||
		!strings.Contains(res.Detail, req.OpID) {
		t.Fatalf("a create whose acknowledgement was lost answered %+v, %v — want the outcome unknown, "+
			"naming its operation", res, err)
	}
	if res.Position != (statelog.Position{}) {
		t.Errorf("an unknown outcome answered position %s, which no caller may raise its floor by", res.Position)
	}
	again := n.life(req)
	if again.Outcome != statelog.OutcomeApplied || again.Repo.Name != "eng-lost" {
		t.Fatalf("the same request again answered %+v, want the create the lost answer landed", again)
	}
}

// WHO MAY CHANGE A REPOSITORY is one judgement over the chart, every kind's: a
// person maintains every tool-made repository and creates projects for any
// owner the chart holds, never a seat's scratch repository; a seat what its
// grant on the repository maintains — the owning unit's effective lead at
// full_access a project, the seat a scratch repository carries its own — and a
// settings change as the repository stands AND as the change leaves it, so a
// seat cannot hand a repository to an owner under which it would not maintain
// it. Below maintain — a lead at review, a member, an outsider, another seat —
// is not_permitted, with nothing written. And a seat leaves no project owned
// by a seat, its own included, by a create or a settings change, though it
// would maintain one it owned: a seat's own repositories are its scratch
// repositories, capped and expiring, and only a person makes a project a
// seat's.
//
// Mutations: judge a settings change as the repository stands alone — the
// lead hands its project to Sales, red; drop the owner-kind check from a
// seat's project create and settings change — a full_access member creates a
// project owned by itself, and the lead takes its unit's project for itself,
// red.
func TestWhoMayChangeARepository(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	kinds := []code.LifecycleKind{code.LifecycleRename, code.LifecycleSettings, code.LifecycleArchive,
		code.LifecycleUnarchive, code.LifecycleDelete, code.LifecycleRestore, code.LifecycleProtect}
	// on asks kind of a fresh repository — a project Engineering owns, or a
	// scratch repository seat dev owns — brought to where the kind applies.
	i := 0
	on := func(actor code.Principal, scratch bool, kind code.LifecycleKind) code.LifecycleResult {
		i++
		name, owner, maker := fmt.Sprintf("p%d", i), unitOwned("eng"), lcPerson
		if scratch {
			name, owner, maker = fmt.Sprintf("dev.s%d", i), seatOwned("dev"), agent("dev")
		}
		id := n.made(maker, name, owner)
		switch kind {
		case code.LifecycleUnarchive:
			n.lands(lcOn(code.LifecycleArchive, lcPerson, id))
		case code.LifecycleRestore:
			n.lands(lcOn(code.LifecycleDelete, lcPerson, id))
		}
		var req code.LifecycleRequest
		switch kind {
		case code.LifecycleRename:
			req = lcRename(actor, id, name+"r")
		case code.LifecycleSettings:
			req = lcSettings(actor, id, code.RepoChange{Description: new("changed")})
		case code.LifecycleProtect:
			req = lcProtect(actor, id, "refs/heads/main", persons)
		default:
			req = lcOn(kind, actor, id)
		}
		return n.life(req)
	}
	for _, tc := range []struct {
		who     string
		actor   code.Principal
		scratch bool
		// may is every kind the actor may ask of the repository; every
		// other is not_permitted.
		may []code.LifecycleKind
	}{
		{"a person on a project", lcPerson, false, kinds},
		{"its unit's lead at full_access", agent("cto"), false, kinds},
		{"a sub-unit's lead at review", agent("plat-lead"), false, nil},
		{"a member of the unit", agent("dev"), false, nil},
		{"an outsider", agent("vp-sales"), false, nil},
		{"a person on a scratch repository", lcPerson, true, kinds[:6]},
		{"the seat it belongs to", agent("dev"), true, kinds[:6]},
		{"the lead of its seat's unit", agent("plat-lead"), true, nil},
		{"another seat", agent("vp-sales"), true, nil},
	} {
		for _, kind := range kinds {
			got := on(tc.actor, tc.scratch, kind)
			allowed := false
			for _, k := range tc.may {
				allowed = allowed || k == kind
			}
			switch {
			case allowed && (got.Refusal != "" || got.Outcome != statelog.OutcomeApplied):
				t.Errorf("%s asking %s: %+v, want it applied", tc.who, kind, got)
			case !allowed && got.Refusal != code.LifecycleNotPermitted:
				t.Errorf("%s asking %s: %+v, want not_permitted", tc.who, kind, got)
			}
		}
	}

	// CREATES: a project for an owner the actor maintains it under; a scratch
	// repository for its own handle, at full_access.
	for _, tc := range []struct {
		who   string
		req   code.LifecycleRequest
		lands bool
	}{
		{"a person's project", lcCreate(lcPerson, "c1", unitOwned("eng")), true},
		{"a person's scratch repository", lcCreate(lcPerson, "dev.c2", seatOwned("dev")), false},
		{"a person's project for no unit of the chart", lcCreate(lcPerson, "c3", unitOwned("gone")), false},
		{"a lead's project for its unit", lcCreate(agent("cto"), "c4", unitOwned("eng")), true},
		{"a lead's project for another unit", lcCreate(agent("cto"), "c5", unitOwned("sales")), false},
		{"a lead at review", lcCreate(agent("plat-lead"), "c6", unitOwned("plat")), false},
		{"a member's project", lcCreate(agent("dev"), "c7", unitOwned("eng")), false},
		{"a seat's own scratch repository", lcCreate(agent("dev"), "dev.c8", seatOwned("dev")), true},
		{"a root seat's own", lcCreate(agent("root-bot"), "root-bot.c9", seatOwned("root-bot")), true},
		{"another seat's scratch repository", lcCreate(agent("dev"), "reader.c10", seatOwned("reader")), false},
		{"a scratch repository at read_only", lcCreate(agent("reader"), "reader.c11", seatOwned("reader")), false},
		// A SEAT MAKES NO PROJECT A SEAT'S, its own included, though it would
		// maintain one it owned: a seat's own are its scratch repositories.
		{"a full_access member's project owned by itself", lcCreate(agent("dev"), "c12", seatOwned("dev")), false},
		{"a lead's project owned by itself", lcCreate(agent("cto"), "c13", seatOwned("cto")), false},
		{"a lead's project owned by another seat", lcCreate(agent("cto"), "c14", seatOwned("dev")), false},
		{"a person's project for a seat", lcCreate(lcPerson, "c15", seatOwned("dev")), true},
	} {
		got := n.life(tc.req)
		seatsProject := tc.req.Actor.Kind == code.PrincipalAgent && tc.req.Create.Owner.Kind == code.OwnerSeat &&
			code.KindOf(tc.req.Name) == code.RepoProject
		switch {
		case tc.lands && (got.Refusal != "" || got.Outcome != statelog.OutcomeApplied):
			t.Errorf("%s: %+v, want it created", tc.who, got)
		case !tc.lands && got.Refusal != code.LifecycleNotPermitted:
			t.Errorf("%s: %+v, want not_permitted", tc.who, got)
		case seatsProject && !strings.Contains(got.Detail, "scratch repositories"):
			t.Errorf("%s: refused %q, want the sentence naming a seat's own repositories", tc.who, got.Detail)
		}
	}

	// AS THE CHANGE LEAVES IT: the lead may not hand its unit's project to a
	// unit it does not lead — it could not take it back — nor to any seat,
	// itself included, and a person may do both.
	id := n.made(lcPerson, "handed", unitOwned("eng"))
	sales, cto := unitOwned("sales"), seatOwned("cto")
	if got := n.life(lcSettings(agent("cto"), id, code.RepoChange{Owner: &sales})); got.Refusal !=
		code.LifecycleNotPermitted || !strings.Contains(got.Detail, "could not take it back") {
		t.Errorf("the lead handing its project to Sales: %+v, want not_permitted saying why", got)
	}
	if got := n.life(lcSettings(agent("cto"), id, code.RepoChange{Owner: &cto})); got.Refusal !=
		code.LifecycleNotPermitted || !strings.Contains(got.Detail, "scratch repositories") {
		t.Errorf("the lead taking its unit's project for itself: %+v, want not_permitted saying why", got)
	}
	n.lands(lcSettings(lcPerson, id, code.RepoChange{Owner: &cto}))
	// A PROJECT A PERSON GAVE A SEAT the seat maintains, and may hand on to a
	// unit it leads.
	eng := unitOwned("eng")
	n.lands(lcSettings(agent("cto"), id, code.RepoChange{Owner: &eng}))
	n.lands(lcSettings(lcPerson, id, code.RepoChange{Owner: &sales}))
}

// A SEAT'S LIFECYCLE REQUEST IS DECIDED AGAIN FOR WHOEVER WOKE ITS TURN
// (LifecycleRequest.Ceiling, M3-decisions U40 (a)): the lead, who maintains
// Engineering's project, changes neither its settings nor its protections for
// nobody — a stranger, a person who may not act — nor for dev, a member who
// only writes it, each refused not_permitted naming whom the work is for and,
// for a colleague, the colleague's own refusal. Its own work is steered by
// either: dev creates its scratch repository and changes its description
// there, and protects a branch of the project a person handed it (a scratch
// repository takes no protections); what discloses a repository or retires it
// — its visibility, a delete — is decided for them like anything else, and
// refused.
//
// Mutations: steeredOwnWork answering false — dev's own create and edit are
// refused, red; its protect answering false — dev's own protection is
// refused, red; or true whoever owns the repository — the lead's protection
// lands for both, red; answering true for every kind on its own repository —
// the visibility change and the delete land, red; lifecycleRefusal skipping
// the ceiling — the lead's change for dev and for nobody lands, red.
func TestALifecycleRequestIsDecidedAgainForWhoeverWokeTheTurn(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	forDev := &code.WriteCeiling{Kind: code.WriteCeilingSeat, Seat: "dev"}
	nobody := &code.WriteCeiling{Kind: code.WriteCeilingNobody}
	project := n.made(lcPerson, "ceilinged", unitOwned("eng"))
	handed := n.made(lcPerson, "handed", seatOwned("dev"))
	tightened := &code.Protection{PushRule: code.PushRule{DirectPush: code.DirectNone}}
	for _, tc := range []struct {
		ceiling *code.WriteCeiling
		says    string
	}{
		{forDev, "this work is for seat dev, whose ask started this turn"},
		{nobody, "who may change nothing of this company's code"},
	} {
		for _, req := range []code.LifecycleRequest{
			lcSettings(agent("cto"), project, code.RepoChange{Description: new("for " + string(tc.ceiling.Kind))}),
			lcProtect(agent("cto"), project, "refs/heads/main", tightened),
		} {
			req.Ceiling = tc.ceiling
			if got := n.life(req); got.Refusal != code.LifecycleNotPermitted ||
				!strings.Contains(got.Detail, tc.says) {
				t.Errorf("the lead's %s for %s: %+v, want not_permitted saying %q", req.Kind, tc.ceiling.Kind, got,
					tc.says)
			}
		}
	}
	n.lands(lcSettings(agent("cto"), project, code.RepoChange{Description: new("for itself")}))

	for i, ceiling := range []*code.WriteCeiling{{Kind: code.WriteCeilingSeat, Seat: "vp-sales"}, nobody} {
		create := lcCreate(agent("dev"), fmt.Sprintf("dev.steered%d", i), seatOwned("dev"))
		create.Ceiling = ceiling
		id := n.lands(create).Repo.ID
		edit := lcSettings(agent("dev"), id, code.RepoChange{Description: new("steered")})
		edit.Ceiling = ceiling
		n.lands(edit)
		protect := lcProtect(agent("dev"), handed, fmt.Sprintf("refs/heads/steered%d", i), tightened)
		protect.Ceiling = ceiling
		n.lands(protect)
		unit := code.VisibilityUnit
		for _, req := range []code.LifecycleRequest{
			lcSettings(agent("dev"), id, code.RepoChange{Visibility: &unit}),
			lcOn(code.LifecycleDelete, agent("dev"), id),
		} {
			req.Ceiling = ceiling
			if got := n.life(req); got.Refusal != code.LifecycleNotPermitted {
				t.Errorf("dev's %s of its own repository for %s: %+v, want not_permitted", req.Kind,
					ceiling.Kind, got)
			}
		}
	}
	hold := lcHold(agent("dev"), project, "0192f00d-0000-7000-8000-000000000001", n.resolved("ceilinged").Position)
	hold.Ceiling = nobody
	if err := hold.Validate(); !errors.Is(err, code.ErrInvalid) {
		t.Errorf("a hold made for nobody: %v, want ErrInvalid", err)
	}
}

// A CONFIGURATION-MANAGED REPOSITORY REFUSES EVERY TOOL: a repository code.repos
// declares is the configuration's to change, and a tool's request names the
// declaration to change instead — keeping its sentinel across the wire.
func TestAConfigManagedRepositoryRefusesEveryTool(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	n.ensure("declared", described("one"), t1)
	id := n.mustID("declared")
	end := n.end()
	for _, req := range []code.LifecycleRequest{
		lcRename(lcPerson, id, "declared-b"),
		lcSettings(lcPerson, id, code.RepoChange{Description: new("two")}),
		lcOn(code.LifecycleArchive, lcPerson, id),
		lcOn(code.LifecycleUnarchive, lcPerson, id),
		lcOn(code.LifecycleDelete, lcPerson, id),
		lcOn(code.LifecycleRestore, lcPerson, id),
		lcProtect(lcPerson, id, "refs/heads/main", nil),
	} {
		got := n.life(req)
		if got.Refusal != code.LifecycleConfigManaged || !strings.Contains(got.Detail, "code.repos[declared]") {
			t.Errorf("a person's %s of a declared repository: %+v, want config_managed naming it", req.Kind, got)
		}
		if !errors.Is(got.Err(), code.ErrConfigManaged) {
			t.Errorf("a person's %s: the refusal as an error is %v, want ErrConfigManaged", req.Kind, got.Err())
		}
	}
	if got := n.end(); got != end {
		t.Fatalf("the refusals wrote %d records", got-end)
	}
}

// A PROTECTION CHANGE IS RESOLVED INSIDE THE DECIDE: a request carries one
// rule for one pattern, and the decide adds it to the rules its own snapshot
// holds — so two requests for two patterns, in either order, leave both, where
// a caller that sent the whole list it had read would drop the other's.
func TestAProtectionChangeIsResolvedInsideTheDecide(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	cto := code.Principal{Kind: code.PrincipalAgent, Seat: "cto"}
	for i, order := range [][2]string{{"refs/heads/main", "refs/heads/release/*"},
		{"refs/heads/release/*", "refs/heads/main"}} {
		id := n.made(lcPerson, fmt.Sprintf("guarded-%d", i), unitOwned("eng"))
		n.lands(lcProtect(cto, id, order[0], persons))
		got := n.lands(lcProtect(lcPerson, id, order[1], persons))
		var patterns []string
		for _, p := range got.Repo.Protections {
			patterns = append(patterns, p.Pattern)
		}
		if strings.Join(patterns, " ") != "refs/heads/main refs/heads/release/*" {
			t.Errorf("protected in the order %v, the repository holds %v, want both rules", order, patterns)
		}
		// AND A NIL RULE REMOVES ITS PATTERN'S, leaving the other's.
		got = n.lands(lcProtect(lcPerson, id, order[0], nil))
		if len(got.Repo.Protections) != 1 || got.Repo.Protections[0].Pattern != order[1] {
			t.Errorf("the removal of %s left %+v", order[0], got.Repo.Protections)
		}
	}
}

// A PROTECT REQUEST IS REFUSED A RULE NO RECORD MAY CARRY at its own
// validation, naming the field its caller sent — a direct_push this build does
// not know, unset among them, allow_force on a tag, a rule carrying a gate
// whole rather than the change the decide resolves, and a gate change no rule
// could take: on a tag, beside the rule's removal, naming nothing, removing the
// gate and naming a field, or naming a field a record may not carry — for
// whoever asks and before any decide judges who may: a person, the unit's lead
// and a member that may not protect the repository are all told the one
// thing, and nothing is written. A rule a record may carry passes, a branch
// rule's gate change included.
//
// Mutation: drop the gate change's check from the request's validation — the
// change naming nothing reaches the decide, resolves to the default gate and
// lands, red.
func TestAProtectRequestIsRefusedARuleNoRecordMayCarry(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	id := n.made(lcPerson, "ruled", unitOwned("eng"))
	end := n.end()
	gate := aGate()
	const main, tags = "refs/heads/main", "refs/tags/v*"
	for _, tc := range []struct {
		pattern string
		rule    *code.Protection
		gate    *code.GateChange
		field   string
	}{
		{main, &code.Protection{}, nil, "protect.rule.direct_push"},
		{main, &code.Protection{PushRule: code.PushRule{DirectPush: "maintainers"}}, nil, "protect.rule.direct_push"},
		{tags, &code.Protection{PushRule: code.PushRule{DirectPush: code.DirectPersons, AllowForce: true}}, nil,
			"protect.rule.allow_force"},
		{main, &code.Protection{PushRule: code.PushRule{DirectPush: code.DirectPersons}, Gate: &gate}, nil,
			"protect.rule.gate"},
		{tags, persons, &code.GateChange{Checks: ptr([]string{"unit"})}, "protect.gate"},
		{main, nil, &code.GateChange{Checks: ptr([]string{"unit"})}, "protect.gate"},
		{main, persons, &code.GateChange{}, "protect.gate"},
		{main, persons, &code.GateChange{Remove: true, Checks: ptr([]string{"unit"})}, "protect.gate.remove"},
		{main, persons, &code.GateChange{MergeBy: ptr(code.MergeBy("everyone"))}, "protect.gate.merge_by"},
		{main, persons, &code.GateChange{ApprovalsUnless: ptr([]code.ChangeClass{code.ChangeClassNoCodeChange})},
			"protect.gate.approvals_unless[0]"},
		{main, persons, &code.GateChange{Checks: ptr([]string{"unit", "unit"})}, "protect.gate.checks[1]"},
		{main, persons, &code.GateChange{Landing: &code.LandingChange{Depth: ptr(code.MaxQueueDepth + 1)}},
			"protect.gate.landing.depth"},
		{main, persons, &code.GateChange{Landing: &code.LandingChange{}}, "protect.gate.landing"},
	} {
		for _, actor := range []code.Principal{lcPerson, agent("cto"), agent("dev")} {
			req := lcProtect(actor, id, tc.pattern, tc.rule)
			req.Protect.Gate = tc.gate
			_, err := n.store.Lifecycle(t.Context(), req)
			if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), tc.field+":") {
				t.Errorf("%s protecting %s with %+v and the gate change %+v = %v, want ErrInvalid naming %s",
					cmp.Or(actor.Seat, actor.OperatorID), tc.pattern, tc.rule, tc.gate, err, tc.field)
			}
		}
	}
	if got := n.end(); got != end {
		t.Fatalf("the refused rules wrote %d records", got-end)
	}
	n.lands(lcProtect(lcPerson, id, tags, persons))
	n.lands(lcGate(lcPerson, id, main, persons, gateTo(gate)))
}

// A PROTECTION BINDS EVERY SEAT, ITS UNIT'S LEAD INCLUDED (ADR-0058): a seat
// that maintains a repository may add a protection rule or tighten one, and
// never remove one or loosen one, whatever its grant — that is a person's. Each
// control is judged against the rule the repository's rows hold for the
// pattern, inside the decide: direct_push made to admit more (none → persons),
// allow_force or allow_delete turned on, the rule's merge gate removed or
// lowered, and the rule removed are each refused not_permitted with the
// sentence naming what the change opens, and nothing is written; a change that
// tightens one control and opens another is refused for the one it opens, and
// only that one is named. A pattern no rule holds loosens nothing, so the lead
// adds a rule, and removing one nobody holds is answered from the row; a gate
// added to a rule that had none only holds merges to more, so the lead adds
// one, sends it back as it stands, and tightens direct_push beside it with a
// request naming no gate field, which keeps the gate. A person's same requests
// land. (A gate's every field is TestAnAgentCannotLoosenAGate's.)
//
// Mutations: drop the protect branch of lifecycleRefusal — the lead's removal
// lands, red; drop allow_delete's clause from loosenings — the lead turns it
// on, red; take persons rather than none for the value that admits nobody in
// direct_push's clause — the lead's tightening to none is refused, red; drop
// the gate's removal arm — the lead removes stable's gate, red.
func TestAnAgentCannotLoosenAProtection(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	cto := agent("cto")
	const main, release, stable = "refs/heads/main", "refs/heads/release/*", "refs/heads/stable"
	rule := func(direct code.DirectPush, force, del bool) *code.Protection {
		return &code.Protection{PushRule: code.PushRule{DirectPush: direct, AllowForce: force, AllowDelete: del}}
	}
	gate := aGate()
	lowered := code.GateChange{MergeBy: ptr(code.MergeByWriters)}
	id := n.made(lcPerson, "guarded", unitOwned("eng"))

	// ADDED, then TIGHTENED control by control, by the unit's lead at
	// full_access: each lands.
	n.lands(lcProtect(cto, id, main, rule(code.DirectPersons, true, true)))
	n.lands(lcProtect(cto, id, main, rule(code.DirectNone, true, true)))
	n.lands(lcProtect(cto, id, main, rule(code.DirectNone, false, true)))
	got := n.lands(lcProtect(cto, id, main, rule(code.DirectNone, false, false)))
	if want := []code.PushRule{{Pattern: main, DirectPush: code.DirectNone}}; !slices.Equal(got.Repo.Protections,
		want) {
		t.Fatalf("added and tightened by the lead, the rules read %+v, want %+v", got.Repo.Protections, want)
	}
	n.lands(lcProtect(cto, id, release, rule(code.DirectPersons, true, false)))
	// A MERGE GATE ADDED where the rule had none, sent back as it stands, and
	// kept by a tightening of direct_push that names no gate field: each lands.
	n.lands(lcProtect(cto, id, stable, rule(code.DirectPersons, false, false)))
	n.lands(lcGate(cto, id, stable, rule(code.DirectPersons, false, false), gateTo(gate)))
	n.lands(lcGate(cto, id, stable, rule(code.DirectPersons, false, false), gateTo(gate)))
	got = n.lands(lcProtect(cto, id, stable, rule(code.DirectNone, false, false)))
	if want := (code.PushRule{Pattern: stable, DirectPush: code.DirectNone}); !slices.Contains(got.Repo.Protections,
		want) {
		t.Fatalf("gated and tightened by the lead, the rules read %+v, want %+v among them", got.Repo.Protections,
			want)
	}
	if gates := n.gatesOf("guarded"); len(gates) != 1 || gates[0].Pattern != stable || !gates[0].Gate.Equal(gate) {
		t.Fatalf("gated and tightened by the lead, the gates read %+v, want stable's %+v", gates, gate)
	}

	// LOOSENED by the lead: each refused, the sentence naming the pattern and
	// what the change opens — and nothing written.
	end := n.end()
	for _, tc := range []struct {
		pattern string
		rule    *code.Protection
		gate    *code.GateChange
		opens   string
		// keeps is what the change tightens or leaves, never named.
		keeps []string
	}{
		{main, nil, nil, "removes its rule", nil},
		{main, rule(code.DirectPersons, false, false), nil, "makes direct_push persons where it was none", nil},
		{main, rule(code.DirectNone, true, false), nil, "turns allow_force on", nil},
		{main, rule(code.DirectNone, false, true), nil, "turns allow_delete on", nil},
		// TIGHTENS direct_push and allow_force, and OPENS allow_delete.
		{release, rule(code.DirectNone, false, true), nil, "turns allow_delete on",
			[]string{"direct_push", "allow_force"}},
		// THE GATE, removed and lowered.
		{stable, rule(code.DirectNone, false, false), &code.GateChange{Remove: true}, "removes its merge gate",
			[]string{"direct_push"}},
		{stable, rule(code.DirectNone, false, false), &lowered, "lowers merge_by from maintainers to writers",
			[]string{"direct_push"}},
	} {
		req := lcProtect(cto, id, tc.pattern, tc.rule)
		req.Protect.Gate = tc.gate
		res := n.life(req)
		switch {
		case res.Refusal != code.LifecycleNotPermitted:
			t.Errorf("the lead's change of %s that %s: %+v, want not_permitted", tc.pattern, tc.opens, res)
			continue
		case !strings.Contains(res.Detail, "only a person may remove or loosen the protection on "+tc.pattern) ||
			!strings.Contains(res.Detail, tc.opens):
			t.Errorf("the lead's change of %s refused %q, want the sentence naming the pattern and that it %s",
				tc.pattern, res.Detail, tc.opens)
		case !errors.Is(res.Err(), code.ErrNotPermitted):
			t.Errorf("the lead's change of %s as an error is %v, want ErrNotPermitted", tc.pattern, res.Err())
		}
		for _, kept := range tc.keeps {
			if strings.Contains(res.Detail, kept) {
				t.Errorf("the lead's change of %s names %s, which it tightens: %q", tc.pattern, kept, res.Detail)
			}
		}
	}
	if got := n.end(); got != end {
		t.Fatalf("the lead's refused loosenings wrote %d records", got-end)
	}
	// REMOVING A RULE NOBODY HOLDS loosens nothing: answered from the row.
	if res := n.life(lcProtect(cto, id, "refs/heads/nothing", nil)); res.Refusal != "" ||
		res.Outcome != statelog.OutcomeApplied || !res.Position.IsZero() {
		t.Errorf("the lead's removal of a rule nobody holds: %+v, want it answered from the row", res)
	}

	// A PERSON LOOSENS: the same requests land.
	n.lands(lcProtect(lcPerson, id, main, rule(code.DirectPersons, false, false)))
	n.lands(lcProtect(lcPerson, id, main, rule(code.DirectPersons, true, false)))
	n.lands(lcProtect(lcPerson, id, main, rule(code.DirectPersons, true, true)))
	n.lands(lcProtect(lcPerson, id, release, rule(code.DirectNone, false, true)))
	n.lands(lcGate(lcPerson, id, stable, rule(code.DirectNone, false, false), lowered))
	n.lands(lcGate(lcPerson, id, stable, rule(code.DirectNone, false, false), code.GateChange{Remove: true}))
	got = n.lands(lcProtect(lcPerson, id, main, nil))
	if want := []code.PushRule{{Pattern: release, DirectPush: code.DirectNone, AllowDelete: true},
		{Pattern: stable, DirectPush: code.DirectNone}}; !slices.Equal(got.Repo.Protections, want) {
		t.Fatalf("loosened by a person, the rules read %+v, want %+v", got.Repo.Protections, want)
	}
	if gates := n.gatesOf("guarded"); len(gates) != 0 {
		t.Fatalf("loosened by a person, the gates read %+v, want stable's removed", gates)
	}
}

// A RENAME REDIRECTS ITS OLD NAME UNTIL ITS RELEASE: the old name answers the
// repository under its new name, the new name answers it as itself, nothing
// else may take the old name while it redirects, and renaming back takes it
// back. The redirect lasts exactly as long as the old name's row, which a
// release record removes — and only a release whose own broker instant is past
// the redirect's end: one decided early on a clock ahead of the broker's
// releases nothing, alike on every node.
//
// Mutation: drop `redirect_until <= ?` from the release's apply — the early
// release removes the row, red.
func TestRenameRedirectsAndReleases(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	id := n.made(lcPerson, "alpha", unitOwned("eng"))
	n.lands(lcRename(lcPerson, id, "beta"))
	old := n.resolved("alpha")
	if old.ID != id || old.Name != "beta" || old.Redirect != "beta" || old.RedirectUntil.IsZero() {
		t.Fatalf("the old name answers %+v, want %s under beta, redirecting", old, id)
	}
	if cur := n.resolved("beta"); cur.ID != id || cur.Redirect != "" {
		t.Fatalf("the new name answers %+v, want %s as itself", cur, id)
	}
	if taken := n.life(lcCreate(lcPerson, "alpha", unitOwned("eng"))); taken.Refusal != code.LifecycleNameTaken ||
		!strings.Contains(taken.Detail, "beta") {
		t.Fatalf("a create of the redirecting name: %+v, want name_taken naming beta", taken)
	}
	n.lands(lcRename(lcPerson, id, "alpha"))
	if back := n.resolved("alpha"); back.ID != id || back.Redirect != "" {
		t.Fatalf("renamed back, alpha answers %+v, want the repository as itself", back)
	}
	beta := n.resolved("beta")
	if beta.Redirect != "alpha" {
		t.Fatalf("renamed back, beta answers %+v, want it redirecting to alpha", beta)
	}

	release := func(at time.Time) {
		t.Helper()
		n.storedAt = func(uint64, time.Time) time.Time { return at }
		defer func() { n.storedAt = nil }()
		if _, err := n.publish(code.NameSubject("beta"), code.OpReleaseName,
			code.Scope{code.NamePath(code.NameToken("beta"))}, code.ReleaseName{Name: "beta", RepoID: id}); err != nil {
			t.Fatalf("release beta: %v", err)
		}
	}
	held := `SELECT COUNT(*) FROM code_names WHERE name = 'beta'`
	release(beta.RedirectUntil.Add(-time.Minute))
	if n.count(held) != 1 {
		t.Fatal("a release whose own instant is before the redirect's end removed the old name")
	}
	release(beta.RedirectUntil.Add(time.Minute))
	if n.count(held) != 0 {
		t.Fatal("a release past the redirect's end left the old name")
	}
}

// TWO RENAMES OF ONE REPOSITORY LAND ONE. They arbitrate on two name subjects,
// so the broker lets both through; the second applies as nothing — the
// repository is no longer under its From — on every node alike, claiming no
// name and moving no redirect. And a rename whose new name another repository
// holds — which only an arbitration the broker let through on another anchor
// can append — applies as nothing WHOLE: read step by step it would rename the
// row and redirect the old name to a name the repository does not hold
// (M3-decisions M3-O48).
//
// Mutation: drop the apply's From check — the repository is renamed twice and
// claims both names, red; skip only the claim of the new name's step — the
// victim is renamed to a name it does not hold, red.
func TestTwoRenamesOfOneRepositoryLandOne(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	id := n.made(lcPerson, "solo", unitOwned("eng"))
	n.hold()
	first := n.life(lcRename(lcPerson, id, "solo-b"))
	second := n.life(lcRename(lcPerson, id, "solo-c"))
	n.release()
	if first.Outcome != statelog.OutcomePending || second.Outcome != statelog.OutcomePending {
		t.Fatalf("decided behind its applier, the renames answered %+v and %+v, want both pending", first, second)
	}
	if _, ok := n.repo("solo-b"); !ok {
		t.Fatal("the first rename did not land")
	}
	if n.count(`SELECT COUNT(*) FROM code_names WHERE name = 'solo-c'`) != 0 {
		t.Fatal("the second rename claimed its name")
	}
	if n.count(`SELECT COUNT(*) FROM code_names WHERE name = 'solo' AND redirect_until IS NOT NULL`) != 1 {
		t.Fatal("the old name does not redirect")
	}

	other := n.made(lcPerson, "taken", unitOwned("eng"))
	victim := n.made(lcPerson, "victim", unitOwned("eng"))
	if _, err := n.publish(code.NameSubject("taken"), code.OpRename,
		code.Scope{code.NamePath(code.NameToken("taken")), code.NamePath(code.NameToken("victim")),
			code.RepoPath(victim)}, code.Rename{RepoID: victim, From: "victim", To: "taken"}); err != nil {
		t.Fatalf("append the rename onto a held name: %v", err)
	}
	if row, ok := n.repo("victim"); !ok || row.id != victim {
		t.Fatalf("the rename onto another's name moved the repository: %+v %v", row, ok)
	}
	if n.count(`SELECT COUNT(*) FROM code_names WHERE name = 'victim' AND redirect_until IS NULL`) != 1 {
		t.Fatal("the rename onto another's name redirected the old name")
	}
	if holder := n.resolved("taken"); holder.ID != other {
		t.Fatalf("taken is held by %s, want %s", holder.ID, other)
	}
}

// A LIFECYCLE REQUEST BEHIND THE CONFIGURATION IS NOT SERVED: every kind's "who
// may" reads the chart, a person's included, so a node confirmed behind the
// company's configuration decides none of them — answered ErrNotServing, ran
// nothing, so the router asks the next data node (M3-decisions M3-O8).
//
// Mutation: drop the gate — the create lands, red.
func TestALifecycleRequestBehindTheConfigurationIsNotServed(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	id := n.made(lcPerson, "kept", unitOwned("eng"))
	end := n.end()
	n.behind.Store(true)
	for _, req := range []code.LifecycleRequest{
		lcCreate(code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}, "dev.x", seatOwned("dev")),
		lcRename(lcPerson, id, "kept-b"),
	} {
		if _, err := n.store.Lifecycle(t.Context(), req); !errors.Is(err, code.ErrNotServing) {
			t.Errorf("a %s behind the configuration = %v, want ErrNotServing", req.Kind, err)
		}
	}
	if got := n.end(); got != end {
		t.Fatalf("a node behind the configuration wrote %d records", got-end)
	}
	// AND A NODE WITH NO EPOCH'S CHART YET, and a store built to decide no
	// lifecycle at all — the code log's eviction gate's, which is handed no
	// chart and no policy — answer it so too, never judging by a nil chart.
	n.behind.Store(false)
	n.chart = nil
	if _, err := n.store.Lifecycle(t.Context(), lcRename(lcPerson, id, "kept-c")); !errors.Is(err, code.ErrNotServing) {
		t.Errorf("a store whose node holds no chart = %v, want ErrNotServing", err)
	}
	gate, err := code.NewStore(code.StoreOptions{Publisher: n.pub, DB: n.db.Reader(), NodeID: "gate"})
	if err != nil {
		t.Fatalf("build the eviction gate's store: %v", err)
	}
	if _, err := gate.Lifecycle(t.Context(), lcRename(lcPerson, id, "kept-d")); !errors.Is(err, code.ErrNotServing) {
		t.Errorf("a store built to decide no lifecycle = %v, want ErrNotServing", err)
	}
	if got := n.end(); got != end {
		t.Fatalf("a node that decides no lifecycle wrote %d records", got-end)
	}
}

// A RENAME KEEPS ITS KIND'S FORM, for every actor: a scratch repository keeps
// its seat's handle and its dot, and a project stays one label — renamed to
// <handle>.<slug>, a project would read as that seat's own scratch repository
// and squat the name its create would take.
//
// Mutation: drop the project arm of the rule — web becomes dev.web, red.
func TestARenameKeepsItsKindsForm(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	dev := code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}
	cto := code.Principal{Kind: code.PrincipalAgent, Seat: "cto"}
	project := n.made(lcPerson, "web", unitOwned("eng"))
	scratch := n.made(dev, "dev.x", seatOwned("dev"))
	for name, req := range map[string]code.LifecycleRequest{
		"a project renamed to a scratch name by its lead": lcRename(cto, project, "dev.web"),
		"a project renamed to a scratch name by a person": lcRename(lcPerson, project, "dev.web"),
		"a scratch repository renamed to a project name":  lcRename(dev, scratch, "web2"),
		"a scratch repository renamed for another seat":   lcRename(dev, scratch, "reader.x"),
		"the same, by a person":                           lcRename(lcPerson, scratch, "reader.x"),
	} {
		if got := n.life(req); got.Refusal != code.LifecycleNotPermitted || got.Detail == "" {
			t.Errorf("%s: %+v, want not_permitted naming the rule", name, got)
		}
	}
	n.lands(lcRename(dev, scratch, "dev.y"))
}

// A SCRATCH REPOSITORY REFUSES AN INDEX, whoever asks: a seat's own experiment
// is searched with grep, so settings naming it indexed are no settings a record
// may carry, a create asking for one is refused before it is decided, and a
// settings change asking for one is refused not_permitted with the sentence
// naming grep — its own seat's and a person's alike — with nothing written. A
// PROJECT's mode is a setting like its description: a person sets it, the
// owning unit's lead (maintain) sets it back, a member below maintain is
// refused, and each applied change is what the row then reads.
//
// Mutations: drop the scratch arm of RepoSettings.Validate — the indexed
// scratch settings validate and the create lands, red; drop the decide's
// scratch refusal — the change is refused only by the settings' validation, as
// ErrInvalid rather than not_permitted, red; apply the change's mode to the
// settings no more (next.Search kept) — the project's change writes nothing,
// red.
func TestAScratchRepositoryRefusesAnIndex(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	dev := agent("dev")

	indexed := code.RepoSettings{ManagedBy: code.ManagedTools, Kind: code.RepoScratch, Owner: seatOwned("dev"),
		Visibility: code.VisibilityCompany, Search: code.SearchIndexed, DefaultBranch: "main"}
	if err := indexed.Validate(); !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "search:") {
		t.Errorf("an indexed scratch repository's settings validate as %v, want ErrInvalid naming search", err)
	}
	end := n.end()
	create := lcCreate(dev, "dev.indexed", seatOwned("dev"))
	create.Create.Search = code.SearchIndexed
	if _, err := n.store.Lifecycle(t.Context(), create); !errors.Is(err, code.ErrInvalid) ||
		!strings.Contains(err.Error(), "create.search:") {
		t.Errorf("a seat's create of its own scratch repository indexed = %v, want ErrInvalid naming "+
			"create.search", err)
	}
	if got := n.end(); got != end {
		t.Fatalf("the refused create wrote %d records", got-end)
	}

	scratch := n.made(dev, "dev.spike", seatOwned("dev"))
	if got := n.resolved("dev.spike").Search; got != code.SearchGrep {
		t.Fatalf("the premise: a scratch repository is created at %q, want %s", got, code.SearchGrep)
	}
	end = n.end()
	for _, actor := range []code.Principal{dev, lcPerson} {
		got := n.life(lcSettings(actor, scratch, code.RepoChange{Search: ptr(code.SearchIndexed)}))
		if got.Refusal != code.LifecycleNotPermitted || !strings.Contains(got.Detail, "searched with grep") {
			t.Errorf("%s asking dev.spike for an index: %+v, want not_permitted naming grep",
				cmp.Or(actor.Seat, actor.OperatorID), got)
		}
	}
	if got := n.end(); got != end {
		t.Fatalf("the refused changes wrote %d records", got-end)
	}

	project := n.made(lcPerson, "searched", unitOwned("eng"))
	if got := n.resolved("searched").Search; got != code.SearchGrep {
		t.Fatalf("the premise: a project a tool made is created at %q, want %s", got, code.SearchGrep)
	}
	n.lands(lcSettings(lcPerson, project, code.RepoChange{Search: ptr(code.SearchIndexed)}))
	if got := n.resolved("searched").Search; got != code.SearchIndexed {
		t.Errorf("after a person indexed it, the project reads %q", got)
	}
	if got := n.life(lcSettings(dev, project, code.RepoChange{Search: ptr(code.SearchGrep)})); got.Refusal !=
		code.LifecycleNotPermitted {
		t.Errorf("a member below maintain changing the project's mode: %+v, want not_permitted", got)
	}
	n.lands(lcSettings(agent("cto"), project, code.RepoChange{Search: ptr(code.SearchGrep)}))
	if got := n.resolved("searched").Search; got != code.SearchGrep {
		t.Errorf("after its unit's lead set it back, the project reads %q", got)
	}
}

// purgeNow steps a deleted repository's purge until it is done — the store's
// clock (harnessAt) is past every deletion this harness makes.
func (n *node) purgeNow(id string) {
	n.t.Helper()
	for range 8 {
		res, err := n.store.PurgeStep(n.t.Context(), id, keep)
		if err != nil {
			n.t.Fatalf("purge %s: %v", id, err)
		}
		if res.Done {
			return
		}
	}
	n.t.Fatalf("eight steps did not purge %s", id)
}

// THE SCRATCH LIMIT COUNTS THE SEAT'S REPOSITORIES UNTIL THEY ARE PURGED: a
// deleted one keeps its room until its purge frees it, so a create-and-delete
// loop cannot hold rows, names and packs past code.scratch.max_per_seat; and a
// limit of 0 lets no seat create one.
//
// Mutation: count only `status IN ('active','archived')` — the create past
// the cap lands, red.
func TestTheScratchLimitCountsTheSeatsRepositories(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	n.scratch.ScratchMaxPerSeat = 2
	dev := code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}
	n.made(dev, "dev.a", seatOwned("dev"))
	b := n.made(dev, "dev.b", seatOwned("dev"))
	n.lands(lcOn(code.LifecycleDelete, dev, b))
	if got := n.life(lcCreate(dev, "dev.c", seatOwned("dev"))); got.Refusal != code.LifecycleScratchLimit ||
		!strings.Contains(got.Detail, "code.scratch.max_per_seat") {
		t.Fatalf("a create past the cap, a deleted one among them: %+v, want scratch_limit", got)
	}
	// ANOTHER SEAT'S ROOM IS ITS OWN.
	n.made(code.Principal{Kind: code.PrincipalAgent, Seat: "root-bot"}, "root-bot.a", seatOwned("root-bot"))
	n.purgeNow(b)
	n.made(dev, "dev.c", seatOwned("dev"))
	n.scratch.ScratchMaxPerSeat = 0
	if got := n.life(lcCreate(dev, "dev.d", seatOwned("dev"))); got.Refusal != code.LifecycleScratchLimit ||
		!strings.Contains(got.Detail, "allows no scratch repositories") {
		t.Fatalf("a create where the cap is 0: %+v, want scratch_limit saying so", got)
	}
}

// THE REPO LIMIT COUNTS PROJECT REPOSITORIES: every one short of purged,
// declared ones included, so declared and tool-made projects share
// config.CodeMaxRepos — and a seat's scratch repositories never count, so
// seats' experiments cannot refuse a person's project. The count is the one
// row the applier keeps (code_repo_kinds), counted in by a create and out by
// the purge.
//
// Mutation: count every kind's repositories — the scratch create is refused,
// red; count a deleted project out at its delete — the project create past the
// limit lands, red.
func TestTheRepoLimitCountsProjectRepositories(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	n.scratch.MaxRepos = 2
	n.ensure("declared", described("one"), t1)
	p := n.made(lcPerson, "p1", unitOwned("eng"))
	n.lands(lcOn(code.LifecycleDelete, lcPerson, p))
	if got := n.life(lcCreate(lcPerson, "p2", unitOwned("eng"))); got.Refusal != code.LifecycleRepoLimit ||
		!strings.Contains(got.Detail, "config.CodeMaxRepos") {
		t.Fatalf("a project create at the limit: %+v, want repo_limit", got)
	}
	n.made(code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}, "dev.s", seatOwned("dev"))
	n.purgeNow(p)
	n.made(lcPerson, "p2", unitOwned("eng"))
	if got := n.count(`SELECT repos FROM code_repo_kinds WHERE kind = 'project'`); got != 2 {
		t.Fatalf("the project count reads %d, want 2: the declared one and p2", got)
	}
}

// A SCRATCH CREATE COSTS THE LOG ONE SMALL RECORD — the figure the dropped
// validate warning would have multiplied (M3-decisions M3-O44): the signed
// frame of a seat's create at the longest name, under a kibibyte with no
// description and under two with the longest one, once a repository.
func TestAScratchCreateCostsTheLogOneSmallRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	dev := code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}
	frame := func(slug, description string, under int) {
		t.Helper()
		name := "dev." + slug + strings.Repeat("s", gitref.MaxRepoNameBytes-len("dev.")-len(slug))
		req := lcCreate(dev, name, seatOwned("dev"))
		req.Create.Description = description
		got := n.lands(req)
		_, payload, _, ok, err := n.log.At(t.Context(), got.Position.Seq)
		if err != nil || !ok {
			t.Fatalf("read the create's frame: %v %v", ok, err)
		}
		t.Logf("a scratch create of a %d-byte name and a %d-byte description is a %d-byte frame",
			len(name), len(description), len(payload))
		if len(payload) >= under {
			t.Errorf("the frame is %d bytes, want under %d", len(payload), under)
		}
	}
	frame("a", "", 1<<10)
	frame("b", strings.Repeat("d", code.MaxDescriptionBytes), 2<<10)
}

// THE ACTIVATION FENCE IGNORES A TOOL'S RECORD: it orders configuration
// activations, and a tool's record comes from none, so one below the fence
// applies — and the applier still refuses a tool's settings that reach a
// repository the configuration manages, by its manager rather than the fence,
// so a race with an activation adopting a name hands nothing over.
//
// Mutation: fence a tool's record like a configuration's — its settings below
// the activation are refused, red.
func TestTheActivationFenceIgnoresAToolsRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	n.activate(t2)
	id := n.made(lcPerson, "tool-made", unitOwned("eng"))
	got := n.lands(lcSettings(lcPerson, id, code.RepoChange{Description: new("changed")}))
	if got.Repo.Description != "changed" {
		t.Fatalf("a tool's settings below the fence read back %q, want them applied", got.Repo.Description)
	}
	n.ensure("declared", described("one"), t3)
	declared := n.mustID("declared")
	s := code.RepoSettings{ManagedBy: code.ManagedTools, Kind: code.RepoProject, Owner: unitOwned("eng"),
		Visibility: code.VisibilityCompany, Search: code.SearchGrep, DefaultBranch: "main", Description: "taken"}
	if _, err := n.publish(code.RepoSubject(declared), code.OpSettings, code.Scope{code.RepoPath(declared)},
		code.SettingsRecord{RepoID: declared, Settings: s}); err != nil {
		t.Fatalf("append a tool's settings onto a declared repository: %v", err)
	}
	if row, ok := n.repo("declared"); !ok || row.description != "one" {
		t.Fatalf("a tool's settings reached a declared repository: %+v", row)
	}
	if got := n.settingsOf("declared"); got.ManagedBy != code.ManagedConfig {
		t.Fatalf("the declared repository is managed by %s now", got.ManagedBy)
	}
}
