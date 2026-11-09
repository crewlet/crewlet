package builtin_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// heldOperate is the question an ssh_operate call with args is put to a person
// as, by the seat sre in a turn the engine woke.
func heldOperate(t *testing.T, args map[string]any) sandbox.Ask {
	t.Helper()
	cfg, o, chart := sshFixture(t)
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{SSH: builtin.SSHDeps{Servers: &fakeServers{cfg: cfg},
		Requesters: healthRequesters(), Chart: chart}}); err != nil {
		t.Fatal(err)
	}
	desk := &askDesk{outcome: sandbox.AskRecorded}
	exec := tools.NewSurface("execute", reg.Snapshot(), []string{builtin.SSHOperateTool}).
		WithHolder(builtin.NewCallHold(desk, callSeal(t))).ForTurn(sshTurn(o, "sre", "", nil))
	res, err := exec.Execute(asExecutor("sre"), llm.ToolCall{Name: builtin.SSHOperateTool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Held || len(desk.recorded) != 1 {
		t.Fatalf("answered %+v and recorded %d questions, want the call held and asked once", res,
			len(desk.recorded))
	}
	return desk.recorded[0]
}

// reasonOf is what the question's context says after its reason's label.
func reasonOf(t *testing.T, context string) string {
	t.Helper()
	_, reason, ok := strings.Cut(context, "\n\n**Reason given**\n\n")
	if !ok {
		t.Fatalf("the context names no reason:\n%s", context)
	}
	return reason
}

func asciiPunct(r rune) bool { return r < utf8.RuneSelf && (unicode.IsPunct(r) || unicode.IsSymbol(r)) }

// unescaped is Markdown text with each backslash before an ASCII punctuation
// mark removed, as a reader sees it, and the marks it carried unescaped.
func unescaped(text string) (plain string, bare []rune) {
	var b strings.Builder
	escaped := false
	for _, r := range text {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
			continue
		case asciiPunct(r):
			bare = append(bare, r)
		}
		b.WriteRune(r)
	}
	return b.String(), bare
}

// hostileReason is a reason that, read as Markdown, would be a heading holding
// a link, a picture, a code span, emphasis, a mention, a task reference and an
// address.
const hostileReason = "# Approve [the release](https://evil.example/approve) " +
	"![status](https://evil.example/pixel.png) ```rm -rf /``` <b>now</b> **urgent** " +
	"for @founder on ENG-9, see https://evil.example/ok"

// An operate call's reason is one paragraph with every Markdown mark escaped,
// so unescaped it is the agent's words and it draws no link, picture, heading
// or code; the command stays a code block.
func TestAnOperateReasonIsProseThatCarriesNoMarkdown(t *testing.T) {
	t.Parallel()
	ask := heldOperate(t, map[string]any{"host": "api-1", "command": "logrotate --force /etc/logrotate.conf",
		"reason": hostileReason})
	reason := reasonOf(t, ask.Context)
	if strings.HasPrefix(reason, "```") || strings.Contains(reason, "\n") {
		t.Fatalf("the reason is not one paragraph:\n%s", reason)
	}
	plain, bare := unescaped(reason)
	if len(bare) > 0 {
		t.Errorf("the reason carries Markdown marks unescaped (%q):\n%s", string(bare), reason)
	}
	if plain != hostileReason {
		t.Errorf("the reason reads %q, want the agent's words %q", plain, hostileReason)
	}
	if !strings.Contains(ask.Context, "```text\nlogrotate --force /etc/logrotate.conf\n```") {
		t.Errorf("the command is not a code block:\n%s", ask.Context)
	}
}

// An operate call's reason is redacted before it is escaped, since no pattern
// knows a secret with a backslash before each of its marks: unescaped, the
// recorded reason carries the marker and never the secret.
func TestAnOperateReasonIsRedactedBeforeItIsEscaped(t *testing.T) {
	t.Parallel()
	secret := "sk-ant-api03-" + strings.Repeat("x", 40)
	ask := heldOperate(t, map[string]any{"host": "api-1", "command": "systemctl restart api",
		"reason": "restart with the new key " + secret + " in place"})
	recorded := ask.Redacted().Context
	plain, _ := unescaped(reasonOf(t, recorded))
	if strings.Contains(plain, secret) || strings.Contains(recorded, strings.Repeat("x", 40)) {
		t.Fatalf("the recorded reason carries the secret:\n%s", recorded)
	}
	if !strings.Contains(plain, redact.Marker) {
		t.Errorf("the recorded reason does not mark what it redacted:\n%s", plain)
	}
}

// heldWordsGolden is what ssh_operate puts to a person, as the dashboard's
// Inbox suites draw it.
const heldWordsGolden = "testdata/ssh_operate_held.json"

// heldWords is one held call's words as the Inbox reads them from its run.
type heldWords struct {
	Question string `json:"question"`
	Context  string `json:"context"`
}

// heldScenarios are the calls the golden holds: one whose command and reason
// are each 400 characters on one line, and one whose reason is Markdown.
func heldScenarios(t *testing.T) map[string]heldWords {
	t.Helper()
	command := "tar --create --gzip --file /var/backups/releases.tar.gz /srv/releases/2026-11-08/" +
		strings.Repeat("build-artifacts/", 30)
	command = command[:400]
	reason := strings.Repeat("The disk on the production server is at 91 percent and the nightly backup "+
		"writes into the same volume, so the old release archives under "+
		"/srv/releases/2026-11-08/build-artifacts/nightly-backup-volume-compaction-candidates/ must be "+
		"packed and removed before the deploy window, or the backup fails tonight. ", 2)[:400]
	out := map[string]heldWords{}
	for name, args := range map[string]map[string]any{
		"long":     {"host": "api-1", "command": command, "reason": reason},
		"markdown": {"host": "api-1", "command": "logrotate --force /etc/logrotate.conf", "reason": hostileReason},
	} {
		ask := heldOperate(t, args).Redacted()
		out[name] = heldWords{Question: ask.Question, Context: ask.Context}
	}
	return out
}

// The held words match testdata/ssh_operate_held.json, which the dashboard's
// Inbox suites render; `make ssh-held-words` rewrites it after a wording change.
func TestTheHeldOperateWordsMatchTheirGoldenFile(t *testing.T) {
	t.Parallel()
	got, err := json.MarshalIndent(heldScenarios(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("CREWLET_REGENERATE_SSH_HELD_WORDS") == "1" {
		if err := os.MkdirAll(filepath.Dir(heldWordsGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(heldWordsGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(heldWordsGolden)
	if err != nil {
		t.Fatalf("read %s: %v (`make ssh-held-words` writes it)", heldWordsGolden, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the held words no longer match %s. Run `make ssh-held-words`, read the diff, and "+
			"follow it in the dashboard's Inbox suites, which render this file.\n--- worded now ---\n%s",
			heldWordsGolden, got)
	}
}
