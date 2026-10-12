package pages_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/pages"
)

// THE DASHBOARD BOUNDS A PAGE'S TEXT WHERE THE ENGINE DOES.
//
// Every form that names a page — the new-page sheet, the composer's "save as
// a page" — and the editor's save note refuse a value past the engine's cap
// before the press, saying by how much, because the engine refuses such a
// value rather than cutting it. The fields used to carry `maxLength={200}`,
// which the browser applies by cutting a paste silently, in characters,
// short of the engine's 256 bytes. That only works while the two figures
// agree: above the engine's, a title the engine refuses gets through; below
// it, one the engine would take is refused.
func TestTheDashboardBoundsPageTextAtTheEnginesCaps(t *testing.T) {
	t.Parallel()
	for name, engine := range map[string]int{
		"PAGE_TITLE_MAX_BYTES":   pages.MaxTitle,
		"PAGE_MESSAGE_MAX_BYTES": pages.MaxMessage,
	} {
		raw, err := clientsource.Scalar(clientsource.Tree(t), name)
		if err != nil {
			t.Fatal(err)
		}
		client, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("%s is %q, which is not an integer: %v", name, raw, err)
		}
		if client != engine {
			t.Errorf("the dashboard bounds %s at %d bytes and the engine at %d — "+
				"change it in contract/pages.ts to the engine's figure", name, client, engine)
		}
	}
}
