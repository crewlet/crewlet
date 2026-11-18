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
// Once an object's deletion marker lands ahead of a record, the deletion gate
// drops it on every node ([statelog.ReasonDeleted]) and nothing will ever write
// it, so [WriterClass] classes it not found, and neither a tool failure nor a
// sentence naming what it stopped ([writeTold]) carries the framework's text
// or "Try again". The walk covers the tracker's, the knowledge base's, the
// artifacts' and the datastores' failures; chat keeps no deletion marker.
//
// Mutation: drop the deletion arm from WriterClass and every row goes red; ask
// Condition before WriterSentence in toldOf and the walk's sentence does.
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
