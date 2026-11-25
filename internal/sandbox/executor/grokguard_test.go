package executor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A HOME CANNOT SWITCH GROK BUILD'S HOOKS OFF UNSEEN THROUGH ITS
// DISABLED-HOOKS FILE. Grok Build 1.0.92 skips a hook whose id that file holds
// on a line (blanks and a carriage return trimmed), and every hook where it
// cannot be read, with no word (measured). So a run with note hooks is refused
// before Grok starts, naming the file and the line, or saying it cannot be
// read; it starts where the file names the box's own hook, another place, a
// hook the run lacks, or is empty or a broken link (no file: measured). The
// nested hook switched off refuses nothing: the run starts, its log saying so
// once. Mutations, each red: drop the check; match untrimmed or blind to case;
// skip the unreadable file; name the hooks at place 0; count the nested hook
// as a note hook, or say nothing of it.
func TestGrokBuildsHooksCannotBeSwitchedOffByItsDisabledHooksUnseen(t *testing.T) {
	t.Parallel()
	const file = ".grok/disabled-hooks"
	const boxHook = "[[hooks.PostToolUse]]\nhooks = [{ type = \"command\", command = \"box-own\" }]\n"
	notes := sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge, Steer: loadsSteer}
	loads := sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge, Skills: handedSkills(), SkillLoads: skillWiring()}
	for name, tc := range map[string]struct {
		req    sandbox.RunRequest
		broken bool
		skip   string // the setting the run's log names, once, switching the nested hook off
		switchOffCase
	}{
		"nothing switches them off": {req: notes},
		"it names the run's finish hook": {req: notes, switchOffCase: switchOffCase{
			plant: map[string]string{file: "user:other[0].hooks[0]\nuser:stop[0].hooks[0]\n"},
			fails: []string{"Grok Build would skip the run's hook user:stop[0].hooks[0]", "/.grok/disabled-hooks names it",
				"remove that line", "refused before Grok Build started"}}},
		"it names the hook after a call, padded, with no newline": {req: notes, switchOffCase: switchOffCase{
			plant: map[string]string{file: "  user:post_tool_use_failure[0].hooks[0] \r"},
			fails: []string{"skip the run's hook user:post_tool_use_failure[0].hooks[0]"}}},
		"it names the hook a run reporting loads alone registers": {req: loads, switchOffCase: switchOffCase{
			plant: map[string]string{file: "user:post_tool_use[0].hooks[0]\n"},
			fails: []string{"skip the run's hook user:post_tool_use[0].hooks[0]"}}},
		"it names hooks a run reporting loads alone does not register": {req: loads, switchOffCase: switchOffCase{
			plant: map[string]string{file: "user:stop[0].hooks[0]\nuser:post_tool_use_failure[0].hooks[0]\n"}}},
		"it names the box's own hook, before which the run's come": {req: notes, switchOffCase: switchOffCase{
			plant: map[string]string{".grok/config.toml": boxHook, file: "user:post_tool_use[0].hooks[0]\n"}}},
		"it names the run's hook after the box's own": {req: notes, switchOffCase: switchOffCase{
			plant: map[string]string{".grok/config.toml": boxHook, file: "user:post_tool_use[1].hooks[0]\n"},
			fails: []string{"skip the run's hook user:post_tool_use[1].hooks[0]"}}},
		"it names other places, scopes and spellings": {req: notes, switchOffCase: switchOffCase{
			plant: map[string]string{file: "user:post_tool_use[2].hooks[0]\nproject:stop[0].hooks[0]\n" +
				"USER:STOP[0].HOOKS[0]\nuser:Stop[0].hooks[0]\n# user:stop[0].hooks[0]\nuser:stop[0].hooks[0]x\n"}}},
		"it is empty":         {req: notes, switchOffCase: switchOffCase{plant: map[string]string{file: ""}}},
		"it is a broken link": {req: notes, broken: true},
		"it cannot be read": {req: notes, switchOffCase: switchOffCase{
			plant: map[string]string{file + "/x": "user:stop[0].hooks[0]\n"},
			fails: []string{"Grok Build would run none of the run's hooks", "/.grok/disabled-hooks cannot be read",
				"skips every hook outside managed policy"}}},
		"it cannot be read, on a run taking no notes": {req: sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge},
			skip: "/.grok/disabled-hooks cannot be read", switchOffCase: switchOffCase{
				plant: map[string]string{file + "/x": "user:stop[0].hooks[0]\n"}}},
		"it names the nested hook of a run taking no notes": {req: sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge},
			skip: "/.grok/disabled-hooks names it", switchOffCase: switchOffCase{
				plant: map[string]string{file: "user:post_tool_use[0].hooks[0]\n"}}},
		"it names the nested hook alone, after the notes' one": {req: notes, skip: "/.grok/disabled-hooks names it",
			switchOffCase: switchOffCase{plant: map[string]string{file: "user:post_tool_use[1].hooks[0]\n"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := standInBox(t, executor.NewGrok(), map[string]string{"grok": grokInspectStandIn})
			plant(t, box, map[string]string{"inspect.json": grokInspect("", [2]string{"NAME", ""})})
			plant(t, box, tc.plant)
			if tc.broken {
				if err := os.MkdirAll(filepath.Join(box.Home(), ".grok"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(box.Home(), "nowhere"), filepath.Join(box.Home(), file)); err != nil {
					t.Fatal(err)
				}
			}
			_, res := runToEnd(t, runner, box, tc.req)
			checkSwitchOff(t, box, res, tc.switchOffCase)
			_, started := os.Stat(filepath.Join(box.Home(), "observed.argv"))
			if (started == nil) != (len(tc.fails) == 0) {
				t.Errorf("grok started %t, want %t", started == nil, len(tc.fails) == 0)
			}
			checkNestedOff(t, box, tc.skip)
		})
	}
}

// grokNestedOffLine opens the run's one line saying its nested hook is off.
const grokNestedOffLine = "crewlet: a repository cloned during this run is not handed its instructions and skills: "

// checkNestedOff checks the run's error stream says once that its nested hook
// is switched off, naming setting (a path under the home where it opens with
// a slash), or, setting "", says nothing of it.
func checkNestedOff(t *testing.T, box sandbox.Sandbox, setting string) {
	t.Helper()
	errs := boxFile(t, box, executor.PathsFor(box).Err())
	if setting == "" {
		if strings.Contains(errs, grokNestedOffLine) {
			t.Errorf("the run says its nested hook is off, want nothing of it:\n%s", errs)
		}
		return
	}
	if strings.HasPrefix(setting, "/") {
		setting = box.Home() + setting
	}
	if n := strings.Count(errs, grokNestedOffLine+setting); n != 1 {
		t.Errorf("the run says %d times that its nested hook is off by %q, want once:\n%s", n, setting, errs)
	}
}
