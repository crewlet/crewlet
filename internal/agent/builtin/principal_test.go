package builtin_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tracker"
)

// boundPerson is somebody signed in whose directory row binds them to a seat.
func boundPerson(login, seat string, grants ...iam.Grant) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: login, Kind: iam.KindPerson,
		Seat: seat, Stage: iam.StageActive, Grants: grants}
}

// machine is a Tier A token nobody is bound through.
func machine(id string, grants ...iam.Grant) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: "token:" + id,
		Kind: iam.KindMachine, Stage: iam.StageActive, Grants: grants}
}

// A PERSON'S OWN WRITE DOES NOT WAKE THEM.
//
// The tracker's router drops the actor from every wake a change produces —
// nobody is paged about what they just did — and it recognises the actor by
// the NAME the record carries. The operator surface recorded a person under
// their TOKEN's id, which is never a seat handle, so the drop never matched:
// the assignee who edited their own task through the dashboard or their
// assistant was woken about their own edit, every time.
//
// The control is that attribution, rebuilt: the same change, authored under
// the token id, reaches the person who made it.
func TestAPersonsWriteSuppressesTheirOwnWake(t *testing.T) {
	t.Parallel()
	ana := boundPerson("ana.silva", "ana")
	task := tracker.Task{ID: "t-1", Key: "ENG-1", Project: "ENG", Assignee: "ana",
		Status: tracker.StatusTodo}
	moved := task
	moved.Status = tracker.StatusInProgress
	notify := tracker.Wake{Kind: tracker.ChangeStatus, Before: task,
		After: moved}.Notify(nil)
	candidates := tracker.Candidates(notify, false)

	woken := func(actor string) bool {
		for _, c := range tracker.Route(candidates, nil, actor) {
			if c.Handle == "ana" {
				return true
			}
		}
		return false
	}
	actor := builtin.ActorOf(ana)
	if actor.Handle != "ana" || actor.Kind != tracker.AuthorHuman {
		t.Fatalf("a bound person writes as %q (%s), want their seat as a human",
			actor.Handle, actor.Kind)
	}
	if woken(actor.Handle) {
		t.Error("ana was woken about the status she moved herself")
	}
	// THE CONTROL: the attribution this replaced, under the token's id.
	if !woken("ana-laptop") {
		t.Fatal("the control woke nobody, so this test cannot tell the two " +
			"attributions apart")
	}

	// AND THE KNOWLEDGE BASE WRITES THE SAME NAME, or the audit feed that
	// reads both histories shows one person as two.
	page, err := builtin.PageActorOf(ana)
	if err != nil || page.Name() != actor.Handle || page.Kind != pages.AuthorHuman {
		t.Errorf("a page write is attributed as %+v (%v), the tracker as %q",
			page, err, actor.Handle)
	}
}

// A MACHINE'S NAME CAN NEVER SATISFY A CHECK THAT COMPARES NAMES TO A SEAT.
//
// An own-record class admits a caller whose name EQUALS the record's owner,
// and a record's owner is a seat handle. A token's login is `token:<id>`: the
// colon is not in the seat-handle grammar, and a person's login needs a dot,
// which is not in it either — so no seat can be named what a credential is
// named, and a token called `ops` cannot mark the inbox of the seat `ops`.
//
// The control is the bare id the operator surface used to record: `ops` IS a
// valid seat handle, and a principal carrying it walks straight into that
// seat's record.
func TestAMachineActorCannotSatisfyAnOwnRecordCheck(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"ops", "ci", "release-bot"} {
		token := machine(id)
		actor := builtin.ActorOf(token)
		if org.ValidHandle(actor.Handle) {
			t.Errorf("a token acts as %q, which a seat could also be called",
				actor.Handle)
		}
		for _, action := range []authz.Action{
			authz.ActionInboxMark, authz.ActionPinsSet,
		} {
			d := authz.Decide(t.Context(), token, action,
				authz.Object{Kind: authz.KindPerson, Owner: id}, chartLeads,
				time.Now())
			if d.Allowed {
				t.Errorf("the token %q passed %s on the seat %q's record (%s)",
					id, action, id, d.Reason)
			}
		}
		// THE CONTROL: the stripped id, which is a legal seat handle.
		bare := token
		bare.Login = id
		if !org.ValidHandle(id) {
			t.Fatalf("the control %q is not a seat handle", id)
		}
		if d := authz.Decide(t.Context(), bare, authz.ActionInboxMark,
			authz.Object{Kind: authz.KindPerson, Owner: id}, chartLeads,
			time.Now()); !d.Allowed {
			t.Fatalf("the control was refused (%s), so this test cannot see "+
				"the collision it guards against", d.Reason)
		}
	}
}

// AN UNBOUND CREDENTIAL WRITES AS ITS WHOLE LOGIN, and says what it is.
func TestAnUnboundCredentialWritesAsItsLogin(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		p    iam.Principal
		name string
		kind tracker.AuthorKind
	}{
		"a token":          {machine("ops-bot"), "token:ops-bot", tracker.AuthorOperator},
		"an unbound human": {boundPerson("jane.doe", ""), "jane.doe", tracker.AuthorOperator},
		"a bound human":    {boundPerson("jane.doe", "jane"), "jane", tracker.AuthorHuman},
	} {
		actor := builtin.ActorOf(c.p)
		if actor.Handle != c.name || actor.Kind != c.kind {
			t.Errorf("%s writes as %q (%s), want %q (%s)", name, actor.Handle,
				actor.Kind, c.name, c.kind)
		}
		if actor.OperatorID != c.p.Login {
			t.Errorf("%s records the credential as %q, want its login %q",
				name, actor.OperatorID, c.p.Login)
		}
	}
}

// A WRITE MADE THROUGH A MACHINE TOKEN NAMES THE TOKEN.
//
// The guard composes a machine token as its OWNER, and the owner is the
// author — it is their authority being exercised, and the tracker's "do not
// wake the author" rule has to recognise them. What tells the write apart
// from one the owner made themselves is the operator column: `pat:<id>` on the
// work record and on the page record alike, for a person bound to a seat and
// for a service account. It used to be the owner's login, so an item a
// person's assistant filed and one they filed themselves were the same row.
// Mutation: record the login again and the operator is the owner's.
func TestAWriteThroughAMachineTokenNamesTheToken(t *testing.T) {
	t.Parallel()
	const token = "0192f00d-0000-7000-8000-00000000000a"
	via := iam.MachineTokenName(token)
	for name, c := range map[string]struct {
		p      iam.Principal
		author string
		kind   tracker.AuthorKind
	}{
		"a person's own token": {func() iam.Principal {
			p := boundPerson("sarah.chen", "sarah")
			p.Via = via
			return p
		}(), "sarah", tracker.AuthorHuman},
		"a service account's token": {iam.Principal{ID: uuid.New(),
			Login: "svc:ci", Kind: iam.KindMachine, Stage: iam.StageActive,
			Via: via}, "svc:ci", tracker.AuthorOperator},
	} {
		work := builtin.ActorOf(c.p)
		if work.Handle != c.author || work.Kind != c.kind || work.OperatorID != via {
			t.Errorf("%s: a work record is written as %q (%s) through %q, "+
				"want %q (%s) through %q", name, work.Handle, work.Kind,
				work.OperatorID, c.author, c.kind, via)
		}
		page, err := builtin.PageActorOf(c.p)
		if err != nil || page.Handle != c.author || page.OperatorID != via {
			t.Errorf("%s: a page record is written as %q through %q (%v), "+
				"want %q through %q", name, page.Handle, page.OperatorID, err,
				c.author, via)
		}
	}
}

// A REQUEST NOBODY RESOLVED IS REFUSED, not written as nobody.
func TestAWriteWithNobodyBehindItIsRefused(t *testing.T) {
	t.Parallel()
	for name, ctx := range map[string]func() (error, error){
		"no principal at all": func() (error, error) {
			_, workErr := builtin.PrincipalActor(t.Context(), nil)
			_, pageErr := builtin.PrincipalPageActor(t.Context(), nil)
			return workErr, pageErr
		},
		"an anonymous caller": func() (error, error) {
			anon := iam.WithAnonymous(t.Context())
			_, workErr := builtin.PrincipalActor(anon, nil)
			_, pageErr := builtin.PrincipalPageActor(anon, nil)
			return workErr, pageErr
		},
	} {
		workErr, pageErr := ctx()
		if workErr == nil || pageErr == nil {
			t.Errorf("%s: a write was attributed (work %v, page %v)",
				name, workErr, pageErr)
		}
	}
	// AND A RESOLVED ONE IS WRITTEN AS THEMSELVES, which is the control.
	resolved := iam.WithPrincipal(t.Context(), machine("ops"))
	if actor, err := builtin.PrincipalActor(resolved, nil); err != nil ||
		actor.Handle != "token:ops" {
		t.Errorf("a resolved token writes as %+v (%v)", actor, err)
	}
}
