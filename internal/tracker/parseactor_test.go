package tracker_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// WHO NOT TO WAKE IS THE AUTHOR, AND A BOUND PERSON'S AUTHOR IS THEIR SEAT.
//
// A wake is dropped for the person who made the change — telling somebody what
// they just did is noise a model spends a round on. A person bound to a seat
// writes AS that seat, kind `human`, with the credential they acted through
// beside it (iam.ActorFor), so the handle their own gestures land on — the
// watch a create leaves, the watch a comment leaves — is the record's author
// and one comparison drops it. A record authored by the credential instead
// woke a founder for every item their own assistant filed.

// personRecord is a record written through a person's own credential by
// somebody the identity directory binds to a human seat: the author is the
// seat, and the credential rides beside it.
func personRecord(n *tracker.Notify, seat string) tracker.MutationRecord {
	record := parseRecord(n)
	record.Actor = seat
	record.ActorKind = tracker.AuthorHuman
	record.OperatorID = "pat:founder-laptop"
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
				Assignee: "cy", Reporter: "jane-founder",
				Watchers: []string{"jane-founder", "cy"},
			},
		},
		// THE COMMENT: the commenter is subscribed automatically, and
		// the same rule applies to the subscription it leaves.
		"a comment": {
			Kind: tracker.ChangeComment, Excerpt: "shipping this today",
			Snapshot: tracker.Snapshot{
				Key: "ENG-1", Project: "ENG", Assignee: "cy",
				Reporter: "jane-founder", Watchers: []string{"jane-founder", "di"},
				CommentAuthorKind: tracker.AuthorHuman,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			routed, err := tracker.NewParser(tracker.ParserOptions{}).Parse(
				t.Context(), delivery(t, personRecord(notify, "jane-founder")),
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
					"that list under the seat they write as, and being told "+
					"what you just did is a turn spent on nothing", name, woken)
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
// is that exception, and an exclusion that dropped the author without honouring
// it would take the whole feature away from the one person it is for.
func TestAnUnblockedNoticeStillReachesTheActorsOwnSeat(t *testing.T) {
	t.Parallel()
	routed, err := tracker.NewParser(tracker.ParserOptions{}).Parse(
		t.Context(), delivery(t, personRecord(&tracker.Notify{
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
