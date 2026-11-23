package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexRolloutFixture is Codex 0.171.0's rollout of a first turn whose
// standing instructions were `STANDING-A thread-so-far-A`, as the app-server
// wrote it (its base instructions trimmed): line 2 is the developer message
// they open the turn's context with.
const codexRolloutFixture = "testdata/codex/0.171.0/rollout.jsonl"

// renewedText is a launch's standing instructions, with the characters a JSON
// encoder escapes unasked and some it must keep as UTF-8.
const renewedText = "You are the seat <ops> & on call.\nThread so far: \"none\" — ünïcode"

// codexDeveloperLine is a rollout line holding a developer message whose
// content items are kinds, each item's text its kind's name.
func codexDeveloperLine(kinds ...string) string {
	content := make([]map[string]string, len(kinds))
	meta := make([]map[string]any, len(kinds))
	for i, k := range kinds {
		content[i] = map[string]string{"type": "input_text", "text": "OLD " + k}
		meta[i] = map[string]any{"provenance": map[string]string{"type": k}}
	}
	line, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{
		"type": "message", "role": "developer", "content": content,
		"internal_chat_message_metadata_passthrough": map[string]any{
			"content_item_kinds": kinds, "content_item_metadata": meta}}})
	return string(line)
}

// renew runs codex-renew on a rollout holding rollout, handing it text, and
// returns its exit status, stderr and the rollout as it then stands.
func renew(t *testing.T, rollout, text string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	path, config := filepath.Join(dir, "rollout.jsonl"), filepath.Join(dir, "renew.json")
	raw, _ := json.Marshal(renewConfig{Instructions: text})
	for name, body := range map[string]string{path: rollout, config: string(raw)} {
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out, errs bytes.Buffer
	code, once := runOnce(context.Background(), []string{"-adapter", "codex-renew", "-config", config,
		"-rollout", path}, func(string) string { return "" }, strings.NewReader(""), &out, &errs)
	if !once {
		t.Fatal("codex-renew is not a one-shot adapter")
	}
	got, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".renew-*")); len(matches) > 0 {
		t.Errorf("codex-renew left %q beside the rollout", matches)
	}
	return code, errs.String(), string(got)
}

// developerItems is every developer message's content items in rollout, at
// any depth, each as `kind=text`.
func developerItems(t *testing.T, rollout string) []string {
	t.Helper()
	var items []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if v["type"] == "message" && v["role"] == "developer" {
				kinds := v["internal_chat_message_metadata_passthrough"].(map[string]any)["content_item_kinds"].([]any)
				for i, item := range v["content"].([]any) {
					items = append(items, kinds[i].(string)+"="+item.(map[string]any)["text"].(string))
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(rollout, "\n"), "\n") {
		var v any
		if json.Unmarshal([]byte(line), &v) == nil {
			walk(v)
		}
	}
	return items
}

// A CONTINUED CODEX SESSION IS HANDED THIS LAUNCH'S STANDING INSTRUCTIONS:
// codex-renew rewrites the item Codex 0.171.0 recorded them in, wherever it
// recorded them (the first turn's context, a compaction's replacement
// history), to the run's text exactly as written, and leaves every other item
// and every other line byte for byte as it was, its line count too. A run
// with none has the item removed. Codex replays these items on a resume and
// sends nothing from the configuration (measured), so the item is what the
// model reads as current.
//
// Mutation: renew no message inside a list (a replacement history), touch a
// managed item, or re-encode every line, and a row goes red.
func TestACodexRenewHandsTheResumedSessionThisLaunchsInstructions(t *testing.T) {
	t.Parallel()
	fixture, err := os.ReadFile(codexRolloutFixture)
	if err != nil {
		t.Fatal(err)
	}
	permissions := developerItems(t, string(fixture))[1]
	compacted := `{"type":"compacted","payload":{"message":"summary","replacement_history":[` +
		strings.TrimSuffix(strings.TrimPrefix(codexDeveloperLine("generic.developer_instructions"),
			`{"payload":`), `,"type":"response_item"}`) + `]}}`
	for name, c := range map[string]struct {
		rollout, text string
		want          []string
	}{
		"the first turn's context": {string(fixture), renewedText,
			[]string{"generic.developer_instructions=" + renewedText, permissions}},
		"a compaction's replacement history": {compacted + "\n", renewedText,
			[]string{"generic.developer_instructions=" + renewedText}},
		"beside a requirements policy's": {codexDeveloperLine("managed_config.developer_instructions",
			"generic.developer_instructions") + "\n", renewedText,
			[]string{"managed_config.developer_instructions=OLD managed_config.developer_instructions",
				"generic.developer_instructions=" + renewedText}},
		"a run with none": {string(fixture), "", []string{permissions}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			code, stderr, got := renew(t, c.rollout, c.text)
			if code != exitDone {
				t.Fatalf("codex-renew exited %d: %s", code, stderr)
			}
			if items := developerItems(t, got); strings.Join(items, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("the developer items are\n%q\nwant\n%q", items, c.want)
			}
			if strings.Contains(got, "STANDING-A") || strings.Contains(got, "OLD generic") {
				t.Error("an earlier launch's standing instructions are still in the rollout")
			}
			before, after := strings.Split(c.rollout, "\n"), strings.Split(got, "\n")
			if len(before) != len(after) {
				t.Fatalf("the rollout has %d lines, was %d", len(after), len(before))
			}
			for i := range before {
				if before[i] != after[i] && !strings.Contains(before[i], codexInstructionsKind) {
					t.Errorf("line %d changed, and holds no standing instructions:\n%s\n%s", i, before[i], after[i])
				}
			}
		})
	}
}

// A ROLLOUT CODEX-RENEW CANNOT RENEW IS LEFT AS IT WAS, and the helper says
// why and exits 1, so the session gate starts a session of its own rather
// than resume one on an earlier launch's text or on none: a rollout recording
// no standing instructions (resumed, Codex would hand the model none), one
// whose only record of them is a torn line (Codex passes it over), and one
// whose content kinds do not pair with its items.
//
// Mutation: rename a rollout renewed nowhere into place, or skip the kinds'
// pairing check, and a row goes red.
func TestARolloutCodexRenewCannotRenewIsLeftAsItWas(t *testing.T) {
	t.Parallel()
	unpaired := strings.Replace(codexDeveloperLine("generic.developer_instructions", "permissions.instructions"),
		`"content_item_kinds":["generic.developer_instructions","permissions.instructions"]`,
		`"content_item_kinds":["generic.developer_instructions"]`, 1)
	whole := codexDeveloperLine("generic.developer_instructions")
	torn := whole[:strings.Index(whole, `["generic.developer_instructions"]`)+40]
	for name, c := range map[string]struct{ rollout, why string }{
		"none recorded":  {codexDeveloperLine("permissions.instructions") + "\n", "records no standing instructions"},
		"a torn line":    {torn + "\n", "records no standing instructions"},
		"kinds unpaired": {unpaired + "\n", "names 1 content kinds for 2 items"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			code, stderr, got := renew(t, c.rollout, renewedText)
			if code != exitError || !strings.Contains(stderr, c.why) {
				t.Errorf("codex-renew exited %d (%s), want 1 saying %q", code, stderr, c.why)
			}
			if got != c.rollout {
				t.Errorf("the rollout changed:\n%s", got)
			}
		})
	}
}
