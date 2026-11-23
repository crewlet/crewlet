package executor

import "testing"

// A BRIEF BEGINNING WITH A SLASH REACHES THE MODEL AS WORDS. Gemini CLI and
// Qwen Code trim a prompt with JavaScript's trim and run it as a command of
// their own where it then begins with `/` (`/init` wrote a memory file on
// both, measured on 0.70.0 and 0.28.4), so the slashes and the space around
// them go, however many, and whatever trim would remove before them; every
// other brief, a slash inside it included, is handed byte for byte.
//
// Mutations, each red: strip one slash only; trim only ASCII space; strip a
// brief that does not begin with a slash.
func TestABriefBeginningWithASlashReachesTheModelAsWords(t *testing.T) {
	t.Parallel()
	for brief, want := range map[string]string{
		"/init the repository":        "init the repository",
		"  \n/review it":              "review it",
		"// a comment":                "a comment",
		"/ /init":                     "init",
		"\u00a0\ufeff/init":           "init",
		"\u2028/init":                 "init",
		"Fix /etc/hosts":              "Fix /etc/hosts",
		"  Fix it\n/init comes later": "  Fix it\n/init comes later",
		"":                            "",
	} {
		if got := briefNotACommand(brief); got != want {
			t.Errorf("briefNotACommand(%q) = %q, want %q", brief, got, want)
		}
	}
}
