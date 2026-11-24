package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// codexProjectDocMaxBytes is Codex 0.171.0's own bound on the instructions a
// session reads from a project where no configuration sets
// project_doc_max_bytes (measured: a 240,000-byte AGENTS.md reached the model
// as its first 32,768 bytes, and `config/read` reports 32768). A run takes the
// value Codex reports for its start directory ([codexGuard]), this one only
// where Codex reported none.
const codexProjectDocMaxBytes = 32 << 10

// codexCheckoutLaunch is the note helper's checkout-launch config for a Codex
// run (internal/notehelper's "Checkouts below the working directory"): what
// Codex 0.171.0 reads from a repository's root started there, its
// AGENTS.override.md, else AGENTS.md, within limit, the run's
// project_doc_max_bytes, and the skills of its .codex/skills and
// .agents/skills, copied into CODEX_HOME's skills as `<repository>:<skill>`,
// the run's own namespace no repository's, listed in [Paths.CodexRepoSkills];
// the repositories the run pushes to first.
func codexCheckoutLaunch(paths Paths, repos []string, limit int) []byte {
	launch, _ := json.Marshal(map[string]any{"state": paths.CodexRepo(), "agents": paths.CodexHome() + "/AGENTS.md",
		"first": repos, "instructions": [][]string{{"AGENTS.override.md", "AGENTS.md"}}, "limit": limit,
		"skillDirs": []string{".codex/skills", ".agents/skills"}, "skills": paths.CodexSkills(),
		"copied": paths.CodexRepoSkills(), "reserved": []string{sandbox.SkillNamespace}})
	return launch // strings, a list and an int cannot fail to encode
}

// CodexCheckoutLaunch is the run's [codexCheckoutLaunch], which its launch
// writes and its script hands the note helper ([codexRepoGate]).
func (p Paths) CodexCheckoutLaunch() string { return p.WorkDir() + "/codex-checkout-launch.json" }

// codexRepoGate is the part of a Codex run's script that hands the harness
// the instructions and skills of each repository checked out directly under
// its working directory, which Codex started there does not read, and sets hk
// where the box holds the note helper the nested hook runs: the helper's
// checkout-launch ([Paths.CodexCheckoutLaunch]) writes each checkout's root
// instructions into CODEX_HOME/AGENTS.md, after the home's own (held,
// [codexRestoreHome]), and copies their skills after removing an earlier
// run's.
func codexRepoGate(paths Paths) string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`rr=%s; rh=%s; rk=%s; ra=%s; rha=%s; rw=%s; hk=`, q(paths.CodexRepo()), q(paths.CodexNoteHelper()),
		q(paths.CodexSkills()), q(paths.CodexHome()+"/AGENTS.md"), q(paths.CodexHomeInstructions()), q(paths.CodexRepoSkills()))
	line(`mkdir -p "$rr" || exit 1`)
	line(`if [ ! -e "$rha" ]; then if [ -e "$ra" ]; then cp "$ra" "$rha"; else : > "$rha"; fi || exit 1; fi`)
	line(`if [ -f "$rw" ]; then while IFS= read -r rl; do case "$rl" in ""|*/*|.|..) ;; *) rm -rf "$rk/$rl";; esac; done < "$rw"; fi`)
	line(`: > "$rw" && rm -rf "$rr" "$ra" && mkdir -p "$rr" || exit 1`)
	line(`if [ -f "$rh" ]; then`)
	line(`  chmod 700 "$rh" || exit 1`)
	line(`  "$rh" -adapter checkout-launch -config %s < /dev/null || { echo 'crewlet: the run could not hand its repositories their instructions' >&2; exit 1; }`,
		q(paths.CodexCheckoutLaunch()))
	line(`  hk=1`)
	line(`else`)
	line(`  echo 'crewlet: this box has no note helper for its machine, so the run starts without the instructions and skills of its repositories, and without the hook that hands a checkout cloned during it its instructions and skills' >&2`)
	line(`fi`)
	line(`if [ -s "$rha" ]; then { cat "$rha"; if [ -s "$ra" ]; then printf '\n\n'; cat "$ra"; fi; } > "$ra.crewlet" && mv -f "$ra.crewlet" "$ra" || exit 1; fi`)
	return b.String()
}

// codexNestedTimeout is the nested hook's bound, in seconds: Claude Code's
// hook bound (ClaudeHookTimeout), the same reads of a few small files.
const codexNestedTimeout = 10

// codexNestedContextLimit is the most tokens of a nested hook's context Codex
// hands the model whole (`additionalContextLimit`); past its 2,500 default
// Codex spills the rest to a file, so it is set to one checkout's
// [codexProjectDocMaxBytes] in bytes, which no more tokens can hold.
const codexNestedContextLimit = codexProjectDocMaxBytes

// codexNestedHookKey is the trust key Codex 0.171.0 gives the first
// PostToolUse hook its session flags declare: a fixed path, whatever the box
// (measured: `hooks/list` names it, and the hook runs only under it).
const codexNestedHookKey = "/<session-flags>/config.toml:post_tool_use:0:0"

// codexNestedCommand is the nested hook's command: the run's note helper as
// the one-shot `codex-nested` adapter, reading the state checkout-launch wrote
// ([codexRepoGate]).
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
