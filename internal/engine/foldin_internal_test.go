package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// requestRun is one triage run of a scripted model deciding request.
func requestRun() llmtest.Run { return decisionRun("request", "new work of its own") }

// foldInOf is the fold-in run-1's current job holds for ev, failing where none.
func foldInOf(t *testing.T, r *routerRig, ev *events.Event) sandbox.FoldIn {
	t.Helper()
	f, ok := mustRow(t, r.store, "run-1").FoldInFor(ev.ID.String())
	if !ok {
		t.Fatalf("no fold-in was asked for wake %s", ev.ID)
	}
	return f
}

// answerFoldIn records approver's answer to f on run-1's row, as answer_executor does.
func answerFoldIn(t *testing.T, r *routerRig, f sandbox.FoldIn, answer sandbox.FoldInAnswer, approver string) {
	t.Helper()
	if _, err := r.store.AnswerFoldIn(t.Context(), "run-1", f.ID, sandbox.FoldInAnswered{Answer: answer,
		By: approver, ByKind: iam.ActorHuman}); err != nil {
		t.Fatalf("AnswerFoldIn by %s: %v", approver, err)
	}
}

// ANOTHER WRITER'S REQUEST ASKS ONE FOLD-IN OF THE REQUESTER AND THE LEADS.
// Triaged request from ceo, who neither asked for the run nor leads the seat,
// it is put once to lead (the run's requester) and boss (who manages the seat),
// never to its asker, and waits undecided: read again by the same router or a
// fresh one, it is neither triaged nor asked again, and joins nothing.
//
// Mutation: ask per read, triage again after a restart, or put it to the asker,
// and this goes red.
func TestAnotherWritersRequestAsksOneFoldInOfTheRequesterAndTheLeads(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newTriageRig(t, requestRun())
	ask := r.post(t, "ceo", "chat:C1", "can you also redo the logo?", 7)
	memo := newRouteMemo()
	r.e.routeSeat(ctx, "swe", r.subject, r.group, memo)
	f := foldInOf(t, r, ask)
	if !slices.Equal(f.PutTo, []string{"lead", "boss"}) || f.Asker != "ceo" || !f.Open() ||
		!strings.Contains(f.Words, "redo the logo") {
		t.Fatalf("the fold-in is %+v, want ceo's open request put to lead then boss", f)
	}
	if memo.done(r.job, ask.ID.String()) {
		t.Error("the request was decided while its fold-in waits")
	}
	// THE SCRIPT HOLDS ONE TRIAGE: a second would decide nothing and fail here.
	r.e.routeSeat(ctx, "swe", r.subject, r.group, memo)
	r.e.routeSeat(ctx, "swe", r.subject, r.group, newRouteMemo())
	if row := mustRow(t, r.store, "run-1"); len(row.FoldIns) != 1 || len(personNotes(t, r)) != 0 ||
		len(r.filed(t)) != 0 {
		t.Errorf("after reads again the row holds fold-ins %+v and notes %d, want the one fold-in and nothing "+
			"joined", row.FoldIns, len(personNotes(t, r)))
	}
}

// A REQUEST FROM THE REQUESTER, OR FROM A CREDENTIAL, IS NEVER A FOLD-IN, and
// one nobody may approve stays with no question asked. The requester's own
// wakes join by the fixed rule; nobody is asked anything for a credential
// (ADR-0109); and where neither the requester nor a lead of the seat is a
// person, the request waits for a turn of its own.
//
// Mutation: drop the requester or credential check, or ask with nobody to put
// it to, and this goes red.
func TestOnlyAnotherPersonsOrSeatsRequestIsAFoldInAndOnlyWithAnApprover(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		o     wakeOrigin
		facts wakeFacts
		want  bool
	}{
		{"the run's requester", wakeOrigin{requester: "lead"}, wakeFacts{requester: true, person: true}, false},
		{"a credential acting as no seat", wakeOrigin{login: "token:ci"}, wakeFacts{}, false},
		{"another person", wakeOrigin{requester: "ceo"}, wakeFacts{person: true}, true},
		{"an agent seat", wakeOrigin{requester: "ops"}, wakeFacts{}, true},
	} {
		if got := foldable(tc.o, tc.facts); got != tc.want {
			t.Errorf("%s: foldable = %v, want %v", tc.name, got, tc.want)
		}
	}

	ctx := t.Context()
	r := newTriageRig(t, requestRun())
	// NOBODY TO PUT IT TO: the run's requester an agent, and no lead.
	chart := &org.Organization{Name: "Acme", Roles: []*org.Role{{Name: "SWE"}, {Name: "Lead"},
		{Name: "CEO", Kind: org.KindHuman}}}
	company := *r.e.Company()
	company.Org = chart
	r.e.epoch.current.Store(&company)
	r.e.notify.registry = notify.NewRegistry(chart, nil)
	ask := r.post(t, "ceo", "chat:C1", "can you also redo the logo?", 7)
	memo := newRouteMemo()
	r.e.routeSeat(ctx, "swe", r.subject, r.group, memo)
	if row := mustRow(t, r.store, "run-1"); len(row.FoldIns) != 0 || !memo.done(r.job, ask.ID.String()) {
		t.Errorf("with nobody to approve, the row holds fold-ins %+v (decided %v), want none and the wake "+
			"left for a turn of its own", row.FoldIns, memo.done(r.job, ask.ID.String()))
	}
}

// AN APPROVED FOLD-IN JOINS AS A STEER NOTE NAMING WHO ASKED AND WHO APPROVED,
// CLAIMED AND BINDING THE TURN, whether the requester or a lead approved it.
// The note is under the approver's name, says the asker asked and the approver
// approved, and carries the asker's words in their fence; the delivery is
// skipped joined naming the turn and the approval; the answer it was owed binds
// the turn.
//
// Mutation: file it under the asker's name, leave the asker's words unfenced,
// skip the claim, or drop its owed answer, and this goes red.
func TestAnApprovedFoldInJoinsAsASteerNamingBothClaimedAndBindingTheTurn(t *testing.T) {
	t.Parallel()
	for _, approver := range []string{"lead", "boss"} {
		t.Run(approver, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			r := newTriageRig(t, requestRun())
			ask := r.post(t, "ceo", "chat:C1", "can you also redo the logo?", 7)
			memo := newRouteMemo()
			r.e.routeSeat(ctx, "swe", r.subject, r.group, memo)
			answerFoldIn(t, r, foldInOf(t, r, ask), sandbox.FoldInApprove, approver)
			r.e.routeSeat(ctx, "swe", r.subject, r.group, memo)

			notes := personNotes(t, r)
			if len(notes) != 1 {
				t.Fatalf("the record holds %d notes, want the approved request as one", len(notes))
			}
			note := notes[0]
			if note.By != approver || !strings.Contains(note.Text, "ceo asked for this") ||
				!strings.Contains(note.Text, approver+" approved") || !strings.Contains(note.Text, "[untrusted:") ||
				!strings.Contains(note.Text, "redo the logo") || steer.Validate(note.Text) != nil {
				t.Fatalf("the note is %+v, want %s's naming ceo's request and its words fenced", note, approver)
			}
			if fence := strings.Index(note.Text, "[untrusted:"); strings.Index(note.Text, "redo the logo") < fence {
				t.Error("the asker's words stand outside their fence")
			}
			if got, want := parkedReply(t, r.runs, mustRow(t, r.store, "run-1")),
				ReplyFor([]*events.Event{ask}); got != want || !got.Awaited() {
				t.Errorf("the turn holding the approved request owes %v, want the answer it asked for, %v", got, want)
			}
			if !memo.done(r.job, ask.ID.String()) {
				t.Error("the approved request is undecided, so a later read files it again")
			}
			var observed []*events.Event
			d := &Dispatcher{Completions: r.ledger, Ledgered: inbox.Ledgered,
				Observe: func(_ context.Context, ev *events.Event) { observed = append(observed, ev) }}
			surviving, skipped := d.dropWorked(ctx, "swe", []*events.Event{ask})
			if len(surviving) != 0 || skipped[ask.ID] != skipFolded {
				t.Fatalf("the approved request's delivery still runs (%d left, cause %v)", len(surviving),
					skipped[ask.ID])
			}
			d.noteSkipped(ctx, "swe", []*events.Event{ask}, surviving, skipped)
			skip, _ := events.DataAs[*types.TurnTriggerSkipped](observed[0])
			if skip == nil || skip.Disposition != types.DispositionJoined || !strings.Contains(skip.Reason, "run-1") ||
				!strings.Contains(skip.Reason, "approved") {
				t.Errorf("the approved request's skip is %+v, want joined naming run-1 and the approval", skip)
			}
		})
	}
}

// A REFUSED FOLD-IN, AND ONE ITS JOB ENDED BEFORE ANYBODY ANSWERED, LEAVE THE
// WAKE TO RUN AS A TURN OF ITS OWN, and a late approval folds nothing. Refused,
// the request is decided and joins nothing; its delivery still runs. Its job
// replaced before an answer, the question closes with the job: a late
// approval is refused, the router forgets it, and the next job asks afresh.
//
// Mutation: fold a refused request, keep a fold-in across jobs, or let the
// late approval land, and this goes red.
func TestARefusedOrExpiredFoldInLeavesTheWakeForATurnOfItsOwn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newTriageRig(t, requestRun())
	ask := r.post(t, "ceo", "chat:C1", "can you also redo the logo?", 7)
	memo := newRouteMemo()
	r.e.routeSeat(ctx, "swe", r.subject, r.group, memo)
	answerFoldIn(t, r, foldInOf(t, r, ask), sandbox.FoldInRefuse, "lead")
	r.e.routeSeat(ctx, "swe", r.subject, r.group, memo)
	if len(personNotes(t, r)) != 0 || len(r.filed(t)) != 0 || !memo.done(r.job, ask.ID.String()) {
		t.Fatal("a refused request joined the running work, or stays undecided")
	}
	d := &Dispatcher{Completions: r.ledger, Ledgered: inbox.Ledgered}
	if surviving, _ := d.dropWorked(ctx, "swe", []*events.Event{ask}); len(surviving) != 1 {
		t.Error("a refused request does not run as a turn of its own")
	}

	x := newTriageRig(t, requestRun(), requestRun())
	late := x.post(t, "ceo", "chat:C1", "can you also redo the logo?", 7)
	xmemo := newRouteMemo()
	x.e.routeSeat(ctx, "swe", x.subject, x.group, xmemo)
	old := foldInOf(t, x, late)
	opened, err := x.store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: "run-1", AgentID: sweAgent,
		AgentHandle: "swe", Role: "SWE", Harness: "claude-code", TraceID: "tr-1", Requester: "lead",
		ConversationKey: "chat:C1", Continues: true}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if ok, err := x.store.MarkSuspended(ctx, "run-1", sandbox.Suspension{State: []byte(`{"version":4}`),
		Iteration: 1}); err != nil || !ok {
		t.Fatalf("MarkSuspended = %v, %v", ok, err)
	}
	if _, err := x.store.AnswerFoldIn(ctx, "run-1", old.ID, sandbox.FoldInAnswered{Answer: sandbox.FoldInApprove,
		By: "lead", ByKind: iam.ActorHuman}); !errors.Is(err, sandbox.ErrFoldInClosed) {
		t.Errorf("a late approval of the ended job's fold-in answered %v, want ErrFoldInClosed", err)
	}
	x.e.routeSeat(ctx, "swe", x.subject, x.group, xmemo)
	if _, kept := xmemo.foldIns[x.job]; kept {
		t.Error("the router still follows the ended job's fold-in")
	}
	if len(personNotes(t, x)) != 0 {
		t.Error("a fold-in whose job ended joined the next one")
	}
	if f, ok := mustRow(t, x.store, "run-1").FoldInFor(late.ID.String()); !ok || f.ID == old.ID ||
		f.Launch != opened.LaunchID || !f.Open() {
		t.Errorf("the next job holds fold-in %+v (found %v), want a fresh question of its own", f, ok)
	}
}

// THE OPEN FOLD-IN BOUND HOLDS: a job asks at most [sandbox.MaxJobFoldIns], and
// the request past them waits for a turn of its own with no question asked.
//
// Mutation: lift the bound, or leave the request past it undecided, and this
// goes red.
func TestAJobsFoldInsStopAtTheirBound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	runs := make([]llmtest.Run, sandbox.MaxJobFoldIns+1)
	for i := range runs {
		runs[i] = requestRun()
	}
	r := newTriageRig(t, runs...)
	var asks []*events.Event
	for i := range sandbox.MaxJobFoldIns + 1 {
		asks = append(asks, r.post(t, "ceo", "chat:C1", "and another thing", uint64(7+i)))
	}
	memo := newRouteMemo()
	r.e.routeSeat(ctx, "swe", r.subject, r.group, memo)
	r.e.routeSeat(ctx, "swe", r.subject, r.group, memo)
	if n := len(mustRow(t, r.store, "run-1").FoldIns); n != sandbox.MaxJobFoldIns {
		t.Errorf("the job asked %d fold-ins, want %d", n, sandbox.MaxJobFoldIns)
	}
	past := asks[len(asks)-1]
	if _, asked := mustRow(t, r.store, "run-1").FoldInFor(past.ID.String()); asked ||
		!memo.done(r.job, past.ID.String()) {
		t.Errorf("the request past the bound was asked (%v) or left undecided", asked)
	}
}

// A WAITING PERSON IS TOLD WHY ONCE PER WAKE, NEVER AGAIN ACROSS READS, AND
// TOLD AGAIN ONLY WHEN THE REASON CHANGES. A request put to its approvers
// raises the queued line saying whom it waits for; reads again, by the same
// router or a fresh one, tell nothing new; refused, the line says it runs
// after the running work.
//
// Mutation: tell on every read, or forget the reason, and this goes red.
func TestAWaitingPersonIsToldWhyOncePerWake(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newTriageRig(t, requestRun())
	hub := newRecordingHub()
	r.e.SetChatLive(ctx, hub)
	r.e.applyChatStatus(ctx, chatletCompany(""))
	t.Cleanup(func() { r.e.stopNotifications(context.Background()) })
	ask := r.postChatlet(t, "ceo", "chat:C1", "can you also redo the logo?", 7)
	memo := newRouteMemo()
	r.e.routeSeat(ctx, "swe", r.subject, r.group, memo)
	approval := "swe in room-1: " + approvalLine([]string{"lead", "boss"})
	if raised := hub.awaitRaise(t); raised[0] != approval {
		t.Fatalf("the waiting person was shown %q, want %q", raised, approval)
	}
	r.e.routeSeat(ctx, "swe", r.subject, r.group, memo)
	r.e.routeSeat(ctx, "swe", r.subject, r.group, newRouteMemo())
	if r.e.waitsTold.tell("swe", ask.ID.String(), approvalLine([]string{"lead", "boss"})) {
		t.Fatal("the engine forgot it told the waiting person why")
	}
	answerFoldIn(t, r, foldInOf(t, r, ask), sandbox.FoldInRefuse, "lead")
	r.e.routeSeat(ctx, "swe", r.subject, r.group, memo)
	after := "swe in room-1: " + runsAfterLine
	raised := hub.awaitLine(t, after)
	if n := slices.Index(raised, after); slices.Index(raised[:n], approval) != 0 ||
		slices.Contains(raised[1:n], approval) {
		t.Errorf("the waiting person was shown %q, want the approval line once, then %q", raised, after)
	}
}

// awaitLine waits for the hub to have been asked to show line, and answers
// everything it was asked to show.
func (h *recordingHub) awaitLine(t *testing.T, line string) []string {
	t.Helper()
	deadline := time.After(10 * time.Second) //fixedwait:liveness
	for {
		h.mu.Lock()
		raised, posted := slices.Clone(h.raised), h.posted
		h.mu.Unlock()
		if slices.Contains(raised, line) {
			return raised
		}
		select {
		case <-posted:
		case <-deadline:
			t.Fatalf("the hub was never asked to show %q; it showed %q", line, raised)
		}
	}
}

// postChatlet is [routerRig.post] as a Chatlet message in room-1, stamped as
// the chat parser stamps what it routes, so its conversation's queued line can
// be drawn.
func (r *routerRig) postChatlet(t *testing.T, seat, conversation, words string, seq uint64) *events.Event {
	t.Helper()
	meta := map[string]string{notify.TransportField: chatlet.Source, notify.ChannelField: "room-1",
		notify.MessageIDField: "msg-1", notify.ThreadField: "",
		notify.FollowReasonField: string(chatlet.ReasonMention)}
	meta[notify.ThreadAnchorField] = notify.Anchor(meta)
	ev := events.New(types.ExternalNotification{NotificationSource: chatlet.Source, SourceEventType: "post",
		Sender: seat, Body: words, Addressed: true, Metadata: meta,
		Writer: &types.NotificationWriter{Kind: iam.ActorHuman, Seat: seat}}, events.TraceContext{})
	ev.Payload = map[string]any{notify.ConversationField: conversation, notify.PartitionField: conversation,
		notify.TriggerField: statelog.Position{Stream: "chat", Generation: 1, Seq: seq}.String()}
	if err := r.q.Publish(t.Context(), r.subject, ev); err != nil {
		t.Fatal(err)
	}
	return ev
}
