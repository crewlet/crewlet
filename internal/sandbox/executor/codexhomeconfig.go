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
// ([codexHomeConfig], [codexRestoreHome]): empty where the home had none.
func (p Paths) CodexHomeConfig() string { return p.WorkDir() + "/codex-home-config.toml" }

// CodexHomeInstructions is the home's own CODEX_HOME/AGENTS.md, held in the
// run's work directory while the run's, the home's with its repositories'
// after it, stands in its place ([codexRepoGate], [codexRestoreHome]): empty
// where the home had none.
func (p Paths) CodexHomeInstructions() string { return p.WorkDir() + "/codex-home-AGENTS.md" }

// codexHomeConfig is the home's own configuration, and whether it is already
// held: the held copy where one is there (an earlier run's configuration is
// still installed, its script having ended before it put the home's back),
// else the live file as the home left it, "" for none.
func codexHomeConfig(ctx context.Context, box sandbox.Sandbox, paths Paths) (string, bool, error) {
	q := shellword.Quote
	read := fmt.Sprintf(`if [ -e %[1]s ]; then echo held; cat %[1]s; else echo home; [ ! -e %[2]s ] || cat %[2]s; fi`,
		q(paths.CodexHomeConfig()), q(paths.CodexLiveConfig()))
	res, err := box.Exec(ctx, read, sandbox.ExecOptions{})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	if err != nil {
		return "", false, fmt.Errorf("executor: reading the box home's own Codex configuration: %w", err)
	}
	state, content, _ := strings.Cut(res.Stdout, "\n")
	return content, state == "held", nil
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

// codexRestoreHome is the shell putting the home's own configuration and
// instructions back once Codex has stopped: each held copy over the run's, or
// neither where the home had none; nothing where none is held, so it runs any
// number of times.
func codexRestoreHome(paths Paths) string {
	q := shellword.Quote
	back := `if [ -e %[1]s ]; then if [ -s %[1]s ]; then mv -f %[1]s %[2]s; else rm -f %[1]s %[2]s; fi; fi`
	return fmt.Sprintf(back, q(paths.CodexHomeConfig()), q(paths.CodexLiveConfig())) + "; " +
		fmt.Sprintf(back, q(paths.CodexHomeInstructions()), q(paths.CodexHome()+"/AGENTS.md"))
}
