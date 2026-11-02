package builtin

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A TRACKER OPERATION NAMES ITS OBJECT IN ONE STEP OF ITS ID, IN EVERY ARM,
// AND THE VERB STAYS READABLE BESIDE IT.
//
// In the grammar a dot begins a step and a name is bounded, and an object is
// whatever the call wrote about: a task's id, but also a file's path, which
// may hold dots, spaces and a kilobyte of anything. Written into the name as
// itself, `file-ENG/docs/a.md` read as the name `file-ENG/docs/a` with a step
// `md`, and the statelog's own rule then escaped the whole name — so a reader
// of the ledger could no longer tell a file write from any other.
//
// Mutation: name the operation from the bare object, and every arm's name
// for a path is one escaped segment with no verb in it.
func TestATrackerOperationNamesItsObjectInOneStep(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC)
	key := statelog.NewOpID(now, "")
	since, _ := statelog.OpMintedAt(key)
	operation := statelog.NewOpID(now, "write_file-0123456789abcdef")
	arms := map[string]Actor{
		"operator":  {Handle: "ops", Kind: worklet.AuthorOperator, Operation: operation},
		"request":   {Handle: "ops", Kind: worklet.AuthorOperator, WorkKey: key, WorkSince: since},
		"turn":      seatInATurn(),
		"no caller": {Handle: "ops", Kind: worklet.AuthorOperator},
	}
	args := map[string]any{"path": "docs/a.md"}
	for arm, actor := range arms {
		for _, object := range []string{"ENG/docs/a.md", "a.b c",
			"ENG/" + strings.Repeat("d", 1020)} {
			id := opIDFor(actor, "write_file", "file", object, args)
			segment := id[len(key)+1:]
			if arm == "operator" {
				segment = strings.TrimPrefix(id, operation+".")
			}
			if strings.Contains(segment, ".") || !strings.HasPrefix(segment, "file-") {
				t.Errorf("%s: the write on %.40q is named %q — want one segment "+
					"that still says it is a file write", arm, object, segment)
			}
			if arm != "operator" {
				if err := statelog.CheckCallerOpID(id); err != nil {
					t.Errorf("%s: the write on %.40q is under an id the engine "+
						"refuses back: %v", arm, object, err)
				}
			}
		}
		id := opIDFor(actor, "update_work_item", "update", "ENG-1", args)
		want := id[:len(key)] + ".update-ENG-1"
		if arm == "operator" {
			want = operation + ".update-ENG-1"
		}
		if id != want {
			t.Errorf("%s: the write on ENG-1 is under %q, want an id named "+
				"`update-ENG-1` as it always was", arm, id)
		}
	}
}

// AN OPERATOR'S OPERATION NAMES ITS CALL IN ONE READABLE STEP — the tool and
// the arguments' digest, joined by a dash rather than the dot that begins a
// step, which had the whole name escaped and the tool lost from the ledger.
//
// Mutation: join the two with a dot, and the name no longer says which tool
// it was minted for.
func TestAnOperatorsOperationNamesItsToolReadably(t *testing.T) {
	t.Parallel()
	name := callName("update_work_item", map[string]any{"item": "ENG-1"})
	id := statelog.NewOpID(time.Now(), name)
	if segment := id[37:]; !strings.HasPrefix(segment, "update_work_item-") ||
		len(segment) != len("update_work_item-")+callDigestLen {
		t.Errorf("the operator's operation is named %q, want the tool, a dash "+
			"and %d hex digits", segment, callDigestLen)
	}
	if !statelog.OpNamed(id, name) {
		t.Errorf("the operation %q is not named for its call %q", id, name)
	}
}
