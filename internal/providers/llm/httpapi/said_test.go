package httpapi

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/httpx"
)

// WHAT A PROVIDER SAID reads as its message first and the fields it filed the
// refusal under after it — only those that say something.
func TestSaidNamesTheMessageThenWhatItWasFiledUnder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		message string
		filed   []Filed
		want    string
	}{
		{"both", "  This model's maximum context length is 8192 tokens\n",
			[]Filed{{"type", "invalid_request_error"}, {"code", " "}, {"param", "input"}},
			"This model's maximum context length is 8192 tokens (type invalid_request_error, param input)"},
		{"a message alone", "Overloaded", []Filed{{"type", ""}}, "Overloaded"},
		{"fields alone", "", []Filed{{"code", "model_not_found"}}, "code model_not_found"},
		{"nothing", " ", []Filed{{"code", ""}}, ""},
	} {
		if got := Said(tc.message, tc.filed...); got != tc.want {
			t.Errorf("%s: Said = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A REFUSAL IS REDACTED BEFORE IT IS BOUNDED. The bound cuts wherever the bytes
// run out, and a key it runs through was left as an opening too short for any
// rule to recognise — `sk-proj-` and a few characters, shown as they were.
// Redacted first, the key is whole when it is matched, and only its marker can
// be cut.
//
// Mutation: bound before redacting in shown, and the key's opening survives
// in both.
func TestARefusalIsRedactedBeforeItIsBounded(t *testing.T) {
	t.Parallel()
	key := "sk-ant-api03-" + strings.Repeat("Qx9", 12)
	lead := strings.Repeat("refused ", httpx.RefusalBytes/8)[:httpx.RefusalBytes-16]
	text := lead + key + " — the rest of the account"
	for name, got := range map[string]string{
		"a message": Said(text, Filed{"type", "authentication_error"}),
		"a body":    SaidBody("text/plain", []byte(text)),
	} {
		if strings.Contains(got, key[:len("sk-ant-")+1]) {
			t.Errorf("%s: the key's opening survived the bound: …%q", name, got[max(len(got)-80, 0):])
		}
		if !strings.Contains(got, "runs past") {
			t.Errorf("%s: …%q, want it marked as cut", name, got[max(len(got)-80, 0):])
		}
	}
	// A body redaction shrinks to within the bound is not cut, and says so
	// by saying nothing about a cut.
	short := strings.Repeat(key+" ", httpx.RefusalBytes/len(key)+4)
	if got := SaidBody("text/plain", []byte(short)); strings.Contains(got, "runs past") ||
		strings.Contains(got, "sk-ant-") {
		t.Errorf("a body of keys redacted to within the bound = %q, want it whole and redacted", got)
	}
}
