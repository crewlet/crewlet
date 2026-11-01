package harnessprofile

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// A SIGN-IN OR LIMIT FIELD THE ENGINE CANNOT USE IS REFUSED, NAMING ITSELF. Each
// of these is read on a path where a wrong value is silent: a sign_in_env name
// that is the token's own variable would be deleted by the very mode that reads
// it, a device pattern with no capture group finds no code, a window the map
// writes that is no window names a limit nobody can act on, and an event matched
// on its type alone could be one carrying the model's words.
//
// Mutation: drop validateSignIn or validateEventReports from problems(), and
// every case loads.
func TestASignInOrLimitFieldTheEngineCannotUseIsRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		overrides map[string]any
		want      string
	}{
		"sign_in_env naming the token variable": {
			overrides: map[string]any{"sign_in_env": []any{"CLAUDE_CODE_OAUTH_TOKEN"}},
			want:      "sign_in_env[0]",
		},
		"sign_in_env outside the grammar": {
			overrides: map[string]any{"sign_in_env": []any{"MY-TOKEN"}},
			want:      "not an environment variable name",
		},
		"overlapping prefixes": {
			overrides: map[string]any{"token_prefix": "sk-ant-", "api_key_prefix": "sk-ant-api03-"},
			want:      "overlap",
		},
		"a negative lifetime": {
			overrides: map[string]any{"token_lifetime_days": -1},
			want:      "token_lifetime_days",
		},
		"a device login with no patterns": {
			overrides: map[string]any{"device_login_args": []any{"login", "--device"}},
			want:      "device_code_patterns",
		},
		"a device pattern with no capture group": {
			overrides: map[string]any{"device_login_args": []any{"login"},
				"device_code_patterns": map[string]any{"url": `https://\S+`, "code": `([A-Z]{4})`}},
			want: "no capture group",
		},
		"a reset event matched on its type alone": {
			overrides: map[string]any{"limit_reset_events": []any{map[string]any{
				"match":     []any{map[string]any{"path": []any{"type"}, "equals": "rate_limit_event"}},
				"resets_at": map[string]any{"path": []any{"resetsAt"}, "unit": "epoch"}}}},
			want: "limit_reset_events[0].match",
		},
		"a reset in no unit": {
			overrides: map[string]any{"limit_reset_events": []any{map[string]any{
				"match": []any{map[string]any{"path": []any{"type"}, "equals": "rate_limit_event"},
					map[string]any{"path": []any{"status"}, "equals": "rejected"}},
				"resets_at": map[string]any{"path": []any{"resetsAt"}, "unit": "fortnights"}}}},
			want: "resets_at.unit",
		},
		"a window that is no window": {
			overrides: map[string]any{"limit_windows": []any{map[string]any{"contains": "weekly limit", "window": "monthly"}}},
			want:      "limit_windows[0].window",
		},
		"an auth event matched on its type alone": {
			overrides: map[string]any{"auth_events": []any{map[string]any{
				"match": []any{map[string]any{"path": []any{"type"}, "equals": "result"}}}}},
			want: "auth_events[0].match",
		},
		"models_docs that is not https": {
			overrides: map[string]any{"models_docs": "http://example.com/models"},
			want:      "models_docs",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load("claude-code", tc.overrides)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// THE SHIPPED PROFILE NAMES ITS WINDOWS IN THE ENGINE'S WORDS, and reads a
// sentence's window the way the docs say.
func TestASentencesWindowIsReadByItsWording(t *testing.T) {
	t.Parallel()
	p, err := Load("claude-code", nil)
	if err != nil {
		t.Fatal(err)
	}
	for said, want := range map[string]llm.LimitWindow{
		"You've hit your weekly limit · resets Mon 9am":    llm.LimitWeekly,
		"You've hit your 5-hour limit · resets 4:30pm":     llm.LimitSession,
		"You've hit your Opus limit · resets Nov 3, 9am":   llm.LimitModel,
		"You've hit your limit, whatever it is, try later": llm.LimitUnknown,
	} {
		if got := p.ReadsLimitWindow(said); got != want {
			t.Errorf("ReadsLimitWindow(%q) = %q, want %q", said, got, want)
		}
	}
}

// THE INSTALL RECORD'S NAME IS THE ENGINE'S. A login file under it would be
// read as the record, and the record carried as a login.
func TestNoLoginFileMayTakeTheInstallRecordsName(t *testing.T) {
	t.Parallel()
	_, err := Load("claude-code", map[string]any{"credential_paths": []any{".claude/" + LoginInstallFile}})
	if err == nil || !strings.Contains(err.Error(), LoginInstallFile) {
		t.Errorf("Load = %v, want a refusal naming %s", err, LoginInstallFile)
	}
}
