package harness

import (
	"strings"
	"testing"
	"time"
)

// shippedEntry is an entry on a shipped profile, unchanged, signed in as auth
// says.
func shippedEntry(t *testing.T, agent string, auth Auth, env map[string]string) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "sub", Harness: agent, StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
		Auth: auth, Env: env,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// A KEY PASTED WHERE THE TOKEN BELONGS IS TOLD FROM THE TOKEN, and the reverse,
// by the prefix the vendor gives each, before any call is made. A value that
// starts with neither is unknown rather than either: a gateway's token or a
// format the vendor changed is no wrong kind, and only the call can say
// whether it signs in.
//
// Mutation: read the kind by the token prefix alone, and the key cases read
// unknown; drop the empty-prefix guard, and every codex value reads as a token.
func TestACredentialsKindIsReadOffItsVendorsPrefix(t *testing.T) {
	t.Parallel()
	claude := shippedEntry(t, "claude-code", Auth{Mode: AuthSubscription}, nil)
	codex := shippedEntry(t, "codex", Auth{Mode: AuthSubscription}, nil)
	for _, tc := range []struct {
		name  string
		p     *Provider
		value string
		want  CredentialKind
	}{
		{"claude token", claude, "sk-ant-oat01-" + strings.Repeat("a", 90), CredentialToken},
		{"claude key", claude, "sk-ant-api03-" + strings.Repeat("b", 90), CredentialAPIKey},
		{"claude, another vendor's key", claude, "sk-proj-" + strings.Repeat("c", 40), CredentialUnknown},
		{"claude, a prefix alone is still that kind", claude, "sk-ant-oat01-", CredentialToken},
		{"claude, nothing", claude, "", CredentialUnknown},
		{"codex key", codex, "sk-proj-" + strings.Repeat("d", 40), CredentialAPIKey},
		{"codex declares no token prefix", codex, "eyJhbGciOi" + strings.Repeat("e", 40), CredentialUnknown},
	} {
		if got := tc.p.CredentialKind(tc.value); got != tc.want {
			t.Errorf("%s: CredentialKind = %q, want %q", tc.name, got, tc.want)
		}
		if !tc.p.CredentialKind(tc.value).Valid() {
			t.Errorf("%s: the kind is not one Valid accepts", tc.name)
		}
	}
	if CredentialKind("bearer").Valid() {
		t.Error("a kind this package never names is Valid")
	}
}

// A HINT TELLS TWO CREDENTIALS APART AND NEVER SPELLS ONE: the vendor's
// prefix where the value starts with it, else its first three characters,
// then the last four. A value too short to keep eight characters out of
// sight is hinted as the ellipsis alone, since its prefix and tail would be
// most of it.
//
// Mutation: drop the hidden minimum, and the short cases print their value;
// take the head from the first three characters always, and the prefixed
// cases lose the kind.
func TestACredentialsHintShowsItsPrefixAndLastFourAndNoMore(t *testing.T) {
	t.Parallel()
	claude := shippedEntry(t, "claude-code", Auth{Mode: AuthSubscription}, nil)
	codex := shippedEntry(t, "codex", Auth{Mode: AuthSubscription}, nil)
	for _, tc := range []struct {
		name  string
		p     *Provider
		value string
		want  string
	}{
		{"token", claude, "sk-ant-oat01-" + strings.Repeat("x", 80) + "Qm4x", "sk-ant-oat01-…Qm4x"},
		{"key", claude, "sk-ant-api03-" + strings.Repeat("y", 80) + "7d2K", "sk-ant-api03-…7d2K"},
		{"no declared prefix", claude, "gw_" + strings.Repeat("z", 30) + "9f0a", "gw_…9f0a"},
		{"codex key", codex, "sk-proj-" + strings.Repeat("w", 40) + "AbCd", "sk-…AbCd"},
		{"shortest that shows", claude, "abc" + strings.Repeat("h", hintHidden) + "1234", "abc…1234"},
		{"one short of showing", claude, "abc" + strings.Repeat("h", hintHidden-1) + "1234", "…"},
		{"a bare prefix", claude, "sk-ant-oat01-abcd", "…"},
		{"short", claude, "hunter2", "…"},
		{"nothing", claude, "", ""},
	} {
		if got := tc.p.CredentialHintOf(tc.value); got != tc.want {
			t.Errorf("%s: CredentialHintOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// THE HINT IS OF THE CREDENTIAL THE MODE HANDS THE HARNESS, read off the child's
// environment: a token under subscription, a key under api-key (never the
// token beside it, which the mode removes), what inherit-env forwards from the
// engine's own environment, and nothing under env, whose sign-in is a set of
// variables rather than one credential, or under a subscription that signs in
// with credential files.
//
// Not parallel: inherit-env reads this process's environment.
//
// Mutation: hint auth.Token whatever the mode, and the api-key case shows the
// token; hint the first credential-named env value under env, and that case
// shows the gateway's.
func TestTheHintIsOfTheCredentialTheModeHandsTheHarness(t *testing.T) {
	token := "sk-ant-oat01-" + strings.Repeat("t", 80) + "TOKN"
	key := "sk-ant-api03-" + strings.Repeat("k", 80) + "KEYS"
	forwarded := "sk-ant-api03-" + strings.Repeat("f", 80) + "FRWD"
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", forwarded)
	for _, tc := range []struct {
		name string
		auth Auth
		env  map[string]string
		want string
	}{
		{"subscription with a token", Auth{Mode: AuthSubscription, Token: token}, nil, "sk-ant-oat01-…TOKN"},
		{"subscription on credential files", Auth{Mode: AuthSubscription}, nil, ""},
		{"api-key", Auth{Mode: AuthAPIKey, Token: token, APIKey: key}, nil, "sk-ant-api03-…KEYS"},
		{"inherit-env", Auth{Mode: AuthInheritEnv, Token: token}, nil, "sk-ant-api03-…FRWD"},
		{"env", Auth{Mode: AuthEnv, Token: token}, map[string]string{
			"ANTHROPIC_AUTH_TOKEN": "gw_" + strings.Repeat("g", 40) + "GATE",
			"ANTHROPIC_BASE_URL":   "https://gateway.example",
		}, ""},
	} {
		p := shippedEntry(t, "claude-code", tc.auth, tc.env)
		if got := p.CredentialHint(); got != tc.want {
			t.Errorf("%s: CredentialHint = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A TOKEN'S LIFETIME IS THE ONE ITS VENDOR DOCUMENTS, and unknown where the
// profile declares none: an expiry computed from a guessed lifetime would
// raise an alarm on a credential that has years left, or none on one that
// lapsed.
//
// Mutation: read the days as hours, or default a missing lifetime, and this
// is red.
func TestATokensLifetimeIsTheOneItsVendorDocuments(t *testing.T) {
	t.Parallel()
	if got := shippedEntry(t, "claude-code", Auth{}, nil).TokenLifetime(); got != 365*24*time.Hour {
		t.Errorf("claude-code TokenLifetime = %v, want 365 days", got)
	}
	if got := shippedEntry(t, "codex", Auth{}, nil).TokenLifetime(); got != 0 {
		t.Errorf("codex TokenLifetime = %v, want zero: it declares no token lifetime", got)
	}
}

// A CANDIDATE IS HINTED BY THE RULE A STORED CREDENTIAL IS, before any provider
// holds it.
func TestHintOfIsTheCredentialHintsRule(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]string{
		"sk-ant-api03-abcdefghijklmnop7d2K": "sk-ant-api03-…7d2K",
		"plainvalue-abcdefghij-wxyz":        "pla…wxyz",
		"short":                             "…",
		"":                                  "",
	} {
		if got := HintOf("sk-ant-api03-", value); got != want {
			t.Errorf("HintOf(%q) = %q, want %q", value, got, want)
		}
	}
}
