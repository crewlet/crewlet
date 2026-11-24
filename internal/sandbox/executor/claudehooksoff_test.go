package executor_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A HOME OR A REPOSITORY CANNOT SWITCH A CLAUDE CODE RUN'S HOOKS OFF UNSEEN.
// Claude Code 2.1.330 runs none of the run's hooks where the settings it
// merges leave `disableAllHooks` true, a later file winning (measured), and the
// run never overrules it. So a run starting a hook of the engine's (its
// skills' plugin, its notes' hooks) is refused before the harness starts,
// naming the file and the key; it starts where a later file sets false, where
// the key is no boolean or the file no JSON (both ignored whole: measured),
// in the home's settings.local.json (never read: measured), nested, as
// allowManagedHooksOnly outside a managed file (ignored: measured), and where
// it starts no hook of the engine's. Mutations, each red: drop the gate; let a
// file's false decide nothing; read a string as true; name no file.
func TestClaudeCodesHooksCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	const off = `{"disableAllHooks": true}`
	const said = "Claude Code would run none of the run's hooks"
	skills := sandbox.RunRequest{Brief: "go", Skills: handedSkills()[1:], SkillLoads: skillWiring()}
	notes := sandbox.RunRequest{Brief: "go", Steer: steerWiring}
	for name, tc := range map[string]struct {
		release string
		req     sandbox.RunRequest
		switchOffCase
	}{
		"nothing switches them off": {req: skills},
		"the home's settings switch them off": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": off},
			fails: []string{said, "/.claude/settings.json sets disableAllHooks to true", "Remove that key"}}},
		"the repository's settings switch them off for the notes' hooks": {req: notes, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": `{"disableAllHooks": false}`,
				"workspace/.claude/settings.json": "{\n  \"disableAllHooks\"\n  :\n  true\n}"},
			fails: []string{said, "/workspace/.claude/settings.json sets disableAllHooks to true"}}},
		"the repository's local settings switch them off": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{"workspace/.claude/settings.local.json": off},
			fails: []string{said, "/workspace/.claude/settings.local.json sets disableAllHooks"}}},
		"the repository's false wins over the home's true": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": off,
				"workspace/.claude/settings.json": `{"disableAllHooks": false}`}}},
		"a string, a comment and a nested key decide nothing": {req: notes, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": `{"disableAllHooks": "true"}`,
				"workspace/.claude/settings.json":       "// off\n" + off,
				"workspace/.claude/settings.local.json": `{"env": {"disableAllHooks": true}}`}}},
		"the home's local settings are never read": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.local.json": off}}},
		"only managed settings can allow managed hooks alone": {req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": `{"allowManagedHooksOnly": true}`,
				"workspace/.claude/settings.json": `{"allowManagedHooksOnly": true}`}}},
		"a run with no hook of the engine's": {req: sandbox.RunRequest{Brief: "go"}, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": off}}},
		"a release older than every hook's floor": {release: "2.1.117", req: skills, switchOffCase: switchOffCase{
			plant: map[string]string{".claude/settings.json": off}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			release := tc.release
			if release == "" {
				release = "2.1.330"
			}
			runner, box := realBox(t)
			writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", "#!/bin/sh\n"+
				`case "$1" in --version) echo '`+release+` (Claude Code)'; exit 0;; esac`+"\ncat > /dev/null\n"+
				`echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}'`+"\n")
			plant(t, box, tc.plant)
			_, res := runToEnd(t, runner, box, tc.req)
			checkSwitchOff(t, box, res, tc.switchOffCase)
		})
	}
}
