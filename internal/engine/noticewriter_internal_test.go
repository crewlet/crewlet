package engine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A WORK ITEM'S OR A PAGE'S NOTICE IS FROM WHOEVER WROTE IT, as the engine's
// own tracker and knowledge base recorded them — never from a stranger.
//
// A vendor names its sender by an id in its own namespace, and the party
// registry maps the ids it knows to seats: a sender it cannot map is somebody
// the company does not know, and a turn such a sender woke reads nothing for
// them (builtin.ceilingOf). The engine's own surfaces have no such namespace:
// their writer is a seat's handle, a credential's login or the engine itself.
// Read as vendor ids, none of them resolved, so every turn a colleague's or a
// person's comment on a work item or a page woke was a stranger's — every code
// read and every artifact refused in it, the counterparty profiled under a
// platform id rather than their handle — and so was the engine's own repair,
// which nobody sent.
//
// THROUGH THE REAL PATH: the record as the feed relays it, the source's own
// parser, the notification service's wake, and the interactions a turn reads
// its requester and its senders off.
func TestAFirstPartyNoticeIsFromWhoeverWroteIt(t *testing.T) {
	t.Parallel()
	o := audienceChart(t)
	reg := notify.NewRegistry(o, nil)
	e := &Engine{}
	e.notify.registry = reg

	type writer struct {
		actor string
		kind  string
	}
	cases := []struct {
		name   string
		writer writer
		// requester is the seat the turn is acting for, "" for none.
		requester string
		// stranger is a turn whose senders nobody the company holds stands
		// behind: one the requester ceiling lets read nothing.
		stranger bool
		// trackerOnly is a writer only the tracker records: no record of
		// the knowledge base that wakes anybody names the engine.
		trackerOnly bool
	}{
		{name: "a colleague seat's", writer: writer{"cto", "agent"}, requester: "cto"},
		{name: "a person's, as the seat they hold", writer: writer{"ada", "human"}, requester: "ada"},
		// THE ENGINE'S OWN WRITE — a duty's repair, an unblocking it
		// reports — is nobody's errand: no counterparty, no requester.
		{name: "the engine's own", writer: writer{"node-a", "system"}, trackerOnly: true},
		// A CREDENTIAL ACTING AS NO SEAT has no seat the ceiling can
		// compose, so it stays the closed end it always was.
		{name: "a credential's acting as no seat", writer: writer{"token:ci", "operator"}, stranger: true},
		// A SEAT THE CHART NO LONGER HOLDS is nobody the company holds now,
		// as a chat message from a removed person is.
		{name: "a removed seat's", writer: writer{"gone", "agent"}, stranger: true},
	}
	for _, source := range []string{worklet.Source, doclet.Source} {
		for _, c := range cases {
			if c.trackerOnly && source != worklet.Source {
				continue
			}
			t.Run(source+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				var payload []byte
				var parser notify.Parser
				var prompt notify.Prompt
				switch source {
				case worklet.Source:
					payload = workItemNotice(t, c.writer.actor, worklet.AuthorKind(c.writer.kind), "swe")
					parser, prompt = worklet.NewParser(worklet.ParserOptions{}), worklet.Prompt{}
				default:
					payload = pageNotice(t, c.writer.actor, doclet.AuthorKind(c.writer.kind), "swe")
					parser, prompt = doclet.NewParser(doclet.ParserOptions{}), doclet.Prompt{}
				}
				evs := []*events.Event{noticeOf(t, reg, parser, prompt, payload, "swe")}

				interactions := e.interactionsOf(evs, "swe")
				if got := requesterOf(evs, interactions, "swe"); got != c.requester {
					t.Errorf("the turn is acting for %q, want %q", got, c.requester)
				}
				senders := sendersSpoken(interactions)
				stranger := c.requester == "" && len(senders) > 0
				if stranger != c.stranger {
					t.Errorf("the turn reads as a stranger's: %v, want %v (senders %+v)",
						stranger, c.stranger, senders)
				}
				profiled := sendersOf(reg, evs)
				switch {
				case c.requester != "":
					if len(senders) != 1 || senders[0].Handle != c.requester {
						t.Errorf("the turn's senders are %+v, want the seat %s", senders, c.requester)
					}
					if len(profiled) != 1 || profiled[0].Handle != c.requester {
						t.Errorf("the counterparty is profiled as %+v, want under the handle %s — "+
							"what a seat learned of them on chat and on its own tracker is one profile",
							profiled, c.requester)
					}
				case !c.stranger:
					if len(senders) != 0 || len(profiled) != 0 {
						t.Errorf("nobody sent the engine's own notice, yet it names senders %+v "+
							"and profiles %+v", senders, profiled)
					}
				}
			})
		}
	}
}

// workItemNotice is a comment on a work item swe is assigned, written by actor
// as kind, encoded as the tracker's feed relays it.
func workItemNotice(t *testing.T, actor string, kind worklet.AuthorKind, assignee string) []byte {
	t.Helper()
	record := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "op-work",
			Subject: worklet.TaskSubject("t-1"), Op: worklet.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: "node-a",
			Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: json.RawMessage(`{}`), Actor: actor, ActorKind: kind,
		Notify: &worklet.Notify{
			Kind: worklet.ChangeComment, Excerpt: "can you read the repository for me",
			Snapshot: worklet.Snapshot{
				Key: "ENG-1", Project: "ENG", Title: "wire it",
				Status: worklet.StatusInProgress, Assignee: assignee,
			},
		},
	}
	payload, err := record.Encode()
	if err != nil {
		t.Fatalf("encode the work item's record: %v", err)
	}
	return payload
}

// pageNotice is a comment on a page that mentions recipient, written by actor
// as kind, encoded as the knowledge base's feed relays it.
func pageNotice(t *testing.T, actor string, kind doclet.AuthorKind, recipient string) []byte {
	t.Helper()
	record := doclet.MutationRecord{
		RecordEnvelope: doclet.RecordEnvelope{
			OpID: "op-page", Subject: doclet.PageSubject("p-1"), Op: doclet.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: "node-a",
			Scope: doclet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: json.RawMessage(`{}`), Actor: actor, ActorKind: kind,
		Notify: &doclet.Notify{
			Kind: doclet.ChangeComment, PageID: "p-1", Version: 2,
			Mentions: []string{recipient}, Excerpt: "can you read the repository for me",
			Container: "ENG", Title: "Runbook",
		},
	}
	payload, err := doclet.Encode(record)
	if err != nil {
		t.Fatalf("encode the page's record: %v", err)
	}
	return payload
}

// noticeOf is the one wake a first-party record puts on recipient's inbox: the
// record delivered as its feed delivers it, routed by the source's own parser
// and woken by the notification service, read back off the recipient's
// mailbox.
func noticeOf(t *testing.T, reg *notify.Registry, parser notify.Parser, prompt notify.Prompt,
	payload []byte, recipient string) *events.Event {

	t.Helper()
	party, ok := reg.ByHandle(recipient)
	if !ok {
		t.Fatalf("the chart holds no seat %q", recipient)
	}
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start the queue: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(t.Context()) })
	// THE MAILBOX FIRST: a subscription created after a publish starts
	// empty, and a seat's mail waits in its durable one.
	topic, group := topics.AgentInbox(party.AgentID), topics.AgentInboxGroup(party.AgentID)
	if _, err := q.EnsureSubscription(t.Context(), topic, group); err != nil {
		t.Fatalf("ensure %s's mailbox: %v", recipient, err)
	}
	svc, err := notify.New(notify.Options{
		Queue: q, Registry: func() *notify.Registry { return reg },
		Prompts: notify.NewPrompts(prompt), Parsers: []notify.Parser{parser},
	})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode the relayed record: %v", err)
	}
	delivery := events.New(types.RawWebhook{Body: body}, events.NewTrace())
	delivery.Source = parser.Source()
	svc.Handle(t.Context(), delivery)
	mail := q.Backlog(topic, group)
	if len(mail) != 1 {
		t.Fatalf("the record woke %s %d times, want once", recipient, len(mail))
	}
	return mail[0]
}
