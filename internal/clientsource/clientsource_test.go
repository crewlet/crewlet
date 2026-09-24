package clientsource_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
)

// The pattern the event-category gate uses, verbatim, because what is under
// test is how many of these a tree may hold rather than how one is written.
const pattern = `(?s)const CATEGORIES = \[(.*?)\] as const`

// tree writes a dashboard-shaped directory: each entry is a path under it and
// the source at that path.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, source := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// ONE DECLARATION IS THE ANSWER, and it is found wherever it lives.
//
// Keyed on the declaration rather than on a path, so a screen moving is
// invisible to the gate — which is the whole reason this walks the tree
// instead of reading a file somebody named.
func TestOneDeclarationAnywhereInTheTreeIsTheAnswer(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"routes/activity/Activity.tsx": "const CATEGORIES = [\"task\", \"system\"] as const;\n",
		"lib/range.ts":                 "export const SPANS = [1, 2];\n",
		// A SUITE QUOTING THE CONSTANT IS NOT A SECOND COPY, which is
		// what the `.test.` exclusion is for: a case asserting the
		// shape of the list would otherwise fail every gate.
		"routes/activity/Activity.test.tsx": "const CATEGORIES = [\"task\"] as const;\n",
	})
	body, err := clientsource.Declaration(root, pattern)
	if err != nil {
		t.Fatalf("Declaration: %v", err)
	}
	if got := clientsource.Strings(body); len(got) != 2 || got[0] != "task" {
		t.Errorf("strings = %q, want the module's own two", got)
	}
}

// TWO DECLARATIONS IN ONE FILE ARE TWO COPIES.
//
// The walk matched once per FILE and appended one entry for each, so `found`
// counted files and a second declaration beside the first — a `const
// CATEGORIES = [...] as const` at block scope inside a component, which is
// legal TypeScript — passed the gate unseen. The gate then validated the
// module-scope list while the screen rendered the other one, which is exactly
// the silent drift this package exists to report: chips naming a category the
// engine never assigns filter the log down to nothing and read as a quiet
// engine.
func TestASecondDeclarationInTheSameFileIsReported(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"routes/activity/Activity.tsx": "const CATEGORIES = [\"task\", \"system\"] as const;\n" +
			"export function Activity() {\n" +
			"  const CATEGORIES = [\"task\", \"chore\"] as const;\n" +
			"  return CATEGORIES;\n" +
			"}\n",
	})
	_, err := clientsource.Declaration(root, pattern)
	if err == nil {
		t.Fatal("two declarations in one file passed the gate")
	}
	// NAMING HOW MANY, because the remedy differs: none means the gate
	// certifies nothing, and more than one means a copy has to go.
	if !strings.Contains(err.Error(), "2 declarations") {
		t.Errorf("err = %v, want it to name both declarations", err)
	}
}

// AND TWO FILES ARE TOO — the failure that was always reported, kept here so
// the count is over declarations in both arrangements rather than over one.
func TestASecondDeclarationInAnotherFileIsReported(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"routes/activity/Activity.tsx": "const CATEGORIES = [\"task\"] as const;\n",
		"routes/inbox/Inbox.tsx":       "const CATEGORIES = [\"system\"] as const;\n",
	})
	if _, err := clientsource.Declaration(root, pattern); err == nil {
		t.Fatal("two files declaring it passed the gate")
	}
}

// A NESTED CHECKOUT'S COPY IS NOT A SECOND DECLARATION.
//
// A worktree or a clone inside the tree carries every file here at some
// other commit, so read as this tree's it would put the constant in two
// places and fail a gate on a tree holding one — or, once the constant had
// been deleted here, pass it on the stale copy. Which directories are this
// tree's is internal/sourcetree's rule, and this walk follows it.
func TestANestedCheckoutsCopyIsNotASecondDeclaration(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"routes/activity/Activity.tsx":         "const CATEGORIES = [\"task\"] as const;\n",
		"wt/.git":                              "gitdir: /elsewhere/.git/worktrees/wt\n",
		"wt/dashboard/src/routes/Activity.tsx": "const CATEGORIES = [\"stale\"] as const;\n",
	})
	body, err := clientsource.Declaration(root, pattern)
	if err != nil {
		t.Fatalf("Declaration: %v", err)
	}
	if got := clientsource.Strings(body); len(got) != 1 || got[0] != "task" {
		t.Errorf("strings = %q, want this tree's one", got)
	}
}

// NOTHING DECLARING IT IS A GATE CERTIFYING NOTHING, and therefore an error
// rather than an empty body every caller would compare an empty list against.
func TestNothingDeclaringItIsAnError(t *testing.T) {
	t.Parallel()
	root := tree(t, map[string]string{
		"routes/activity/Activity.tsx": "const KINDS = [\"task\"] as const;\n",
	})
	if _, err := clientsource.Declaration(root, pattern); err == nil {
		t.Fatal("a tree declaring nothing passed the gate")
	}
}
