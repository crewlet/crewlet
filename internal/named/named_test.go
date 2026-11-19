package named

import (
	"reflect"
	"strings"
	"testing"
)

// located is where a reader finds each ref's words: left to right, each search
// starting after the last match, as the dashboard's parser reads a line.
func located(t *testing.T, l Line) []int {
	t.Helper()
	at, out := 0, make([]int, 0, len(l.Refs))
	for _, r := range l.Refs {
		i := strings.Index(l.Text[at:], r.Text)
		if i < 0 {
			t.Fatalf("%q names %+v, whose words are not in it after byte %d", l.Text, r, at)
		}
		out = append(out, at+i)
		at += i + len(r.Text)
	}
	return out
}

func TestALinesRefsAreRunsOfItsTextInOrder(t *testing.T) {
	t.Parallel()
	l := Words("merged ").Name(ChangeRef("web", 42)).Words(" at ").
		Name(CommitOf("web", "0123456789abcdef0123")).Words(" for ").Name(Item("t-1", "ENG-287"))
	if want := "merged web!42 at 0123456789ab for ENG-287"; l.Text != want {
		t.Fatalf("text = %q, want %q", l.Text, want)
	}
	if got, want := located(t, l), []int{7, 17, 34}; !reflect.DeepEqual(got, want) {
		t.Fatalf("refs found at %v, want %v", got, want)
	}
	want := []Ref{
		{Kind: Change, ID: "web!42", Text: "web!42"},
		{Kind: Commit, ID: "web@0123456789abcdef0123", Text: "0123456789ab"},
		{Kind: WorkItem, ID: "t-1", Text: "ENG-287"},
	}
	if !reflect.DeepEqual(l.Refs, want) {
		t.Fatalf("refs = %+v, want %+v", l.Refs, want)
	}
}

// A payload missing an id still says what it said: the words are written and
// no reference a reader could not resolve is recorded.
func TestARefMissingItsIDIsWrittenAsWordsAlone(t *testing.T) {
	t.Parallel()
	for name, ref := range map[string]Ref{
		"a change with no number":    ChangeRef("web", 0),
		"a commit with no repo":      CommitOf("", "abc"),
		"a schedule with no scope":   ScheduleRef("", "", "standup", "standup"),
		"a kind this build lacks":    {Kind: "galaxy", ID: "x", Text: "x"},
		"a ref with no words at all": {Kind: Page, ID: "p-1"},
	} {
		l := Words("on ").Name(ref)
		if len(l.Refs) != 0 {
			t.Errorf("%s: recorded %+v", name, l.Refs)
		}
		if l.Text != "on "+ref.Text {
			t.Errorf("%s: text = %q, want its words %q", name, l.Text, "on "+ref.Text)
		}
	}
}

// Two lines grown from one prefix must not share a backing array, or naming
// something in one rewrites the other's refs.
func TestLinesGrownFromOnePrefixKeepTheirOwnRefs(t *testing.T) {
	t.Parallel()
	prefix := Words("").Name(Repo("web")).Words(" ")
	prefix.Refs = append(make([]Ref, 0, 8), prefix.Refs...)
	a := prefix.Name(TurnRef("turn-a"))
	b := prefix.Name(TurnRef("turn-b"))
	if a.Refs[1].ID != "turn-a" || b.Refs[1].ID != "turn-b" {
		t.Fatalf("a names %+v and b names %+v; each must keep its own", a.Refs, b.Refs)
	}
	joined := Words("x ").Then(a).Words(" y ").Then(b)
	if got := len(joined.Refs); got != 4 {
		t.Fatalf("joined line names %d refs, want 4: %+v", got, joined.Refs)
	}
	located(t, joined)
}

func TestEveryKindIsValidAndNoOtherIs(t *testing.T) {
	t.Parallel()
	for _, k := range Kinds {
		if !k.Valid() {
			t.Errorf("%q is listed and not valid", k)
		}
	}
	if Kind("").Valid() || Kind("work item").Valid() {
		t.Error("an unlisted kind reads as valid")
	}
}

func TestItemPrefersItsIDAndIsSpelledAsItsKey(t *testing.T) {
	t.Parallel()
	cases := []struct{ id, key, wantID, wantText string }{
		{"0198-task", "ENG-287", "0198-task", "ENG-287"},
		{"", "ENG-287", "ENG-287", "ENG-287"},
		{"0198-task", "", "0198-task", "0198-task"},
	}
	for _, c := range cases {
		got := Item(c.id, c.key)
		if got.ID != c.wantID || got.Text != c.wantText || got.Kind != WorkItem {
			t.Errorf("Item(%q, %q) = %+v, want id %q spelled %q", c.id, c.key, got, c.wantID, c.wantText)
		}
	}
}
