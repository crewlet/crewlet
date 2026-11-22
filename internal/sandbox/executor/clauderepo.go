package executor

import (
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// claudeRepoFloor is the oldest Claude Code release a run is handed its
// repositories' instructions on ([claudeRepoGate]): 2.1.118, the oldest read
// that takes `--plugin-dir` and reads CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD.
// An older one starts without them, and its error stream says so.
const claudeRepoFloor = "2.1.118"

// claudeRepoKeys is the frontmatter a repository's skill, command or agent
// keeps when it is copied into a plugin of the run's own: what describes it or
// limits it, nothing else. Dropped: `hooks`, which a plugin's skill runs
// (measured on 2.1.330); an agent's `mcpServers` and `permissionMode`; `model`,
// since the run's model is its seat's entry's, never a repository's choice; and
// every key not named here.
var claudeRepoKeys = []string{"name", "description", "when_to_use", "argument-hint", "allowed-tools", "tools",
	"disable-model-invocation"}

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
// to [claudeRepoKeys]. The roots are listed for the nested hook
// ([claudeNestedHook]), whose settings the run is handed.
func claudeRepoGate(paths Paths) string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`if at %s; then`, floorArgs(claudeRepoFloor))
	line(`  rr=%s; rm -rf "$rr"; mkdir -p "$rr/local" "$rr/seen"; ri=0; rs=" %s "; cs=%s`,
		q(paths.ClaudeRepoPlugins()), sandbox.SkillNamespace, q(paths.ClaudeRepoSettings()))
	line(`  for rd in "$PWD" "$PWD"/*/; do rd=${rd%%/}`)
	line(`    [ "$rd" = "$PWD" ] || { [ -e "$rd/.git" ] && [ ! -L "$rd" ]; } || continue`)
	line(`    set -- "$@" --add-dir "$rd"; printf '%%s\n' "$rd" >> "$rr/roots"`)
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

// claudeNestedHook is the script the run's PostToolUse hook runs
// ([claudeNestedHooks]) for each file the model reads or writes, its first
// argument the run's repository directory ([Paths.ClaudeRepoPlugins]): what a
// native session loads lazily, handed back as the hook's additional context,
// which the harness puts before the model's next request (measured on
// 2.1.330). For the root the file is under ([claudeRepoGate]'s `roots`), each
// CLAUDE.md between the root and the file, outermost first, with its imports,
// and each rule of the root's `.claude/rules` whose `paths` globs match the
// file. Each once a run: the first hook to claim a file (`set -C`) hands it.
const claudeNestedHook = `rr=$1; in=$rr/in.$$; out=$rr/out.$$; trap 'rm -f "$in" "$out"' EXIT; cat > "$in"; : > "$out"
f=$(awk 'BEGIN { RS = "\001" } {
	i = index($0, "\"tool_input\":{"); if (!i) exit; s = substr($0, i)
	for (k = 1; k <= 2; k++) { key = k == 1 ? "\"file_path\":\"" : "\"notebook_path\":\""
		j = index(s, key); if (j) { s = substr(s, j + length(key)); e = index(s, "\""); p = substr(s, 1, e - 1)
			if (p !~ /\\/) print p; exit } } }' "$in")
case $f in /*) ;; *) exit 0 ;; esac
root=; while IFS= read -r r; do case $f in "$r"/*) [ ${#r} -gt ${#root} ] && root=$r ;; esac; done < "$rr/roots"
[ -n "$root" ] || exit 0
claim() { k=$(printf '%s' "$1" | cksum | cut -d' ' -f1); ( set -C; : > "$rr/seen/$k" ) 2> /dev/null; }
add() { [ -f "$1" ] && claim "$1" && printf 'Contents of %s:\n\n%s\n\n' "$1" "$(cat "$1")" >> "$out"; }
d=${f%/*}; dirs=; while [ "$d" != "$root" ] && [ ${#d} -gt ${#root} ]; do dirs="$d
$dirs"; d=${d%/*}; done
printf '%s' "$dirs" | while IFS= read -r d; do [ -n "$d" ] || continue
	add "$d/CLAUDE.md" && sed -n 's/^@//p' "$d/CLAUDE.md" | while IFS= read -r i; do
		case $i in /*) add "$i" ;; *) add "$d/$i" ;; esac; done; done
rel=${f#"$root"/}
[ ! -d "$root/.claude/rules" ] || find "$root/.claude/rules" -type f -name '*.md' | while IFS= read -r rule; do
	awk -v rel="$rel" '
	function re(g,   o, c, i, b) { o = ""; b = 0
		for (i = 1; i <= length(g); i++) { c = substr(g, i, 1)
			if (substr(g, i, 3) == "**/") { o = o "(.*/)?"; i += 2 } else if (substr(g, i, 2) == "**") { o = o ".*"; i++ }
			else if (c == "*") o = o "[^/]*"; else if (c == "?") o = o "[^/]"
			else if (c == "{") { o = o "("; b++ } else if (c == "}" && b) { o = o ")"; b-- } else if (c == "," && b) o = o "|"
			else if (index(".+()|^$[]\\", c)) o = o "\\" c; else o = o c }
		return "^" o "$" }
	function try(v) { gsub(/^[ \t"'"'"']+|[ \t"'"'"']+$/, "", v); if (v != "" && rel ~ re(v)) hit = 1 }
	NR == 1 { if ($0 != "---") exit; fm = 1; next }
	fm && $0 == "---" { exit }
	fm && /^[^ \t-]/ { inp = 0 }
	fm && /^paths:/ { inp = 1; v = $0; sub(/^paths:[ \t]*/, "", v); gsub(/[][]/, "", v); n = split(v, a, ","); for (x = 1; x <= n; x++) try(a[x]); next }
	fm && inp && /^[ \t]*-/ { v = $0; sub(/^[ \t]*-[ \t]*/, "", v); try(v) }
	END { exit hit ? 0 : 1 }' "$rule" && add "$rule"; done
[ -s "$out" ] || exit 0
awk 'BEGIN { RS = "\001"; printf "{\"hookSpecificOutput\":{\"hookEventName\":\"PostToolUse\",\"additionalContext\":\"" }
{ gsub(/\\/, "\\\\"); gsub(/"/, "\\\""); gsub(/\t/, "\\t"); gsub(/\r/, "\\r"); gsub(/\n/, "\\n"); gsub(/[\001-\037]/, ""); printf "%s", $0 }
END { printf "\"}}" }' "$out"
`

// claudeNestedTools are the tools whose file the nested hook reads by.
const claudeNestedTools = "Read|Edit|Write|MultiEdit|NotebookEdit"

// claudeNestedHooks is the settings `hooks` entry that runs [claudeNestedHook]
// after each of [claudeNestedTools], under a ten-second timeout.
func claudeNestedHooks(paths Paths) []claudeHookMatcher {
	return []claudeHookMatcher{{Matcher: claudeNestedTools, Hooks: []claudeHook{{Type: "command",
		Command: "sh " + shellword.Quote(paths.ClaudeNestedHook()) + " " + shellword.Quote(paths.ClaudeRepoPlugins()),
		Timeout: int(ClaudeHookTimeout / time.Second)}}}}
}

// claudeRepoSettings is the settings of a run handed its repositories'
// instructions ([Paths.ClaudeRepoSettings]): every run's and the nested hook.
type claudeRepoSettings struct {
	claudeSettings

	Hooks map[string][]claudeHookMatcher `json:"hooks"`
}
