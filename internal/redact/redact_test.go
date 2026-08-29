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
		"plane_api_" + strings.Repeat("g", 32):                            "plane-token",
		"plane_wh_" + strings.Repeat("h", 32):                             "plane-webhook-secret",
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
		strings.Repeat("b3BlbnNzaC1rZXktdjEAAAAA\n", 20) +
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
	key := "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----"
	got := redact.Secrets(key + "\nIMPORTANT LOG\n" + key)
	if !strings.Contains(got, "IMPORTANT LOG") {
		t.Fatalf("a greedy match ate the text between two keys: %q", got)
	}
	if strings.Count(got, redact.Marker+"private-key]") != 2 {
		t.Fatalf("want two markers, got %q", got)
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

// A credential split across the cut boundary matches no rule in either half,
// so the cut has to come first and the redaction after it.
func TestTailRedactsAfterCuttingNotBefore(t *testing.T) {
	secret := "glpat-" + strings.Repeat("e", 20)
	text := strings.Repeat("x", 500) + "\n" + secret + " at the end"
	got := redact.Tail(text, 40)
	if strings.Contains(got, secret) {
		t.Fatalf("the tail leaked a credential: %q", got)
	}
	if !strings.Contains(got, "elided") {
		t.Fatalf("the tail did not say it had cut: %q", got)
	}
}

func TestTailKeepsShortTextWhole(t *testing.T) {
	if got := redact.Tail("all of it", 100); got != "all of it" {
		t.Fatalf("Tail = %q", got)
	}
	if got := redact.Tail("anything", 0); got != "" {
		t.Fatalf("Tail with no budget = %q, want empty", got)
	}
}

func TestTailOpensAtALineBoundaryWhenOneIsNear(t *testing.T) {
	text := "old\n" + strings.Repeat("a", 20) + "\nthe last line"
	got := redact.Tail(text, 20)
	if strings.HasPrefix(strings.TrimPrefix(got, "… earlier output elided …\n"), "a") {
		return // opened mid-run because no boundary was near enough
	}
	if !strings.Contains(got, "the last line") {
		t.Fatalf("Tail lost the end of the text: %q", got)
	}
}
