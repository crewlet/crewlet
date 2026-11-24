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

// codexRepoBudget is the most bytes of repository instructions a Codex run is
// handed, its launch's and its nested hook's together: Codex 0.171.0's own
// project_doc_max_bytes default (32 KiB), the bound a native session holds its
// project docs to. A file past what is left is cut to it, as Codex cuts one;
// a checkout found once nothing is left is handed nothing.
const codexRepoBudget = 32 << 10

// codexRepoFrontmatter is the awk program that renames a copied SKILL.md to
// `name`, the run's `<repository>:<skill>`, and keeps the rest of the file as
// it is: Codex lists a skill by its frontmatter's name, and a seat's skill
// loads are reported by names a repository's never take. A file that opens
// with no frontmatter exits 1: Codex lists no skill without one.
const codexRepoFrontmatter = `{ l = $0; sub(/\r$/, "", l) }
NR == 1 { if (l != "---") exit 1; fm = 1; print; printf "name: \"%s\"\n", name; next }
fm && l == "---" { fm = 0; print; next }
fm { if (l ~ /^[^ \t#-][^:]*:/) { key = l; sub(/:.*/, "", key); sub(/[ \t]+$/, "", key); drop = (key == "name") } if (!drop) print; next }
{ print }
END { if (NR == 0) exit 1 }`

// codexRepoGate is the part of a Codex run's script that hands the harness
// the instructions and skills of each repository checked out directly under
// its working directory, which Codex started there does not read, and sets hk
// where the box holds the note helper the nested hook runs:
//   - codex-launch writes each checkout's root instructions within
//     [codexRepoBudget] into CODEX_HOME/AGENTS.md, after the home's own (held,
//     [codexRestoreHome]), the run's pushed repositories first
//     ([sandbox.RunRequest.ReportedRepos]), and records them ([Paths.CodexRepo]);
//   - each one's .codex/skills and .agents/skills are copied whole, links
//     followed, as `<repository>:<skill>` ([codexRepoFrontmatter]), never over
//     a home's skill, and listed for the next run to remove ([Paths.CodexRepoSkills]).
func codexRepoGate(paths Paths, repos []string) string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	launch, _ := json.Marshal(map[string]any{"state": paths.CodexRepo(), "agents": paths.CodexRepoAgents(),
		"budget": codexRepoBudget, "first": repos}) // strings and an int cannot fail to encode
	line(`rr=%s; rh=%s; rk=%s; ra=%s; rha=%s; rw=%s; rq=%s; hk=`, q(paths.CodexRepo()), q(paths.CodexNoteHelper()),
		q(paths.CodexSkills()), q(paths.CodexHome()+"/AGENTS.md"), q(paths.CodexHomeInstructions()), q(paths.CodexRepoSkills()),
		q(paths.CodexRepoAgents()))
	// AN EARLIER RUN'S INSTRUCTIONS GO FIRST, the home's held copy put back
	// ([putBackHome]); then the home's own is held afresh, the file untouched.
	line(`%s || exit 1`, putBackHome(paths.CodexHome()+"/AGENTS.md", paths.CodexHomeInstructions()))
	line(`if [ -e "$ra" ]; then cp "$ra" "$rha"; else : > "$rha%s"; fi || exit 1`, heldNone)
	line(`if [ -f "$rw" ]; then while IFS= read -r rl; do case "$rl" in ""|*/*|.|..) ;; *) rm -rf "$rk/$rl";; esac; done < "$rw"; fi`)
	line(`: > "$rw" && rm -rf "$rr" "$rq" && mkdir -p "$rr" || exit 1`)
	line(`if [ -f "$rh" ]; then`)
	line(`  chmod 700 "$rh" && printf '%%s\n' %s > "$rr/launch.json" || exit 1`, q(string(launch)))
	line(`  "$rh" -adapter codex-launch -config "$rr/launch.json" < /dev/null || { echo 'crewlet: the run could not hand its repositories their instructions' >&2; exit 1; }`)
	line(`  hk=1`)
	line(`else`)
	line(`  echo 'crewlet: this box has no note helper for its machine, so the run starts without the instructions and skills of its repositories, and without the hook that hands a checkout cloned during it its instructions' >&2`)
	line(`fi`)
	line(`if [ -n "$hk" ] && ! command -v awk > /dev/null; then`)
	line(`  echo 'crewlet: this box has no awk, so the run starts without the skills of its repositories' >&2`)
	line(`elif [ -n "$hk" ]; then`)
	line(`  rs=" %s "; mkdir -p "$rk" || exit 1`, sandbox.SkillNamespace)
	line(`  while IFS= read -r rn; do`)
	line(`    rt=$(printf '%%s' "$rn" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9' '-'); rc=$rt; ri=1`)
	line(`    while case "$rs" in *" $rt "*) true;; *) false;; esac; do ri=$((ri + 1)); rt=$rc-$ri; done; rs="$rs$rt "`)
	line(`    for rsd in .codex/skills .agents/skills; do [ -d "./$rn/$rsd" ] || continue`)
	line(`      for rsk in "./$rn/$rsd"/*/; do rsk=${rsk%%/}; rsn=${rsk##*/}; [ -f "$rsk/SKILL.md" ] || continue`)
	line(`        rl=$rt:$(printf '%%s' "$rsn" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9_.' '-'); rto=$rk/$rl; [ ! -e "$rto" ] || continue`)
	line(`        { cp -RL "$rsk" "$rto" && awk -v name="$rl" %s "$rsk/SKILL.md" > "$rto/SKILL.md" && printf '%%s\n' "$rl" >> "$rw"; } || rm -rf "$rto"`,
		q(codexRepoFrontmatter))
	line(`      done`)
	line(`    done`)
	line(`  done < "$rr/handed"`)
	line(`fi`)
	// THE REPOSITORIES' AFTER THE HOME'S OWN, written through a link as the
	// home keeps the file; the file untouched where they hand nothing.
	line(`if [ -s "$rq" ]; then { if [ -s "$rha" ]; then cat "$rha"; printf '\n\n'; fi; cat "$rq"; } > "$rq.all" && cat "$rq.all" > "$ra" && rm -f "$rq.all" || exit 1; fi`)
	return b.String()
}

// codexNestedTimeout is the nested hook's bound, in seconds: Claude Code's
// hook bound (ClaudeHookTimeout), the same reads of a few small files.
const codexNestedTimeout = 10

// codexNestedContextLimit is the most tokens of a nested hook's context Codex
// hands the model whole (`additionalContextLimit`); past its 2,500 default
// Codex spills the rest to a file, so it is set past [codexRepoBudget] in
// bytes, which no more tokens can hold.
const codexNestedContextLimit = codexRepoBudget

// codexNestedHookKey is the trust key Codex 0.171.0 gives the first
// PostToolUse hook its session flags declare: a fixed path, whatever the box
// (measured: `hooks/list` names it, and the hook runs only under it).
const codexNestedHookKey = "/<session-flags>/config.toml:post_tool_use:0:0"

// codexNestedCommand is the nested hook's command: the run's note helper as
// the one-shot `codex-nested` adapter, reading the state codex-launch wrote
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
