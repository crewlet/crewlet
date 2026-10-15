package config

import (
	"errors"
	"strings"
	"testing"
)

// A PROVIDER'S base_url IS HELD TO WHAT ITS SDK CAN SEND, on every type that
// reads one and on the embeddings provider: a query or a fragment is refused
// at the field, because both SDKs resolve each request's path against the base
// and that drops them — anthropic.TestABaseURLsQueryIsNeverSent and
// openai.TestABaseURLsQueryIsNeverSent measure it — and a value that is not an
// http(s) URL with a host is refused because no call on it could be sent. A
// value written around `${VAR}` references is judged on its own text.
//
// The refusal never repeats the value: a query is where a key gets written
// into a URL, and a validation error is printed, logged and pasted.
//
// Mutation: drop either CheckBaseURL call from validation, and that block's
// cases load clean.
func TestABaseURLTheSDKCannotSendIsRefusedAtTheField(t *testing.T) {
	t.Parallel()
	const secret = "sk-in-the-query"
	for name, value := range map[string]string{
		"a query":                         "https://gw.example.com/v1?key=" + secret,
		"an empty query":                  "https://gw.example.com/v1?",
		"a fragment":                      "https://gw.example.com/v1#" + secret,
		"a query after a reference":       "${LLM_BASE_URL}?key=" + secret,
		"a fragment after a reference":    "${LLM_BASE_URL}/v1#" + secret,
		"no scheme":                       "gw.example.com/v1",
		"a host and port with no scheme":  "localhost:4000",
		"a scheme that is not http(s)":    "ftp://gw.example.com/v1",
		"a scheme with no host":           "https:///v1",
		"a query written before the path": "https://gw.example.com?key=" + secret + "/v1",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, doc := range []struct{ yaml, path string }{
				{"name: Acme\nproviders:\n  llm:\n    claude:\n      type: anthropic\n      model: claude-opus-5-5\n" +
					"      base_url: \"" + value + "\"\n", "providers.llm.claude.base_url"},
				{"name: Acme\nproviders:\n  llm:\n    gpt:\n      type: openai\n      model: gpt-5\n" +
					"      base_url: \"" + value + "\"\n", "providers.llm.gpt.base_url"},
				{"name: Acme\nproviders:\n  llm:\n    local:\n      type: openai-compatible\n      model: m\n" +
					"      base_url: \"" + value + "\"\n", "providers.llm.local.base_url"},
				{"name: Acme\nproviders:\n  embeddings:\n    type: openai-compatible\n    model: text-embedding-3-small\n" +
					"    base_url: \"" + value + "\"\n", "providers.embeddings.base_url"},
			} {
				err := rejects(t, doc.yaml, doc.path)
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the refusal repeats the value's query or fragment:\n%v", err)
				}
				if !errors.Is(err, ErrUnknownValue) {
					t.Errorf("the refusal is not ErrUnknownValue:\n%v", err)
				}
			}
		})
	}
}

// AND WHAT AN SDK DOES SEND LOADS: a path, a port, userinfo — sent as Basic
// credentials, which a gateway may read — and a value whose only unknowns are
// its references, which the engine judges once they resolve.
func TestABaseURLTheSDKSendsLoads(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"https://gw.example.com",
		"https://gw.example.com/v1/",
		"http://127.0.0.1:8317",
		"https://user:pass@gw.example.com/anthropic",
		"${LLM_BASE_URL}",
		"${LLM_PROXY_URL}/v1",
		"https://${GATEWAY_HOST}/v1",
	} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			mustCompany(t, "name: Acme\nproviders:\n  llm:\n    claude:\n      type: anthropic\n"+
				"      model: claude-opus-5-5\n      base_url: \""+value+"\"\n")
			mustCompany(t, "name: Acme\nproviders:\n  embeddings:\n    type: openai-compatible\n"+
				"    model: text-embedding-3-small\n    base_url: \""+value+"\"\n")
		})
	}
}

// THE RULE ITSELF, as the engine applies it to a resolved value: a query or a
// fragment and a non-URL are refused by sentences that never repeat the value,
// and nothing — an unset field — is not this rule's to refuse.
func TestCheckBaseURLJudgesAResolvedValueWhole(t *testing.T) {
	t.Parallel()
	if err := CheckBaseURL(""); err != nil {
		t.Errorf("an empty base_url: %v, want nil (the type's default endpoint)", err)
	}
	if err := CheckBaseURL("https://gw.example.com/v1"); err != nil {
		t.Errorf("a plain endpoint: %v", err)
	}
	for _, value := range []string{"https://gw.example.com/v1?api-version=2025", "https://gw.example.com/#x", "gw.example.com"} {
		err := CheckBaseURL(value)
		if err == nil {
			t.Errorf("%q was admitted", value)
			continue
		}
		if strings.Contains(err.Error(), "gw.example.com") {
			t.Errorf("the refusal of %q repeats the value: %v", value, err)
		}
	}
}
