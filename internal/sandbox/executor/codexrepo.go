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

// codexRepoKeys is the frontmatter a repository's skill keeps when it is
// copied into the run's Codex home ([codexRepoGate]): what describes or limits
// it, as a Claude Code run's restore keeps (claudeRepoKeys). Everything else,
// hooks, MCP servers, a permission, a model and every key not named here,
// goes; its name is the run's own, `<repository>:<skill>`.
var codexRepoKeys = []string{"description", "when_to_use", "argument-hint", "allowed-tools", "tools",
	"disable-model-invocation"}

// codexRepoFrontmatter is the awk program that rewrites a copied SKILL.md's
// frontmatter to the name `name` and the keys `keys` names, a key's indented
// or listed lines going with it; the body is kept as it is. A file that opens
// with no frontmatter exits 1: Codex lists no skill without one.
const codexRepoFrontmatter = `BEGIN { n = split(keys, k, " "); for (i = 1; i <= n; i++) ok[k[i]] = 1 }
{ l = $0; sub(/\r$/, "", l) }
NR == 1 { if (l != "---") exit 1; fm = 1; print; printf "name: \"%s\"\n", name; next }
fm && l == "---" { fm = 0; print; next }
fm { if (l ~ /^[^ \t#-][^:]*:/) { key = l; sub(/:.*/, "", key); keep = (key in ok) } if (keep) print; next }
{ print }
END { if (NR == 0) exit 1 }`

// codexRepoGate is the part of a Codex run's script that hands the harness
// the instructions and skills of each repository checked out directly under
// its working directory, none of their configuration, and sets hk where the
// box holds the note helper the nested hook runs:
//   - the helper's codex-launch writes each checkout's root instructions into
//     CODEX_HOME/AGENTS.md (replaced) within [codexRepoBudget], the
//     repositories the run pushes to first ([sandbox.RunRequest.ReportedRepos])
//     and then the rest by name, and records the checkouts in that order
//     ([Paths.CodexRepo]);
//   - each one's .codex/skills and .agents/skills, reached through no link,
//     are copied as `<repository>:<skill>` with [codexRepoFrontmatter].
func codexRepoGate(paths Paths, repos []string) string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	launch, _ := json.Marshal(map[string]any{"state": paths.CodexRepo(), "agents": paths.CodexHome() + "/AGENTS.md",
		"budget": codexRepoBudget, "first": repos}) // strings and an int cannot fail to encode
	line(`rr=%s; rh=%s; rk=%s; hk=; rm -rf "$rr" %s; mkdir -p "$rr" || exit 1`,
		q(paths.CodexRepo()), q(paths.CodexNoteHelper()), q(paths.CodexSkills()), q(paths.CodexHome()+"/AGENTS.md"))
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
	line(`  rs=" %s "; rw=$(pwd -P)`, sandbox.SkillNamespace)
	line(`  while IFS= read -r rn; do`)
	line(`    rt=$(printf '%%s' "$rn" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9' '-'); rc=$rt; ri=1`)
	line(`    while case "$rs" in *" $rt "*) true;; *) false;; esac; do ri=$((ri + 1)); rt=$rc-$ri; done; rs="$rs$rt "`)
	line(`    for rsd in .codex/skills .agents/skills; do [ -d "./$rn/$rsd" ] && [ "$(readlink -f "./$rn/$rsd")" = "$rw/$rn/$rsd" ] || continue`)
	line(`      for rsk in "./$rn/$rsd"/*/; do rsk=${rsk%%/}; rsn=${rsk##*/}`)
	line(`        [ -d "$rsk" ] && [ "$(readlink -f "$rsk")" = "$rw/$rn/$rsd/$rsn" ] && [ -f "$rsk/SKILL.md" ] && [ ! -L "$rsk/SKILL.md" ] || continue`)
	line(`        rl=$rt:$(printf '%%s' "$rsn" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9_.' '-'); rto=$rk/$rl; [ ! -e "$rto" ] || continue`)
	line(`        ( cd "$rsk" && find . -type d ) | while IFS= read -r rx; do mkdir -p "$rto/$rx"; done`)
	line(`        ( cd "$rsk" && find . -type f ) | while IFS= read -r rx; do cp "$rsk/$rx" "$rto/$rx"; done`)
	line(`        awk -v keys=%s -v name="$rl" %s "$rsk/SKILL.md" > "$rto/SKILL.md" || rm -rf "$rto"`,
		q(strings.Join(codexRepoKeys, " ")), q(codexRepoFrontmatter))
	line(`      done`)
	line(`    done`)
	line(`  done < "$rr/handed"`)
	line(`fi`)
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
// ([codexRepoGate]). It starts from an empty environment (`env -i`), since
// Codex runs a hook with its own: the helper and its state are the box user's
// files, and the adapter reads nothing from the environment.
func codexNestedCommand(paths Paths) string {
	q := shellword.Quote
	return "env -i " + q(paths.CodexNoteHelper()) + " -adapter codex-nested -config " + q(paths.CodexRepo()+"/nested.json")
}

// codexHookHash is the trust hash Codex 0.171.0 computes for a PostToolUse
// hook matching every tool with command (codex-rs/hooks discovery.rs
// hook_hash): "sha256:" and the SHA-256 of the hook's identity as JSON, every
// object's keys sorted, no space, no HTML escaping. A run-owned hook runs only
// under it ([codexNestedHookFlags]); a repository's or the home's runs not.
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
// run alone writes: a repository's hooks stay untrusted (its layer is
// disabled while the project is not trusted, measured on 0.171.0).
func codexNestedHookFlags(paths Paths) (hook, state string) {
	command := codexNestedCommand(paths)
	hook = fmt.Sprintf(`hooks.PostToolUse=[{matcher=".*",hooks=[{type="command",command=%s,timeout=%d,additionalContextLimit=%d}]}]`,
		tomlString(command), codexNestedTimeout, codexNestedContextLimit)
	state = fmt.Sprintf(`hooks.state={%s={trusted_hash=%s}}`, tomlString(codexNestedHookKey),
		tomlString(codexHookHash(command, codexNestedTimeout, codexNestedContextLimit)))
	return hook, state
}
