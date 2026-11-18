package builtin

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/mcp"
)

// readSuffixes are how a dotted verb names itself a read: reading, listing,
// exporting and a usage reading change nothing.
var readSuffixes = []string{".read", ".list", ".export", ".usage"}

// A VERB THE ASSISTANT READS IS A TOOL THAT ONLY READS, AND ONE IT CARRIES ONLY
// ON APPROVAL IS ONE THAT WRITES (ADR-0052).
//
// The authority table (internal/authz) and the tools' annotations are stated
// apart, and an assist-read row on a tool that writes would let a turn's read
// loop write with no approval. So an assist-read row named after a tool is
// annotated read-only, a dotted one names itself a read ([readSuffixes]), and
// an assist-approve row's tool is not read-only: approving a read is a step
// taken for nothing.
//
// Mutation: mark `create_work_item` assist-read in internal/authz and this
// names it.
func TestAnAssistReadVerbIsAToolThatOnlyReads(t *testing.T) {
	t.Parallel()
	reads := 0
	for _, a := range authz.Actions() {
		class, _ := authz.AssistOf(a)
		name := string(a)
		dotted := strings.Contains(name, ".")
		switch {
		case class == authz.AssistRead && dotted:
			reads++
			if !slices.ContainsFunc(readSuffixes, func(suffix string) bool {
				return strings.HasSuffix(name, suffix)
			}) {
				t.Errorf("%s is the assistant's to read and is a dotted verb that "+
					"does not name itself a read (%v)", a, readSuffixes)
			}
		case class == authz.AssistRead:
			reads++
			if got := annotationsFor(name); got.ReadOnly != mcp.Yes {
				t.Errorf("%s is the assistant's to read with nothing to approve, "+
					"and its tool is annotated %+v, not read-only", a, got)
			}
		case class == authz.AssistApprove && !dotted:
			if got := annotationsFor(name); got.ReadOnly == mcp.Yes {
				t.Errorf("%s is carried only on approval, and its tool is "+
					"read-only: nothing it does needs one", a)
			}
		}
	}
	if reads == 0 {
		t.Fatal("no verb is the assistant's to read, so this certifies nothing")
	}
}
