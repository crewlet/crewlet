package builtin

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/mcp"
)

// A VERB THE ASSISTANT READS IS A TOOL THAT ONLY READS, AND ONE IT CARRIES ONLY
// ON APPROVAL IS ONE THAT WRITES (ADR-0052).
//
// The authority table says which verbs a read grant may take with nothing for
// the person to approve; the tool's own annotations say whether calling it
// changes anything. The two are stated in different packages, and a row
// marked as the assistant's to read on a verb whose tool writes would let a
// turn's read loop make that write with no approval at all. So every
// assist-read row named after a tool must be a tool this package annotates as
// read-only, every dotted one must name itself a read ([readSuffixes]), and every
// assist-approve row named after a tool must be one that is not: an approval
// on a read is a step the person was asked to take for nothing, and reads as
// a row somebody classified without looking.
//
// Mutation: mark `create_work_item` assist-read in internal/authz and this
// names it.
// readSuffixes are how a dotted verb names itself a read: reading, listing,
// exporting and a usage reading change nothing.
var readSuffixes = []string{".read", ".list", ".export", ".usage"}

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
