package gitlet_test

import (
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DASHBOARD KNOWS EVERY STATE A REPOSITORY CAN BE IN, in the engine's
// order, and no other: the Gitlet listing filters and marks rows by
// `REPO_STATUSES` (`contract/code.ts`). A state it lacks cannot be filtered
// to and draws as its raw word; one the engine dropped is refused
// `bad_params`.
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
	want := make([]string, 0, len(gitlet.RepoStatuses()))
	for _, s := range gitlet.RepoStatuses() {
		want = append(want, string(s))
	}
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's REPO_STATUSES is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD OFFERS EVERY SEARCH MODE OF A REPOSITORY, in the engine's
// order, and no other: a repository's Settings tab sends `REPO_SEARCH_MODES`
// (`contract/code.ts`) as code_update_repo's `search`. A mode it lacks cannot
// be set; one the engine dropped is refused on every press.
//
// Mutation: drop a mode from either side, or reorder the dashboard's, and
// this fails.
func TestTheDashboardOffersEverySearchModeOfARepository(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "REPO_SEARCH_MODES")
	if err != nil {
		t.Fatal(err)
	}
	got := clientsource.Strings(body)
	if len(got) == 0 {
		t.Fatal("REPO_SEARCH_MODES reads as empty, so this gate certifies nothing")
	}
	want := make([]string, 0, len(gitlet.SearchModes()))
	for _, m := range gitlet.SearchModes() {
		want = append(want, string(m))
	}
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's REPO_SEARCH_MODES is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD OFFERS EVERY PATH CLASS A GATE EXEMPTS, in the engine's
// order, and no other: a protection rule's form offers `CODE_PATH_CLASSES`
// (`contract/code.ts`) as its approvals' and human paths' exemptions. A class
// it lacks cannot be chosen; one the engine dropped is refused by every
// protect call.
//
// Mutation: drop a class from either side, or reorder the dashboard's, and
// this fails.
func TestTheDashboardOffersEveryPathClassAGateExempts(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "CODE_PATH_CLASSES")
	if err != nil {
		t.Fatal(err)
	}
	got := clientsource.Strings(body)
	if len(got) == 0 {
		t.Fatal("CODE_PATH_CLASSES reads as empty, so this gate certifies nothing")
	}
	want := make([]string, 0, len(gitlet.PathClasses()))
	for _, c := range gitlet.PathClasses() {
		want = append(want, string(c))
	}
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's CODE_PATH_CLASSES is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD BOUNDS A LANDING AS THE ENGINE DOES, and starts a new one at
// the engine's defaults. A protection rule's form bounds queue depth and
// batch by `CODE_MAX_QUEUE_DEPTH` and `CODE_MAX_QUEUE_BATCH`
// (`contract/code.ts`): above the engine's offers values every protect call
// refuses, below hides values a rule may hold. The form sends only a value
// moved from `CODE_DEFAULT_QUEUE_DEPTH` and `CODE_DEFAULT_QUEUE_BATCH`, so a
// wrong default sends a landing nobody chose.
//
// Mutation: change any of the four on either side, and this fails.
func TestTheDashboardBoundsALandingAsTheEngineDoes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		want int
	}{
		{"CODE_MAX_QUEUE_DEPTH", gitlet.MaxQueueDepth},
		{"CODE_MAX_QUEUE_BATCH", gitlet.MaxQueueBatch},
		{"CODE_DEFAULT_QUEUE_DEPTH", config.CodeDefaultQueueDepth},
		{"CODE_DEFAULT_QUEUE_BATCH", config.CodeDefaultQueueBatch},
	} {
		got, err := clientsource.Scalar(clientsource.Tree(t), c.name)
		if err != nil {
			t.Fatal(err)
		}
		if want := strconv.Itoa(c.want); got != want {
			t.Errorf("the dashboard's %s is %s, want the engine's %s", c.name, got, want)
		}
	}
}

// THE DASHBOARD KNOWS EVERY OUTCOME OF A COMMAND, in the engine's order, and
// no other: a push read in brief counts its omitted commands by outcome
// (`PushRow.Omitted`, keyed by CommandCounts' JSON keys), and the workspace
// words each count by `COMMAND_OUTCOMES` (`contract/code.ts`). An outcome it
// lacks is said as the raw key; one the engine dropped is never counted.
//
// Mutation: drop an outcome from either side, or reorder the dashboard's, and
// this fails.
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
	counts := reflect.TypeFor[gitlet.CommandCounts]()
	want := make([]string, 0, counts.NumField())
	for i := range counts.NumField() {
		key, _, _ := strings.Cut(counts.Field(i).Tag.Get("json"), ",")
		want = append(want, key)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's COMMAND_OUTCOMES is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD NAMES A REPOSITORY BY THE ENGINE'S GRAMMAR, name for name:
// the workspace checks a `repo=` address against `REPO_NAME` and
// `REPO_NAME_MAX_BYTES` (`contract/code.ts`) before asking, so an impossible
// name is refused where the reader is. Held by behaviour, not spelling: a
// corpus runs through the compiled pattern and bound (as `lib/code.ts`'s
// isRepoName applies them) and [gitlet.ValidName], project labels and
// scratch `<handle>.<slug>` names alike; the controls the engine admits fail
// a pattern that matches nothing.
//
// Mutation: raise REPO_NAME_MAX_BYTES to 64, let a label begin with a hyphen,
// take an underscore or a second dot, or admit the slug `git`, and a name
// below fails.
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
		engine := gitlet.ValidName(name)
		if engine {
			admitted++
		} else {
			refused++
		}
		if got := len(name) <= maxBytes && pattern.MatchString(name); got != engine {
			t.Errorf("a repository named %q: the dashboard's REPO_NAME and REPO_NAME_MAX_BYTES answer "+
				"%v, gitlet.ValidName %v", name, got, engine)
		}
	}
	if admitted == 0 || refused == 0 {
		t.Fatalf("the engine admits %d and refuses %d names of the corpus, so it certifies only one "+
			"answer", admitted, refused)
	}
}

// theDashboardsList is the list of strings the dashboard's source declares
// under name, failed when it reads as empty: an empty list certifies nothing.
func theDashboardsList(t *testing.T, name string) []string {
	t.Helper()
	body, err := clientsource.Literal(clientsource.Tree(t), name)
	if err != nil {
		t.Fatal(err)
	}
	got := clientsource.Strings(body)
	if len(got) == 0 {
		t.Fatalf("%s reads as empty, so this gate certifies nothing", name)
	}
	return got
}

// spelledOut is a closed set of the engine's as the strings it sends.
func spelledOut[S ~string](set []S) []string {
	out := make([]string, 0, len(set))
	for _, s := range set {
		out = append(out, string(s))
	}
	return out
}

// THE DASHBOARD KNOWS EVERY STATE A CHANGE CAN BE IN, in the engine's order,
// and no other: Gitlet › Changes lists by and marks rows by `CHANGE_STATES`
// (`contract/code.ts`). A state it lacks cannot be listed and reads as its
// raw word; one the engine dropped is refused `bad_params`.
//
// Mutation: drop a state from either side, or reorder the dashboard's, and
// this fails.
func TestTheDashboardKnowsEveryStateOfAChange(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "CHANGE_STATES"), spelledOut(gitlet.ChangeStates())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's CHANGE_STATES is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD WORDS A CHANGE ITS SOURCE'S DELETION CLOSED: the merge box and
// a change's peek say its branch was deleted where its row's close reason is
// `SOURCE_DELETED` (`contract/code.ts`), the engine's [gitlet.SourceDeleted].
// A reason spelled apart from the engine's is drawn as its raw code.
//
// Mutation: respell either side, and this fails.
func TestTheDashboardKnowsWhyAPushClosedAChange(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Scalar(clientsource.Tree(t), "SOURCE_DELETED")
	if err != nil {
		t.Fatal(err)
	}
	if got != gitlet.SourceDeleted {
		t.Errorf("the dashboard's SOURCE_DELETED is %q, want the engine's %q", got, gitlet.SourceDeleted)
	}
}

// THE DASHBOARD DRAWS EVERY MOVE OF A CHANGE'S LIFECYCLE on its timeline, by
// `CHANGE_MOVE_KINDS` (`contract/code.ts`), in the engine's order: a kind it
// lacks is drawn in general words, one the engine dropped is a sentence nobody
// reads.
//
// Mutation: drop a kind from either side, or reorder the dashboard's, and this
// fails.
func TestTheDashboardDrawsEveryMoveOfAChange(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "CHANGE_MOVE_KINDS"), spelledOut(gitlet.ChangeMoveKinds())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's CHANGE_MOVE_KINDS is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD ASKS WHAT A CHANGE WAITS ON IN THE ENGINE'S WORDS, every one
// of them: Gitlet › Changes narrows its listing by `CHANGE_NEEDS`
// (`contract/code.ts`), and its Waiting on you box asks me, or person for a
// reader bound to no seat. A need it lacks cannot be asked for; one the
// engine dropped is refused `bad_params`.
//
// Mutation: drop a need from either side, or reorder the dashboard's, and
// this fails.
func TestTheDashboardKnowsEveryNeedOfAListingOfChanges(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "CHANGE_NEEDS"), spelledOut(gitlet.Needs())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's CHANGE_NEEDS is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD NARROWS A LISTING OF CHANGES TO A WINDOW IN THE ENGINE'S
// WORDS, every one of them: Gitlet › Changes narrows by `CHANGE_WINDOWS`
// (`contract/code.ts`). A window it lacks cannot be asked for; one the engine
// dropped is refused `bad_params`.
//
// Mutation: drop a window from either side, or reorder the dashboard's, and
// this fails.
func TestTheDashboardKnowsEveryWindowOfAListingOfChanges(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "CHANGE_WINDOWS"), spelledOut(gitlet.ChangeWindows())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's CHANGE_WINDOWS is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD ORDERS A LISTING OF CHANGES IN THE ENGINE'S WORDS (`CHANGE_SORTS`), both
// ways, in order.
// Mutation: drop a sort from either side, or reorder the dashboard's, and this fails.
func TestTheDashboardKnowsEverySortOfAListingOfChanges(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "CHANGE_SORTS"), spelledOut(gitlet.ChangeSorts())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's CHANGE_SORTS is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD NARROWS A LISTING OF CHANGES BY READINESS IN THE ENGINE'S WORDS (`READINESS_BUCKETS`).
// Mutation: drop a bucket from either side, or reorder the dashboard's, and this fails.
func TestTheDashboardKnowsEveryReadinessBucket(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "READINESS_BUCKETS"), spelledOut(gitlet.ReadinessBuckets())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's READINESS_BUCKETS is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD KNOWS EVERY PRECONDITION A CHANGE'S READINESS JUDGES, in the
// engine's order, and every state one stands in: a change row's meter and
// its readiness word each by `PRECONDITION_KINDS` and `PRECONDITION_STATES`
// (`contract/code.ts`), in [gitlet.PreconditionKinds]' order. A kind it lacks
// (the merge gate adds its own) draws as its raw word; one the engine dropped
// is words for nothing judged.
//
// Mutation: drop a kind or a state from either side, or reorder the
// dashboard's, and this fails.
func TestTheDashboardKnowsEveryPreconditionOfAChange(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "PRECONDITION_KINDS"), spelledOut(gitlet.PreconditionKinds())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's PRECONDITION_KINDS is %v, want the engine's, in order: %v", got, want)
	}
	got, want = theDashboardsList(t, "PRECONDITION_STATES"), spelledOut(gitlet.PreconditionStates())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's PRECONDITION_STATES is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD COUNTS EVERY PUSHER KIND: a repository's rail draws each
// pusher by its kind (`PUSHER_KINDS`, `contract/code.ts`), Gitlet by its mark.
// Mutation: drop a kind from either side, or reorder the dashboard's, and this fails.
func TestTheDashboardCountsEveryPusherKind(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "PUSHER_KINDS"), spelledOut(gitlet.PusherKinds())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's PUSHER_KINDS is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD NARROWS A LISTING OF REPOSITORIES TO EACH KIND THE ENGINE KNOWS: Gitlet ›
// Repositories offers `REPO_KINDS` (`contract/code.ts`) as its listing's `kind`.
// Mutation: drop a kind from either side, or reorder the dashboard's, and this fails.
func TestTheDashboardKnowsEveryKindOfRepository(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "REPO_KINDS"), spelledOut(gitlet.RepoKinds())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's REPO_KINDS is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD SORTS A LISTING OF REPOSITORIES AS THE ENGINE DOES: Gitlet › Repositories asks
// its listing's `sort` from `REPO_SORTS` (`contract/code.ts`).
// Mutation: drop an order from either side, or reorder the dashboard's, and this fails.
func TestTheDashboardSortsRepositoriesAsTheEngineDoes(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "REPO_SORTS"), spelledOut(gitlet.RepoSorts())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's REPO_SORTS is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD RAISES FINDINGS IN THE ENGINE'S WORDS, in its order: a change
// page's finding composer offers `FINDING_SEVERITIES` and `FINDING_CATEGORIES`
// (`contract/code.ts`), sent as code_review_change's `severity` and
// `category`. A value they lack cannot be given; one the engine dropped gets
// the whole review refused, every finding in it lost.
//
// Mutation: drop a value from either side, or reorder the dashboard's, and
// this fails.
func TestTheDashboardRaisesFindingsInTheEnginesWords(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "FINDING_SEVERITIES"), spelledOut(gitlet.Severities())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's FINDING_SEVERITIES is %v, want the engine's, in order: %v", got, want)
	}
	got, want = theDashboardsList(t, "FINDING_CATEGORIES"), spelledOut(gitlet.Categories())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's FINDING_CATEGORIES is %v, want the engine's, in order: %v", got, want)
	}
}

// EVERY CODE REASON HAS ENGLISH ON THE OTHER SIDE, in all three voices.
//
// A change's Woke tab lists the one reason of [gitlet.Reasons] each recipient
// heard of it under (code_notifications); a reason without a `CODE_PHRASES`
// (`contract/code.ts`) phrase renders as its snake_case value. The table is
// read from the client's own source, as the tracker's `PHRASES` gate reads
// its, both ways: a phrase for a reason nothing records is how a rename
// leaves a dead entry behind.
//
// Mutation: drop a reason's phrase, add one for a reason nothing records, or
// leave a voice empty, and this fails.
func TestEveryCodeReasonReadsAsEnglishOnTheClient(t *testing.T) {
	t.Parallel()
	table, err := clientsource.Literal(clientsource.Tree(t), "CODE_PHRASES")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clientsource.Keys(table)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Fatal("CODE_PHRASES reads as empty, so this gate certifies nothing")
	}
	want := spelledOut(gitlet.Reasons())
	for _, reason := range want {
		if !slices.Contains(keys, reason) {
			t.Errorf("the engine records %q and CODE_PHRASES has no phrase for it", reason)
			continue
		}
		row, err := clientsource.Property(table, reason)
		if err != nil {
			t.Errorf("%s: %v", reason, err)
			continue
		}
		for _, voice := range []string{"short", "about", "why"} {
			if said := clientsource.Field(row, voice); len(said) != 1 || strings.TrimSpace(said[0]) == "" {
				t.Errorf("CODE_PHRASES.%s says nothing in its %q voice (%q)", reason, voice, said)
			}
		}
	}
	for _, key := range keys {
		if !slices.Contains(want, key) {
			t.Errorf("CODE_PHRASES phrases %q, which the engine never records — a renamed reason "+
				"leaves exactly this behind", key)
		}
	}
}

// THE DASHBOARD KNOWS EVERY ALARM GITLET RAISES, and no other: Settings ›
// Gitlet shows the node alarms `CODE_ALARM_KINDS` (`contract/code.ts`) names.
// A kind it lacks is an alarm administrators never see; one the table dropped
// is a filter for nothing. Gitlet's kinds are statelog's `code_` block
// ("Gitlet's"), compared in `statelog.Kinds()`' sorted order so a kind added
// on one side is checked at one place on the other.
//
// Mutation: drop a kind from either side, or reorder the dashboard's, and
// this fails.
func TestTheDashboardKnowsEveryCodeAlarm(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "CODE_ALARM_KINDS")
	if err != nil {
		t.Fatal(err)
	}
	got := clientsource.Strings(body)
	if len(got) == 0 {
		t.Fatal("CODE_ALARM_KINDS reads as empty, so this gate certifies nothing")
	}
	var want []string
	for _, k := range statelog.Kinds() {
		if strings.HasPrefix(string(k), "code_") {
			want = append(want, string(k))
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's CODE_ALARM_KINDS is %v, want the alarm table's code kinds, in order: %v",
			got, want)
	}
}

// THE DASHBOARD BLAMES NO WIDER A RANGE THAN THE ENGINE ANSWERS, and no
// narrower: the Files tab pages a blame `CODE_MAX_BLAME_LINES`
// (`contract/code.ts`) lines at a time. Wider than [gitlet.MaxBlameLines] is
// refused on every blame; narrower costs needless round trips.
//
// Mutation: change the bound on either side, and this fails.
func TestTheDashboardBlamesNoWiderARangeThanTheEngineAnswers(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Scalar(clientsource.Tree(t), "CODE_MAX_BLAME_LINES")
	if err != nil {
		t.Fatal(err)
	}
	if want := strconv.Itoa(gitlet.MaxBlameLines); got != want {
		t.Errorf("the dashboard's CODE_MAX_BLAME_LINES is %s, want the engine's gitlet.MaxBlameLines, %s", got, want)
	}
}

// THE DASHBOARD READS A CHANGE'S CONVERSATION A PAGE OF THE ENGINE'S BOUND AT
// A TIME: `CHANGE_PART_PAGE` (`contract/code.ts`) pages every part, so it
// equals the comments', reviews' and findings' (standing ones too) bounds and
// stays within the reviewers'. Past a part's bound every page of it is
// refused; smaller costs needless round trips.
//
// Mutation: move CHANGE_PART_PAGE, or any one part's bound, and this fails.
func TestTheDashboardReadsAChangesConversationAtTheEnginesPage(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Scalar(clientsource.Tree(t), "CHANGE_PART_PAGE")
	if err != nil {
		t.Fatal(err)
	}
	page, err := strconv.Atoi(got)
	if err != nil {
		t.Fatalf("CHANGE_PART_PAGE is %q, no count of rows: %v", got, err)
	}
	for name, bound := range map[string]int{
		"MaxCommentsPerPage": gitlet.MaxCommentsPerPage,
		"MaxReviewsPerPage":  gitlet.MaxReviewsPerPage,
		"MaxFindingsPerPage": gitlet.MaxFindingsPerPage,
	} {
		if page != bound {
			t.Errorf("the dashboard reads a change's parts %d rows a page and the engine's "+
				"%s is %d: change CHANGE_PART_PAGE in contract/code.ts", page, name, bound)
		}
	}
	if page > gitlet.MaxReviewersPerPage {
		t.Errorf("the dashboard reads %d reviewers a page, past the engine's %d", page,
			gitlet.MaxReviewersPerPage)
	}
}

// THE DASHBOARD NAMES AN OBJECT AS THE ENGINE'S REPOSITORIES DO: a commit
// page's address is a whole id of [gitref.OIDHexLen] digits (git's null id is
// that many zeros), and a row prints [gitlet.MinAbbrevDigits], the fewest the
// engine resolves, so a copied id works in a tool. A width moved on one side
// alone strands a commit page or prints ids the engine refuses.
//
// Mutation: move either figure on either side and this fails.
func TestTheDashboardNamesAnObjectAsTheEnginesRepositoriesDo(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, engine := range map[string]int{
		"OBJECT_ID_HEX_DIGITS":        gitref.OIDHexLen,
		"OBJECT_ID_MIN_ABBREV_DIGITS": gitlet.MinAbbrevDigits,
	} {
		got, err := clientsource.Scalar(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		if got != strconv.Itoa(engine) {
			t.Errorf("the dashboard's %s is %s and the engine's is %d: change it in "+
				"contract/objectids.ts", name, got, engine)
		}
	}
}

// THE DASHBOARD KNOWS EVERY STATE OF A CHECK, and every purpose one checks a
// commit for, in the engine's order: the Checks section's state tabs and the
// checks tabs key on `CHECK_STATES` and `CHECK_PURPOSES` (`contract/checks.ts`).
// A state they lack cannot be narrowed to; one the engine dropped is refused
// as an unknown `state` ([gitlet.CheckState.Valid]).
//
// Mutation: drop a state or a purpose from either side, or reorder the
// dashboard's, and this fails.
func TestTheDashboardKnowsEveryStateOfACheck(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "CHECK_STATES"), spelledOut(gitlet.CheckStates())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's CHECK_STATES is %v, want the engine's, in order: %v", got, want)
	}
	got, want = theDashboardsList(t, "CHECK_PURPOSES"), spelledOut(gitlet.CheckPurposes())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's CHECK_PURPOSES is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD KNOWS EVERY STATE OF A QUEUE'S ENTRY, in the engine's order:
// the Queue section words an entry's state by `QUEUE_STATES`
// (`contract/checks.ts`). A state it lacks draws as its raw word; one the
// engine dropped is words for nothing recorded.
//
// Mutation: drop a state from either side, or reorder the dashboard's, and
// this fails.
func TestTheDashboardKnowsEveryStateOfAQueue(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "QUEUE_STATES"), spelledOut(gitlet.QueueStates())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's QUEUE_STATES is %v, want the engine's, in order: %v", got, want)
	}
}

// THE DASHBOARD KNOWS EVERY MERGE REFUSAL, in the engine's order, and no
// other: a change page's merge controls word a refused merge, enqueue or
// override by the `refusal` the act route names (builtin.HostRefusal), from
// `MERGE_REFUSALS` (`contract/merge.ts`), whose sentences the compiler holds
// complete. A refusal it lacks gets only its class's general words ("changed
// by somebody else" for a still-moving target); one the engine dropped is a
// sentence nobody reads.
//
// Mutation: drop a refusal from either side, or reorder the dashboard's, and
// this fails.
func TestTheDashboardKnowsEveryMergeRefusal(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "MERGE_REFUSALS"), spelledOut(gitlet.MergeRefusals())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's MERGE_REFUSALS is %v, want the engine's, in order: %v", got, want)
	}
	// A MERGE'S BUILD answers a change that does not merge cleanly, and a
	// fast-forward whose head does not hold its target's tip, in the write's
	// own words, which the controls name too.
	for name, word := range map[string]gitlet.WriteRefusal{"MERGE_CONFLICT": gitlet.WriteConflict,
		"MERGE_NOT_FAST_FORWARD": gitlet.WriteNotFastForward} {
		got, err := clientsource.Scalar(clientsource.Tree(t), name)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(word) {
			t.Errorf("the dashboard's %s is %q, want the engine's %q", name, got, word)
		}
	}
}

// THE DASHBOARD OFFERS EVERY WAY A CHANGE MERGES, in the engine's order, and
// no other: the merge controls offer `MERGE_METHODS` (`contract/merge.ts`) as
// code_merge_change's `method`, opening on the first, the engine's default. A
// method it lacks cannot be chosen; one the engine dropped is refused
// `invalid` on every press.
//
// Mutation: drop a method from either side, or reorder the dashboard's, and
// this fails.
func TestTheDashboardOffersEveryMergeMethod(t *testing.T) {
	t.Parallel()
	got, want := theDashboardsList(t, "MERGE_METHODS"), spelledOut(gitlet.MergeMethods())
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's MERGE_METHODS is %v, want the engine's, in order: %v", got, want)
	}
}
