package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// CodexHomeConfig is the home's own `config.toml`, held in the run's work
// directory while a run's configuration stands in its place
// ([codexHomeConfig], [codexRestoreHome], [holdHome]).
func (p Paths) CodexHomeConfig() string { return p.WorkDir() + "/codex-home-config.toml" }

// CodexHomeInstructions is the home's own CODEX_HOME/AGENTS.md, held in the
// run's work directory while the run's, the home's with its repositories'
// after it, stands in its place ([Codex.checkoutHome], [codexRestoreHome],
// [holdHome]).
func (p Paths) CodexHomeInstructions() string { return p.WorkDir() + "/codex-home-AGENTS.md" }

// codexHomeConfig is the home's own configuration, "" for none, read once an
// earlier run's leftovers are gone. A RUN KILLED BEFORE ITS SCRIPT PUT THE
// HOME'S FILES BACK left its own configuration, its bridge token in it, and
// instructions installed, the home's held beside them, so the next run's start
// puts the held copies back ([codexRestoreHome]) before anything else, a run
// it then refuses included: no home file keeps an earlier run's entries.
func codexHomeConfig(ctx context.Context, box sandbox.Sandbox, paths Paths) (string, error) {
	read := fmt.Sprintf(`set -e; %s; [ ! -e %[2]s ] || cat %[2]s`, codexRestoreHome(paths),
		shellword.Quote(paths.CodexLiveConfig()))
	res, err := box.Exec(ctx, read, sandbox.ExecOptions{})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	if err != nil {
		return "", fmt.Errorf("executor: putting back the box home's own Codex configuration an earlier run "+
			"left held, and reading it: %w", err)
	}
	return res.Stdout, nil
}

// codexConfigOver is the configuration a run installs: the home's own with
// the run's over it ([codexConfigTOML]), as a person's session loads the home's
// MCP servers, profiles and hook trust. The run's servers replace any of the
// home's by the same name; the run's standing instructions follow the home's
// own, both kept; its compaction point replaces the home's, being where the
// engine judges a continuation. A home with none is the run's alone, as written.
func codexConfigOver(home, run string) (string, error) {
	if strings.TrimSpace(home) == "" {
		return run, nil
	}
	var base, own map[string]any
	if _, err := toml.Decode(home, &base); err != nil {
		return "", fmt.Errorf("executor: the box home's ~/.codex/config.toml does not read as TOML, so the "+
			"run's configuration cannot be added to it; Codex itself would refuse it: %w", err)
	}
	if _, err := toml.Decode(run, &own); err != nil {
		return "", fmt.Errorf("executor: reading back the run's Codex configuration: %w", err)
	}
	for key, value := range own {
		switch key {
		case "mcp_servers":
			servers, ok := base[key].(map[string]any)
			if !ok && base[key] != nil {
				return "", fmt.Errorf("executor: the box home's ~/.codex/config.toml holds mcp_servers as "+
					"%T, not a table, so the run's servers cannot be added to it", base[key])
			}
			if servers == nil {
				servers = map[string]any{}
			}
			for name, s := range value.(map[string]any) {
				servers[name] = s
			}
			base[key] = servers
		case "developer_instructions":
			if mine, ok := base[key].(string); ok && strings.TrimSpace(mine) != "" {
				value = mine + "\n\n" + value.(string)
			}
			base[key] = value
		default:
			base[key] = value
		}
	}
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(base); err != nil {
		return "", fmt.Errorf("executor: rendering the run's Codex configuration over the home's: %w", err)
	}
	return b.String(), nil
}

var _ homeKeeper = Codex{}

// homeRestore is [codexRestoreHome], run by the clear before every run
// whichever harness it runs, so a killed Codex run's configuration, its bridge
// token in it, is in no home file once the next run starts.
func (Codex) homeRestore(p Paths) string { return codexRestoreHome(p) }

// codexRestoreHome is the shell putting the home's own configuration and
// instructions back once Codex has stopped ([putBackHome]): each held copy
// written over the run's through a link as the home keeps it, or the run's
// removed where the home had none; nothing where none is held, so it runs any
// number of times, and fails, naming the file, where a copy will not go back,
// the other files put back all the same ([everyStep]); and the skip log no
// script printed dropped ([checkoutHome.dropSkipped]).
func codexRestoreHome(paths Paths) string {
	h := Codex{}.checkoutHome(paths)
	return everyStep(putBackHome(paths.CodexLiveConfig(), paths.CodexHomeConfig()), h.restore(), h.dropSkipped())
}
