package worklet_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/worklet"
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
	record := parseRecord(&worklet.Notify{
		Kind:     worklet.ChangeComment,
		Excerpt:  "can you look at this",
		Mentions: []string{"bo"},
		Snapshot: worklet.Snapshot{
			Key: "ENG-1", Project: "ENG", Title: "wire it",
			Status: worklet.StatusInProgress, Assignee: "cy",
			CommentAuthorKind:  worklet.AuthorHuman,
			ThreadParticipants: []string{"di"},
		},
	})

	routed, err := worklet.NewParser(worklet.ParserOptions{}).Parse(
		t.Context(), delivery(t, record), registry(t, "ana", "bo", "cy", "di"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reasons := map[string]string{}
	for _, r := range routed {
		reasons[r.To.Handle] = r.Metadata[worklet.MetaVia]
	}
	want := map[string]string{
		"bo": string(worklet.ReasonMention),
		"cy": string(worklet.ReasonAssignee),
		"di": string(worklet.ReasonThread),
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
	record := parseRecord(&worklet.Notify{
		Kind: worklet.ChangeComment,
		Snapshot: worklet.Snapshot{
			Key: "ENG-1", Project: "ENG", Assignee: "somebody-who-left",
			CommentAuthorKind:  worklet.AuthorHuman,
			ThreadParticipants: []string{"di"},
		},
	})
	routed, err := worklet.NewParser(worklet.ParserOptions{}).Parse(
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
		t.Fatalf("the change reached %v, and the thread participant who is "+
			"still here should have been told", routed)
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
	record := parseRecord(&worklet.Notify{
		Kind: worklet.ChangeComment,
		Snapshot: worklet.Snapshot{
			Key: "ENG-1", Project: "ENG", ThreadParticipants: []string{"di", "bo"},
		},
	})
	parser := worklet.NewParser(worklet.ParserOptions{})
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

// EVERY WAKE CARRIES ITS RECORD'S OWN INSTANT, which every redelivery of the
// record reproduces — see notify.Routed.WakeAt for what the delivery's own
// clock cost a re-run turn.
func TestAWakeIsStampedWithItsRecordsInstant(t *testing.T) {
	t.Parallel()
	record := parseRecord(&worklet.Notify{
		Kind: worklet.ChangeComment,
		Snapshot: worklet.Snapshot{
			Key: "ENG-1", Project: "ENG", ThreadParticipants: []string{"di", "bo"},
		},
	})
	routed, err := worklet.NewParser(worklet.ParserOptions{}).Parse(t.Context(),
		delivery(t, record), registry(t, "ana", "bo", "di"))
	if err != nil || len(routed) == 0 {
		t.Fatalf("Parse = (%d, %v)", len(routed), err)
	}
	for _, r := range routed {
		if !r.WakeAt.Equal(record.CreatedAt) {
			t.Errorf("%s's wake is stamped %s, want the record's %s",
				r.To.Handle, r.WakeAt, record.CreatedAt)
		}
	}
}

func parseRecord(n *worklet.Notify) worklet.MutationRecord {
	return worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "op-1",
			Subject: worklet.TaskSubject("t-1"), Op: worklet.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: "node-a",
			Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: json.RawMessage(`{}`), Actor: "ana",
		ActorKind: worklet.AuthorHuman, Notify: n,
	}
}

func delivery(t *testing.T, record worklet.MutationRecord) types.RawWebhook {
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
	return notify.NewRegistry(o, nil)
}

// A WOKEN SEAT READS THE WHOLE COMMENT, NOT THE CARD'S EXCERPT. The card is a
// preview cut at MaxExcerpt for a list of notices; the wake prompt is what a
// seat acts on, and a review cut there is a review whose "but" never arrived.
// The record carries the comment whole, so the wake is given that — and a
// stated excerpt (a hand-off's reason) that was never derived from the payload
// is shown as itself.
func TestAWokenSeatReadsTheWholeComment(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("Looks good overall. ", 60) + "BUT the migration is missing."
	wake := worklet.Wake{
		Kind:    worklet.ChangeComment,
		Comment: &worklet.Comment{ID: "c-1", Body: long, Author: "ana", AuthorKind: worklet.AuthorHuman},
		After:   worklet.Task{ID: "t-1", Key: "ENG-1", Project: "ENG", Assignee: "bo"},
	}.Notify(nil)
	if wake == nil || !strings.HasSuffix(wake.Excerpt, "…") {
		t.Fatal("the fixture must produce a cut card excerpt")
	}
	record := parseRecord(wake)
	payload, err := json.Marshal(map[string]any{"comment": worklet.Comment{ID: "c-1", Body: long, Author: "ana"}})
	if err != nil {
		t.Fatal(err)
	}
	record.Mutation = payload
	routed, err := worklet.NewParser(worklet.ParserOptions{}).Parse(
		t.Context(), delivery(t, record), registry(t, "ana", "bo"))
	if err != nil || len(routed) == 0 {
		t.Fatalf("Parse: %d routed, %v", len(routed), err)
	}
	if routed[0].Body != long {
		t.Fatalf("the woken seat read %d bytes of a %d-byte comment", len(routed[0].Body), len(long))
	}

	// A STATED EXCERPT IS SHOWN AS ITSELF: it was not cut from the payload.
	stated := parseRecord(&worklet.Notify{
		Kind: worklet.ChangeComment, Excerpt: "handing this to bo: he owns the schema",
		Snapshot: worklet.Snapshot{
			Key: "ENG-1", Project: "ENG", Assignee: "bo",
			CommentAuthorKind: worklet.AuthorHuman,
		},
	})
	stated.Mutation = payload
	routed, err = worklet.NewParser(worklet.ParserOptions{}).Parse(
		t.Context(), delivery(t, stated), registry(t, "ana", "bo"))
	if err != nil || len(routed) == 0 {
		t.Fatalf("Parse: %d routed, %v", len(routed), err)
	}
	if routed[0].Body != "handing this to bo: he owns the schema" {
		t.Fatalf("a stated excerpt was replaced: %q", routed[0].Body)
	}
}
