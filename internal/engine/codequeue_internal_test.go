package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	gitlethome "github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A QUEUED CLOSE IS WRITTEN AS ITS ENQUEUER (M5-D27): the work item of a
// change its queue landed is closed in the enqueuer's own name — a seat as
// itself, a person as the human seat they hold with the credential they
// enqueued through beside it, the inverse of builtin.CodeAskerOf — and never
// in a name of the queue's: a principal bound to no seat closes nothing, and
// an item on a vendor's tracker is refused before anything is read or
// written, the engine holding no credential of the enqueuer's there. A SEAT'S
// CLOSE IS WRITTEN FOR WHOM ITS ENQUEUE WAS MADE FOR (ADR-0109): the enqueuing
// turn's write made late, so the close a seat's enqueue made for founder asks
// is written for founder, as the turn's own writes state it, and one made for
// nobody for nobody, the seat's own work. A person writes for nobody but
// themselves, whatever their entry holds.
//
// Mutation: write a person's close as an operator (or drop its credential) →
// red; give an unbound credential a name of its own → red; close a vendor's
// item through the engine's tracker → it reaches the nil engine, red; write
// a seat's close for nobody whatever its enqueue was made for → the close
// founder's enqueue asks is written for nobody, red.
func TestAQueuedCloseIsWrittenAsItsEnqueuer(t *testing.T) {
	t.Parallel()
	founder := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "founder"}
	for _, c := range []struct {
		name string
		as   gitlet.Principal
		// madeFor is whom the entry's enqueue was made for, as its row keeps
		// it.
		madeFor *types.WorkingFor
		want    builtin.Actor
	}{
		{"a seat whose enqueue was made for founder", gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"},
			founder, builtin.Actor{Handle: "dev", Kind: worklet.AuthorAgent, WrittenFor: types.ForTurn(founder)}},
		{"a seat whose enqueue was made for nobody", gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"},
			nil, builtin.Actor{Handle: "dev", Kind: worklet.AuthorAgent, WrittenFor: types.ForNobody()}},
		{"a person", gitlet.Principal{Kind: gitlet.PrincipalOperator, Seat: "founder", OperatorID: "pat:ada"},
			nil, builtin.Actor{Handle: "founder", Kind: worklet.AuthorHuman, OperatorID: "pat:ada"}},
		{"a person, whatever the entry holds", gitlet.Principal{Kind: gitlet.PrincipalOperator, Seat: "founder",
			OperatorID: "pat:ada"}, founder,
			builtin.Actor{Handle: "founder", Kind: worklet.AuthorHuman, OperatorID: "pat:ada"}},
	} {
		got, err := enqueuerActor(c.as, c.madeFor)
		if err != nil || got.Handle != c.want.Handle || got.Kind != c.want.Kind ||
			got.OperatorID != c.want.OperatorID || !reflect.DeepEqual(got.WrittenFor, c.want.WrittenFor) {
			t.Errorf("%s's close is written as %+v (%v), want %+v", c.name, got, err, c.want)
		}
	}
	for _, unbound := range []gitlet.Principal{
		{Kind: gitlet.PrincipalOperator, OperatorID: "token:founder"},
		{Kind: gitlet.PrincipalSystem},
	} {
		if got, err := enqueuerActor(unbound, nil); err == nil {
			t.Errorf("%+v, bound to no seat, closes as %+v, want refused", unbound, got)
		}
	}

	err := queueCloser{}.CloseWorkItem(t.Context(), gitlethome.TaskClose{
		As:       gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"},
		WorkItem: types.WorkItem{Backend: types.WorkJira, ID: "10042", Key: "ENG-42"},
	})
	var refused *gitlethome.NotClosed
	if !errors.Is(err, errQueueCloseNotWorklet) || !errors.As(err, &refused) ||
		!strings.Contains(refused.Why, "ENG-42") {
		t.Fatalf("closing a Jira issue answered %v, want it settled not closed, naming ENG-42, as not Worklet's",
			err)
	}
	// A CLOSE NO ASKING AGAIN CHANGES is final; an unbound enqueuer is one.
	err = queueCloser{}.CloseWorkItem(t.Context(), gitlethome.TaskClose{
		As:       gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:founder"},
		WorkItem: types.WorkItem{Backend: types.WorkWorklet, ID: "task-1", Key: "ENG-1"},
	})
	if !errors.As(err, &refused) {
		t.Fatalf("closing as a credential bound to no seat answered %v, want it settled not closed", err)
	}
}

// A CLOSE WORKLET REFUSES FOR GOOD IS NOT ASKED AGAIN (M5-D27): a queued
// change's close that Worklet answers with a refusal it MARKS as final — the
// item gone, not the enqueuer's to write, its content or its state refusing
// the close — is answered NotClosed, in the engine's own sentence naming the
// item, so the queue settles it and tells the change's people once; a
// failure of the node's own stays a plain error, which leaves the close owed
// for the next turn.
//
// Mutations: let a gone item through as the node's failure → it is asked
// again every turn, red; let a refused write through → red; settle the
// node's own failure → it is never asked again, red.
func TestACloseWorkletRefusesForGoodIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	item := types.WorkItem{Backend: types.WorkWorklet, ID: "task-42", Key: "ENG-42"}
	for _, c := range []struct {
		name  string
		err   error
		final bool
	}{
		{"an item gone", fmt.Errorf("%w: task-42", worklet.ErrNoTask), true},
		{"a write not the enqueuer's", &worklet.Refusal{Mark: worklet.ErrForbidden,
			Err: errors.New("not dev's to write")}, true},
		{"a close its content refuses", &worklet.Refusal{Mark: worklet.ErrInvalid,
			Err: errors.New("a required field is empty")}, true},
		{"a close its state refuses", &worklet.Refusal{Mark: worklet.ErrState,
			Err: errors.New("it is in the trash")}, true},
		{"the node's own failure", errors.New("the data node did not answer"), false},
	} {
		got := closeFailed(item, "dev", "close", c.err)
		var refused *gitlethome.NotClosed
		switch {
		case !errors.Is(got, c.err):
			t.Errorf("%s: answered %v, which lost the cause %v", c.name, got, c.err)
		case c.final && (!errors.As(got, &refused) || !strings.Contains(refused.Why, "ENG-42")):
			t.Errorf("%s: answered %v, want it settled not closed naming ENG-42", c.name, got)
		case !c.final && errors.As(got, &refused):
			t.Errorf("%s: settled not closed (%q), want it left owed and asked again", c.name, refused.Why)
		}
	}
}

// closeRequesters composes the principals a queued close is decided for, as
// the engine's turnRequesters does on the closing node: a seat or a person by
// handle, a credential by "login:" and its login, any other handle as an
// agent seat holding every app — save a handle in left, which the
// organisation holds no seat for any more (errNoSuchSeat, the composer's
// definitive answer), and one in unreadable, whose row the directory cannot
// read now.
type closeRequesters struct {
	held       map[string]iam.Principal
	left       []string
	unreadable []string
}

func (r closeRequesters) Requester(_ context.Context, handle string) (iam.Principal, error) {
	switch {
	case slices.Contains(r.left, handle):
		return iam.Principal{}, fmt.Errorf("%w %q", errNoSuchSeat, handle)
	case slices.Contains(r.unreadable, handle):
		return iam.Principal{}, errors.New("engine: the identity directory did not answer")
	}
	if p, ok := r.held[handle]; ok {
		return p, nil
	}
	return iam.Principal{Kind: iam.KindSeat, Seat: handle, Stage: iam.StageActive, Grants: iam.Apps}, nil
}

func (r closeRequesters) Credential(_ context.Context, login string) (iam.Principal, bool, error) {
	p, ok := r.held["login:"+login]
	return p, ok, nil
}

// A QUEUED CLOSE IS DECIDED WITHIN THE APPS OF WHOM ITS ENQUEUE WAS MADE FOR
// (ADR-0109, M5-D27): swe queued a change in a turn somebody woke, and the
// queue's close of its work item, that turn's write made late, is decided as
// a call of the turn would be now — for swe, within what whom the enqueue was
// made for may use, composed again on the closing node. Each close is decided
// as the queue's close composes it, from the close the home hands over (the
// entry's enqueuer and the carrier its row keeps), and a close made is made
// as the very actor it was decided for: swe, written for whom the enqueue was
// made for. Made for ada, who does not use Worklet, it is not closed, naming
// her and the app; for the token whose write carried Worklet, closed, and for
// the same token whose write carried Chatlet alone, not, the app named; for
// cto, a colleague seat, closed; for nobody, decided for swe alone and
// closed, as before the carrier; for omar, whose seat the organisation no
// longer holds, not closed, naming him, so the queue settles it once rather
// than ask it on every turn; for bea, whose row the directory cannot read
// now, a plain error, which leaves the close owed for the next turn; and for
// a stranger, who uses no app, not closed. A requester named by a kind this
// build does not know is decided not at all: the close stays owed, its error
// saying the fleet's builds differ, for a home of the build that knows the
// kind.
//
// Mutations: decide the close for the enqueuer alone — ada's is closed, red;
// drop the entry's carrier where the close is composed from what the home
// handed over — ada's is closed, red; write a close made for nobody whatever
// it was decided for — the token's and cto's are made as swe's own work, red;
// read omar's seat leaving as a read that failed — his close is asked on
// every turn, red; settle the unknown kind not closed — this build decides a
// close it cannot judge, red.
func TestAQueuedCloseIsDecidedWithinTheAppsOfWhomItsEnqueueWasMadeFor(t *testing.T) {
	t.Parallel()
	chart := orgchart.Of(writtenChart(t))
	r := closeRequesters{
		held: map[string]iam.Principal{
			"ada": {Kind: iam.KindPerson, Seat: "ada", Stage: iam.StageActive,
				Grants: []iam.Grant{iam.GrantChatlet, iam.GrantGitlet}},
			"login:token:ci": {Kind: iam.KindMachine, Login: "token:ci", Stage: iam.StageActive,
				Grants: []iam.Grant{iam.GrantWorklet, iam.GrantChatlet}},
		},
		left: []string{"omar"}, unreadable: []string{"bea"},
	}
	seat := func(handle string) *types.WorkingFor {
		return &types.WorkingFor{Kind: types.WorkingForSeat, Seat: handle}
	}
	token := func(apps ...iam.Grant) *types.WorkingFor {
		return &types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci", Apps: apps}
	}
	item := types.WorkItem{Backend: types.WorkWorklet, ID: "task-42", Key: "ENG-42"}
	swe := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "swe"}
	const (
		closed    = "closed"
		notClosed = "not closed"
		owed      = "owed"
	)
	for _, tc := range []struct {
		name    string
		madeFor *types.WorkingFor
		want    string
		// names is what a close not made says, each of it.
		names []string
	}{
		{"for ada, who does not use Worklet", seat("ada"), notClosed, []string{"ada", "Worklet"}},
		{"for the token, whose write carried Worklet", token(iam.GrantWorklet), closed, nil},
		{"for the token, whose write carried Chatlet alone", token(iam.GrantChatlet), notClosed,
			[]string{"token:ci", "Worklet"}},
		{"for cto, a colleague seat", seat("cto"), closed, nil},
		{"for nobody", nil, closed, nil},
		{"for omar, whose seat the organisation no longer holds", seat("omar"), notClosed, []string{"omar"}},
		{"for bea, whose row the directory cannot read now", seat("bea"), owed, nil},
		{"for a stranger", &types.WorkingFor{Kind: types.WorkingForStranger}, notClosed, nil},
	} {
		task := gitlethome.TaskClose{As: swe, WorkItem: item, WorkingFor: tc.madeFor}
		actor, err := decideClose(t.Context(), r, chart, task, time.Now())
		var refused *gitlethome.NotClosed
		switch {
		case tc.want == closed && err != nil:
			t.Errorf("the close %s answered %v, want it made", tc.name, err)
		case tc.want == closed && (actor.Handle != "swe" || actor.Kind != worklet.AuthorAgent ||
			!reflect.DeepEqual(actor.WrittenFor, types.ForTurn(tc.madeFor))):
			t.Errorf("the close %s is made as %+v, want it made as swe, written for whom its enqueue was made "+
				"for (%+v)", tc.name, actor, tc.madeFor)
		case tc.want == notClosed && !errors.As(err, &refused):
			t.Errorf("the close %s answered %v, want it settled not closed", tc.name, err)
		case tc.want == owed && (err == nil || errors.As(err, &refused)):
			t.Errorf("the close %s answered %v, want a plain error that leaves it owed", tc.name, err)
		case tc.want == notClosed:
			for _, name := range tc.names {
				if !strings.Contains(refused.Why, name) {
					t.Errorf("the close %s says %q, which does not name %s", tc.name, refused.Why, name)
				}
			}
		}
	}

	// A KIND THIS BUILD DOES NOT KNOW, decided before anything is read: the
	// nil engine is never reached.
	err := queueCloser{}.CloseWorkItem(t.Context(), gitlethome.TaskClose{As: swe, WorkItem: item,
		WorkingFor: &types.WorkingFor{Kind: "unit", Seat: "platform"}})
	var refused *gitlethome.NotClosed
	if err == nil || errors.As(err, &refused) || !strings.Contains(err.Error(), "builds differ") {
		t.Errorf("the close of an entry enqueued for a kind this build does not know answered %v, want a plain "+
			"error saying the fleet's builds differ, which leaves it owed", err)
	}
}
