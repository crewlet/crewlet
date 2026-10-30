package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ASSISTANT'S FILE STORE TELLS EVERY DOMAIN REFUSAL AS THE ORCHESTRATOR'S:
// somebody else's file, a purged conversation and an erased owner are all not
// found (never forbidden, ADR-0053), a full conversation is invalid and still
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

// A FILE A CONDITION OF THE NODE KEPT OUT IS TOLD IN A MODEL'S WORDS for it:
// the company's file storage full, a store that did not take the bytes and a
// log that refused the file's record each reach the orchestrator as
// [assistant.Unkept], saying what kept the file out without the error's own
// text, which stays beneath for the route that tells a person; a fault is
// returned as it is. Mutation: return every failure as it is and a deep run's
// note says only that the file could not be kept; word a fault as a condition
// and the last row goes red.
func TestAFileAConditionKeptOutIsToldInAModelsWords(t *testing.T) {
	t.Parallel()
	for name, cause := range map[string]error{
		"a full store": fmt.Errorf("assistantdomain: upload file x: %w", objstore.ErrFull),
		"a store that did not answer": fmt.Errorf("assistantdomain: upload file x: %w: %w",
			objstore.ErrUnanswered, errors.New("nats: timeout")),
		"a refused record": &statelog.Unavailable{Reason: statelog.ReasonLogFull,
			Detail: "raise stream.assistant_log_max_bytes"},
	} {
		said, _ := builtin.Condition(cause)
		var condition *assistant.Unkept
		got := unkept(cause)
		if !errors.As(got, &condition) || condition.Said != said || said == "" ||
			!errors.Is(got, cause) {
			t.Errorf("%s reached the orchestrator as %v, want it told as %q with the "+
				"error beneath", name, got, said)
			continue
		}
		if strings.Contains(condition.Said, "nats") || strings.Contains(condition.Said, "objstore") ||
			strings.Contains(condition.Said, "assistant_log_max_bytes") {
			t.Errorf("%s is told in its error's own text: %q", name, condition.Said)
		}
	}
	fault := errors.New("open /var/lib/crewlet/objects: disk I/O error")
	var condition *assistant.Unkept
	if got := unkept(fault); errors.As(got, &condition) || !errors.Is(got, fault) {
		t.Errorf("a fault reached the orchestrator as %v, want it as it is", got)
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
