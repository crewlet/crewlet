package tracker_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// WHO NOT TO WAKE IS A PARTY, NOT A HANDLE.
//
// A wake is dropped for the person who made the change — telling somebody what
// they just did is noise a model spends a round on — and the comparison was
// against the record's AUTHOR alone. That author is the token on
// `/operator/mcp`, deliberately and permanently, while a bound operator's own
// gestures land on their SEAT: the watch a create leaves, the watch a comment
// leaves. So the record said `founder`, the candidate said `jane-founder`, and
// the exclusion matched neither — a founder was woken by every item their own
// assistant filed and every comment it left, each one an addressed turn about
// work they had just done themselves.
//
// The record now carries the bound seat beside the author
// ([tracker.MutationRecord.ActorSeat]) and the exclusion reads both. The
// author fields are untouched: this changes who is woken and nothing about
// who wrote it.

// operatorRecord is a record written through `/operator/mcp` by a token the
// company bound to a human seat — the author is the credential, the kind says
// it is not a seat, and the seat travels beside them.
func operatorRecord(n *tracker.Notify, seat string) tracker.MutationRecord {
	record := parseRecord(n)
	record.Actor = "founder"
	record.ActorKind = tracker.AuthorOperator
	record.OperatorID = "founder"
	record.ActorSeat = seat
	return record
}

func TestABoundOperatorIsNotWokenByTheirOwnWrite(t *testing.T) {
	t.Parallel()
	for name, notify := range map[string]*tracker.Notify{
		// THE CREATE: the reporter watches what they filed, and
		// through an assistant that reporter's watch is their seat's.
		"a create": {
			Kind: tracker.ChangeCreated,
			Snapshot: tracker.Snapshot{
				Key: "ENG-1", Project: "ENG", Title: "wire it",
				Assignee: "cy", Reporter: "founder",
				Watchers: []string{"jane-founder", "cy"},
			},
		},
		// THE COMMENT: the commenter is subscribed automatically, and
		// the same rule applies to the subscription it leaves.
		"a comment": {
			Kind: tracker.ChangeComment, Excerpt: "shipping this today",
			Snapshot: tracker.Snapshot{
				Key: "ENG-1", Project: "ENG", Assignee: "cy",
				Reporter: "founder", Watchers: []string{"jane-founder", "di"},
				CommentAuthorKind: tracker.AuthorOperator,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			routed, err := tracker.NewParser(tracker.ParserOptions{}).Parse(
				t.Context(), delivery(t, operatorRecord(notify, "jane-founder")),
				registry(t, "cy", "di", "jane-founder"))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			var woken []string
			for _, r := range routed {
				woken = append(woken, r.To.Handle)
			}
			if slices.Contains(woken, "jane-founder") {
				t.Errorf("%s woke %v — the person who made the change is on "+
					"that list under the seat their own credential is bound "+
					"to, and being told what you just did is a turn spent on "+
					"nothing", name, woken)
			}
			// AND EVERYBODY ELSE STILL HEARS IT. An exclusion that
			// widened until it reached the other watchers would be a
			// change nobody is told about, which is the worse failure
			// and the one this half is here to catch.
			if !slices.Contains(woken, "cy") {
				t.Errorf("%s woke %v and the assignee is not among them", name, woken)
			}
		})
	}
}

// AND THE ONE REASON THAT REACHES THE ACTOR STILL REACHES THEIR SEAT.
//
// An unblocked notice is about ANOTHER task becoming workable, so the person
// who closed the blocker is exactly who needs to hear it — [Reason.WakesActor]
// is that exception, and an exclusion that read the seat without honouring it
// would take the whole feature away from the one person it is for.
func TestAnUnblockedNoticeStillReachesTheActorsOwnSeat(t *testing.T) {
	t.Parallel()
	routed, err := tracker.NewParser(tracker.ParserOptions{}).Parse(
		t.Context(), delivery(t, operatorRecord(&tracker.Notify{
			Kind: tracker.ChangeStatus,
			Snapshot: tracker.Snapshot{
				Key: "ENG-1", Project: "ENG",
				StatusGroup: tracker.GroupDone, PrevStatusGroup: tracker.GroupActive,
				Unblocked: []tracker.TaskParty{
					{Task: "t-2", Key: "ENG-2", Assignee: "jane-founder"},
				},
			},
		}, "jane-founder")), registry(t, "jane-founder"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(routed) != 1 || routed[0].To.Handle != "jane-founder" {
		t.Fatalf("the unblocked notice reached %v — it is about the OTHER "+
			"task, so the person who cleared the blocker is who it is for",
			routed)
	}
	if got := routed[0].Metadata[tracker.MetaVia]; got != string(tracker.ReasonUnblocked) {
		t.Errorf("the notice was routed via %q, want %q", got, tracker.ReasonUnblocked)
	}
}

// AND A WRITER WITH NO SEAT BOUND ROUTES EXACTLY AS BEFORE.
//
// Every seat, every human at the dashboard and every token nobody bound is a
// party of ONE — the author's own handle, which is what the exclusion
// compared against before the field existed. The wire says so too: a record
// with no bound seat carries no `actor_seat` key at all, so an older build
// reading one sees the bytes it has always seen.
func TestAnUnboundWriterRoutesAsBefore(t *testing.T) {
	t.Parallel()
	// `ana` is [parseRecord]'s own author, and a watcher here.
	record := parseRecord(&tracker.Notify{
		Kind: tracker.ChangeStatus,
		Snapshot: tracker.Snapshot{
			Key: "ENG-1", Project: "ENG", Assignee: "cy",
			Watchers: []string{"ana", "di"},
		},
	})
	payload, err := record.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, held := wire["actor_seat"]; held {
		t.Errorf("a record with no bound seat carries %s — an omitted field is "+
			"what keeps the wire what every older build already reads", payload)
	}
	routed, err := tracker.NewParser(tracker.ParserOptions{}).Parse(
		t.Context(), delivery(t, record), registry(t, "ana", "cy", "di"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var woken []string
	for _, r := range routed {
		woken = append(woken, r.To.Handle)
	}
	if slices.Contains(woken, "ana") {
		t.Errorf("the writer was woken about their own change: %v", woken)
	}
	if !slices.Contains(woken, "cy") || !slices.Contains(woken, "di") {
		t.Errorf("the change reached %v, want the assignee and the other "+
			"watcher", woken)
	}
}
