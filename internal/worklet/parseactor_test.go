package worklet_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/worklet"
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
func personRecord(n *worklet.Notify, seat string) worklet.MutationRecord {
	record := parseRecord(n)
	record.Actor = seat
	record.ActorKind = worklet.AuthorHuman
	record.OperatorID = "pat:founder-laptop"
	return record
}

func TestABoundOperatorIsNotWokenByTheirOwnWrite(t *testing.T) {
	t.Parallel()
	for name, notify := range map[string]*worklet.Notify{
		// THE CREATE: the reporter watches what they filed, and
		// through an assistant that reporter's watch is their seat's.
		"a create": {
			Kind: worklet.ChangeCreated,
			Snapshot: worklet.Snapshot{
				Key: "ENG-1", Project: "ENG", Title: "wire it",
				Assignee: "cy", Reporter: "jane-founder",
				Watchers: []string{"jane-founder", "cy"},
			},
		},
		// THE COMMENT: the commenter is subscribed automatically, and
		// the same rule applies to the subscription it leaves.
		"a comment": {
			Kind: worklet.ChangeComment, Excerpt: "shipping this today",
			Snapshot: worklet.Snapshot{
				Key: "ENG-1", Project: "ENG", Assignee: "cy",
				Reporter: "jane-founder", Watchers: []string{"jane-founder", "di"},
				CommentAuthorKind: worklet.AuthorHuman,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			routed, err := worklet.NewParser(worklet.ParserOptions{}).Parse(
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
// That a task of yours became workable is news whoever wrote the record that
// says so — [Reason.WakesActor] is that exception — and an exclusion that
// dropped the author without honouring it would silence the notice for the
// one person it is for whenever they are its writer. The record is the
// repair's own shape ([worklet.Writer.TellUnblocked]): a late relations commit
// on the dependent, naming its assignee alone, here written through that
// person's own credential.
func TestAnUnblockedNoticeStillReachesTheActorsOwnSeat(t *testing.T) {
	t.Parallel()
	routed, err := worklet.NewParser(worklet.ParserOptions{}).Parse(
		t.Context(), delivery(t, personRecord(&worklet.Notify{
			Kind: worklet.ChangeRelations,
			Late: true,
			Snapshot: worklet.Snapshot{
				Key: "ENG-1", Project: "ENG",
				Unblocked: []worklet.TaskParty{
					{Task: "t-1", Key: "ENG-1", Assignee: "jane-founder"},
				},
			},
		}, "jane-founder")), registry(t, "jane-founder"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(routed) != 1 || routed[0].To.Handle != "jane-founder" {
		t.Fatalf("the unblocked notice reached %v — a task of hers became "+
			"workable, so she is who it is for, whoever wrote it", routed)
	}
	if got := routed[0].Metadata[worklet.MetaVia]; got != string(worklet.ReasonUnblocked) {
		t.Errorf("the notice was routed via %q, want %q", got, worklet.ReasonUnblocked)
	}
	// AND THE SPINE DELIVERS WHAT THE ROUTER KEPT. The copy names its actor
	// under the one key every parser stamps, so the self-action guard sees
	// a notice to the person who caused it — and lets it through because
	// the prompt reads the reason this copy carries.
	if got := routed[0].Metadata[notify.ActorField]; got != "jane-founder" {
		t.Fatalf("the copy names its actor as %q, want the seat that wrote it", got)
	}
	reg := firstPartyRegistry(t, "jane-founder")
	seat, _ := reg.ByHandle("jane-founder")
	prompts := notify.NewPrompts(worklet.Prompt{})
	if ok, why := notify.Deliverable(prompts, reg, routed[0].Inbound, seat); !ok {
		t.Errorf("the spine dropped the unblocked notice to its actor (%s)", why)
	}
}

// firstPartyRegistry is [registry] answering the tracker's source by the
// handle its ids are, as the engine's own registry does.
func firstPartyRegistry(t *testing.T, handles ...string) *notify.Registry {
	t.Helper()
	o := &org.Organization{Name: "nimbus"}
	for _, handle := range handles {
		o.Roles = append(o.Roles, &org.Role{
			Name: strings.ToUpper(handle), DeclaredHandle: handle,
		})
	}
	o.Normalize()
	return notify.NewRegistry(o, nil, worklet.Source)
}
