package executor

import (
	"context"
	"errors"
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
// to [claudeRepoKeys] by awk (a file whose cut fails is not copied; a box with
// no awk copies none, and says so). The roots are listed, NUL-separated, for
// the nested hook ([claudeNestedHooks]), whose settings the run is handed; a
// box with no note helper says it runs without it ([writeClaudeNested]).
func claudeRepoGate(paths Paths) string {
	q := shellword.Quote
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`if at %s; then`, floorArgs(claudeRepoFloor))
	line(`  rr=%s; rm -rf "$rr"; mkdir -p "$rr/local" "$rr/seen"; ri=0; rs=" %s "; cs=%s`,
		q(paths.ClaudeRepoPlugins()), sandbox.SkillNamespace, q(paths.ClaudeRepoSettings()))
	line(`  [ ! -s %s ] || cat %s >&2`, q(paths.ClaudeNestedOff()), q(paths.ClaudeNestedOff()))
	line(`  ra=1; command -v awk > /dev/null 2>&1 || { ra=; echo "crewlet: this box has no awk, so the run is ` +
		`handed no repository's skills, commands or agents" >&2; }`)
	line(`  for rd in "$PWD" "$PWD"/*/; do rd=${rd%%/}`)
	line(`    [ "$rd" = "$PWD" ] || { [ -e "$rd/.git" ] && [ ! -L "$rd" ]; } || continue`)
	line(`    set -- "$@" --add-dir "$rd"; printf '%%s\000' "$rd" >> %s`, q(paths.ClaudeRepoRoots()))
	line(`    [ ! -f "$rd/CLAUDE.local.md" ] || printf '@%%s\n' "$rd/CLAUDE.local.md" >> "$rr/local/CLAUDE.md"`)
	line(`    [ -n "$ra" ] || continue`)
	line(`    [ -d "$rd/.claude/skills" ] || [ -d "$rd/.claude/commands" ] || [ -d "$rd/.claude/agents" ] || continue`)
	line(`    ri=$((ri + 1)); rp=$rr/$ri; rn=$(basename "$rd" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9\n' '-')`)
	line(`    case "$rs" in *" $rn "*) rn=$rn-$ri;; esac; rs="$rs$rn "`)
	line(`    mkdir -p "$rp/.claude-plugin" && printf '{"name":"%%s"}\n' "$rn" > "$rp/.claude-plugin/plugin.json" || continue`)
	line(`    for rk in skills commands agents; do [ ! -d "$rd/.claude/$rk" ] || cp -R "$rd/.claude/$rk" "$rp/$rk"; done`)
	line(`    find "$rp" -type l -exec rm -f {} +`)
	line(`    find "$rp" -type f \( -path "$rp/skills/*/SKILL.md" -o -path "$rp/commands/*.md" -o -path "$rp/agents/*.md" \) |`)
	line(`      while IFS= read -r rf; do awk -v keys=%s %s "$rf" > "$rf.crewlet" && mv -f "$rf.crewlet" "$rf" ||`+
		` rm -f "$rf" "$rf.crewlet"; done`,
		q(strings.Join(claudeRepoKeys, " ")), q(claudeRepoFrontmatter))
	line(`    set -- "$@" --plugin-dir "$rp"`)
	line(`  done`)
	line(`  [ ! -s "$rr/local/CLAUDE.md" ] || set -- "$@" --add-dir "$rr/local"`)
	line(`else echo "crewlet: this box runs Claude Code $n, older than %s, the oldest that takes a `+
		`repository's instructions without its settings; the run starts without them" >&2; fi`, claudeRepoFloor)
	return b.String()
}

// claudeNestedTools are the tools whose file the nested hook reads by.
const claudeNestedTools = "Read|Edit|Write|MultiEdit|NotebookEdit"

// claudeNestedHooks is the settings `hooks` entry that runs the note helper's
// claude-nested adapter (internal/notehelper's "Nested instructions") on the
// gate's index of the run's roots ([claudeRepoGate]) after each of
// [claudeNestedTools], under a ten-second timeout: what a native session loads
// as it works under a directory. None where the box has no helper
// ([writeClaudeNested]).
func claudeNestedHooks(paths Paths) []claudeHookMatcher {
	return []claudeHookMatcher{{Matcher: claudeNestedTools, Hooks: []claudeHook{{Type: "command",
		Command: shellword.Quote(paths.NoteHelper()) + " -adapter claude-nested -config " +
			shellword.Quote(paths.ClaudeRepoRoots()),
		Timeout: int(ClaudeHookTimeout / time.Second)}}}}
}

// writeClaudeNested writes the note helper the nested hook runs, and is the
// hook's settings entry: none, with the reason written where the gate tells
// the run's error stream ([Paths.ClaudeNestedOff]), on a machine the engine
// carries no helper for.
func writeClaudeNested(ctx context.Context, box sandbox.Sandbox, paths Paths) ([]claudeHookMatcher, error) {
	err := writeNoteHelper(ctx, box, paths.NoteHelper())
	if errors.Is(err, errNoNoteHelper) {
		why := "crewlet: " + strings.TrimPrefix(err.Error(), "executor: ") + ", so the run is not handed a " +
			"nested CLAUDE.md or a path-scoped rule as it works under one\n"
		if err = box.WriteFile(ctx, paths.ClaudeNestedOff(), []byte(why)); err != nil {
			return nil, fmt.Errorf("executor: writing why the run has no nested hook: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return claudeNestedHooks(paths), nil
}

// claudeRepoSettings is the settings of a run handed its repositories'
// instructions ([Paths.ClaudeRepoSettings]): every run's and the nested hook,
// where the box runs it.
type claudeRepoSettings struct {
	claudeSettings

	Hooks map[string][]claudeHookMatcher `json:"hooks,omitempty"`
}

// claudeToolUsed is a settings `hooks` map holding nested under
// [claudeEventToolUsed] beside hooks, or hooks alone where nested is none.
func claudeToolUsed(hooks map[string][]claudeHookMatcher, nested []claudeHookMatcher) map[string][]claudeHookMatcher {
	if len(nested) > 0 {
		if hooks == nil {
			hooks = map[string][]claudeHookMatcher{}
		}
		hooks[claudeEventToolUsed] = nested
	}
	return hooks
}
