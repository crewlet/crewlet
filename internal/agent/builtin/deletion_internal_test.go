package builtin

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A WRITE ABOUT WHAT A DELETION KEEPS DELETED IS NOT FOUND, IN WORDS THAT DO NOT
// SEND A MODEL BACK TO TRY AGAIN.
//
// The deletion gate drops a record on every node once its object's deletion
// marker landed ahead of it, and the framework states that as one of its
// conditions ([statelog.Unavailable], [statelog.ReasonDeleted]). [WriterClass]
// did not class it, so every tool whose failure falls back to that table
// reached [Condition] and told a model "update_work_item did not land
// (statelog: unavailable (deleted): the record at 3:4 was durable and applied
// nowhere — the broker holds the operation id for 2m0s)", and the artifacts'
// and the datastores' tools added "Try again shortly" — about something
// nothing will ever write again, on any node. The same refusal read inside a
// sentence that names what it stopped ([writeTold]) carried the framework's
// whole text too.
//
// EVERY TOOL FAILURE A DELETION CAN REACH: the tracker's, the knowledge base's,
// the artifacts' and the datastores'. Chat keeps no deletion marker — its gates
// drop an evicted writer's records alone — so no chat write meets one.
//
// Mutation: drop the deletion arm from WriterClass and every row goes red; ask
// Condition before WriterSentence in toldOf again and the walk's sentence does.
func TestAWriteAboutWhatADeletionKeepsDeletedIsNotFound(t *testing.T) {
	t.Parallel()
	deleted := &statelog.Unavailable{Reason: statelog.ReasonDeleted,
		Detail: "the record at 3:4 was durable and applied nowhere — the broker " +
			"holds the operation id for 2m0s"}
	leaks := func(text string) string {
		for _, p := range []string{"statelog", "crewlet.", "unavailable", "Try again",
			"try again", "did not land"} {
			if strings.Contains(text, p) {
				return p
			}
		}
		return ""
	}
	if class, ok := WriterClass(deleted); !ok || class != tools.RefusalNotFound {
		t.Errorf("WriterClass(deletion) = %q, %v, want %q", class, ok, tools.RefusalNotFound)
	}
	reached := 0
	for _, domain := range slices.Sorted(maps.Keys(toolFailures)) {
		if domain == (chatlet.Domain{}).Name() {
			continue
		}
		for _, tool := range slices.Sorted(maps.Keys(toolFailures[domain])) {
			reached++
			got := toolFailures[domain][tool](deleted)
			if class := tools.RefusalOf(got); class != tools.RefusalNotFound {
				t.Errorf("%s's %s classes a deletion %q, want %q: %s", domain, tool, class,
					tools.RefusalNotFound, got.Output)
			}
			if p := leaks(got.Output); p != "" {
				t.Errorf("%s's %s tells a deletion with %q: %s", domain, tool, p, got.Output)
			}
		}
	}
	if reached < 4 {
		t.Fatalf("the walk reached %d tool failures; it is not reading the table", reached)
	}
	said := writeTold(context.Background(), Actor{}, worklet.UpdateWorkItemTool, deleted)
	if said != statelog.Deleted {
		t.Errorf("a walk stopped by a deletion tells it %q, want %q", said, statelog.Deleted)
	}
}
