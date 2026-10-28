package code_test

import (
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/code"
)

// THE DASHBOARD KNOWS EVERY STATE A REPOSITORY CAN BE IN, in the engine's
// order, and no other.
//
// The Code workspace's listing offers `REPO_STATUSES` (`contract/code.ts`) as
// the `status` it asks for and marks each row by it: a state the engine grew
// that the list lacks is one nobody can filter to and one a row draws as its
// raw word, and a state the list keeps that the engine dropped is a choice
// every listing refuses `bad_params`. Read from the client's own source, both
// ways, in order.
//
// Mutation: drop a state from either side, or reorder the dashboard's, and
// this fails.
func TestTheDashboardKnowsEveryRepositoryState(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "REPO_STATUSES")
	if err != nil {
		t.Fatal(err)
	}
	got := clientsource.Strings(body)
	if len(got) == 0 {
		t.Fatal("REPO_STATUSES reads as empty, so this gate certifies nothing")
	}
	want := make([]string, 0, len(code.RepoStatuses()))
	for _, s := range code.RepoStatuses() {
		want = append(want, string(s))
	}
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's REPO_STATUSES is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD KNOWS EVERY OUTCOME OF A COMMAND, in the engine's order, and
// no other.
//
// A push read in brief counts the commands it leaves out by what became of
// each (`PushRow.Omitted`), keyed by the outcome's word, and the Code
// workspace says each count by `COMMAND_OUTCOMES` (`contract/code.ts`) — the
// words its rows say of each ref. An outcome the engine grew that the list
// lacks is a count said in the engine's raw key; one the list keeps that the
// engine dropped is a word no push is ever counted by. Read from the client's
// own source and from CommandCounts' JSON keys, both ways, in order.
//
// Mutation: drop an outcome from either side, or reorder the dashboard's,
// and this fails.
func TestTheDashboardKnowsEveryOutcomeOfACommand(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "COMMAND_OUTCOMES")
	if err != nil {
		t.Fatal(err)
	}
	got := clientsource.Strings(body)
	if len(got) == 0 {
		t.Fatal("COMMAND_OUTCOMES reads as empty, so this gate certifies nothing")
	}
	counts := reflect.TypeFor[code.CommandCounts]()
	want := make([]string, 0, counts.NumField())
	for i := range counts.NumField() {
		key, _, _ := strings.Cut(counts.Field(i).Tag.Get("json"), ",")
		want = append(want, key)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's COMMAND_OUTCOMES is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD NAMES A REPOSITORY BY THE ENGINE'S GRAMMAR, name for name.
//
// The Code workspace holds the repository an address names (`repo=` on its
// Pushes section) to `REPO_NAME` and `REPO_NAME_MAX_BYTES` (`contract/code.ts`)
// before it asks anything about it, so a name no repository can have is said
// where the reader is, as one, rather than sent to the engine and answered as
// its refusal of the request. Held by BEHAVIOUR rather than by spelling: the
// pattern is compiled here and a corpus run through it and the length bound,
// as `lib/code.ts`'s isRepoName applies them, and through [code.ValidName], the
// one reading of a name every read and every push takes — a project's one
// label and a seat's scratch repository's `<handle>.<slug>` alike — so a change
// to either side that answers any name below differently fails, and a pattern
// that compiles to nothing fails on the corpus's controls, which the engine
// admits.
//
// Mutation: raise REPO_NAME_MAX_BYTES to 64, let a label begin with a hyphen,
// take an underscore or a second dot, or admit the slug `git`, and a name below
// fails.
func TestTheDashboardNamesARepositoryByTheEnginesGrammar(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	source, err := clientsource.Scalar(tree, "REPO_NAME")
	if err != nil {
		t.Fatal(err)
	}
	pattern, err := regexp.Compile(source)
	if err != nil {
		t.Fatalf("REPO_NAME does not compile: %v", err)
	}
	bound, err := clientsource.Scalar(tree, "REPO_NAME_MAX_BYTES")
	if err != nil {
		t.Fatal(err)
	}
	maxBytes, err := strconv.Atoi(bound)
	if err != nil {
		t.Fatalf("REPO_NAME_MAX_BYTES is %q, no count of bytes: %v", bound, err)
	}
	// A SCRATCH NAME AT THE BOUND AND PAST IT: two labels and their dot
	// together, which the pattern cannot count and the bound does.
	handle := strings.Repeat("h", 30)
	corpus := []string{
		"web", "a", "0", "9lives", "nimbus-web", "a-b-c", "a--b",
		strings.Repeat("a", 62), strings.Repeat("a", 63), strings.Repeat("a", 64),
		"a" + strings.Repeat("-", 61) + "b", "a" + strings.Repeat("-", 62) + "b",
		"", "-", "-web", "web-", "Web", "wEb", "WEB", "web_app", "web app",
		"wéb", "web/app", "..", "w@b", " web", "web\n",
		// THE SCRATCH FORM: <handle>.<slug>, one dot, each side a label, the
		// slug never `git` — and every way to be nearly one.
		"web.app", "dev.x", "a.b", "dev-1.my-test", "web.git", "dev.git", "dev.gitx", "dev.git-1",
		"dev.gi", "dev.gi-t", "dev.gix", "dev.g", "dev.g-x", "dev.gamma", "dev.gu", "dev.-x", "dev.x-",
		"-dev.x", "dev-.x", "a.b.c", ".web", "web.", "web..app", "Dev.x", "dev.X", "dev_1.x", "dev.x y",
		handle + "." + strings.Repeat("s", 32), handle + "." + strings.Repeat("s", 33),
	}
	admitted, refused := 0, 0
	for _, name := range corpus {
		engine := code.ValidName(name)
		if engine {
			admitted++
		} else {
			refused++
		}
		if got := len(name) <= maxBytes && pattern.MatchString(name); got != engine {
			t.Errorf("a repository named %q: the dashboard's REPO_NAME and REPO_NAME_MAX_BYTES answer "+
				"%v, code.ValidName %v", name, got, engine)
		}
	}
	if admitted == 0 || refused == 0 {
		t.Fatalf("the engine admits %d and refuses %d names of the corpus, so it certifies only one "+
			"answer", admitted, refused)
	}
}
