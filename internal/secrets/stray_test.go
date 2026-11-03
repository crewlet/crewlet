package secrets_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/secrets"
)

// A PASTED CREDENTIAL WITH ANYTHING BUT ONE UNBROKEN LINE IN IT IS NAMED BY
// WHAT AND WHERE, never by its value; a clean one says nothing.
func TestStrayProblemNamesWhatAndWhere(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, value, says string
	}{
		{"a clean token", "cfat_AbC-123", ""},
		{"a wrapped paste", "sk-ant-oat01-abc\n  def", "1 line break and 2 spaces (characters 17, 18 and 19)"},
		{"a tab", "abc\tdef", "1 tab (character 4)"},
		{"a zero-width space", "abc\u200bdef", "1 invisible character (character 4)"},
		{"a line separator", "abc\u2028def", "1 line break"},
		{"many spaces", "a b c d e", "4 spaces (characters 2, 4, 6 and 1 more)"},
	} {
		got := secrets.StrayProblem("API key", tc.value)
		if (got == "") != (tc.says == "") || !strings.Contains(got, tc.says) {
			t.Errorf("%s: %q, want it to say %q", tc.name, got, tc.says)
		}
		if got != "" && (!strings.HasPrefix(got, "This API key contains") || strings.Contains(got, "def")) {
			t.Errorf("%s: %q does not name the key, or quotes it", tc.name, got)
		}
	}
	if got := secrets.StrayProblem("token", "a b"); !strings.Contains(got, "A token is one unbroken line") {
		t.Errorf("the article does not follow the noun: %q", got)
	}
}
