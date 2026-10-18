package redact_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// The values here are SHAPES, not credentials: each is a syntactically valid
// example of a vendor's format built from filler characters, which is what the
// denylist matches on.
func TestEveryKnownShapeIsReplaced(t *testing.T) {
	cases := map[string]string{
		"sk-proj-" + strings.Repeat("A", 32):                              "api-key",
		"sk-ant-api03-" + strings.Repeat("x", 40):                         "api-key",
		"sk-" + strings.Repeat("z", 40):                                   "api-key",
		"xoxb-" + strings.Repeat("1", 12) + "-" + strings.Repeat("2", 12): "slack-token",
		"xoxp-" + strings.Repeat("3", 24):                                 "slack-token",
		"AKIA" + strings.Repeat("Q", 16):                                  "aws-key",
		"ghp_" + strings.Repeat("b", 36):                                  "github-token",
		"gho_" + strings.Repeat("c", 36):                                  "github-token",
		"github_pat_" + strings.Repeat("d", 60):                           "github-token",
		"glpat-" + strings.Repeat("e", 20):                                "gitlab-token",
		"glrt-" + strings.Repeat("f", 20):                                 "gitlab-token",
		"password: hunter2":                                               "password",
		"PASSWORD=hunter2":                                                "password",
		"pwd = hunter2":                                                   "password",
	}
	for secret, want := range cases {
		text := "before " + secret + " after"
		got := redact.Secrets(text)
		if strings.Contains(got, secret) {
			t.Fatalf("%s survived redaction: %q", want, got)
		}
		if !strings.Contains(got, redact.Marker+want+"]") {
			t.Fatalf("redacting a %s gave %q, want a %s marker", want, got, want)
		}
		if !strings.HasPrefix(got, "before ") || !strings.HasSuffix(got, " after") {
			t.Fatalf("surrounding text was disturbed: %q", got)
		}
	}
}

// A project key must not be labelled a plain api-key: the bare sk- rule would
// swallow it, so order is load-bearing.
func TestTheMoreSpecificShapeWinsOverThePrefixThatContainsIt(t *testing.T) {
	got := redact.Secrets("sk-proj-" + strings.Repeat("A", 32))
	if got != redact.Marker+"api-key]" {
		t.Fatalf("got %q, want exactly one marker with nothing left over", got)
	}
}

func TestAPrivateKeyBlockGoesWholeRatherThanLineByLine(t *testing.T) {
	block := "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
		strings.Repeat(sshLine+"\n", 20) +
		"-----END OPENSSH PRIVATE KEY-----"
	got := redact.Secrets("log line\n" + block + "\ntrailing")
	if strings.Contains(got, "b3BlbnNzaC") {
		t.Fatalf("key material survived: %q", got)
	}
	if !strings.Contains(got, redact.Marker+"private-key]") {
		t.Fatalf("no private-key marker in %q", got)
	}
	if !strings.Contains(got, "log line") || !strings.Contains(got, "trailing") {
		t.Fatalf("the surrounding log was eaten: %q", got)
	}
}

// Two keys in one payload must not be merged into a single span by a greedy
// match that runs from the first BEGIN to the last END.
func TestTwoPrivateKeysDoNotSwallowWhatIsBetweenThem(t *testing.T) {
	key := "-----BEGIN RSA PRIVATE KEY-----\n" + pemBody(2) + "-----END RSA PRIVATE KEY-----"
	got := redact.Secrets(key + "\nIMPORTANT LOG\n" + key)
	if !strings.Contains(got, "IMPORTANT LOG") {
		t.Fatalf("a greedy match ate the text between two keys: %q", got)
	}
	if strings.Count(got, redact.Marker+"private-key]") != 2 {
		t.Fatalf("want two markers, got %q", got)
	}
}

// A PASSWORD'S VALUE IS ALL ITS RULE REPLACES: the key and the separator stay
// as they were, line breaks and all, real or escaped in a string, and the value
// ends where no password goes on — at an armour line's armour, a marker. An
// escaped line break in it stays where it is, the value redacted on either
// side of it, so a password holding a backslash and an `n` or an `r` is not
// published past them. A token's match that runs into an armour stops at it.
// Each comes out as given, and redacting it again changes nothing.
//
// Mutation: replace the whole match, as the rule did, and the key goes with
// its value, its line break too; replace the value whole, and the escaped
// break in it goes with it; end it at an escaped break, and what follows the
// break is published; through an armour, and the END goes with it; through a
// marker, and a value already redacted is replaced again; let a token run into
// an armour, and the END is taken with it.
func TestAPasswordsValueIsAllItsRuleReplaces(t *testing.T) {
	m := redact.Marker + "password]"
	key := "-----BEGIN RSA PRIVATE KEY----- " + strings.Repeat("Ab1", 22) + " -----END RSA PRIVATE KEY-----"
	for name, c := range map[string]struct{ text, want string }{
		"on its line":             {"password: hunter2 rest", "password: " + m + " rest"},
		"on the next line":        {"login\npassword:\n  hunter2\nnext\n", "login\npassword:\n  " + m + "\nnext\n"},
		"after an escaped break":  {`{"k": "password:\n  hunter2 x"}`, `{"k": "password:\n  ` + m + ` x"}`},
		"after an escaped CRLF":   {`"pwd=\r\nhunter2 x"`, `"pwd=\r\n` + m + ` x"`},
		"across an escaped break": {`"pwd=hunter2\nnext" x`, `"pwd=` + m + `\n` + m + ` x`},
		"across an escaped CRLF":  {`pwd=Xk9\r\nQ2#vL8 x`, `pwd=` + m + `\r\n` + m + ` x`},
		"holding a backslash-n":   {`export DB_PASSWORD=Xk9\nQ2#vL8pZ`, `export DB_PASSWORD=` + m + `\n` + m},
		"holding a backslash-r":   {`mysql -u root --password=r00t\rT0ps3cret -e x`, `mysql -u root --password=` + m + ` -e x`},
		"in a string escaped twice, across its break": {
			`{"v": "pwd=\\nabc"}`, `{"v": "pwd=` + m + `\n` + m,
		},
		"up to a marker":   {"pwd=x" + redact.Marker + "api-key] y", "pwd=" + m + redact.Marker + "api-key] y"},
		"already redacted": {"password: " + m, "password: " + m},
		"a key redacted":   {"PASSWORD=" + key, "PASSWORD=" + redact.Marker + "private-key]"},
		"an armour line":   {"password:\n-----END RSA PRIVATE KEY-----\n", "password:\n-----END RSA PRIVATE KEY-----\n"},
		"up to an armour":  {"pwd=hunter2-----END RSA PRIVATE KEY-----\n", "pwd=" + m + "-----END RSA PRIVATE KEY-----\n"},
		"a token written up to an armour": {
			"sk-" + strings.Repeat("q", 21) + "-----END RSA PRIVATE KEY-----\n",
			redact.Marker + "api-key]-----END RSA PRIVATE KEY-----\n",
		},
		"a token too short without the armour": {
			"sk-" + strings.Repeat("q", 15) + "-----END RSA PRIVATE KEY-----\n",
			"sk-" + strings.Repeat("q", 15) + "-----END RSA PRIVATE KEY-----\n",
		},
	} {
		got := redact.Secrets(c.text)
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
		if again := redact.Secrets(got); again != got {
			t.Errorf("%s: a second pass changed %q to %q", name, got, again)
		}
		if redact.Contains(got) {
			t.Errorf("%s: %q still holds something to replace", name, got)
		}
	}
}

// This pass runs at more than one layer and a transcript can cross both.
func TestRedactionIsIdempotent(t *testing.T) {
	text := "token " + strings.Repeat("k", 40) + " and sk-" + strings.Repeat("q", 40)
	once := redact.Secrets(text)
	if twice := redact.Secrets(once); twice != once {
		t.Fatalf("a second pass changed the text:\n once: %q\ntwice: %q", once, twice)
	}
}

func TestOrdinaryTextIsUntouched(t *testing.T) {
	for _, text := range []string{
		"",
		"the build passed",
		"sk-short", // below the length floor: not a key shape
		"see docs/concepts/code-sandbox.md",
		"password rotation is documented in SECURITY.md",
	} {
		if got := redact.Secrets(text); got != text {
			t.Fatalf("Secrets(%q) = %q, want it unchanged", text, got)
		}
	}
}

func TestContainsAnswersWithoutRewriting(t *testing.T) {
	if redact.Contains("nothing here") {
		t.Fatal("Contains flagged clean text")
	}
	if !redact.Contains("glpat-" + strings.Repeat("e", 20)) {
		t.Fatal("Contains missed a known shape")
	}
}

// AND DIRTY TEXT IS STILL REDACTED WHOLE. The guard must not turn into a
// short-circuit that stops at the first rule that matched.
func TestEveryMatchingRuleStillFiresWhenSeveralDo(t *testing.T) {
	text := "glpat-" + strings.Repeat("e", 20) +
		" and sk-ant-" + strings.Repeat("a", 40) +
		" and password: hunter2"
	got := redact.Secrets(text)
	if redact.Contains(got) {
		t.Errorf("Secrets left something behind: %q", got)
	}
	for _, want := range []string{"gitlab-token", "api-key", "password"} {
		if !strings.Contains(got, want) {
			t.Errorf("Secrets(%q) = %q, missing the %s marker", text, got, want)
		}
	}
}
