package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// THE ASSISTANT'S FILE STORE TELLS EVERY DOMAIN REFUSAL AS THE ORCHESTRATOR'S:
// somebody else's file, a purged conversation and an erased owner are all not
// found (never forbidden, ADR-0052), a full conversation is invalid and still
// a full conversation to the surface that answers it, and a refusal as written
// names what to change without the domain's prefix. Mutation: pass a purged
// conversation through untouched and the route answers it as a failure.
func TestAssistantFilesTellTheDomainsRefusalsAsTheOrchestrators(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err  error
		want []error
	}{
		"another person's": {fmt.Errorf("%w: file x", assistantdomain.ErrNotFound),
			[]error{assistant.ErrNotFound}},
		"purged":   {assistantdomain.ErrPurged, []error{assistant.ErrNotFound}},
		"erased":   {assistantdomain.ErrErased, []error{assistant.ErrNotFound}},
		"too many": {assistantdomain.ErrTooManyFiles, []error{assistant.ErrInvalid, assistantdomain.ErrTooManyFiles}},
		"invalid": {fmt.Errorf("%w: a file needs a name", assistantdomain.ErrInvalid),
			[]error{assistant.ErrInvalid}},
	} {
		got := fileRefusal(c.err)
		for _, want := range c.want {
			if !errors.Is(got, want) {
				t.Errorf("%s: %v is not %v", name, got, want)
			}
		}
	}
	if got := fileRefusal(fmt.Errorf("%w: a file needs a name", assistantdomain.ErrInvalid)); got.Error() != "assistant: invalid: a file needs a name" {
		t.Errorf("a refusal as written reads %q", got)
	}
}

// A NODE WITHOUT AN OBJECT STORE KEEPS NO FILE, and says so with a nil seam
// rather than a store whose every call fails: the orchestrator then offers no
// attachment and no deep work, and the routes are not mounted.
func TestAssistantFilesAreNilWithoutAnObjectStore(t *testing.T) {
	t.Parallel()
	if files := AssistantFiles(&Engine{}); files != nil {
		t.Errorf("an engine with no object store answered the file store %v", files)
	}
	if store := AssistantFileStore(&Engine{}); store != nil {
		t.Errorf("an engine with no object store answered the domain's file store %v", store)
	}
}
