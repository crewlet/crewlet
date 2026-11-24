package executor_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A HOME CANNOT SWITCH GROK BUILD'S HOOKS OFF UNSEEN THROUGH ITS
// DISABLED-HOOKS FILE. Grok Build 1.0.92 skips a hook whose id that file holds
// on a line (blanks and a carriage return trimmed), and every hook where it
// cannot be read, with no word (measured). So a hooked run is refused before
// Grok starts, naming the file and the line, or saying it cannot be read; it
// starts where the file names the box's own hook, before which the run's
// come, another place, scope or spelling, a hook the run lacks, where it is
// empty or a broken link (no file: measured), or the run has no hooks.
// Mutations, each red: drop the check; match untrimmed; match blind to case;
// skip the unreadable file; name the run's hooks at place 0.
func TestGrokBuildsHooksCannotBeSwitchedOffByItsDisabledHooksUnseen(t *testing.T) {
	t.Parallel()
	const file = ".grok/disabled-hooks"
	const boxHook = "[[hooks.PostToolUse]]\nhooks = [{ type = \"command\", command = \"box-own\" }]\n"
	notes := sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge, Steer: loadsSteer}
	loads := sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge, Skills: handedSkills(), SkillLoads: skillWiring()}
	for name, tc := range map[string]struct {
		req    sandbox.RunRequest
		broken bool
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
			plant: map[string]string{file: "user:post_tool_use[1].hooks[0]\nproject:stop[0].hooks[0]\n" +
				"USER:STOP[0].HOOKS[0]\nuser:Stop[0].hooks[0]\n# user:stop[0].hooks[0]\nuser:stop[0].hooks[0]x\n"}}},
		"it is empty":         {req: notes, switchOffCase: switchOffCase{plant: map[string]string{file: ""}}},
		"it is a broken link": {req: notes, broken: true},
		"it cannot be read": {req: notes, switchOffCase: switchOffCase{
			plant: map[string]string{file + "/x": "user:stop[0].hooks[0]\n"},
			fails: []string{"Grok Build would run none of the run's hooks", "/.grok/disabled-hooks cannot be read",
				"skips every hook outside managed policy"}}},
		"it cannot be read, on a run with no hooks": {req: sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge},
			switchOffCase: switchOffCase{plant: map[string]string{file + "/x": "user:stop[0].hooks[0]\n"}}},
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
		})
	}
}
