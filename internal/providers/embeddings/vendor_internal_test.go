package embeddings

import (
	"io"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/openai/openai-go/v3"

	"github.com/crewlet/crewlet/internal/httpx"
)

// WHAT AN ENDPOINT SAID reads as its message first and the fields it filed the
// refusal under after it — only those that say something.
func TestSaidNamesTheMessageThenWhatItWasFiledUnder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		message string
		fields  []filed
		want    string
	}{
		{"both", "  This model's maximum context length is 8192 tokens\n",
			[]filed{{"type", "invalid_request_error"}, {"code", " "}, {"param", "input"}},
			"This model's maximum context length is 8192 tokens (type invalid_request_error, param input)"},
		{"a message alone", "Overloaded", []filed{{"type", ""}}, "Overloaded"},
		{"fields alone", "", []filed{{"code", "model_not_found"}}, "code model_not_found"},
		{"nothing", " ", []filed{{"code", ""}}, ""},
	} {
		if got := said(tc.message, tc.fields...); got != tc.want {
			t.Errorf("%s: said = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A DETAIL IS A DIAGNOSTIC, NOT A DOCUMENT: an endpoint that answers with a
// page of text is shown the bound's worth, marked as cut, on one line.
func TestADetailIsBoundedAndMarked(t *testing.T) {
	t.Parallel()
	message := strings.Repeat("the request was refused for a reason\n", 200)
	got := detail(&sdk.Error{Message: message, Code: "too_long"})
	if len(got) > httpx.RefusalBytes+100 {
		t.Errorf("detail is %d bytes, want it bounded near %d", len(got), httpx.RefusalBytes)
	}
	if !strings.Contains(got, "runs past") || strings.Contains(got, "\n") {
		t.Errorf("detail = %q, want one line marked as cut", got)
	}
	if detail(nil) != "" || detail(&sdk.Error{StatusCode: 500}) != "" {
		t.Error("an error that said nothing was given a detail")
	}
}

// A KEY THE BOUND RUNS THROUGH IS REDACTED WHOLE, in the envelope's message and
// in a body outside it alike. Bounded first, the cut left the key's opening —
// `sk-proj-` and a few characters, too short for any rule to recognise — shown
// as it was.
//
// Mutation: bound before redacting in shown, and the key's opening survives
// in both.
func TestAKeyTheBoundRunsThroughIsRedactedWhole(t *testing.T) {
	t.Parallel()
	key := "sk-proj-" + strings.Repeat("Zq7", 12)
	// The text runs to twelve bytes short of the bound, so a cut at the
	// bound leaves `sk-proj-Zq7Z` of the key.
	lead := strings.Repeat("refused ", httpx.RefusalBytes/8)[:httpx.RefusalBytes-12]
	text := lead + key + " and the rest of the account"
	for name, apiErr := range map[string]*sdk.Error{
		"the envelope": {Message: text, Code: "invalid_api_key"},
		"outside the envelope": {StatusCode: 502, Response: &http.Response{
			Header: http.Header{"Content-Type": {"text/plain"}},
			Body:   io.NopCloser(strings.NewReader(text)),
		}},
	} {
		got := detail(apiErr)
		if strings.Contains(got, key[:len("sk-proj-")+1]) {
			t.Errorf("%s: the key's opening survived the bound: …%q", name, got[max(len(got)-80, 0):])
		}
		if !strings.Contains(got, "runs past") {
			t.Errorf("%s: detail = …%q, want it marked as cut", name, got[max(len(got)-80, 0):])
		}
	}
	// A body redaction shrinks to within the bound is not cut, and says so
	// by saying nothing about a cut.
	short := strings.Repeat(key+" ", httpx.RefusalBytes/len(key)+4)
	if got := shown("text/plain", short); strings.Contains(got, "runs past") ||
		strings.Contains(got, "sk-proj-") {
		t.Errorf("a body of keys redacted to within the bound = %q, want it whole and redacted", got)
	}
}
