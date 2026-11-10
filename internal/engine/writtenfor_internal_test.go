package engine

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// WHAT A SEAT WRITES CARRIES WHOM IT WROTE FOR (ADR-0109), end to end over the
// engine's own surfaces: cto, an agent, writes in a turn somebody woke; the
// record its store keeps carries them; the change feed relays it; the
// notification service wakes swe, another agent, with cto as the writer and
// them beside it; and the turn swe is handed works for them, decided at the
// gate a seat's registry decides every builtin by.

// writtenChart is the company these cases run in: three agents (cto writes,
// swe is woken, qa is woken in turn by swe) and ada, a person.
func writtenChart(t *testing.T) *org.Organization {
	t.Helper()
	o := &org.Organization{Name: "Nimbus", Roles: []*org.Role{
		{Name: "CTO", DeclaredHandle: "cto"},
		{Name: "SWE", DeclaredHandle: "swe"},
		{Name: "QA", DeclaredHandle: "qa"},
		{Name: "Ada Okonkwo", DeclaredHandle: "ada", Kind: org.KindHuman},
	}}
	o.Normalize()
	return o
}

// writeFamily is how one engine surface carries an agent's write to the seat
// it wakes: the family's own write tool, decided at the gate for whoever the
// writing turn works for, whose writer answers whom the write was made for;
// the record its store keeps of that write, relayed as its change feed relays
// it; the app the write uses; and a probe of an app it does not, which the
// woken turn is held to.
type writeFamily struct {
	name string
	app  iam.Grant
	// write is the seat's write in turn naming woken, through the family's
	// own tool, answering whom its writer was handed it was made for.
	write func(t *testing.T, o *org.Organization, people personSeats, turn *turnctx.Turn,
		woken string) *types.WorkingFor
	wake func(t *testing.T, author, woken string, carrier *types.WorkingFor) *events.Event
	// probe is a call the woken turn makes of an app the write does not use,
	// and other is that app.
	probe func(t *testing.T, o *org.Organization, people personSeats, turn *turnctx.Turn) tools.Result
	other iam.Grant
}

// writeFamilies are Worklet's, Doclet's and Chatlet's: an item assigned to the
// woken seat, a page comment naming it, and a message asking it in a room.
func writeFamilies() []writeFamily {
	return []writeFamily{
		{name: "an item assigned", app: iam.GrantWorklet, write: fileWork, wake: assignedWake,
			probe: postInRoom, other: iam.GrantChatlet},
		{name: "a page comment naming the seat", app: iam.GrantDoclet, write: commentOnPage,
			wake: namedOnPageWake, probe: postInRoom, other: iam.GrantChatlet},
		{name: "a message asking the seat", app: iam.GrantChatlet, write: askInRoom, wake: roomMessageWake,
			probe: readPage, other: iam.GrantDoclet},
	}
}

// seatTool is one builtin of a seat's registry over deps, decided at the gate
// a seat's registry decides every builtin by, for whoever the turn works for,
// composed from people.
func seatTool(t *testing.T, o *org.Organization, people personSeats, deps builtin.Deps,
	name string) tools.SeatCallable {

	t.Helper()
	deps.Requesters, deps.Authorize = people, builtin.Decide(orgchart.Of(o))
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, deps); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	entry, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	return entry.Tool.(tools.SeatCallable)
}

// called is one call of tool in turn, failing the case on an error.
func called(t *testing.T, tool tools.SeatCallable, turn *turnctx.Turn, args map[string]any) tools.Result {
	t.Helper()
	got, err := tool.CallForTurn(t.Context(), turn, args)
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return got
}

// filedWork is Worklet's writer as a seat's create hands it a task, recording
// the actor of every write.
type filedWork struct {
	// WorkWriter is the writes no case here makes, which panic if one does.
	builtin.WorkWriter
	actors []builtin.Actor
}

func (w *filedWork) as(a builtin.Actor) builtin.WorkWriter {
	w.actors = append(w.actors, a)
	return w
}

func (w *filedWork) CreateTask(_ context.Context, _ string, task worklet.Task,
	_ *worklet.Notify) (worklet.WriteResult, error) {

	return worklet.WriteResult{Key: "ENG-1", Assignee: task.Assignee, Outcome: statelog.OutcomeApplied,
		Version: 1}, nil
}

// fileWork is create_work_item in turn, assigning woken an item.
func fileWork(t *testing.T, o *org.Organization, people personSeats, turn *turnctx.Turn,
	woken string) *types.WorkingFor {

	t.Helper()
	work := &filedWork{}
	got := called(t, seatTool(t, o, people, builtin.Deps{Work: builtin.WorkDeps{Writer: work.as}},
		builtin.CreateWorkItemTool), turn, map[string]any{
		"title": "Read the runbook", "project": "ENG", "assignee": woken})
	if got.Failed || len(work.actors) != 1 {
		t.Fatalf("create_work_item answered %q, writing %d times", got.Output, len(work.actors))
	}
	return carrierOf(t, work.actors[0].WrittenFor)
}

// runbookPages is the knowledge base holding one page, recording the actor of
// every comment made on it.
type runbookPages struct {
	// PageWriter is the writes no case here makes, which panic if one does.
	builtin.PageWriter
	actors []doclet.Actor
}

func (p *runbookPages) List(context.Context, doclet.Filter, statelog.Freshness) (doclet.Listing, error) {
	return doclet.Listing{}, nil
}

func (p *runbookPages) Get(context.Context, string, statelog.Freshness) (doclet.Detail, error) {
	return doclet.Detail{Page: doclet.Page{ID: "p-1", Container: "ENG", Title: "Runbook"}}, nil
}

func (p *runbookPages) Comment(_ context.Context, actor doclet.Actor, _ string,
	in doclet.NewComment) (doclet.Comment, doclet.Written, error) {

	p.actors = append(p.actors, actor)
	return doclet.Comment{ID: "c-1", Mentions: in.Mentions}, doclet.Written{}, nil
}

// commentOnPage is comment_on_page in turn, naming woken.
func commentOnPage(t *testing.T, o *org.Organization, people personSeats, turn *turnctx.Turn,
	woken string) *types.WorkingFor {

	t.Helper()
	pages := &runbookPages{}
	got := called(t, seatTool(t, o, people, builtin.Deps{Pages: builtin.PageDeps{Reader: pages, Writer: pages,
		Mentions: seatMentions{org: o}}}, builtin.CommentOnPageTool), turn, map[string]any{
		"page": "p-1", "asks": true, "body": "@" + woken + " is this current?"})
	if got.Failed || len(pages.actors) != 1 {
		t.Fatalf("comment_on_page answered %q, writing %d times", got.Output, len(pages.actors))
	}
	return carrierOf(t, pages.actors[0].WrittenFor)
}

// readPage is get_page in turn.
func readPage(t *testing.T, o *org.Organization, people personSeats, turn *turnctx.Turn) tools.Result {
	t.Helper()
	pages := &runbookPages{}
	return called(t, seatTool(t, o, people, builtin.Deps{Pages: builtin.PageDeps{Reader: pages, Writer: pages}},
		builtin.GetPageTool), turn, map[string]any{"page": "p-1"})
}

// launchRoom is the room these cases' messages are said in.
const launchRoom = "6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f"

// roomSaid is chat's writer as a seat's post hands it a message, recording the
// actor of every message.
type roomSaid struct {
	// ChatWriter is the gestures no case here makes, which panic if one does.
	builtin.ChatWriter
	actors []chatlet.Actor
}

func (r *roomSaid) Post(_ context.Context, actor chatlet.Actor, channelID string,
	_ chatlet.NewMessage) (chatlet.Written, error) {

	r.actors = append(r.actors, actor)
	return chatlet.Written{Channel: chatlet.Channel{ID: channelID, Name: "launch"},
		Message: chatlet.Message{ID: "m-1", ChannelID: channelID},
		Outcome: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

// askInRoom is post_message in turn, asking woken.
func askInRoom(t *testing.T, o *org.Organization, people personSeats, turn *turnctx.Turn,
	woken string) *types.WorkingFor {

	t.Helper()
	room := &roomSaid{}
	got := called(t, seatTool(t, o, people, builtin.Deps{Chat: builtin.ChatDeps{Writer: room}},
		builtin.PostMessageTool), turn, map[string]any{
		"channel": launchRoom, "asks": true, "body": "@" + woken + " can you read the runbook?"})
	if got.Failed || len(room.actors) != 1 {
		t.Fatalf("post_message answered %q, writing %d times", got.Output, len(room.actors))
	}
	return carrierOf(t, room.actors[0].WrittenFor)
}

// postInRoom is post_message in turn, saying something to the room.
func postInRoom(t *testing.T, o *org.Organization, people personSeats, turn *turnctx.Turn) tools.Result {
	t.Helper()
	return called(t, seatTool(t, o, people, builtin.Deps{Chat: builtin.ChatDeps{Writer: &roomSaid{}}},
		builtin.PostMessageTool), turn, map[string]any{"channel": launchRoom, "asks": false, "body": "on it"})
}

// carrierOf is whom an agent's write was made for, as the store reads it off
// the actor ([types.WrittenFor.For]): a write whose actor states nobody fails
// the case, as the store refuses it.
func carrierOf(t *testing.T, w types.WrittenFor) *types.WorkingFor {
	t.Helper()
	carrier, err := w.For(iam.ActorAgent)
	if err != nil {
		t.Fatalf("the write's actor: %v", err)
	}
	return carrier
}

// assignedWake is author's assignment of a task to woken, written for carrier.
func assignedWake(t *testing.T, author, woken string, carrier *types.WorkingFor) *events.Event {
	t.Helper()
	before := worklet.Task{ID: "t-" + woken, Key: "ENG-1", Project: "ENG", Title: "Read the runbook"}
	after := before
	after.Assignee = woken
	n := worklet.Wake{Kind: worklet.ChangeAssignee, Before: before, After: after}.Notify(nil)
	if n == nil {
		t.Fatal("the fixture's assignment announces nothing")
	}
	record := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			OpID: "op-assign-" + woken, Subject: worklet.TaskSubject(before.ID), Op: worklet.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: "node-a",
			Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Kind: worklet.ChangeAssignee, Mutation: json.RawMessage(`{}`), Actor: author,
		ActorKind: worklet.AuthorAgent, WorkingFor: carrier, Notify: n,
	}
	payload, err := record.Encode()
	if err != nil {
		t.Fatalf("encode the assignment: %v", err)
	}
	return inboundDelivery(t, worklet.Source, payload)
}

// namedOnPageWake is author's comment on a page naming woken and asking it,
// written for carrier: the record the store makes of [commentOnPage]'s call,
// whose agent author states that it asks.
func namedOnPageWake(t *testing.T, author, woken string, carrier *types.WorkingFor) *events.Event {
	t.Helper()
	asks := true
	record := doclet.MutationRecord{
		RecordEnvelope: doclet.RecordEnvelope{
			OpID: "op-comment-" + woken, Subject: doclet.PageSubject("p-1"), Op: doclet.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: "node-a",
		},
		Actor: author, ActorKind: doclet.AuthorAgent, WorkingFor: carrier,
		Notify: &doclet.Notify{
			Kind: doclet.ChangeComment, PageID: "p-1", Version: 1,
			Mentions: []string{woken}, Asks: &asks, Excerpt: "@" + woken + " is this current?",
			Container: "ENG", Title: "Runbook",
		},
	}
	payload, err := doclet.Encode(record)
	if err != nil {
		t.Fatalf("encode the comment: %v", err)
	}
	delivery, wake, err := doclet.NewTranslator(func() string { return "" }).Translate(
		t.Context(), changefeed.Record{Payload: payload, Key: record.Subject.Wire()})
	if err != nil || !wake {
		t.Fatalf("the comment relayed nothing: %v", err)
	}
	return rawDelivery(t, doclet.Source, delivery.Body)
}

// roomMessageWake is author's message in a public room asking woken, written
// for carrier.
func roomMessageWake(t *testing.T, author, woken string, carrier *types.WorkingFor) *events.Event {
	t.Helper()
	record := chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			V: chatlet.RecordVersion, OpID: "op-message-" + woken,
			Subject: chatlet.MessageSubject("room-1"), Op: chatlet.OpPost, Writer: "node-a",
		},
		Actor: author, ActorKind: chatlet.AuthorAgent, WorkingFor: carrier,
		Notify: &chatlet.Notify{
			MessageID: "msg-" + woken, ChannelID: "room-1", ChannelName: "launch",
			ChannelKind: chatlet.KindPublic, Author: author, AuthorKind: chatlet.AuthorAgent,
			Excerpt:    "@" + woken + " can you read the runbook?",
			Recipients: []chatlet.Recipient{{Handle: woken, Reason: chatlet.ReasonMention}},
		},
	}
	payload, err := chatlet.Encode(record)
	if err != nil {
		t.Fatalf("encode the message: %v", err)
	}
	delivery, wake, err := chatlet.NewTranslator(nil).Translate(t.Context(),
		changefeed.Record{Payload: payload, Key: record.Subject.Wire()})
	if err != nil || !wake {
		t.Fatalf("the message relayed nothing: %v", err)
	}
	return rawDelivery(t, chatlet.Source, delivery.Body)
}

// wokenTurn is the turn handle is handed for the one wake delivery puts on its
// inbox: the delivery through the notification service with every first-party
// parser, and the turn's identity as the runner is handed it.
func wokenTurn(t *testing.T, o *org.Organization, reg *notify.Registry, handle string,
	delivery *events.Event) *turnctx.Turn {

	t.Helper()
	e := &Engine{}
	e.notify.registry = reg
	company := &Company{Org: o}
	req := Request{Handle: handle, Events: []*events.Event{deliveredTo(t, o, reg, handle, delivery)},
		RunID: statelog.NewOpID(time.Now(), "")}
	tel, err := e.describeTurn(t.Context(), company, req)
	if err != nil {
		t.Fatalf("describe %s's turn: %v", handle, err)
	}
	return tel.runnerTurn(company, 0, nil, "task", ReplyFor(req.Events), nil).Context
}

// inAppsOrder is grants in iam.Apps order.
func inAppsOrder(grants ...iam.Grant) []iam.Grant {
	var out []iam.Grant
	for _, app := range iam.Apps {
		if slices.Contains(grants, app) {
			out = append(out, app)
		}
	}
	return out
}

// heldToTheCeiling reports a call the gate refused because whoever the turn
// works for does not use the row's app.
func heldToTheCeiling(r tools.Result) bool {
	var refused *builtin.DecisionRefused
	return r.Failed && errors.As(r.Cause, &refused) && refused.Decision.Reason == authz.ReasonCeiling
}

// THE TURN A WRITE WAKES IS HELD TO WHOEVER THE WRITE WAS MADE FOR (ADR-0109),
// on every family's write alike, through the family's own write tool. Ada uses
// the family's app alone; she wakes cto, and cto assigns swe an item, names swe
// on a page, or asks swe in a room, as she may; swe's turn, worked for ada, is
// refused the app she does not use, as she would be. A credential's carried
// apps hold swe as tightly: a write that carried the family's app alone leaves
// swe refused the other, and one that carried both admits it. A write a turn
// nobody woke made carries nobody, and the turn it wakes works for cto, which
// uses every app.
//
// Mutation: drop the carrier from one family's actor (that family's tool hands
// its writer an actor written for nobody), and that family's turn is admitted
// for ada.
func TestATurnAWriteWakesIsHeldToWhoeverTheWriteWasMadeFor(t *testing.T) {
	t.Parallel()
	o := writtenChart(t)
	reg := partyRegistry(o, nil)
	for _, family := range writeFamilies() {
		people := personSeats{
			"ada": {Kind: iam.KindPerson, Seat: "ada", Stage: iam.StageActive,
				Grants: []iam.Grant{family.app}},
			"login:token:ci": {Kind: iam.KindMachine, Login: "token:ci", Stage: iam.StageActive,
				Grants: []iam.Grant{family.app, family.other}},
		}
		ctoFor := func(requester string, login string, apps ...iam.Grant) *turnctx.Turn {
			return &turnctx.Turn{Org: o, Seat: o.AgentSeatByHandle("cto"),
				RunID: statelog.NewOpID(time.Now(), ""), Requester: requester,
				RequesterLogin: login, RequesterApps: apps}
		}
		for name, c := range map[string]struct {
			cto     *turnctx.Turn
			want    *types.WorkingFor
			refused bool
		}{
			"for ada, who uses the family's app alone": {ctoFor("ada", ""),
				&types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}, true},
			"for a token whose write carried the family's app alone": {ctoFor("", "token:ci", family.app),
				&types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
					Apps: iam.AppsOf([]iam.Grant{family.app})}, true},
			// IN iam.Apps ORDER, the order a wake's writer carries a
			// credential's apps in, whatever order the write named them.
			"for a token whose write carried both apps": {ctoFor("", "token:ci", inAppsOrder(family.app, family.other)...),
				&types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
					Apps: inAppsOrder(family.app, family.other)}, false},
			"of a turn nobody woke": {ctoFor("", ""), nil, false},
		} {
			carried := family.write(t, o, people, c.cto, "swe")
			if !reflect.DeepEqual(carried, c.want) {
				t.Errorf("%s %s: cto's write is made for %+v, want %+v", family.name, name, carried, c.want)
				continue
			}
			woken := wokenTurn(t, o, reg, "swe", family.wake(t, "cto", "swe", carried))
			if !reflect.DeepEqual(woken.WorkingFor, c.want) || woken.Requester != "cto" {
				t.Errorf("%s %s: swe's turn works for %+v with requester %q, want %+v and cto",
					family.name, name, woken.WorkingFor, woken.Requester, c.want)
				continue
			}
			got := family.probe(t, o, people, woken)
			if heldToTheCeiling(got) != c.refused {
				t.Errorf("%s %s: swe's use of %s answered %q, refused at the ceiling: %v, want %v",
					family.name, name, family.other, got.Output, heldToTheCeiling(got), c.refused)
			}
		}
	}
}

// A REQUESTER RIDES EVERY HOP OF WHAT SEATS WRITE (ADR-0109): ada wakes cto,
// cto writes for her and wakes swe, swe writes in the turn that wakes and wakes
// qa, each write through its family's own tool, so qa's turn works for ada. A
// chain of writes never widens: ada uses every app the writes need, and qa is
// still held to her.
//
// Mutation: compose a write as a turn nobody woke, and qa's turn works for swe,
// every app.
func TestARequesterRidesEveryHopOfWhatSeatsWrite(t *testing.T) {
	t.Parallel()
	o := writtenChart(t)
	reg := partyRegistry(o, nil)
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	people := personSeats{"ada": {Kind: iam.KindPerson, Seat: "ada", Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantWorklet, iam.GrantDoclet, iam.GrantChatlet}}}
	for _, family := range writeFamilies() {
		cto := &turnctx.Turn{Org: o, Seat: o.AgentSeatByHandle("cto"), RunID: statelog.NewOpID(time.Now(), ""),
			Requester: "ada"}
		toSWE := family.write(t, o, people, cto, "swe")
		swe := wokenTurn(t, o, reg, "swe", family.wake(t, "cto", "swe", toSWE))
		toQA := family.write(t, o, people, swe, "qa")
		if !reflect.DeepEqual(toQA, ada) {
			t.Errorf("%s: swe's write, a write from ada, is made for %+v, want ada", family.name, toQA)
		}
		qa := wokenTurn(t, o, reg, "qa", family.wake(t, "swe", "qa", toQA))
		if !reflect.DeepEqual(qa.WorkingFor, ada) {
			t.Errorf("%s: qa's turn, two writes from ada, works for %+v, want ada", family.name, qa.WorkingFor)
		}
	}
}

// ONE SEAT'S WRITES FOR TWO REQUESTERS WAKE ITS COLLEAGUE TWICE (ADR-0109): cto
// writes for ada and for omar, and swe's inbox, partitioned as the broker
// partitions it (notify.KeyOf), is two turns, each worked for its own; cto's
// two writes for ada are one. Through the real parser and notification
// service, so the carrier is on the event the broker keys, not only on the
// record.
//
// Mutation: leave the carrier out of the partition, and ada's and omar's
// writes are one turn, worked for ada.
func TestOneSeatsWritesForTwoRequestersWakeItsColleagueTwice(t *testing.T) {
	t.Parallel()
	o := writtenChart(t)
	reg := partyRegistry(o, nil)
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	omar := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "omar"}
	for _, family := range writeFamilies() {
		inbox := []*events.Event{
			deliveredTo(t, o, reg, "swe", family.wake(t, "cto", "swe", ada)),
			deliveredTo(t, o, reg, "swe", family.wake(t, "cto", "swe", omar)),
			deliveredTo(t, o, reg, "swe", family.wake(t, "cto", "swe", ada)),
		}
		parts := queue.PartitionByKey(inbox, notify.KeyOf, func(e *events.Event) *events.Event { return e })
		if len(parts) != 2 || len(parts[0].Items) != 2 || len(parts[1].Items) != 1 {
			t.Errorf("%s: cto's writes for ada, omar and ada again made %d turns, want ada's two as one "+
				"and omar's as another", family.name, len(parts))
			continue
		}
		for _, part := range parts {
			want := ada
			if part.Items[0] == inbox[1] {
				want = omar
			}
			if got := workingForIn(part.Items); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: a turn of cto's writes works for %+v, want %+v", family.name, got, want)
			}
		}
	}
}

// A WRITE WAKES A TURN WORKING FOR WHOEVER IT WAS WRITTEN FOR (ADR-0109): the
// woken turn reads an agent's carrier off the notification's first
// constituent, which the partition makes every constituent's, and nothing off
// a writer that is no agent's, nor off a vendor's notification, which names no
// writer at all.
//
// Mutation: drop the ExternalNotification arm from workingForIn, and the turn
// cto's write for ada wakes works for cto.
func TestAWriteWakesATurnWorkingForWhoeverItWasWrittenFor(t *testing.T) {
	t.Parallel()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	omar := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "omar"}
	by := func(kind iam.ActorKind, w *types.WorkingFor) *types.NotificationWriter {
		return &types.NotificationWriter{Kind: kind, Seat: "cto", WorkingFor: w}
	}
	wake := func(n types.ExternalNotification) []*events.Event {
		return []*events.Event{events.New(n, events.TraceContext{})}
	}
	for name, c := range map[string]struct {
		evs  []*events.Event
		want *types.WorkingFor
	}{
		"an agent's write for ada": {wake(types.ExternalNotification{NotificationSource: worklet.Source,
			Writer: by(iam.ActorAgent, ada)}), ada},
		"a burst, read off its first constituent": {wake(types.ExternalNotification{
			NotificationSource: chatlet.Source, Writer: by(iam.ActorAgent, omar),
			Messages: []types.CoalescedMessage{{Writer: by(iam.ActorAgent, ada)}, {Writer: by(iam.ActorAgent, ada)}},
		}), ada},
		"an agent's write for nobody else": {wake(types.ExternalNotification{NotificationSource: doclet.Source,
			Writer: by(iam.ActorAgent, nil)}), nil},
		"a person's writer handed a carrier": {wake(types.ExternalNotification{
			NotificationSource: worklet.Source, Writer: by(iam.ActorHuman, ada)}), nil},
		"a vendor's notification": {wake(types.ExternalNotification{NotificationSource: "slack",
			Sender: "U0ADA"}), nil},
	} {
		if got := workingForIn(c.evs); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: the turn works for %+v, want %+v", name, got, c.want)
		}
	}
}

// A TURN A WRITE WOKE IS STILL ITS REQUESTER'S WHEN IT RESUMES (ADR-0109), on
// every family's write: whom cto's write was made for reaches swe's dispatched
// turn, the launch that records the run's row, and the turn a resumed segment
// rebuilds from that row, so every call swe's turn makes after its executor
// parks is held to ada, never to cto.
//
// Mutation: leave WorkingFor off the run's row (sandboxTurnRef), and the
// resumed turn works for cto.
func TestATurnAWriteWokeIsStillItsRequestersWhenItResumes(t *testing.T) {
	t.Parallel()
	e, _ := starting(t, refusingModels(t))
	company := e.Company()
	o := writtenChart(t)
	reg := partyRegistry(o, nil)
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	for _, family := range writeFamilies() {
		wake := deliveredTo(t, o, reg, "swe", family.wake(t, "cto", "swe", ada))
		tel, err := e.describeTurn(t.Context(), company, Request{
			RunID: "run-1", Handle: "swe", Events: []*events.Event{wake},
		})
		if err != nil {
			t.Fatalf("%s: describeTurn: %v", family.name, err)
		}
		dispatched := tel.runnerTurn(company, 0, nil, "task", turn.Reply{}, nil).Context
		if !reflect.DeepEqual(dispatched.WorkingFor, ada) {
			t.Errorf("%s: the dispatched turn works for %+v, want ada", family.name, dispatched.WorkingFor)
			continue
		}
		ref := sandboxTurnRef(t.Context(), dispatched, "SWE", nil)
		if !reflect.DeepEqual(ref.WorkingFor, ada) {
			t.Errorf("%s: the launch records the run working for %+v, want ada", family.name, ref.WorkingFor)
		}
		run := sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe", Requester: ref.Requester,
			WorkingFor: ref.WorkingFor}
		resumed, err := e.describeResume(t.Context(), company, resumeInput{
			Run: run,
			Turn: resumedTurn(run, company.Org.AgentSeatByHandle("swe"), company.Org,
				turnctx.NewHolds(run.CodeHolds)),
		})
		if err != nil {
			t.Fatalf("%s: describeResume: %v", family.name, err)
		}
		if again := resumed.runnerTurn(company, 0, nil, "task", turn.Reply{}, nil).Context; !reflect.DeepEqual(
			again.WorkingFor, ada) {
			t.Errorf("%s: the resumed turn works for %+v, want the requester its row recorded", family.name,
				again.WorkingFor)
		}
	}
}
