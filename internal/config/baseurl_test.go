package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// baseURLDocs is a value written as the base_url of every provider kind that
// reads one, each with the path its refusal is located at.
func baseURLDocs(value string) []struct{ yaml, path string } {
	return []struct{ yaml, path string }{
		{"name: Acme\nproviders:\n  llm:\n    claude:\n      type: anthropic\n      model: claude-opus-5-5\n" +
			"      base_url: \"" + value + "\"\n", "providers.llm.claude.base_url"},
		{"name: Acme\nproviders:\n  llm:\n    gpt:\n      type: openai\n      model: gpt-5\n" +
			"      base_url: \"" + value + "\"\n", "providers.llm.gpt.base_url"},
		{"name: Acme\nproviders:\n  llm:\n    local:\n      type: openai-compatible\n      model: m\n" +
			"      base_url: \"" + value + "\"\n", "providers.llm.local.base_url"},
		{"name: Acme\nproviders:\n  embeddings:\n    type: openai-compatible\n    model: text-embedding-3-small\n" +
			"    base_url: \"" + value + "\"\n", "providers.embeddings.base_url"},
	}
}

// A base_url NO CALL CAN BE SENT ON IS A RUNNABLE REFUSAL, at the field, on
// every type that reads one and on the embeddings provider: no build runs a
// provider whose every request has no host to go to, so an apply refuses it
// too. The refusal never repeats the value.
//
// Mutation: drop either CheckBaseURL call from the runnable validation, and
// that block's cases apply clean.
func TestABaseURLNoCallCanBeSentOnIsRefusedOnApply(t *testing.T) {
	t.Parallel()
	const secret = "sk-in-the-value"
	for name, value := range map[string]string{
		"no scheme":                      "gw.example.com/v1?key=" + secret,
		"a host and port with no scheme": "localhost:4000",
		"a scheme that is not http(s)":   "ftp://gw.example.com/" + secret,
		"a scheme with no host":          "https:///" + secret,
		"userinfo with no scheme":        "ops:" + secret + "@gw.example.com",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, doc := range baseURLDocs(value) {
				cfg, err := ParseCompanyDocument([]byte(doc.yaml))
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				err = cfg.ValidateRunnable()
				if err == nil || !strings.Contains(err.Error(), doc.path) {
					t.Fatalf("ValidateRunnable() = %v, want a refusal at %s", err, doc.path)
				}
				if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "gw.example.com") {
					t.Errorf("the refusal repeats the value:\n%v", err)
				}
				if !errors.Is(err, ErrUnknownValue) {
					t.Errorf("the refusal is not ErrUnknownValue:\n%v", err)
				}
			}
		})
	}
}

// WHAT A BACKEND NEVER SENDS IS AN ADMISSION RULE: a query or a fragment is
// refused on every write — both SDKs resolve each request's path against the
// base and that drops them (anthropic.TestABaseURLsQueryIsNeverSent and
// openai.TestABaseURLsQueryIsNeverSent measure it) — and a stored revision
// carrying one is APPLIED with a warning, because the entry runs: every call
// is sent, only without the part, and a later build may send it, so refusing
// it at apply would split a rolling upgrade's epoch. A value written around
// `${VAR}` references is judged on its own text. The refusal never repeats
// the value.
//
// Mutation: drop the BaseURLUnsent loop from either validateDials, and that
// block's cases write clean; move it into the runnable validate, and the
// apply refuses them.
func TestABaseURLPartTheSDKDropsIsRefusedOnWriteAndWarnedOnApply(t *testing.T) {
	t.Parallel()
	const secret = "sk-in-the-query"
	for name, value := range map[string]string{
		"a query":                         "https://gw.example.com/v1?key=" + secret,
		"an empty query":                  "https://gw.example.com/v1?",
		"a fragment":                      "https://gw.example.com/v1#" + secret,
		"an empty fragment":               "https://gw.example.com/v1#",
		"a query after a reference":       "${LLM_BASE_URL}?key=" + secret,
		"a fragment after a reference":    "${LLM_BASE_URL}/v1#" + secret,
		"a query written before the path": "https://gw.example.com?key=" + secret + "/v1",
		"a query with surrounding space":  "  https://gw.example.com/v1?key=" + secret + "  ",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, doc := range baseURLDocs(value) {
				assertAdmissionOnly(t, doc.yaml, doc.path)
				err := rejects(t, doc.yaml, doc.path)
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the refusal repeats the value's query or fragment:\n%v", err)
				}
			}
		})
	}
}

// USERINFO IS UNSENT EXACTLY WHERE A BEARER KEY TAKES THE HEADER. openai-go
// sends any key it holds as `Authorization: Bearer`, and Go's HTTP client sends
// a URL's userinfo as Basic credentials only on a request with no
// Authorization of its own — so on an openai, openai-compatible or embeddings
// entry that names a key, the gateway credential in the URL never leaves the
// process (engine.TestWhereABaseURLsUserinfoIsSent measures every backend).
// Such an entry is refused on write and applied with a warning; the same URL
// on an entry naming no key, and on an anthropic entry, whose key travels as
// `x-api-key`, loads. A userinfo written around references is judged too.
//
// Mutation: make bearerKeyed always false, and the keyed cases write clean;
// always true, and the keyless and anthropic cases are refused.
func TestUserinfoBesideABearerKeyIsRefusedOnWrite(t *testing.T) {
	t.Parallel()
	const secret = "gw-password"
	for _, value := range []string{
		"https://gw-user:" + secret + "@gw.example.com/v1",
		"https://${GW_USER}:${GW_PASS}@gw.example.com/v1",
	} {
		refused := []struct{ yaml, path string }{
			{"name: Acme\nproviders:\n  llm:\n    gpt:\n      type: openai\n      model: gpt-5\n" +
				"      api_keys: [\"${KEY}\"]\n      base_url: \"" + value + "\"\n", "providers.llm.gpt.base_url"},
			{"name: Acme\nproviders:\n  llm:\n    local:\n      type: openai-compatible\n      model: m\n" +
				"      api_keys: [\"${KEY}\"]\n      base_url: \"" + value + "\"\n", "providers.llm.local.base_url"},
			{"name: Acme\nproviders:\n  embeddings:\n    type: openai-compatible\n    model: text-embedding-3-small\n" +
				"    api_key: \"${KEY}\"\n    base_url: \"" + value + "\"\n", "providers.embeddings.base_url"},
		}
		for _, doc := range refused {
			assertAdmissionOnly(t, doc.yaml, doc.path)
			err := rejects(t, doc.yaml, doc.path)
			if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "userinfo") {
				t.Errorf("%s: the refusal repeats the password or names no userinfo:\n%v", value, err)
			}
		}
		for _, doc := range []string{
			"name: Acme\nproviders:\n  llm:\n    gpt:\n      type: openai\n      model: gpt-5\n" +
				"      base_url: \"" + value + "\"\n",
			"name: Acme\nproviders:\n  llm:\n    local:\n      type: openai-compatible\n      model: m\n" +
				"      base_url: \"" + value + "\"\n",
			"name: Acme\nproviders:\n  embeddings:\n    type: openai-compatible\n    model: text-embedding-3-small\n" +
				"    base_url: \"" + value + "\"\n",
			"name: Acme\nproviders:\n  llm:\n    claude:\n      type: anthropic\n      model: claude-opus-5-5\n" +
				"      api_keys: [\"${KEY}\"]\n      base_url: \"" + value + "\"\n",
		} {
			mustCompany(t, doc)
		}
	}
}

// AND WHAT AN SDK DOES SEND LOADS: a path, a port, a value whose only unknowns
// are its references, which the engine judges once they resolve, and space
// around any of them, which the engine trims before it judges or sends.
func TestABaseURLTheSDKSendsLoads(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"https://gw.example.com",
		"https://gw.example.com/v1/",
		"http://127.0.0.1:8317",
		"  https://gw.example.com/v1  ",
		"${LLM_BASE_URL}",
		"${LLM_PROXY_URL}/v1",
		"https://${GATEWAY_HOST}/v1",
	} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			for _, doc := range baseURLDocs(value) {
				mustCompany(t, doc.yaml)
			}
		})
	}
}

// THE ENGINE'S ONE RESOLUTION: what is judged is what is sent. The value is
// expanded and trimmed ONCE, and that string is the URL — a stored value with
// a trailing CR or a leading space passed a trimmed check and then failed
// every call with the whole URL, userinfo included, in the SDK's parse error.
// No call can be sent on it: refused naming the variable, never the value.
// What its backend drops is REVEALED only where the written text did not
// already say so — a part a variable carried, or userinfo made unsent by the
// conventional OPENAI_API_KEY an entry naming no api_keys resolved — because
// the written text's own are the applied revision's admission warnings.
//
// Mutation: hand back the untrimmed expansion, and the CR case keeps its CR;
// report every unsent part rather than the revealed ones, and the literal
// query is reported twice.
func TestResolveBaseURLJudgesTheStringItHandsOn(t *testing.T) {
	t.Parallel()
	const secret = "sk-in-the-query"
	r := NewResolver(MapSource{
		"LLM_BASE_URL":   "https://gw.example.com/v1\r",
		"LLM_QUERY_URL":  " https://gw.example.com/v1?key=" + secret,
		"LLM_NO_SCHEME":  "gw.example.com/v1?key=" + secret,
		"LLM_USERINFO":   "https://gw-user:gw-pass@gw.example.com/v1",
		"OPENAI_API_KEY": "sk-conventional",
	})
	got, err := (&LLMProvider{Type: LLMAnthropic, BaseURL: "${LLM_BASE_URL}"}).ResolveBaseURL(r)
	if err != nil || got.URL != "https://gw.example.com/v1" || len(got.Revealed) != 0 {
		t.Errorf("a CR-terminated value = %+v, %v; want the trimmed URL, nothing revealed", got, err)
	}
	if !slices.Equal(got.From, []string{"LLM_BASE_URL"}) {
		t.Errorf("From = %v, want the variable it was resolved from", got.From)
	}

	got, err = (&LLMProvider{Type: LLMOpenAI, BaseURL: "${LLM_QUERY_URL}"}).ResolveBaseURL(r)
	if err != nil || got.URL != "https://gw.example.com/v1?key="+secret ||
		!slices.Equal(got.Revealed, []BaseURLPart{BaseURLQuery}) {
		t.Errorf("a query a variable carried = %+v, %v; want it revealed and the entry built", got, err)
	}
	got, _ = (&LLMProvider{Type: LLMOpenAI, BaseURL: "https://gw.example.com/v1?key=" + secret}).ResolveBaseURL(r)
	if len(got.Revealed) != 0 {
		t.Errorf("a written query revealed %v, want nothing: the admission warning already names it", got.Revealed)
	}

	_, err = (&LLMProvider{Type: LLMOpenAI, BaseURL: "${LLM_NO_SCHEME}"}).ResolveBaseURL(r)
	if err == nil || !strings.Contains(err.Error(), "LLM_NO_SCHEME") ||
		strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "gw.example.com") {
		t.Errorf("a resolved value no call can be sent on: %v, want a refusal naming the "+
			"variable and not the value", err)
	}

	// The conventional key: an openai entry naming no api_keys reads
	// OPENAI_API_KEY, which no write could see, and that key is what takes
	// the Authorization header from the userinfo.
	got, err = (&LLMProvider{Type: LLMOpenAI, BaseURL: "${LLM_USERINFO}"}).ResolveBaseURL(r)
	if err != nil || !slices.Equal(got.Revealed, []BaseURLPart{BaseURLUserinfo}) {
		t.Errorf("userinfo beside the conventional key = %+v, %v; want it revealed", got, err)
	}
	keyless := NewResolver(MapSource{"LLM_USERINFO": "https://gw-user:gw-pass@gw.example.com/v1"})
	if got, _ := (&LLMProvider{Type: LLMOpenAI, BaseURL: "${LLM_USERINFO}"}).ResolveBaseURL(keyless); len(got.Revealed) != 0 {
		t.Errorf("userinfo on an entry running on no key revealed %v: it IS sent, as Basic", got.Revealed)
	}
	if got, _ := (&LLMProvider{Type: LLMAnthropic, BaseURL: "${LLM_USERINFO}"}).ResolveBaseURL(r); len(got.Revealed) != 0 {
		t.Errorf("userinfo on an anthropic entry revealed %v: its key is x-api-key, so the userinfo is sent", got.Revealed)
	}
	if got, _ := (&EmbeddingProvider{BaseURL: "${LLM_USERINFO}"}).ResolveBaseURL(r); !slices.Equal(got.Revealed, []BaseURLPart{BaseURLUserinfo}) {
		t.Errorf("an embedder's userinfo beside the conventional key revealed %v, want userinfo", got.Revealed)
	}

	if got, err := (&LLMProvider{Type: LLMOpenAI, BaseURL: "${UNSET_BASE_URL}"}).ResolveBaseURL(r); err != nil ||
		got.URL != "" || !slices.Equal(got.Missing, []string{"UNSET_BASE_URL"}) {
		t.Errorf("an unresolved reference = %+v, %v; want no URL and the variable missing", got, err)
	}
}

// THE INERT DIALS ARE ADMISSION RULES TOO: each is a setting the entry's
// backend never sends, so the entry runs on every build and a later one may
// start sending it. Refused on write, applied with a warning.
//
// Mutation: move any of these back into LLMProvider.validate, and its case
// fails ValidateRunnable.
func TestADialTheBackendNeverSendsIsAnAdmissionRule(t *testing.T) {
	t.Parallel()
	const llm = "name: Acme\nproviders:\n  llm:\n    p:\n"
	for name, tc := range map[string]struct{ entry, path string }{
		"base_url on a cli-agent entry": {"      type: cli-agent\n      model: m\n      cli: {agent: claude-code}\n" +
			"      base_url: https://gw.example.com\n", "providers.llm.p.base_url"},
		"reasoning on a cli-agent entry": {"      type: cli-agent\n      model: m\n      cli: {agent: claude-code}\n" +
			"      reasoning: true\n", "providers.llm.p.reasoning"},
		"reasoning_effort on a cli-agent entry": {"      type: cli-agent\n      model: m\n      cli: {agent: claude-code}\n" +
			"      reasoning_effort: high\n", "providers.llm.p.reasoning_effort"},
		"a thinking budget on a cli-agent entry": {"      type: cli-agent\n      model: m\n      cli: {agent: claude-code}\n" +
			"      reasoning_budget_tokens: 2048\n", "providers.llm.p.reasoning_budget_tokens"},
		"reasoning on an anthropic entry": {"      type: anthropic\n      model: claude-opus-5-5\n      reasoning: true\n",
			"providers.llm.p.reasoning"},
		"reasoning_effort with reasoning off": {"      type: openai\n      model: gpt-5\n      reasoning_effort: high\n",
			"providers.llm.p.reasoning_effort"},
		"a thinking budget on an openai entry": {"      type: openai\n      model: gpt-5\n      reasoning_budget_tokens: 2048\n",
			"providers.llm.p.reasoning_budget_tokens"},
		"claude_model on an openai entry": {"      type: openai\n      model: gpt-5\n      claude_model: claude-haiku-4-5\n",
			"providers.llm.p.claude_model"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assertAdmissionOnly(t, llm+tc.entry, tc.path)
		})
	}
}
