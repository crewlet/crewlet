package redact_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// A SETTLED END IS THE WHOLE TEXT'S REDACTION FROM A LINE ON: read with a
// context holding a password's key or a key's BEGIN, the end shown is a tail
// of what Secrets makes of the whole read, opening on a line, through the log
// after it, and holds neither secret; what came before it is text's bytes up
// to it where nothing before it redacts.
//
// Mutation: show the text past the context redacted alone (start 0 in place of
// where the two redactions part): the value under its key shows, red.
func TestASettledEndIsTheWholeRedactionsEndFromALine(t *testing.T) {
	t.Parallel()
	filler := strings.Repeat("compiling a package of the build\n", redact.LookBack/33+10)
	log := strings.Repeat("ok\n", 200)
	const begin = "-----BEGIN RSA PRIVATE KEY-----\n"
	const line = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
	for _, tc := range []struct {
		name, head, rest, secret string
	}{
		{"a password's value under its key in the context", "the database password:\n", "hunter2x9\n", "hunter2x9"},
		{"a key's block whose BEGIN is in the context", begin + strings.Repeat(line+"\n", 3),
			strings.Repeat(line+"\n", 4) + "Zq9XwV==\n-----END RSA PRIVATE KEY-----\n", "Zq9XwV"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			text := filler + tc.head + tc.rest + log
			whole := redact.Secrets(text)
			if strings.Contains(whole, tc.secret) {
				t.Fatalf("the whole redaction keeps %q; the case needs one that takes it out", tc.secret)
			}
			context := len(filler) + len(tc.head)
			end, before := redact.SettledEnd(text, context)
			opens := len(end) == len(whole) || whole[len(whole)-len(end)-1] == '\n'
			if !strings.HasSuffix(whole, end) || !opens || strings.Contains(end, tc.secret) ||
				!strings.HasSuffix(end, log) {
				t.Errorf("the end is %d bytes opening %.60q; want a line-opening tail of the whole redaction, "+
					"through the log, without %q", len(end), end, tc.secret)
			}
			if strings.Contains(redact.Secrets(tc.rest), tc.secret) && before != len(text)-len(end) {
				t.Errorf("%d bytes came before the end; want the %d of text before it", before, len(text)-len(end))
			}
		})
	}
}

// A CONTEXT OF NOTHING IS THE WHOLE TEXT, and a context that settles no line
// shows nothing: a reading begun inside its last line has no line it opens.
//
// Mutation: show the last line's rest where no line opens (red).
func TestASettledEndOfAWholeTextOrOfNoLine(t *testing.T) {
	t.Parallel()
	text := "the build\nghp_" + strings.Repeat("a", 36) + " failed\n"
	if end, before := redact.SettledEnd(text, 0); end != redact.Secrets(text) || before != 0 {
		t.Errorf("SettledEnd(text, 0) = %q, %d; want the whole redaction, 0", end, before)
	}
	one := strings.Repeat("x", 100)
	if end, before := redact.SettledEnd(one, 10); end != "" || before != len(one) {
		t.Errorf("SettledEnd of one line read from inside it = %q, %d; want nothing, %d", end, before, len(one))
	}
}
