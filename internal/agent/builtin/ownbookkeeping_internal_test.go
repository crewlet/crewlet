package builtin

import (
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
)

// A TOOL THAT KEEPS A PERSON'S OWN BOOKKEEPING TAKES NO NAME. Its every call is
// audited apart from the company's activity (internal/api/operator), which is
// right only while the call can act for nobody but its caller: a tool on the
// list that took a handle, or named its record by a personal flag, could write
// a colleague's record under a type the company's feed never shows.
func TestOwnBookkeepingActsForItsCallerAlone(t *testing.T) {
	t.Parallel()
	if len(ownBookkeeping) == 0 {
		t.Fatal("no tool is declared a person's own bookkeeping")
	}
	for tool := range ownBookkeeping {
		about, ok := subjectOf[tool]
		if !ok {
			t.Errorf("%s keeps a person's own bookkeeping and has no subject row", tool)
			continue
		}
		if about.kind != authz.KindPerson || about.owner != "" || about.personal != "" ||
			about.inTool {
			t.Errorf("%s keeps a person's own bookkeeping but is about %+v, "+
				"not the caller's own record by no name", tool, about)
		}
	}
}
