package tracker_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// asOperator is the harness's writer acting for one API token, spelled the way
// every surface that serves an operator spells it.
func asOperator(r *roundTrip, token string) *tracker.Writer {
	return r.writer.As(tracker.OperatorActor(token), tracker.AuthorOperator,
		tracker.Provenance{OperatorID: token})
}

// AN OPERATOR IS RECORDED AS `operator:` AND ITS TOKEN'S NAME, never as the
// bare name, which a seat can hold.
//
// The token here is named exactly like the harness's own seat, which is the
// case the spelling exists for: every surface that compares a record's author
// with seats' handles — the wake that leaves out whoever wrote a record, the
// person-record guard — would otherwise read the token's writes as that
// seat's. Asserted at the reader's end, on the history row an audit reads.
func TestAnOperatorIsRecordedUnderItsOperatorName(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")

	done := tracker.StatusDone
	if _, err := asOperator(r, "ana").UpdateTask(t.Context(), "op-token", "t-1",
		"ENG", tracker.NoIfMatch, tracker.TaskPatch{Status: &done},
		tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTask as the operator: %v", err)
	}
	r.drain()

	byOperator := r.activity(tracker.ActivityQuery{
		Workspace: true, ActorKinds: []tracker.AuthorKind{tracker.AuthorOperator},
	})
	if len(byOperator.Records) != 1 {
		t.Fatalf("the feed holds %d operator records, want the one the token made",
			len(byOperator.Records))
	}
	got := byOperator.Records[0]
	if got.Actor != "operator:ana" || got.OperatorID != "ana" {
		t.Errorf("the operator's change is recorded as %q with credential %q, want "+
			"operator:ana and ana — the bare name is the seat ana's", got.Actor,
			got.OperatorID)
	}
}

// EVERY WRITER IS HELD TO ONE IDENTITY RULE, AT BOTH DOORS.
//
// A writer is built by NewWriter or derived by As, and a rule held at one of
// the two is a rule the other walks round. Each identity below names the wrong
// party: an operator under a bare name a seat can hold, or under another
// token's name than the credential it presented, or under no token at all; a
// seat, a person or the engine spelled as an operator, so that every screen
// and audit would show an operator's hand in what they did.
func TestAWriterNamingTheWrongPartyIsRefusedAtBothDoors(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")

	type identity struct {
		actor    string
		kind     tracker.AuthorKind
		operator string
	}
	refused := map[string]identity{
		"an operator under its bare name":        {"ana", tracker.AuthorOperator, "ana"},
		"an operator under another token's name": {"operator:bob", tracker.AuthorOperator, "ana"},
		"an operator naming no credential":       {"operator:ana", tracker.AuthorOperator, ""},
		"an operator under no token at all":      {"operator:", tracker.AuthorOperator, ""},
		"an operator under a blank token":        {"operator: ", tracker.AuthorOperator, " "},
		"a seat spelled as an operator":          {"operator:ana", tracker.AuthorAgent, ""},
		"a person spelled as an operator":        {"operator:ana", tracker.AuthorHuman, "ana"},
		"the engine spelled as an operator":      {"operator:ana", tracker.AuthorSystem, ""},
		"nobody":                                 {"", tracker.AuthorAgent, ""},
		"a kind this build does not have":        {"ana", "robot", ""},
	}
	for name, id := range refused {
		t.Run(name, func(t *testing.T) {
			if _, err := tracker.NewWriter(tracker.WriterDeps{
				Publisher: r.publisher, Actor: id.actor, ActorKind: id.kind,
				OperatorID: id.operator,
			}); err == nil {
				t.Errorf("NewWriter built a writer acting as %q of kind %q for "+
					"credential %q", id.actor, id.kind, id.operator)
			}
			done := tracker.StatusDone
			_, err := r.writer.As(id.actor, id.kind, tracker.Provenance{
				OperatorID: id.operator,
			}).UpdateTask(t.Context(), "op-refused", "t-1", "ENG", tracker.NoIfMatch,
				tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil)
			if err == nil {
				t.Errorf("a writer derived as %q of kind %q for credential %q "+
					"wrote", id.actor, id.kind, id.operator)
			}
		})
	}

	// AND THE IDENTITIES THE RULE ADMITS ARE ADMITTED AT BOTH DOORS, or
	// the refusals above would be a rule refusing everything.
	for name, id := range map[string]identity{
		"an operator": {tracker.OperatorActor("ana"), tracker.AuthorOperator, "ana"},
		"a seat":      {"ana", tracker.AuthorAgent, ""},
		"a person":    {"ana", tracker.AuthorHuman, "ana"},
		"the engine":  {"node-a", tracker.AuthorSystem, ""},
	} {
		t.Run("admitted "+name, func(t *testing.T) {
			if _, err := tracker.NewWriter(tracker.WriterDeps{
				Publisher: r.publisher, Actor: id.actor, ActorKind: id.kind,
				OperatorID: id.operator,
			}); err != nil {
				t.Errorf("NewWriter refused %s: %v", name, err)
			}
			done := tracker.StatusDone
			if _, err := r.writer.As(id.actor, id.kind, tracker.Provenance{
				OperatorID: id.operator,
			}).UpdateTask(t.Context(), "op-admitted-"+string(id.kind), "t-1",
				"ENG", tracker.NoIfMatch, tracker.TaskPatch{Status: &done},
				tracker.ChangeStatus, nil); err != nil {
				t.Errorf("%s could not write: %v", name, err)
			}
			// THIS HARNESS APPLIES WHEN TOLD TO, and the next case's
			// write is decided against rows that hold this one.
			r.drain()
		})
	}
}

// A REFUSAL BELONGS TO THE IDENTITY IT REFUSED. As replaces a writer's identity
// whole, so a writer derived from a refused one under an identity the rule
// admits writes — rather than refusing every write in the name of an actor it
// no longer is.
func TestAWriterDerivedFromARefusedOneWritesAsItsOwnIdentity(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")

	refused := r.writer.As("ana", tracker.AuthorOperator,
		tracker.Provenance{OperatorID: "ana"})
	done := tracker.StatusDone
	if _, err := refused.UpdateTask(t.Context(), "op-bare", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus,
		nil); err == nil {
		t.Fatal("an operator under a bare name wrote, so this case asserts nothing")
	}
	admitted := refused.As(tracker.OperatorActor("ana"), tracker.AuthorOperator,
		tracker.Provenance{OperatorID: "ana"})
	if _, err := admitted.UpdateTask(t.Context(), "op-spelled", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus,
		nil); err != nil {
		t.Errorf("a writer derived as operator:ana was refused for the identity "+
			"it replaced: %v", err)
	}
}

// ONLY A SEAT'S OWN WRITER WRITES ITS INBOX AND ITS PINS, and a writer that
// holds no seat writes nobody's.
//
// The name alone does not decide it. An engine writer is named by its node,
// and a node's id can spell a seat's handle; an operator is named
// `operator:` and its token's name, which a record could be filed under. Asked
// of the name alone, the first would own that seat's inbox and pins and the
// second a person record that belongs to no person.
//
// NOT A CONFLICT: a conflict tells a caller to read again and decide, and no
// re-read gives a writer an authority it does not hold — the tool layer tells a
// model somebody else is editing the item when it sees one.
func TestOnlyASeatsOwnWriterWritesItsInboxAndItsPins(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("The thing")

	node := r.writer.As("ana", tracker.AuthorSystem, tracker.Provenance{})
	operator := asOperator(r, "ana")
	for name, tc := range map[string]struct {
		writer *tracker.Writer
		handle string
		// says is what the refusal tells its caller: a writer with no
		// seat asking for its own is told it has none, which is what the
		// operator's assistant reads when it marks "its" inbox.
		says string
	}{
		"the engine, named like the seat": {node, "ana", "cannot write ana's"},
		"an operator, on its own name": {operator, tracker.OperatorActor("ana"),
			"of its own"},
		"an operator, on the seat's": {operator, "ana", "cannot write ana's"},
	} {
		t.Run(name, func(t *testing.T) {
			for what, write := range map[string]func() error{
				"inbox": func() error {
					_, err := tc.writer.WriteInbox(t.Context(), "op-inbox-"+name,
						tc.handle, nil, []tracker.InboxEntry{{RecordID: "rec-1",
							Position: 9}}, nil, nil, tracker.Position{})
					return err
				},
				"pins": func() error {
					_, err := tc.writer.WritePins(t.Context(), "op-pins-"+name,
						tc.handle, []string{"v-1"}, nil)
					return err
				},
			} {
				err := write()
				switch {
				case err == nil:
					t.Errorf("it wrote %s's %s", tc.handle, what)
				case !errors.Is(err, tracker.ErrNotYours):
					t.Errorf("writing %s's %s was refused with %v, want "+
						"ErrNotYours", tc.handle, what, err)
				case errors.Is(err, statelog.ErrConflict):
					t.Errorf("writing %s's %s was refused as a conflict, which "+
						"tells a caller to read again: %v", tc.handle, what, err)
				case !strings.Contains(err.Error(), tc.says):
					t.Errorf("writing %s's %s was refused as %q, which does not "+
						"say %q", tc.handle, what, err, tc.says)
				}
			}
		})
	}

	// AND IT IS NOT A LIST OF ITS OWN EITHER: the engine named like the
	// seat, with no authority over anybody's list, is refused hers. Asked
	// with every earlier write applied, so the only refusal left is that.
	r.drain()
	if _, err := node.WritePriorities(t.Context(), "op-prio-node", "ana",
		[]string{task.ID}, tracker.PersonAuthority{}); err == nil {
		t.Error("the engine, named like the seat ana, set ana's priorities as " +
			"though the list were its own")
	}

	r.drain()
	for _, handle := range []string{"ana", tracker.OperatorActor("ana")} {
		if got := r.person(handle); got.Held {
			t.Errorf("a refused write left a person record for %s: %+v", handle, got)
		}
	}

	// AND THE SEAT'S OWN WRITER WRITES HERS, or the guard would be refusing
	// everything.
	if _, err := r.writer.As("ana", tracker.AuthorAgent, tracker.Provenance{}).
		WritePins(t.Context(), "op-own-pins", "ana", []string{"v-1"}, nil); err != nil {
		t.Fatalf("ana's own pins: %v", err)
	}
	r.drain()
	if got := r.person("ana"); len(got.PinnedViews) != 1 {
		t.Errorf("ana's pins are %v, want the one she wrote", got.PinnedViews)
	}
}
