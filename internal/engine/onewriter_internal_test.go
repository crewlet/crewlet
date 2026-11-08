package engine

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A REPLY'S WRITER IS THE SEAT THE PARTY REGISTRY NAMES, never the reply's
// words: a vendor's chat by the actor its parser stamped, in that source's
// own namespace, and a first-party notice by the seat its record names while
// the chart still holds it. A credential acting as no seat, the engine, a
// stranger and a seat the chart lost hold no seat, so their reply answers no
// parked question (sandbox.Reply.putTo). A vendor still delivers a suspended
// person's chat, and the registry withholds their identities, so their reply
// names nobody here either.
//
// Mutation: take a first-party writer's seat as its record states it,
// without asking the registry, and the seat the chart lost writes the reply.
func TestAReplysWriterIsTheSeatTheRegistryNames(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Units: []*org.Unit{{
		Name: "Eng", Type: org.UnitTypeTeam, Lead: "lead",
		Roles: []*org.Role{{Name: "Lead", DeclaredHandle: "lead"}, {
			Name: "Sarah Chen", DeclaredHandle: "sarah", Kind: org.KindHuman,
			Contact: &org.HumanContact{SlackUserID: "U0SARAH"},
		}},
	}}}
	o.Normalize()
	nobody := func(string) (string, bool) { return "", false }
	registry := func(holders ...notify.Holder) *notify.Registry {
		reg := notify.NewRegistry(o, nobody)
		reg.ReconcileHumanContacts(o, nobody, notify.StandingOf(holders))
		return reg
	}
	notice := func(n types.ExternalNotification) []*events.Event {
		return []*events.Event{events.New(n, events.TraceContext{})}
	}
	chat := func(actor, sender string) []*events.Event {
		return notice(types.ExternalNotification{NotificationSource: "slack", Sender: sender,
			Metadata: map[string]string{notify.ActorField: actor}})
	}
	firstParty := func(w types.NotificationWriter) []*events.Event {
		return notice(types.ExternalNotification{NotificationSource: worklet.Source, Writer: &w})
	}
	active := registry(notify.Holder{Seat: "sarah", Stage: iam.StageActive})
	for name, tc := range map[string]struct {
		parties notify.Parties
		evs     []*events.Event
		want    string
	}{
		"a person's chat":                   {active, chat("U0SARAH", "Sarah Chen"), "sarah"},
		"a notice naming her as its sender": {active, chat("", "U0SARAH"), "sarah"},
		"a stranger calling themselves her": {active, chat("U0NOBODY", "Sarah Chen"), ""},
		"a suspended person's chat": {registry(notify.Holder{Seat: "sarah", Stage: iam.StageSuspended}),
			chat("U0SARAH", "Sarah Chen"), ""},
		"a person's comment, as their seat": {active,
			firstParty(types.NotificationWriter{Kind: iam.ActorHuman, Seat: "sarah"}), "sarah"},
		"a colleague seat's comment": {active,
			firstParty(types.NotificationWriter{Kind: iam.ActorAgent, Seat: "lead"}), "lead"},
		"a seat the chart lost": {active,
			firstParty(types.NotificationWriter{Kind: iam.ActorAgent, Seat: "gone"}), ""},
		"a credential acting as no seat": {active, firstParty(types.NotificationWriter{
			Kind: iam.ActorOperator, Login: "token:ci", Apps: []iam.Grant{iam.GrantWorklet}}), ""},
		"the engine itself": {active,
			firstParty(types.NotificationWriter{Kind: iam.ActorSystem}), ""},
		"no registry": {nil, chat("U0SARAH", "Sarah Chen"), ""},
	} {
		if got := replyWriter(tc.parties, tc.evs); got != tc.want {
			t.Errorf("%s: the reply is written by %q, want %q", name, got, tc.want)
		}
	}
}

// A SEAT ANSWERS A PARKED QUESTION ONLY WHILE ITS HOLDER MAY ACT, read when
// the reply arrives: an agent seat the chart holds, or a human seat a person
// holds and may act in. A suspended or removed person, a human seat nobody
// holds and a seat the chart lost answer nothing; a directory that cannot say
// is unknown, never a refusal of somebody who may well act.
//
// Mutation: drop the stage from holderMayAnswer, and the suspended person's
// reply answers the question.
func TestASeatAnswersOnlyWhileItsHolderMayAct(t *testing.T) {
	t.Parallel()
	person := func(stage iam.Stage) iam.Principal {
		return iam.Principal{Kind: iam.KindPerson, ID: uuid.New(), Seat: "sarah", Stage: stage}
	}
	for name, tc := range map[string]struct {
		p       iam.Principal
		err     error
		may     bool
		unknown bool
	}{
		"an agent seat":               {p: iam.Principal{Kind: iam.KindSeat, Seat: "lead", Stage: iam.StageActive}, may: true},
		"an active person":            {p: person(iam.StageActive), may: true},
		"a suspended person":          {p: person(iam.StageSuspended)},
		"a removed person":            {p: person(iam.StageRetired)},
		"a human seat nobody holds":   {p: iam.Principal{Kind: iam.KindPerson, Seat: "sarah", Stage: iam.StageActive}},
		"a seat the chart lost":       {err: errNoSuchSeat},
		"a directory that cannot say": {err: errors.New("the directory is behind"), unknown: true},
	} {
		may, err := holderMayAnswer(tc.p, tc.err)
		if may != tc.may || (err != nil) != tc.unknown {
			t.Errorf("%s: may answer = %v (%v), want %v, unknown %v", name, may, err, tc.may, tc.unknown)
		}
	}
}

// ONE TURN WORKS FOR ONE WRITER, END TO END (ADR-0109). A person's comment and
// a stranger's on one work item reach the seat in one drain, and the partition
// function makes them two turns. The person's acts for the person, and the
// stranger's for nobody the company holds. It is never one turn acting for
// whoever spoke first: that handed the stranger's words the person's apps, or,
// with the stranger first, read the person's as a stranger's.
//
// THROUGH THE REAL PATH: each record as the feed relays it, the tracker's own
// parser, the notification service's wake and its stamped key, the partition
// function the node drains by, and the interactions a turn reads its
// requester and its senders off.
//
// Mutation: drop the writer from notify.KeyOf, and the two comments are one
// turn acting for the person.
func TestAPersonAndAStrangerInOneThreadAreTwoTurns(t *testing.T) {
	t.Parallel()
	o := audienceChart(t)
	reg := notify.NewRegistry(o, nil)
	e := &Engine{}
	e.notify.registry = reg
	parser, prompt := worklet.NewParser(worklet.ParserOptions{}), worklet.Prompt{}
	person := noticeOf(t, reg, parser, prompt, workItemNotice(t, "ada", worklet.AuthorHuman, nil, "swe"), "swe")
	// A SEAT THE CHART NO LONGER HOLDS is nobody the company holds now
	// (TestAFirstPartyNoticeIsFromWhoeverWroteIt).
	stranger := noticeOf(t, reg, parser, prompt, workItemNotice(t, "gone", worklet.AuthorAgent, nil, "swe"), "swe")
	thread := notify.KeyOfAll([]*events.Event{person})
	if thread == "" || notify.KeyOfAll([]*events.Event{stranger}) != thread {
		t.Fatalf("the fixture's two comments are stamped %q and %q, want one thread", thread,
			notify.KeyOfAll([]*events.Event{stranger}))
	}

	for name, drain := range map[string][]*events.Event{
		"the person first":   {person, stranger},
		"the stranger first": {stranger, person},
	} {
		parts := queue.PartitionByKey(drain, notify.KeyOf, func(ev *events.Event) *events.Event { return ev })
		if len(parts) != 2 {
			t.Errorf("%s: one drain of two writers' comments made %d turns, want one each", name, len(parts))
			continue
		}
		for _, part := range parts {
			if len(part.Items) != 1 {
				t.Errorf("%s: a turn holds %d comments, want its writer's one", name, len(part.Items))
				continue
			}
			// THE THREAD'S OWN KEY on what outlives the drain: a merged
			// event and a parked run's batch are the thread's, never a
			// writer's.
			if got := notify.KeyOfAll(part.Items); got != thread {
				t.Errorf("%s: the turn's stamped key is %q, want the thread's %q", name, got, thread)
			}
			interactions := e.interactionsOf(part.Items, "swe")
			requester := requesterOf(part.Items, interactions, "swe")
			senders := sendersSpoken(interactions)
			switch part.Items[0] {
			case person:
				if requester != "ada" || len(senders) != 1 || senders[0].Handle != "ada" {
					t.Errorf("%s: the person's turn acts for %q with senders %+v, want ada alone", name,
						requester, senders)
				}
			case stranger:
				if requester != "" || len(senders) != 1 || senders[0].Handle != "" {
					t.Errorf("%s: the stranger's turn acts for %q with senders %+v, want nobody the "+
						"company holds", name, requester, senders)
				}
			}
		}
	}
}
