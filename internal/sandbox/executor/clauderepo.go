package executor

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// claudeRepoFloor is the oldest Claude Code release a run is handed its
// repositories' instructions on ([claudeRepoGate]): 2.1.118, the oldest read
// that takes `--plugin-dir` and reads CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD.
// An older one starts without them, and its error stream says so.
const claudeRepoFloor = "2.1.118"

// claudeRepoKeys is the frontmatter a repository's skill, command or agent
// keeps when it is copied into a plugin of the run's own: what names, describes
// and scopes it. Everything else is dropped, `hooks` above all, which a
// plugin's skill runs (measured on 2.1.330), and an agent's `mcpServers` and
// `permissionMode`, so a copy brings back no hook, server or permission.
var claudeRepoKeys = []string{"name", "description", "when_to_use", "argument-hint", "allowed-tools", "tools",
	"model", "color", "disable-model-invocation", "user-invocable", "context", "agent", "license", "version"}

// claudeRepoFrontmatter is the awk program that rewrites a copied Markdown
// file's frontmatter to the keys `keys` names, a key's indented or listed
// lines going with it; the body is kept as it is.
const claudeRepoFrontmatter = `BEGIN { n = split(keys, k, " "); for (i = 1; i <= n; i++) ok[k[i]] = 1 }
{ l = $0; sub(/\r$/, "", l) }
NR == 1 && l == "---" { fm = 1; print; next }
fm && l == "---" { fm = 0; print; next }
fm { if (l ~ /^[^ \t#-][^:]*:/) { key = l; sub(/:.*/, "", key); keep = (key in ok) } if (keep) print; next }
{ print }`

// claudeRepoGate is the part of a Claude Code run's script that hands the
// harness, at or after [claudeRepoFloor], its repositories' instructions
// without their settings: the working directory and each repository checked
// out directly under it. Each is added with `--add-dir`, which with
// CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD ([claudeSessionLayout]) loads its
// CLAUDE.md, imports, `.claude/CLAUDE.md` and `.claude/rules` as a native
// session does, and nothing of its settings. Its CLAUDE.local.md is imported
// from a directory of the run's own, added too. Its `.claude/skills`,
// `commands` and `agents` are copied, links dropped, into a plugin of the run's
// own named after it (never [sandbox.SkillNamespace]), their frontmatter cut
// to [claudeRepoKeys] (docs/concepts/code-sandbox.md, "What a Claude Code
// run loads").
func claudeRepoGate(paths Paths) string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`if at %s; then`, floorArgs(claudeRepoFloor))
	line(`  rr=%s; rm -rf "$rr"; mkdir -p "$rr/local"; ri=0; rs=" %s "`, q(paths.ClaudeRepoPlugins()),
		sandbox.SkillNamespace)
	line(`  for rd in "$PWD" "$PWD"/*/; do rd=${rd%%/}`)
	line(`    [ "$rd" = "$PWD" ] || { [ -e "$rd/.git" ] && [ ! -L "$rd" ]; } || continue`)
	line(`    set -- "$@" --add-dir "$rd"`)
	line(`    [ ! -f "$rd/CLAUDE.local.md" ] || printf '@%%s\n' "$rd/CLAUDE.local.md" >> "$rr/local/CLAUDE.md"`)
	line(`    [ -d "$rd/.claude/skills" ] || [ -d "$rd/.claude/commands" ] || [ -d "$rd/.claude/agents" ] || continue`)
	line(`    ri=$((ri + 1)); rp=$rr/$ri; rn=$(basename "$rd" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9\n' '-')`)
	line(`    case "$rs" in *" $rn "*) rn=$rn-$ri;; esac; rs="$rs$rn "`)
	line(`    mkdir -p "$rp/.claude-plugin" && printf '{"name":"%%s"}\n' "$rn" > "$rp/.claude-plugin/plugin.json" || continue`)
	line(`    for rk in skills commands agents; do [ ! -d "$rd/.claude/$rk" ] || cp -R "$rd/.claude/$rk" "$rp/$rk"; done`)
	line(`    find "$rp" -type l -exec rm -f {} +`)
	line(`    find "$rp" -type f \( -path "$rp/skills/*/SKILL.md" -o -path "$rp/commands/*.md" -o -path "$rp/agents/*.md" \) |`)
	line(`      while IFS= read -r rf; do awk -v keys=%s %s "$rf" > "$rf.crewlet" && mv -f "$rf.crewlet" "$rf"; done`,
		q(strings.Join(claudeRepoKeys, " ")), q(claudeRepoFrontmatter))
	line(`    set -- "$@" --plugin-dir "$rp"`)
	line(`  done`)
	line(`  [ ! -s "$rr/local/CLAUDE.md" ] || set -- "$@" --add-dir "$rr/local"`)
	line(`else echo "crewlet: this box runs Claude Code $n, older than %s, the oldest that takes a `+
		`repository's instructions without its settings; the run starts without them" >&2; fi`, claudeRepoFloor)
	return b.String()
}
