package tracker

import (
	"strings"
	"testing"
)

// THE OPTION LISTS ARE PAGED BY WHOLE FIELDS, and a field that does not fit
// comes back with NONE of its options rather than some.
//
// Half a list is worse than none: a model setting a value would choose from
// what it was shown and believe that was the set. The counts beside it —
// `options_total` against `options_shown` — are what say otherwise, and they
// are the pair because a single number cannot answer "am I seeing all the
// choices".
func TestOptionListsPageByWholeFields(t *testing.T) {
	t.Parallel()
	field := func(id string, options int) FieldDef {
		f := FieldDef{ID: id, Slug: id, Name: id, Type: FieldDropdown}
		for i := range options {
			f.Config.Options = append(f.Config.Options, Option{
				ID: id + itoaSmall(i), Slug: id + itoaSmall(i), Name: "n",
			})
		}
		return f
	}
	fields := []FieldDef{field("a", 100), field("b", 100), field("c", 100)}
	got, shown := pageOptions(fields, 250)

	if shown != 200 {
		t.Errorf("the page shows %d options, want the two whole lists that fit "+
			"in 250 — a budget filled to the byte would cut the third list in "+
			"half", shown)
	}
	if len(got) != 3 {
		t.Fatalf("paging dropped a field: %d of 3 — a field whose options do "+
			"not fit is still a field the caller has to know exists", len(got))
	}
	if len(got[2].Config.Options) != 0 {
		t.Errorf("the third field carries %d of its 100 options — a model "+
			"would choose from that and believe it was the set",
			len(got[2].Config.Options))
	}
	for i := range 2 {
		if len(got[i].Config.Options) != 100 {
			t.Errorf("field %d carries %d of its 100 options", i,
				len(got[i].Config.Options))
		}
	}
	// AND A CATALOGUE THAT FITS IS UNTOUCHED, which is every ordinary
	// company: the paging is for the deep vocabulary, not a tax on the
	// common case.
	whole, shown := pageOptions(fields, 300)
	if shown != 300 {
		t.Errorf("a catalogue inside the budget showed %d of 300 options", shown)
	}
	for i, f := range whole {
		if len(f.Config.Options) != 100 {
			t.Errorf("field %d lost options inside the budget: %d of 100",
				i, len(f.Config.Options))
		}
	}
}

// A COMMENT BODY IS ELIDED TO WHAT A READER SKIMS.
//
// Twenty comments at [MaxCommentBody] is 640 KiB — ten times the ceiling on
// one tool answer — for a thread nobody asked to read in full. The excerpt is
// what a reader skims; opening one is a second call.
func TestACommentBodyIsElidedForTheThread(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", MaxCommentBody)
	if len(long) <= CommentBodyShown {
		t.Fatal("the fixture is not longer than the elision, so this tests nothing")
	}
	// THE ELISION IS THE READER'S, so this asserts the rule rather than
	// the arithmetic: what matters is that the body a detail read carries
	// is bounded and MARKED, because a body cut without a marker reads as
	// a comment that ended there.
	got := elideCommentBody(long)
	if len(got) > CommentBodyShown+len("…") {
		t.Errorf("an elided body is %d bytes and the cap is %d",
			len(got), CommentBodyShown)
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("the elided body carries no marker, so it reads as a comment " +
			"that ended where the cut fell")
	}
	if short := elideCommentBody("brief"); short != "brief" {
		t.Errorf("a short body was changed to %q", short)
	}
}

// itoaSmall is enough for the fixtures here.
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

// THE COUNT SAYS WHETHER IT REACHED THE END.
//
// The hint stops at a ceiling, and before this the answer carried the stopping
// value — 10001 — with nothing to say it was a stopping value. Every reader
// that did not carry its own copy of the ceiling reported it as an exact
// total, and a model asked "how much open work is there" answered "10001".
// The one reader that did carry a copy compared with `>=`, so a set of exactly
// ten thousand — which IS exact — rendered as "10000+".
func TestTheTotalHintSaysWhenItStoppedCounting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		counted int
		want    int
		capped  bool
	}{
		{"an ordinary set", 42, 42, false},
		{"one below the ceiling", TotalHintCeiling - 1, TotalHintCeiling - 1, false},
		// THE BOUNDARY, and it is exact on this side: the query counts
		// one PAST the ceiling, so a result of exactly the ceiling means
		// the set ended there.
		{"exactly the ceiling", TotalHintCeiling, TotalHintCeiling, false},
		// And one more row is the smallest set that is not.
		{"one past it", TotalHintCeiling + 1, TotalHintCeiling, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, capped := capHint(tc.counted)
			if got != tc.want || capped != tc.capped {
				t.Errorf("capHint(%d) = (%d, %v), want (%d, %v)",
					tc.counted, got, capped, tc.want, tc.capped)
			}
		})
	}
}
