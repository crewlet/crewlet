package gitlab_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlab"
)

// THE NEGATIVE RULE IS THE WHOLE POINT. An `@` counts only where what
// precedes it is not part of a word — and Go's regexp engine is RE2, which
// has no lookbehind, so a scanner is the only correct implementation rather
// than a workaround for one.
func TestMentionsReadOnlyRealMentions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"a plain mention", "@ana can you look", []string{"ana"}},
		{"mid-sentence", "ping @ana about this", []string{"ana"}},
		{"an email is not a mention", "mail deploy@example.com", nil},
		{"a path fragment is not a mention", "see docs/@internal/readme", nil},
		{"a handle after punctuation", "(@ana)", []string{"ana"}},
		{"a handle after a newline", "cc\n@ana", []string{"ana"}},
		{"trailing punctuation is not part of the name",
			"thanks @ana.", []string{"ana"}},
		{"a comma ends it", "@ana, @bo", []string{"ana", "bo"}},
		{"interior dots and hyphens are kept",
			"@agent-swe.two", []string{"agent-swe.two"}},
		{"underscores are kept", "@agent_swe", []string{"agent_swe"}},
		{"a bare at is nothing", "@ and @", nil},
		{"case is folded", "@Ana and @ANA", []string{"ana"}},
		{"order is preserved and duplicates dropped",
			"@bo @ana @bo", []string{"bo", "ana"}},
		{"collectives come back for the caller to filter",
			"@here everyone", []string{"here"}},
		{"a username may not start with punctuation", "@-ana", nil},
		{"an underscore before the at continues the word", "x_@ana", nil},
		{"empty text", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := gitlab.Mentions(tc.text); !slices.Equal(got, tc.want) {
				t.Fatalf("Mentions(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// Two mentions separated by punctuation are two mentions — the case where a
// name-terminating byte sits between them.
func TestPunctuationBetweenTwoMentionsSeparatesThem(t *testing.T) {
	t.Parallel()
	got := gitlab.Mentions("@ana...@bo")
	if !slices.Equal(got, []string{"ana", "bo"}) {
		t.Fatalf("Mentions = %v", got)
	}
}

// A SECOND `@` INSIDE A WORD IS NOT A MENTION, which is what makes the
// boundary rule and not the character class the load-bearing part: `@a@b`
// names one person, not two.
func TestASecondAtInsideAWordIsNotAMention(t *testing.T) {
	t.Parallel()
	if got := gitlab.Mentions("@ana@bo"); !slices.Equal(got, []string{"ana"}) {
		t.Fatalf("Mentions = %v, want only the first", got)
	}
}
