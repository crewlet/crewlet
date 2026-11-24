package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/shellword"
)

// codexProjectDocMaxBytes is Codex 0.171.0's own bound on the instructions a
// session reads from a project where no configuration sets
// project_doc_max_bytes (measured: a 240,000-byte AGENTS.md reached the model
// as its first 32,768 bytes, and `config/read` reports 32768). A run takes the
// value Codex reports for its start directory ([codexGuard]), this one only
// where Codex reported none.
const codexProjectDocMaxBytes = 32 << 10

// codexCheckouts is what Codex 0.171.0 reads from a repository's root started
// there: its AGENTS.override.md, else AGENTS.md, within limit, the run's
// project_doc_max_bytes, and the skills of its .codex/skills and
// .agents/skills.
func codexCheckouts(limit int) checkoutSpec {
	return checkoutSpec{Instructions: []checkoutSlot{{Names: []string{"AGENTS.override.md", "AGENTS.md"}}}, Limit: &limit,
		SkillDirs: []string{".codex/skills", ".agents/skills"}}
}

// codexCheckoutHome is where a Codex run keeps its checkouts' instructions
// and skills ([checkoutHome]): CODEX_HOME/AGENTS.md, the home's own held
// beside the run ([codexRestoreHome]), and CODEX_HOME's skills, each copied as
// `<repository>:<skill>`.
func codexCheckoutHome(paths Paths) checkoutHome {
	return checkoutHome{state: paths.CodexRepo(), helper: paths.CodexNoteHelper(), launch: paths.CodexCheckoutLaunch(),
		agents: paths.CodexHome() + "/AGENTS.md", held: paths.CodexHomeInstructions(), sections: paths.CodexRepoAgents(),
		skills: paths.CodexSkills(), staging: checkoutStaging(paths.CodexSkills()), copied: paths.CodexRepoSkills()}
}

// CodexCheckoutLaunch is the run's checkout-launch config, which its launch
// writes ([codexCheckouts]) and its script hands the note helper.
func (p Paths) CodexCheckoutLaunch() string { return p.WorkDir() + "/codex-checkout-launch.json" }

// codexNestedTimeout is the nested hook's bound, in seconds: Claude Code's
// hook bound (ClaudeHookTimeout). A firing reads a few small files and copies
// skills within the note helper's per-firing bound, which took 0.12 s at its
// largest on the build VM (internal/notehelper's maxCopyBytes).
const codexNestedTimeout = 10

// codexNestedContextLimit is the most tokens of a nested hook's context Codex
// hands the model whole (`additionalContextLimit`), the rest spilled to a file
// as Codex spills any hook's: set past its 2,500 default to the default
// [codexProjectDocMaxBytes], in tokens, which one checkout's instructions under
// the default limit never pass. A limit the configuration raises, or several
// checkouts cloned at once, can pass it; the trust hash holds this value.
const codexNestedContextLimit = codexProjectDocMaxBytes

// codexNestedHookKey is the trust key Codex 0.171.0 gives the first
// PostToolUse hook its session flags declare: a fixed path, whatever the box
// (measured: `hooks/list` names it, and the hook runs only under it).
const codexNestedHookKey = "/<session-flags>/config.toml:post_tool_use:0:0"

// codexNestedCommand is the nested hook's command: the run's note helper as
// the one-shot `codex-nested` adapter, reading the state checkout-launch wrote
// ([codexCheckoutHome]).
func codexNestedCommand(paths Paths) string {
	q := shellword.Quote
	return q(paths.CodexNoteHelper()) + " -adapter codex-nested -config " + q(paths.CodexRepo()+"/nested.json")
}

// codexHookHash is the trust hash Codex 0.171.0 computes for a PostToolUse
// hook matching every tool with command (codex-rs/hooks discovery.rs
// hook_hash): "sha256:" and the SHA-256 of the hook's identity as JSON, every
// object's keys sorted, no space, no HTML escaping. A hook runs only under
// its hash ([codexNestedHookFlags]).
func codexHookHash(command string, timeout, contextLimit int) string {
	handler := map[string]any{"type": "command", "command": command, "timeout": timeout, "async": false}
	if contextLimit != 2500 {
		handler["additionalContextLimit"] = contextLimit
	}
	identity := map[string]any{"event_name": "post_tool_use", "matcher": ".*", "hooks": []any{handler}}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(identity) // maps encode with sorted keys and cannot fail here
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// codexNestedHookFlags is the app-server's `-c` overrides that declare the
// nested hook and trust it by its hash, as session flags, the one layer the
// run alone writes, so no other layer's trust changes for it.
func codexNestedHookFlags(paths Paths) (hook, state string) {
	command := codexNestedCommand(paths)
	hook = fmt.Sprintf(`hooks.PostToolUse=[{matcher=".*",hooks=[{type="command",command=%s,timeout=%d,additionalContextLimit=%d}]}]`,
		tomlString(command), codexNestedTimeout, codexNestedContextLimit)
	state = fmt.Sprintf(`hooks.state={%s={trusted_hash=%s}}`, tomlString(codexNestedHookKey),
		tomlString(codexHookHash(command, codexNestedTimeout, codexNestedContextLimit)))
	return hook, state
}
