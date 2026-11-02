package assistantdomain_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// EVERY SEALED VALUE A CONVERSATION HOLDS IS COUNTED BY ITS KEY, none of them
// opened: the count is the assistant's line in a keyring rotation's "is
// anything still under the old key", so a key is never retired while a live
// conversation is still sealed under it. Mutation: drop a table from the
// counted columns and the messages or the proposal go uncounted.
func TestSealedValuesAreCountedByTheirKey(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ada := uuid.New()
	c := person(ada, "ada")
	conversation := n.conversation(c, "Payroll")
	turn := n.openTurn(c, conversation)
	n.say(c, turn, 1, wire.RoleUser, "What did we spend?")
	n.propose(c, turn)

	got, err := n.reader.SealedKeys(t.Context())
	if err != nil {
		t.Fatalf("SealedKeys: %v", err)
	}
	// The title, the turn's page title, the message's text, and the
	// proposal's body and card: five, each counted once.
	if got.Values["k1"] != 5 || len(got.Values) != 1 {
		t.Fatalf("the sealed values count %v, want every one under k1", got.Values)
	}
	if got.Outside("k1") != 0 || got.Outside("k2") != got.Values["k1"] {
		t.Errorf("outside k1 is %d and outside k2 is %d of %d", got.Outside("k1"),
			got.Outside("k2"), got.Values["k1"])
	}
}

// EVERY SEALED COLUMN THE SCHEMA DECLARES IS ONE THE COUNT READS: a column
// named `*_sealed` on an assistant table is a keyring envelope, and one the
// count missed would let a rotation retire a key that still opens it.
// Mutation: add a sealed column to a migration without counting it, or drop
// one from the counted list, and the sets differ.
func TestEverySealedColumnIsCounted(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "store", "schema", "replicated", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no replicated migrations found: %v", err)
	}
	table := regexp.MustCompile(`(?s)CREATE TABLE (assistant_\w+) \((.*?)\n\);`)
	added := regexp.MustCompile(`ALTER TABLE (assistant_\w+) ADD COLUMN (\w+_sealed)`)
	column := regexp.MustCompile(`(?m)^\s*(\w+_sealed)\s`)
	var schema []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range table.FindAllStringSubmatch(string(raw), -1) {
			for _, col := range column.FindAllStringSubmatch(m[2], -1) {
				schema = append(schema, m[1]+"."+col[1])
			}
		}
		for _, m := range added.FindAllStringSubmatch(string(raw), -1) {
			schema = append(schema, m[1]+"."+m[2])
		}
	}
	counted := assistantdomain.SealedColumns()
	slices.Sort(schema)
	slices.Sort(counted)
	if len(schema) == 0 || !slices.Equal(schema, counted) {
		t.Errorf("the schema's sealed columns are %v\nthe count reads %v", schema, counted)
	}
}
