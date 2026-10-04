package tracker_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// A CHANGED HANDLE IS A NEW SEAT.
//
// A seat's handle is immutable (ADR-0013): a company document that changes one
// has removed a seat and created another, so this domain keys every column
// that names somebody on the handle as written and resolves nothing. The work,
// the notices and the priorities filed under the old handle stay the old
// seat's, and the new handle starts with none of them — rather than following
// a rename this engine no longer has.
//
// The control is the old handle itself, which still reads everything: so the
// new handle's empty day is about the handle, not about a write that never
// landed.
func TestAChangedHandleIsANewSeat(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// A GESTURE HERE WRITES ONE SUBJECT TWICE — a person's record and a
	// task — so the node applies while it writes, as a running one does.
	r.applyWhileWriting()
	ctx := t.Context()
	cto := r.writer.As("cto", tracker.AuthorAgent, tracker.Provenance{})

	routeTo(t, r, "held", "ENG-1", "cto")
	if _, err := cto.WritePriorities(ctx, "op-prio", "cto", []string{"held"},
		nil, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("cto's own priorities: %v", err)
	}
	r.drain()

	// THE CONTROL: the handle the work was filed under still holds it.
	before := r.myWork("cto")
	if got := rowIDs(before.Assigned); !slices.Equal(got, []string{"held"}) {
		t.Fatalf("cto's assignments are %v, want the work filed at cto", got)
	}
	if got := rowIDs(before.Priorities); !slices.Equal(got, []string{"held"}) {
		t.Fatalf("cto's priorities are %v, want the list it set", got)
	}
	if notices := r.inbox(tracker.InboxQuery{Handle: "cto"}).Notices; len(notices) != 1 {
		t.Fatalf("cto's inbox holds %d notices, want the one its work sent", len(notices))
	}

	// A NEW HANDLE STARTS EMPTY.
	day := r.myWork("chief")
	if len(day.Assigned) != 0 || len(day.Priorities) != 0 {
		t.Errorf("chief's day holds %v assigned and %v prioritised — a new "+
			"handle is a new seat, and the old one's work is not its own",
			rowIDs(day.Assigned), rowIDs(day.Priorities))
	}
	if notices := r.inbox(tracker.InboxQuery{Handle: "chief"}).Notices; len(notices) != 0 {
		t.Errorf("chief's inbox holds %d notices meant for cto", len(notices))
	}
}

// rowIDs is the ids of a block, in its order.
func rowIDs(rows []tracker.TaskRow) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}
