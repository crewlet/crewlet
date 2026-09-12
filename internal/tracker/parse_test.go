package tracker_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY RECIPIENT IS TOLD WHY, AND THE REASONS DO NOT LEAK INTO EACH OTHER.
//
// # The failure this exists to catch
//
// One record produces one copy per recipient, each with its own reason — and
// the copies share a metadata map unless somebody makes one. Written in place,
// every recipient of a change gets whichever reason happened to be rendered
// last: the person who was mentioned is told they are watching, and the
// watcher is told they were mentioned. The prompt renders those as an ask and
// as news, so it is not a cosmetic difference.
func TestEveryRecipientCarriesItsOwnReason(t *testing.T) {
	t.Parallel()
	record := parseRecord(&tracker.Notify{
		Kind:     tracker.ChangeComment,
		Excerpt:  "can you look at this",
		Mentions: []string{"bo"},
		Snapshot: tracker.Snapshot{
			Key: "ENG-1", Project: "ENG", Title: "wire it",
			Status: tracker.StatusInProgress, Assignee: "cy",
			Watchers: []string{"di"},
		},
	})

	routed, err := tracker.NewParser(tracker.ParserOptions{}).Parse(
		t.Context(), delivery(t, record), registry(t, "ana", "bo", "cy", "di"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reasons := map[string]string{}
	for _, r := range routed {
		reasons[r.To.Handle] = r.Metadata[tracker.MetaVia]
	}
	want := map[string]string{
		"bo": string(tracker.ReasonMention),
		"cy": string(tracker.ReasonAssignee),
		"di": string(tracker.ReasonWatcher),
	}
	for handle, reason := range want {
		if reasons[handle] != reason {
			t.Errorf("%s was told %q and the reason is %q — the prompt renders "+
				"a mention as an ask and a watch as news, so a reason that "+
				"leaked from another copy asks the wrong person for something",
				handle, reasons[handle], reason)
		}
	}
	if _, woken := reasons["ana"]; woken {
		t.Error("the actor was woken about their own comment")
	}
}

// A HANDLE THAT IS NO LONGER A SEAT IS DROPPED, NOT ROUTED.
//
// A notification addressed to nobody is one nothing reports: it leaves the
// spine, finds no party, and disappears — so the person who was supposed to
// hear about the change learns nothing and no error names them.
func TestAHandleThatIsNoLongerASeatIsDropped(t *testing.T) {
	t.Parallel()
	record := parseRecord(&tracker.Notify{
		Kind: tracker.ChangeStatus,
		Snapshot: tracker.Snapshot{
			Key: "ENG-1", Project: "ENG", Assignee: "somebody-who-left",
			Watchers: []string{"di"},
		},
	})
	routed, err := tracker.NewParser(tracker.ParserOptions{}).Parse(
		t.Context(), delivery(t, record), registry(t, "ana", "di"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, r := range routed {
		if r.To.Handle == "somebody-who-left" {
			t.Fatal("a notification was routed to a handle that is no longer " +
				"a seat, which nothing downstream reports")
		}
	}
	if len(routed) != 1 || routed[0].To.Handle != "di" {
		t.Fatalf("the change reached %v, and the watcher who is still here "+
			"should have been told", routed)
	}
}

// THE WAKE ID IS DERIVED FROM THE RECORD AND THE RECIPIENT.
//
// The feed's own claim is the first dedupe layer and it FAILS OPEN — a
// coordination store that cannot be reached must not silently stop
// notifications — so this is what catches the redelivery that slips through.
// With a random id neither the inbox nor the completion ledger can see the
// pair, and the seat is woken twice about one change.
func TestTheWakeIdIsDerivedAndStable(t *testing.T) {
	t.Parallel()
	record := parseRecord(&tracker.Notify{
		Kind: tracker.ChangeStatus,
		Snapshot: tracker.Snapshot{
			Key: "ENG-1", Project: "ENG", Watchers: []string{"di", "bo"},
		},
	})
	parser := tracker.NewParser(tracker.ParserOptions{})
	first, err := parser.Parse(t.Context(), delivery(t, record),
		registry(t, "ana", "bo", "di"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	again, err := parser.Parse(t.Context(), delivery(t, record),
		registry(t, "ana", "bo", "di"))
	if err != nil {
		t.Fatalf("Parse again: %v", err)
	}
	if len(first) != len(again) || len(first) == 0 {
		t.Fatalf("one record parsed to %d and then %d copies", len(first), len(again))
	}
	seen := map[string]bool{}
	for i := range first {
		if first[i].WakeID != again[i].WakeID {
			t.Fatalf("%s's wake id changed between two deliveries of one "+
				"record — a redelivery that slips the feed's claim would wake "+
				"them twice, and neither the inbox nor the ledger could see it",
				first[i].To.Handle)
		}
		if seen[first[i].WakeID.String()] {
			t.Fatalf("two recipients share one wake id, so the inbox collapses " +
				"one person's wake into another's")
		}
		seen[first[i].WakeID.String()] = true
	}
}

func parseRecord(n *tracker.Notify) tracker.MutationRecord {
	return tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "op-1",
			Subject: tracker.TaskSubject("t-1"), Op: tracker.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: "node-a",
			Scope: tracker.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: json.RawMessage(`{}`), Actor: "ana",
		ActorKind: tracker.AuthorHuman, Notify: n,
	}
}

func delivery(t *testing.T, record tracker.MutationRecord) types.RawWebhook {
	t.Helper()
	payload, err := record.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return types.RawWebhook{Body: body}
}

func registry(t *testing.T, handles ...string) *notify.Registry {
	t.Helper()
	o := &org.Organization{Name: "nimbus"}
	for _, handle := range handles {
		o.Roles = append(o.Roles, &org.Role{
			Name: strings.ToUpper(handle), DeclaredHandle: handle,
		})
	}
	o.Normalize()
	return notify.NewRegistry(o)
}
