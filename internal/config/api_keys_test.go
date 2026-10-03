package config_test

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// WHICH KEY AN ENTRY RUNS ON is one rule, and these are its cases. The
// provider packages used to hold half of it — a fallback to the conventional
// variable whenever the bag they were handed was empty — and they are handed
// the RESOLVED bag, so an entry whose every reference resolved to nothing
// could not be told from one that named none, and ran on the company's
// ANTHROPIC_API_KEY.
func TestAnEntryRunsOnTheKeysItNames(t *testing.T) {
	t.Parallel()
	r := config.NewResolver(config.MapSource{
		"KEY_A":             " sk-a ",
		"KEY_B":             "sk-b",
		"ANTHROPIC_API_KEY": "sk-conventional",
		"OPENAI_API_KEY":    "sk-openai",
	})

	cases := []struct {
		name  string
		entry config.LLMProvider
		keys  []config.APIKey
		bag   []string
	}{
		{
			name:  "references resolve in declaration order, trimmed",
			entry: config.LLMProvider{Type: config.LLMAnthropic, APIKeys: []string{"${KEY_B}", "${KEY_A}"}},
			keys: []config.APIKey{
				{Ref: "KEY_B", Value: "sk-b"},
				{Ref: "KEY_A", Value: "sk-a"},
			},
			bag: []string{"sk-b", "sk-a"},
		},
		{
			name:  "an entry naming no key reads its vendor's conventional variable",
			entry: config.LLMProvider{Type: config.LLMAnthropic},
			keys:  []config.APIKey{{Ref: "ANTHROPIC_API_KEY", Default: true, Value: "sk-conventional"}},
			bag:   []string{"sk-conventional"},
		},
		{
			name:  "the default type is openai",
			entry: config.LLMProvider{},
			keys:  []config.APIKey{{Ref: "OPENAI_API_KEY", Default: true, Value: "sk-openai"}},
			bag:   []string{"sk-openai"},
		},
		{
			// THE BUG THIS RULE REPLACED: ACME_KEY unset, and the
			// entry ran on the conventional key instead.
			name:  "a reference that resolves to nothing stays nothing",
			entry: config.LLMProvider{Type: config.LLMAnthropic, APIKeys: []string{"${ACME_KEY}"}},
			keys:  []config.APIKey{{Ref: "ACME_KEY"}},
			bag:   nil,
		},
		{
			name:  "a duplicate is reported and rotated once",
			entry: config.LLMProvider{Type: config.LLMOpenAI, APIKeys: []string{"${KEY_A}", "sk-a"}},
			keys: []config.APIKey{
				{Ref: "KEY_A", Value: "sk-a"},
				{Inline: true, Value: "sk-a"},
			},
			bag: []string{"sk-a"},
		},
		{
			name:  "a literal with an embedded reference is inline",
			entry: config.LLMProvider{Type: config.LLMOpenAI, APIKeys: []string{"prefix-${KEY_B}"}},
			keys:  []config.APIKey{{Inline: true, Value: "prefix-sk-b"}},
			bag:   []string{"prefix-sk-b"},
		},
		{
			name:  "a cli-agent entry has no conventional key",
			entry: config.LLMProvider{Type: config.LLMCLIAgent},
			keys:  nil,
			bag:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.entry.Keys(r); !reflect.DeepEqual(got, tc.keys) {
				t.Errorf("Keys = %+v, want %+v", got, tc.keys)
			}
			if got := tc.entry.ResolvedKeys(r); !reflect.DeepEqual(got, tc.bag) {
				t.Errorf("ResolvedKeys = %q, want %q", got, tc.bag)
			}
		})
	}
}

// The embedder follows the same rule: an empty field reads OPENAI_API_KEY and
// an unresolved reference stays empty rather than borrowing the chat key.
func TestAnEmbedderRunsOnTheKeyItNames(t *testing.T) {
	t.Parallel()
	r := config.NewResolver(config.MapSource{"OPENAI_API_KEY": "sk-openai", "EMBED_KEY": " sk-embed "})
	for _, tc := range []struct {
		field, want string
	}{
		{"", "sk-openai"},
		{"${EMBED_KEY}", "sk-embed"},
		{"${UNSET_KEY}", ""},
	} {
		e := config.EmbeddingProvider{APIKey: tc.field}
		if got := e.ResolvedKey(r); got != tc.want {
			t.Errorf("api_key %q resolved to %q, want %q", tc.field, got, tc.want)
		}
	}
}
