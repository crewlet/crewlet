package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp/syntax"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// cursorCheckouts is what Cursor Agent 2026.11.22-e9042b2 reads from a
// repository's root started there (measured): its AGENTS.md, CLAUDE.md and
// .cursorrules, each whole, and the skills directly in its .cursor/skills,
// .claude/skills and .agents/skills. A checkout present at launch has its
// skills loaded by Cursor itself through --add-dir ([cursorRules]), so only a
// firing copies any.
func cursorCheckouts() checkoutSpec {
	return checkoutSpec{Instructions: []checkoutSlot{{Names: []string{"AGENTS.md"}}, {Names: []string{"CLAUDE.md"}},
		{Names: []string{".cursorrules"}}}, Limit: checkoutUnlimited,
		SkillDirs:          []checkoutSkillDir{{Dir: ".cursor/skills"}, {Dir: ".claude/skills"}, {Dir: ".agents/skills"}},
		LaunchSkillsLoaded: true}
}

// checkoutHome is where a Cursor Agent run keeps its checkouts' instructions
// and skills ([checkoutHome]): no home file, since the launch's sections go in
// a rule of the run's own ([cursorCheckoutRule]), and a skills directory of
// the run's own that a firing copies a clone's skills into, each listed by
// its copy's path, as Cursor lists no skill added after it starts (measured).
func (CursorAgent) checkoutHome(paths Paths) checkoutHome {
	return checkoutHome{state: paths.CursorCheckouts(), helper: paths.CursorNoteHelper(),
		launch: paths.CursorCheckoutLaunch(), sections: paths.CursorCheckoutSections(),
		skills: paths.CursorCheckoutSkills(), copied: paths.CursorCheckoutCopied()}
}

var _ homeKeeper = CursorAgent{}

// homeRestore puts the home's own hooks.json back and the run's copies out,
// run by the clear before every run whichever harness it runs, so a killed
// run's postToolUse hook is in no home file once the next run starts.
func (CursorAgent) homeRestore(p Paths) string {
	h := CursorAgent{}.checkoutHome(p)
	return everyStep(putBackHome(p.CursorHooks(), p.CursorHeldHooks()), h.restore(), h.dropSkipped())
}

// cursorCheckoutRule is the rule the launch's checkouts' sections are handed
// in, in the run's directory beside its standing instructions' rule: always
// applied, as Cursor reads rules only where a run starts (measured).
const cursorCheckoutRule = ".cursor/rules/crewlet-checkouts.mdc"

// cursorCheckoutsGate is the part of a run's script that hands Cursor its
// launch checkouts before it starts ([checkoutHome.gate]), writes their
// sections as [cursorCheckoutRule], and says once in the skip log why a
// checkout cloned during the run is not handed, where the run laid no hook
// ([Paths.CursorHooksOff]).
func cursorCheckoutsGate(paths Paths) string {
	q := shellword.Quote
	h := CursorAgent{}.checkoutHome(paths)
	return h.gate() +
		`if [ -s "$rq" ]; then { printf -- '---\ndescription: The instructions of the repositories below this directory\nalwaysApply: true\n---\n'; cat "$rq"; } > ` +
		q(cursorCheckoutRule) + " || exit 1; fi\n" +
		`if [ -n "$hk" ] && [ -s ` + q(paths.CursorHooksOff()) + ` ]; then cat ` + q(paths.CursorHooksOff()) + ` >> ` +
		q(h.skipped()) + "; fi\n"
}

// cursorCheckoutsEnd is the shell every end of a run's script runs, the
// teardown's signals and the EXIT trap included: the home's hooks.json put
// back, the run marked ended and its copies removed, then what its firings
// left out said, each step whatever came of the others ([everyStep]).
func cursorCheckoutsEnd(paths Paths) string {
	h := CursorAgent{}.checkoutHome(paths)
	return everyStep(putBackHome(paths.CursorHooks(), paths.CursorHeldHooks()), h.restore(), h.saySkipped())
}

// cursorHookTimeout is the seconds Cursor waits on the run's hook before it
// drops its answer and goes on (measured: an entry's "timeout" holds; the
// default is 60 s): twice the note helper's own claim deadline
// (cursorClaimWithin, the firing bound of Pi and OpenCode,
// [codexNestedTimeout]), past which a firing claims nothing and the clone goes
// at the next MCP call. What follows a claim, reading at most 39,000 bytes of
// files and copying at most 32 MiB of skills (0.12 s on the build VM), gets
// as long again, so a loaded box answers a claim in time.
const cursorHookTimeout = 2 * codexNestedTimeout

// cursorHookSources is every file Cursor Agent 2026.11.22-e9042b2 reads hooks
// from, by where it sits, and the key its postToolUse entries are under (read
// from the release's bundle): its own enterprise, team, user and project
// files, and Claude Code's settings, which Cursor reads too.
func cursorHookSources(box sandbox.Sandbox, p Paths) []cursorHookSource {
	start := path.Join(box.Home(), sandbox.WorkspaceSubdir)
	return []cursorHookSource{
		{p.CursorHooks(), "postToolUse"},
		{"/etc/cursor/hooks.json", "postToolUse"},
		{p.CursorDir() + "/managed/active-team-hooks/hooks.json", "postToolUse"},
		{start + "/.cursor/hooks.json", "postToolUse"},
		{p.Home + "/.claude/settings.json", "PostToolUse"},
		{start + "/.claude/settings.json", "PostToolUse"},
		{start + "/.claude/settings.local.json", "PostToolUse"},
	}
}

// cursorHookSource is one file Cursor reads hooks from and the event key of
// its post-tool entries.
type cursorHookSource struct{ file, key string }

// errCursorHookUnread is a home hooks.json the run cannot lay its hook into.
var errCursorHookUnread = errors.New("does not read as a JSON object with an object of hooks")

// cursorHooked is the home's hooks.json (homeFile, holding home) with the
// run's postToolUse entry (cmd) added: every key and entry of the home's as
// the home wrote it, a
// version where it names none. "" and the reason where the run lays none:
// another post-tool hook in any of files (path to contents) that can fire on
// an MCP call ([cursorPostToolEntries]), since Cursor runs
// each on the call's own result and hands the model only one answer
// (measured), so the run's would drop that hook's or be dropped with a clone
// it claimed; or a home file the run cannot read.
func cursorHooked(homeFile string, home []byte, files map[string][]byte, sources []cursorHookSource,
	cmd string) ([]byte, string) {
	for _, s := range sources {
		if key, ok := cursorPostToolEntries(files[s.file], s.key); ok {
			return nil, fmt.Sprintf("%s holds a %s hook that can fire on an MCP call, and Cursor hands the "+
				"model only one post-tool hook's answer, so the run lays none of its own beside it", s.file, key)
		}
	}
	doc := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(home)) > 0 {
		if err := json.Unmarshal(home, &doc); err != nil || doc == nil {
			return nil, fmt.Sprintf("%s %v, so the run lays no hook into it", homeFile, errCursorHookUnread)
		}
	}
	hooks := map[string]json.RawMessage{}
	if raw := bytes.TrimSpace(doc["hooks"]); len(raw) > 0 {
		if err := json.Unmarshal(raw, &hooks); err != nil || hooks == nil {
			return nil, fmt.Sprintf("%s %v, so the run lays no hook into it", homeFile, errCursorHookUnread)
		}
	}
	var post []json.RawMessage
	if raw := bytes.TrimSpace(hooks["postToolUse"]); len(raw) > 0 && json.Unmarshal(raw, &post) != nil {
		return nil, fmt.Sprintf("%s %v, so the run lays no hook into it", homeFile, errCursorHookUnread)
	}
	entry, _ := json.Marshal(map[string]any{"command": cmd, "timeout": cursorHookTimeout})
	hooks["postToolUse"], _ = json.Marshal(append(post, entry))
	doc["hooks"], _ = json.Marshal(hooks)
	if _, named := doc["version"]; !named {
		doc["version"] = json.RawMessage("1")
	}
	out, _ := json.MarshalIndent(doc, "", "  ")
	return append(out, '\n'), ""
}

// cursorPostToolEntries reports whether raw, a hooks file, lists an entry
// under hooks.<key> that can fire on an MCP call: Claude Code's any (Cursor
// maps their tool names its own way), Cursor's any whose matcher can match an
// MCP call's name ([cursorMatcherSkipsMCP]). A file that does not read lists
// none, as Cursor loads none from it.
func cursorPostToolEntries(raw []byte, key string) (string, bool) {
	var file struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if json.Unmarshal(raw, &file) != nil {
		return "", false
	}
	for _, entry := range file.Hooks[key] {
		var e struct {
			Matcher *string `json:"matcher"`
		}
		if key != "postToolUse" || json.Unmarshal(entry, &e) != nil || e.Matcher == nil ||
			!cursorMatcherSkipsMCP(*e.Matcher) {
			return key, true
		}
	}
	return key, false
}

// cursorMCPName is how an MCP call's tool name begins when Cursor matches a
// hook's matcher against it: a regular expression searched for anywhere in
// `MCP:<tool>` (measured: `t_tool` and `MCP` fire on MCP:t_tool, `^t_tool$`
// and `Shell` do not).
const cursorMCPName = "MCP:"

// cursorMatcherSkipsMCP reports whether matcher can match no MCP call's name,
// whatever the tool: each alternative anchored at the start on a literal that
// departs from [cursorMCPName]. An unanchored one can, as a server may name a
// tool anything, and so can a matcher this cannot read.
func cursorMatcherSkipsMCP(matcher string) bool {
	re, err := syntax.Parse(matcher, syntax.Perl)
	if err != nil {
		return false
	}
	re = re.Simplify()
	alts := []*syntax.Regexp{re}
	if re.Op == syntax.OpAlternate {
		alts = re.Sub
	}
	for _, alt := range alts {
		if alt.Op != syntax.OpConcat || len(alt.Sub) < 2 ||
			(alt.Sub[0].Op != syntax.OpBeginText && alt.Sub[0].Op != syntax.OpBeginLine) {
			return false
		}
		if !cursorDepartsFromMCP(alt.Sub[1]) {
			return false
		}
	}
	return true
}

// cursorDepartsFromMCP reports whether every name re matches at its start
// departs from [cursorMCPName] within its first bytes: a literal that does,
// or an alternation or group of such, case kept.
func cursorDepartsFromMCP(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpLiteral:
		if re.Flags&syntax.FoldCase != 0 {
			return false
		}
		lit := string(re.Rune)
		n := min(len(lit), len(cursorMCPName))
		return lit[:n] != cursorMCPName[:n]
	case syntax.OpConcat:
		return len(re.Sub) > 0 && cursorDepartsFromMCP(re.Sub[0])
	case syntax.OpCapture:
		return cursorDepartsFromMCP(re.Sub[0])
	case syntax.OpAlternate:
		for _, sub := range re.Sub {
			if !cursorDepartsFromMCP(sub) {
				return false
			}
		}
		return true
	}
	return false
}

// cursorHookSep ends each file [cursorHookFiles] reads: a control character
// no JSON file holds, so a file that holds one reads as none.
const cursorHookSep = "\x1e"

// cursorHookFiles is each source's file as the box's own shell reads it, which
// reaches /etc/cursor in a direct box as Cursor does; empty for one it does
// not hold; nil where one holds the separator, so none can be told apart.
func cursorHookFiles(ctx context.Context, box sandbox.Sandbox, sources []cursorHookSource) (map[string][]byte, error) {
	var cmd strings.Builder
	for _, s := range sources {
		cmd.WriteString("cat " + shellword.Quote(s.file) + " 2> /dev/null; printf '\\036'; ")
	}
	res, err := box.Exec(ctx, cmd.String(), sandbox.ExecOptions{})
	if err != nil {
		return nil, fmt.Errorf("executor: reading the files Cursor reads hooks from: %w", err)
	}
	parts := strings.Split(res.Stdout, cursorHookSep)
	if len(parts) != len(sources)+1 {
		return nil, nil
	}
	files := map[string][]byte{}
	for i, s := range sources {
		files[s.file] = []byte(parts[i])
	}
	return files, nil
}

// cursorNestedCommand is the run's postToolUse entry's command: the note
// helper by its absolute path, as Cursor runs a hook on PATH
// /usr/local/bin:/usr/bin:/bin (measured), with the firing's config.
func cursorNestedCommand(paths Paths) string {
	q := shellword.Quote
	return q(paths.CursorNoteHelper()) + " -adapter cursor-nested -config " + q(paths.CursorCheckouts()+"/nested.json")
}

// cursorCheckoutsSetup writes what hands a Cursor run its checkouts: the note
// helper, where the engine has a build for the box, the launch config, and
// the run's postToolUse hook laid into the home's hooks.json, held whole
// first ([holdHome]); or, where it lays none, why ([Paths.CursorHooksOff]). A
// box with no helper build starts without either, its script saying so.
func cursorCheckoutsSetup(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) error {
	launch := CursorAgent{}.checkoutHome(paths).launchConfig(cursorCheckouts(), req.ReportedRepos)
	if err := box.WriteFile(ctx, paths.CursorCheckoutLaunch(), launch); err != nil {
		return fmt.Errorf("executor: writing what hands the run's repositories their instructions: %w", err)
	}
	helper, err := noteHelperFor(ctx, box)
	if errors.Is(err, errNoHelperBuild) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = writeHelperBuild(ctx, box, paths.CursorNoteHelper(), helper); err != nil {
		return err
	}
	sources := cursorHookSources(box, paths)
	files, err := cursorHookFiles(ctx, box, sources)
	if err != nil {
		return err
	}
	hooked, off := []byte(nil), "a file Cursor reads hooks from holds a byte no JSON file holds, so the run lays no hook"
	if files != nil {
		hooked, off = cursorHooked(paths.CursorHooks(), files[paths.CursorHooks()], files, sources,
			cursorNestedCommand(paths))
	}
	if off != "" {
		line := "crewlet: a checkout cloned during the run is not handed its instructions and skills: " + off + "\n"
		if err = box.WriteFile(ctx, paths.CursorHooksOff(), []byte(line)); err != nil {
			return fmt.Errorf("executor: writing why the run lays no hook: %w", err)
		}
		return nil
	}
	if err = holdHome(ctx, box, paths.CursorHooks(), paths.CursorHeldHooks()); err != nil {
		return err
	}
	if err = writeHome(ctx, box, paths, paths.CursorHooks(), hooked); err != nil {
		return fmt.Errorf("executor: laying the run's hook into %s: %w", paths.CursorHooks(), err)
	}
	return nil
}

// cursorEndTraps is the script's put-back on every end it can see: once the
// harness has stopped, on EXIT, and on a teardown's TERM, INT or HUP, the
// harness ended first ([end]); a run killed outright is put back by the next
// run's clear ([CursorAgent.homeRestore]).
func cursorEndTraps(paths Paths) string {
	return fmt.Sprintf("cput() { %s; }\n"+
		`cquit() { trap '' USR1; trap - EXIT TERM INT HUP; end "$harness"; harness=; cput; exit "$1"; }`+"\n"+
		`trap cput EXIT; trap 'cquit 143' TERM; trap 'cquit 130' INT; trap 'cquit 129' HUP`+"\n",
		cursorCheckoutsEnd(paths))
}
