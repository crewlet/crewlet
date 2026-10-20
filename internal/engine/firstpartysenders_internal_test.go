package engine

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// firstPartyChart is one agent seat and two people, each person holding a
// seat of their own — the shape in which a person's word on the engine's own
// surfaces wakes an agent.
func firstPartyChart(t *testing.T) *org.Organization {
	t.Helper()
	o := &org.Organization{Name: "Nimbus", Roles: []*org.Role{
		{Name: "SWE", DeclaredHandle: "swe"},
		{Name: "Ada Okonkwo", DeclaredHandle: "ada", Kind: org.KindHuman},
		{Name: "Bo Lindqvist", DeclaredHandle: "bo", Kind: org.KindHuman},
	}}
	o.Normalize()
	return o
}

// EVERY SOURCE THE ENGINE HOSTS IS A NAMESPACE WHOSE IDS ARE HANDLES, on the
// one registry this node builds.
//
// A native source left off [firstPartySources] fails nothing: its every
// sender reads as an outside party, so the learning profile files a person
// under a platform id, the thread block fences a colleague as a stranger, and
// a turn a person woke acts for nobody. So the list is held to the parsers the
// node registers for a company that runs every half natively, rather than to
// a second list beside it.
func TestEveryNativeSourceNamesItsSendersByHandle(t *testing.T) {
	t.Parallel()
	o := firstPartyChart(t)
	e := &Engine{}
	e.remote.Store(&remoteNative{tracker: true, wiki: true, chat: true})
	parsers, _ := e.nativeParsers(&Company{Org: o})
	if len(parsers) != 3 {
		t.Fatalf("a company running every half natively registered %d parsers, "+
			"want the tracker's, the knowledge base's and chat's", len(parsers))
	}
	reg := partyRegistry(o, nil)
	for _, p := range parsers {
		party, ok := reg.ByExternalID(p.Source(), "ada")
		if !ok || party.Handle != "ada" || !party.Human {
			t.Errorf("the %s source's sender ada resolved to %+v (%v), want her "+
				"seat: a native source the registry does not read by handle "+
				"names every person on it as an outside party", p.Source(), party, ok)
		}
	}
}

// A TURN A PERSON WOKE ON THE ENGINE'S OWN SURFACES ACTS FOR THEM.
//
// The requester ceiling bounds an agent's artifact tools by whoever woke its
// turn, and a turn whose sender nobody could name is a stranger's, refused
// every read. A native sender is a seat handle, which no vendor namespace
// holds — so read through a registry that did not know the engine's own
// sources were handles, a person's comment on a task, remark on a page or
// message in a room woke a turn that could not read for them an artifact
// shared with them. Each case runs the source's own parser and the
// notification service's real delivery, then the turn's identity as the
// runner is handed it, then the tool.
//
// AND THE CEILING STILL HOLDS: the same wake from somebody the artifact is not
// shared with is refused, so what admits ada is her relation to it and not a
// ceiling lifted.
func TestATurnAPersonWokeOnAFirstPartySurfaceActsForThem(t *testing.T) {
	t.Parallel()
	o := firstPartyChart(t)
	reg := partyRegistry(o, nil)
	for name, wake := range map[string]func(t *testing.T, author string) *events.Event{
		"a comment on a native task":    trackerCommentWake,
		"a remark on a native page":     pageCommentWake,
		"a message in a room of chat's": chatMessageWake,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for author, read := range map[string]bool{"ada": true, "bo": false} {
				turn := firstPartyTurn(t, o, reg, deliveredTo(t, o, reg, "swe", wake(t, author)))
				if turn.Requester != author {
					t.Fatalf("%s's wake started a turn acting for %q, want %s",
						author, turn.Requester, author)
				}
				if len(turn.Senders) != 1 || turn.Senders[0].Handle != author {
					t.Errorf("%s's wake names its senders %+v, want her seat — the "+
						"learning profile is filed under it", author, turn.Senders)
				}
				got := readSharedArtifact(t, o, turn, "ada")
				if read && got.Failed {
					t.Errorf("an agent %s woke could not read an artifact shared "+
						"with her: %s", author, got.Output)
				}
				if !read && !got.Failed {
					t.Errorf("an agent %s woke read an artifact shared with ada "+
						"alone", author)
				}
			}
		})
	}
}

// A NATIVE THREAD FENCES ITS COLLEAGUES AS COLLEAGUES.
//
// The thread block names every speaker as an author of the fence it writes,
// and a speaker the party registry does not know is an outside party — which
// a native author, a seat's handle, was on every native thread. Read through
// the registry the engine builds, a person and the seat itself are the
// company's, and the fence says so.
func TestANativeThreadFencesItsColleaguesAsColleagues(t *testing.T) {
	t.Parallel()
	o := firstPartyChart(t)
	reg := partyRegistry(o, nil)
	seat := o.AgentSeatByHandle("swe")
	thread := notify.Thread{Backend: chat.Source, Channel: "room-1", Root: "msg-1"}
	f := prefetch.New(prefetch.Sources{Parties: reg, Threads: oneThread{
		thread: notify.Transcript{Messages: []notify.Message{
			{ID: "msg-1", SenderID: "ada", SenderName: "ada", Text: "what ships friday?"},
			{ID: "msg-2", SenderID: "swe", Text: "the importer", Own: true},
			{ID: "msg-3", SenderID: "ada", SenderName: "ada", Text: "and the export?"},
		}},
	}})
	boundary := provenance.NewBoundary()
	blocks := f.Fetch(t.Context(), prefetch.Request{
		Seat: seat, Org: o, Thread: thread, Message: "msg-3", Boundary: boundary,
	})
	text := blocks.ThreadContext
	want := "[untrusted:" + string(boundary) + " source=chat_thread trust=colleague " +
		"authors=human:ada,agent:swe]"
	if !strings.Contains(text, want) {
		t.Fatalf("the native thread is not fenced as a colleague's — want %q in:\n%s",
			want, text)
	}
	if !strings.Contains(text, "Ada Okonkwo (ada, human colleague)") {
		t.Errorf("the thread does not name ada as the colleague she is:\n%s", text)
	}
}

// oneThread answers every read with one transcript.
type oneThread struct{ thread notify.Transcript }

func (r oneThread) ReadThread(context.Context, string, notify.Thread) (notify.Transcript, bool) {
	return r.thread, true
}

// trackerCommentWake is author's comment on a task swe is assigned, as the
// tracker's change feed hands it to the inbound edge.
func trackerCommentWake(t *testing.T, author string) *events.Event {
	t.Helper()
	comment := tracker.Comment{ID: "c-1", Body: "can you check the numbers?", Author: author}
	n := tracker.Wake{
		Kind: tracker.ChangeComment, Comment: &comment,
		After: tracker.Task{ID: "t-1", Key: "ENG-1", Project: "ENG", Assignee: "swe"},
	}.Notify(nil)
	if n == nil {
		t.Fatal("the fixture's comment announces nothing")
	}
	mutation, err := json.Marshal(map[string]any{"comment": comment})
	if err != nil {
		t.Fatal(err)
	}
	record := tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "op-" + author,
			Subject: tracker.TaskSubject("t-1"), Op: tracker.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: "node-a",
			Scope: tracker.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: mutation, Actor: author, ActorKind: tracker.AuthorHuman, Notify: n,
	}
	payload, err := record.Encode()
	if err != nil {
		t.Fatalf("encode the task record: %v", err)
	}
	return inboundDelivery(t, tracker.Source, payload)
}

// pageCommentWake is author's remark on a page, naming swe.
func pageCommentWake(t *testing.T, author string) *events.Event {
	t.Helper()
	record := pages.MutationRecord{
		RecordEnvelope: pages.RecordEnvelope{
			V: pages.RecordVersion, OpID: "op-" + author,
			Subject: pages.PageSubject("p-1"), Op: pages.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: "node-a",
		},
		Actor: author, ActorKind: pages.AuthorHuman,
		Notify: &pages.Notify{
			Kind: pages.ChangeComment, PageID: "p-1", Version: 1,
			Mentions: []string{"swe"}, Excerpt: "@swe is this current?",
			Container: "ENG", Title: "Runbook",
		},
	}
	payload, err := pages.Encode(record)
	if err != nil {
		t.Fatalf("encode the page record: %v", err)
	}
	delivery, wake, err := pages.NewTranslator(func() string { return "" }).Translate(
		t.Context(), changefeed.Record{Payload: payload, Key: record.Subject.Wire()})
	if err != nil || !wake {
		t.Fatalf("the page record relayed nothing: %v", err)
	}
	return rawDelivery(t, pages.Source, delivery.Body)
}

// chatMessageWake is author's message in a public room, naming swe.
func chatMessageWake(t *testing.T, author string) *events.Event {
	t.Helper()
	record := chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			V: chat.RecordVersion, OpID: "op-" + author,
			Subject: chat.MessageSubject("room-1"), Op: chat.OpPost, Writer: "node-a",
		},
		Actor: author, ActorKind: chat.AuthorHuman,
		Notify: &chat.Notify{
			MessageID: "msg-" + author, ChannelID: "room-1", ChannelName: "launch",
			ChannelKind: chat.KindPublic, Author: author, AuthorKind: chat.AuthorHuman,
			Excerpt:    "@swe where are we?",
			Recipients: []chat.Recipient{{Handle: "swe", Reason: chat.ReasonMention}},
		},
	}
	payload, err := chat.Encode(record)
	if err != nil {
		t.Fatalf("encode the chat record: %v", err)
	}
	delivery, wake, err := chat.NewTranslator().Translate(t.Context(),
		changefeed.Record{Payload: payload, Key: record.Subject.Wire()})
	if err != nil || !wake {
		t.Fatalf("the chat record relayed nothing: %v", err)
	}
	return rawDelivery(t, chat.Source, delivery.Body)
}

func inboundDelivery(t *testing.T, source string, payload []byte) *events.Event {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode the record: %v", err)
	}
	return rawDelivery(t, source, body)
}

func rawDelivery(t *testing.T, source string, body map[string]any) *events.Event {
	t.Helper()
	ev := events.New(types.RawWebhook{Body: body, Headers: map[string]string{}}, events.NewTrace())
	ev.Source = source
	return ev
}

// deliveredTo runs one delivery through the notification service, with every
// first-party parser and prompt registered, and answers the one wake it
// published into handle's inbox.
func deliveredTo(t *testing.T, o *org.Organization, reg *notify.Registry, handle string,
	delivery *events.Event) *events.Event {

	t.Helper()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start the queue: %v", err)
	}
	id, ok := o.AgentIDFor(o.AgentSeatByHandle(handle))
	if !ok {
		t.Fatalf("no agent id for %s", handle)
	}
	var (
		mu    sync.Mutex
		woken []*events.Event
	)
	if err := q.Subscribe(t.Context(), topics.AgentInbox(id), "collect",
		func(_ context.Context, ev *events.Event) queue.Result {
			mu.Lock()
			woken = append(woken, ev)
			mu.Unlock()
			return queue.Ack()
		}); err != nil {
		t.Fatalf("collect the inbox: %v", err)
	}
	svc, err := notify.New(notify.Options{
		Queue:    q,
		Registry: func() *notify.Registry { return reg },
		Prompts:  notify.NewPrompts(tracker.Prompt{}, pages.Prompt{}, chat.NewPrompt()),
		Parsers: []notify.Parser{
			tracker.NewParser(tracker.ParserOptions{}),
			pages.NewParser(pages.ParserOptions{}),
			chat.NewParser(),
		},
	})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	if got := svc.Handle(t.Context(), delivery); got.Outcome != queue.OutcomeAck {
		t.Fatalf("the delivery was not handled: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(woken) != 1 {
		t.Fatalf("%s was woken %d times, want once", handle, len(woken))
	}
	return woken[0]
}

// firstPartyTurn is the turn identity the runner is handed for one wake.
func firstPartyTurn(t *testing.T, o *org.Organization, reg *notify.Registry,
	wake *events.Event) *turnctx.Turn {

	t.Helper()
	e := &Engine{}
	e.notify.registry = reg
	company := &Company{Org: o}
	req := Request{Handle: "swe", Events: []*events.Event{wake},
		RunID: statelog.NewOpID(time.Now(), "")}
	tel, err := e.describeTurn(t.Context(), company, req)
	if err != nil {
		t.Fatalf("describe the turn: %v", err)
	}
	return tel.runnerTurn(company, 0, nil, "task", ReplyFor(req.Events), nil).Context
}

// readSharedArtifact is get_artifact, in turn, on an artifact swe owns and has
// shared with one person alone.
func readSharedArtifact(t *testing.T, o *org.Organization, turn *turnctx.Turn,
	sharedWith string) tools.Result {

	t.Helper()
	id := artifacts.IDFor("review-sheet")
	agent := iam.Actor{Name: "swe", Kind: iam.ActorAgent}
	at := time.Unix(1_700_000_000, 0).UTC()
	reader := oneArtifact{
		head: artifactsdomain.Head{
			ID: id, Label: id.Label(), Kind: artifacts.KindHTML, Title: "Review sheet",
			Owner: "swe", OwnerKind: iam.ActorAgent, HeadNumber: 1,
			Status: artifactsdomain.StatusLive,
			Shares: []artifacts.Share{{
				Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: sharedWith},
				Role:     artifacts.RoleViewer,
			}},
			CreatedBy: agent, UpdatedBy: agent, CreatedAt: at, UpdatedAt: at,
		},
		version: artifactsdomain.Version{VersionSummary: artifactsdomain.VersionSummary{
			Artifact: id, Number: 1, Author: agent, CreatedAt: at,
		}},
	}
	people := map[string]iam.Principal{}
	for _, handle := range []string{"ada", "bo"} {
		people[handle] = iam.Principal{Kind: iam.KindPerson, Seat: handle,
			Stage: iam.StageActive, Grants: iam.Apps}
	}
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Artifacts: builtin.ArtifactDeps{
			Reader: reader, Writer: readOnlyArtifacts{}, Publisher: readOnlyArtifacts{},
			Chart: orgchart.Of(o), Requesters: personSeats(people),
			Limits: func() artifacts.Limits { return artifacts.Limits{} },
			Now:    func() time.Time { return at },
		},
		Authorize: builtin.Decide(orgchart.Of(o)),
	}); err != nil {
		t.Fatalf("register the artifact tools: %v", err)
	}
	entry, ok := reg.Lookup(builtin.GetArtifactTool)
	if !ok {
		t.Fatal("get_artifact is not registered")
	}
	got, err := entry.Tool.(tools.SeatCallable).CallForTurn(t.Context(), turn,
		map[string]any{"artifact": id.String()})
	if err != nil {
		t.Fatalf("get_artifact: %v", err)
	}
	return got
}

// oneArtifact is the artifacts domain holding one artifact at one version.
type oneArtifact struct {
	head    artifactsdomain.Head
	version artifactsdomain.Version
}

func (a oneArtifact) Artifact(context.Context, artifacts.ID, statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {
	return a.head, statelog.Position{}, nil
}

func (a oneArtifact) List(context.Context, artifactsdomain.Viewer,
	artifactsdomain.ListQuery) (artifactsdomain.ListPage, error) {
	return artifactsdomain.ListPage{Complete: true}, nil
}

func (a oneArtifact) Version(context.Context, artifacts.ID, int) (artifactsdomain.Version, error) {
	return a.version, nil
}

func (a oneArtifact) Versions(context.Context, artifacts.ID, string, int) (
	artifactsdomain.VersionPage, error) {
	return artifactsdomain.VersionPage{
		Versions: []artifactsdomain.VersionSummary{a.version.VersionSummary},
	}, nil
}

// readOnlyArtifacts stands in for the write side and the publish pipeline,
// which a read never reaches: a call to either is a nil interface's panic.
type readOnlyArtifacts struct {
	builtin.ArtifactWriter
	builtin.ArtifactPublisher
}

// personSeats composes each person's seat as the requester they are.
type personSeats map[string]iam.Principal

func (p personSeats) Requester(_ context.Context, handle string) (iam.Principal, error) {
	if principal, ok := p[handle]; ok {
		return principal, nil
	}
	return iam.Principal{Kind: iam.KindSeat, Seat: handle, Stage: iam.StageActive,
		Grants: iam.Apps}, nil
}
