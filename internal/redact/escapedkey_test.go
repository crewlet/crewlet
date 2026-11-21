package redact_test

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// redactsAsWritten checks that text redacts to want, that a second pass
// changes nothing, and that it settles to want a byte at a time and in pieces.
func redactsAsWritten(t *testing.T, name, text, want string) {
	t.Helper()
	got := redact.Secrets(text)
	if got != want {
		t.Errorf("%s:\n got %q\nwant %q", name, got, want)
	}
	if again := redact.Secrets(got); again != got {
		t.Errorf("%s: a second pass changed %q to %q", name, got, again)
	}
	for _, sizes := range [][]int{{1}, {5, 400}} {
		if shown, _ := settle(text, sizes); shown != want {
			t.Errorf("%s in pieces of %v: shown as it settled\n%q\nwant\n%q", name, sizes, shown, want)
		}
	}
}

// A PASSWORD'S KEY WRITTEN INSIDE A STRING IS A KEY: a JSON object logged as
// a string escapes its quotes, once for each string it is written in, and the
// key is read through its escapes as it is read written plainly, its value
// replaced as a plain key's is. Mutation: drop the backslashes from what
// closes a key (passwordClose), and every row is published.
func TestAPasswordsKeyWrittenInsideAStringIsReadThroughItsEscapes(t *testing.T) {
	t.Parallel()
	m := redact.Marker + "password]"
	for name, c := range map[string]struct{ text, want string }{
		"an object logged as a string": {
			`{"log":"{\"password\": \"hunter2x9\", \"user\": \"bob\"}"}`,
			`{"log":"{\"password\": ` + m + ` \"user\": \"bob\"}"}`,
		},
		"an object escaped twice": {
			`{"log":"{\"msg\":\"{\\\"password\\\": \\\"hunter2x9\\\", \\\"user\\\": \\\"bob\\\"}\"}"}`,
			`{"log":"{\"msg\":\"{\\\"password\\\": ` + m + ` \\\"user\\\": \\\"bob\\\"}\"}"}`,
		},
		"single quotes escaped": {
			`echo '{\'password\': \'hunter2x9\', \'user\': \'bob\'}'`,
			`echo '{\'password\': ` + m + ` \'user\': \'bob\'}'`,
		},
		"an assignment": {
			`{"log":"\"password\"=\"hunter2x9\" \"user\"=\"bob\""}`,
			`{"log":"\"password\"=` + m + ` \"user\"=\"bob\""}`,
		},
		"its value on the next escaped line": {
			`{"log":"{\n  \"password\":\n    \"hunter2x9\"\n}"}`,
			`{"log":"{\n  \"password\":\n    \"` + m + `\n}"}`,
		},
	} {
		redactsAsWritten(t, name, c.text, c.want)
	}
}

// AN ESCAPED QUOTE CLOSES A KEY ONLY WHERE IT CLOSES THE KEY'S OWN STRING, as a
// plain quote does: a longer string ending in the key's word, written inside
// another string, names no value. Mutation: read the close's quote without
// passing its backslashes (quotedElsewhere), and the string after it is
// replaced.
func TestAnEscapedQuoteClosesAKeyOnlyWhereItClosesTheKeysOwnString(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"a route ending in the key's word":    `{"log":"\"POST /auth/password\": () => ok"}`,
		"a choice between two strings":        `{"log":"x ? \"Choose your password\" : \"Choose a new one\""}`,
		"a sentence ending in the key's word": `{"msg":"{\"Enter your password\": \"Retype it\"}"}`,
	} {
		redactsAsWritten(t, name, text, text)
	}
}

// AN AWS SECRET UNDER A KEY WRITTEN INSIDE A STRING IS TAKEN, its value read
// past its escaped opening quote as past a plain one, the escaped close kept.
// Mutation: read the value's opening quote without passing its backslashes
// (awsSecretValue), and the secret is published.
func TestAnAWSSecretUnderAKeyWrittenInsideAStringIsTaken(t *testing.T) {
	t.Parallel()
	secret := "q8Zr4TfL2wAbCdEfGhIjKlMnOpQrStUvWxYz0123"
	m := redact.Marker + "secret-access-key]"
	redactsAsWritten(t, "a credentials object logged as a string",
		`{"log":"{\"aws_secret_access_key\": \"`+secret+`\"}"}`,
		`{"log":"{\"aws_secret_access_key\": \"`+m+`\"}"}`)
	redactsAsWritten(t, "escaped twice",
		`{"log":"{\"body\":\"{\\\"AWS_SECRET_ACCESS_KEY\\\":\\\"`+secret+`\\\"}\"}"}`,
		`{"log":"{\"body\":\"{\\\"AWS_SECRET_ACCESS_KEY\\\":\\\"`+m+`\\\"}\"}"}`)
}

// AN AWS SECRET'S SEPARATOR CROSSES AN ESCAPED LINE BREAK, NEVER A REAL ONE: a
// JSON object a string holds, pretty-printed, writes its value on the line
// after its key's as `\n`, which is on the key's own real line. The stream
// reads a separator of spaces and escaped breaks longer than its windows as
// the whole text does, wherever a window parts a break's two bytes.
// Mutation: hold the separator to spaces and tabs (awsSpace), and the first
// row is published.
func TestAnAWSSecretsSeparatorCrossesAnEscapedLineBreakButNoRealOne(t *testing.T) {
	t.Parallel()
	token := strings.Repeat("Ab3+", 60) + "=="
	m := redact.Marker + "session-token]"
	redactsAsWritten(t, "a pretty-printed object a string holds",
		`{"log":"{\n  \"aws_session_token\":\n    \"`+token+`\"\n}"}`,
		`{"log":"{\n  \"aws_session_token\":\n    \"`+m+`\"\n}"}`)
	redactsAsWritten(t, "escaped twice",
		`{"log":"{\"body\":\"{\\\"aws_session_token\\\":\\n  \\\"`+token+`\\\"}\"}"}`,
		`{"log":"{\"body\":\"{\\\"aws_session_token\\\":\\n  \\\"`+m+`\\\"}\"}"}`)
	redactsAsWritten(t, "a real line break", "AWS_SESSION_TOKEN=\n"+token+"\n", "AWS_SESSION_TOKEN=\n"+token+"\n")
	r := rand.New(rand.NewPCG(1213, 7))
	streams := func(name, text string) {
		t.Helper()
		want := redact.Secrets(text)
		if got := cut(r, text, 64<<10); got != want {
			t.Errorf("%s: the stream wrote what Secrets does not: %s", name, parted(got, want))
		}
	}
	// Half the bound in spaces and escaped breaks, past every window.
	run := strings.Repeat(` \n`, reach/6)
	for pad := range 3 {
		text := strings.Repeat("x", pad) + "Password: hunter2x9 AWS_SESSION_TOKEN =" + run + token + "\n"
		if strings.Contains(redact.Secrets(text), token) {
			t.Fatalf("pad %d: the token after the separator is published", pad)
		}
		streams(fmt.Sprintf("a long separator, pad %d", pad), text)
	}
	// A stage's first window ending at every byte of a key and its
	// separator, a key's block before one of them.
	filler := strings.Repeat("the build passed ok\n", 2*redact.StageAdvance/20)
	b64 := strings.Repeat("Ab3+", 10)
	block := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\n"
	for _, c := range []struct{ before, key, after string }{
		{"{", `\"SessionToken\":\n\"`, `\"}`},
		{"", `aws_secret_access_key =\n`, " "},
		{"", `SessionToken\":\r\n`, ""},
		{"", `SessionToken\\\":\\n\\\"`, ""},
		{block, `SessionToken\":\n`, "\n-----END RSA PRIVATE KEY-----\n"},
	} {
		for k := 1; k <= len(c.key)+1; k++ {
			streams(fmt.Sprintf("%q, a window's end %d bytes in", c.key, k),
				filler[:redact.StageAdvance-len(c.before)-k]+c.before+c.key+b64+c.after+filler)
		}
	}
}
