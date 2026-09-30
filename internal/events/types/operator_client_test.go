package types

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/events"
)

// THE AUDIT LOG READS EVERY RUNTIME AUDIT TYPE, by the source this package
// stamps on them.
//
// The dashboard narrows the event log to `source=operator` and labels each row
// by its type, so both are copies of what this package owns: a source spelled
// differently there reads an empty runtime audit, and a record added here that
// the screen does not know arrives as a row labelled with nothing.
func TestTheAuditLogReadsEveryRuntimeAuditType(t *testing.T) {
	t.Parallel()
	source, err := clientsource.Scalar(clientsource.Tree(t), "OPERATOR_SOURCE")
	if err != nil {
		t.Fatal(err)
	}
	if source != OperatorSource {
		t.Errorf("the dashboard narrows the runtime audit to source %q; the engine stamps %q",
			source, OperatorSource)
	}
	body, err := clientsource.Literal(clientsource.Tree(t), "RUNTIME_AUDIT_TYPES")
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(slices.Values(clientsource.Strings(body)))
	want := slices.Sorted(slices.Values(RuntimeAuditTypes))
	if len(got) == 0 || !slices.Equal(got, want) {
		t.Errorf("the dashboard draws the runtime audit types %v; the engine writes %v", got, want)
	}
	registered := events.RegisteredTypes()
	for _, typ := range RuntimeAuditTypes {
		if !slices.Contains(registered, typ) {
			t.Errorf("runtime audit type %q is not a registered event", typ)
		}
	}
}
