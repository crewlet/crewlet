package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
	"github.com/crewlet/crewlet/internal/words"
)

// A REPOSITORY'S CONFIGURATION MAY NOT CHANGE CREWLET'S OWN SERVER. Codex
// 0.171.0 merges a trusted project's `.codex/config.toml` (the start
// directory's and each parent's up to the project root) key by key above the
// home's `config.toml`, where the run writes its servers, so any key of
// `[mcp_servers.crewlet]` there silently redirects, narrows or retimes the
// bridge (measured). No `-c` unsets a key, and a url there would put the
// run's token on argv. So the launch asks Codex itself for every layer it
// loads for the start directory ([codexGuardCommand]) and refuses the run
// where one other than the run's own file or command line holds the server's
// entry, naming the file, key and entry ([codexGuardVerdict]); the same
// answer is the run's project_doc_max_bytes ([codexDocLimit]), and where hooked
// it refuses a layer that switches the run's nested hook off ([codexHooksVerdict]).

// codexGuardAnswer is how long [codexGuardCommand] waits for its app-server's
// answer: as long as the run's script waits for its own to listen
// ([codexListenWait]), the same binary starting in the same box. It answered
// in under half a second when measured.
const codexGuardAnswer = codexListenWait

// codexGuard refuses req's run where a layer of Codex's configuration other
// than the run's own holds the entry of one of Crewlet's own servers
// ([codexGuardVerdict]), and is the project_doc_max_bytes Codex reads for the
// start directory ([codexDocLimit]) with what the run's error stream is to say
// of it. ONLY CREWLET'S SERVERS REFUSE A RUN: where the ask itself fails, a run
// with none of them starts on [codexProjectDocMaxBytes], saying so. hooked: the
// box holds the helper the nested hook runs. home is the home's own
// configuration ([codexHomeConfig]).
func codexGuard(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths, home string,
	hooked bool) (int, string, error) {
	servers := crewletServers(req.MCPServers)
	config, err := codexConfigOver(home, codexConfigTOML(codexGuardServers(req, servers), "", req.Model))
	if err != nil {
		return 0, "", err
	}
	if err = box.WriteFile(ctx, paths.CodexGuardConfig(), []byte(config)); err != nil {
		return 0, "", fmt.Errorf("executor: writing the configuration the launch asks Codex about: %w", err)
	}
	// A LOGIN SHELL WITH THE BOX'S BIN DIRECTORY FIRST, as the run's wrapper
	// finds its codex ([Runner.boxRelease]).
	res, err := box.Exec(ctx, "sh -lc "+shellword.Quote(codexGuardCommand(req, paths)),
		sandbox.ExecOptions{TimeoutSec: codexGuardAnswer.Seconds() + 10})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	if err != nil {
		if len(servers) == 0 {
			return codexProjectDocMaxBytes, fmt.Sprintf("crewlet: the box's Codex could not be asked which "+
				"project_doc_max_bytes it reads for the start directory (%v), so each repository's instructions "+
				"are handed within its default, %d bytes", err, codexProjectDocMaxBytes), nil
		}
		return 0, "", fmt.Errorf("executor: asking the box's Codex which configuration it loads for the start directory: %w", err)
	}
	if err = codexGuardVerdict(res.Stdout, servers); err != nil {
		return 0, "", err
	}
	if hooked {
		if err = codexHooksVerdict(res.Stdout, paths.CodexHome()+"/config.toml"); err != nil {
			return 0, "", err
		}
	}
	if err = codexSkillsVerdict(res.Stdout, req.Skills, paths); err != nil {
		return 0, "", err
	}
	return codexDocLimit(res.Stdout), "", nil
}

// CodexGuardSaid is what the launch's guard has the run's error stream say
// ([codexGuard]), printed by the run's script as it starts: empty for nothing.
func (p Paths) CodexGuardSaid() string { return p.WorkDir() + "/codex-guard-said" }

// codexGuardNoCodex is what [codexGuardCommand] prints in a box with no codex
// on its PATH: no verdict, left to the run's script, which refuses the box in
// its own words.
const codexGuardNoCodex = "crewlet: no codex"

// codexGuardServers is each of servers as the run's configuration writes it,
// at a URL that reaches nothing and holds no token, its headers dropped: the
// keys Codex merges a project's over are the run's, and nothing secret is
// written for the guard.
func codexGuardServers(req sandbox.RunRequest, servers []string) map[string]sandbox.MCPServer {
	out := make(map[string]sandbox.MCPServer, len(servers))
	for _, name := range servers {
		s := req.MCPServers[name]
		s.URL, s.Headers = "http://"+name+".invalid/", nil
		out[name] = s
	}
	return out
}

// CodexGuardConfig is where the launch writes the configuration its guard's
// Codex starts from ([codexGuard]), which the guard's shell moves into a home
// of its own.
func (p Paths) CodexGuardConfig() string { return p.WorkDir() + "/codex-guard-config.toml" }

// codexGuardCommand is the shell asking Codex, as an app-server of its own over
// stdio, for every layer it loads in the start directory (`config/read` with
// its layers), and printing its answer: under a CODEX_HOME of its own holding
// [Paths.CodexGuardConfig] (the home's own configuration, whose `projects` say
// which project is trusted, with the run's servers), so nothing of the box's
// home is touched, and with the run's `-c` posture. A box with no codex prints
// [codexGuardNoCodex] alone.
func codexGuardCommand(req sandbox.RunRequest, paths Paths) string {
	q := shellword.Quote
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"crewlet","version":"1"}}}`
	initialized := `{"jsonrpc":"2.0","method":"initialized"}`
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`PATH=%s:"$PATH"; export PATH`, q(paths.BinDir()))
	line(`gf=%s; gt=; trap 'rm -rf "$gt" "$gf"' EXIT`, q(paths.CodexGuardConfig()))
	line(`command -v codex > /dev/null || { echo %s; exit 0; }`, q(codexGuardNoCodex))
	line(`gt=$(mktemp -d) && mv -f "$gf" "$gt/config.toml" && mkfifo "$gt/in" || exit 1`)
	line(`gc=$(pwd -P | sed 's/[\\"]/\\&/g')`)
	line(`CODEX_HOME="$gt" codex app-server %s < "$gt/in" > "$gt/out" 2> /dev/null &`, strings.Join(codexPosture(req), " "))
	line(`gp=$!`)
	line(`trap '' PIPE`)
	line(`exec 3> "$gt/in"`)
	line(`printf '%%s\n' %s %s "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"config/read\",\"params\":{\"includeLayers\":true,\"cwd\":\"$gc\"}}" >&3`,
		q(initialize), q(initialized))
	line(`gn=0; while [ "$gn" -lt %d ] && kill -0 "$gp" 2> /dev/null && ! grep -q -F '"id":2' "$gt/out"; do gn=$((gn + 1)); sleep 0.1; done`,
		int(codexGuardAnswer.Seconds())*10)
	line(`exec 3>&-`)
	line(`kill "$gp" 2> /dev/null; wait "$gp" 2> /dev/null`)
	line(`cat "$gt/out"`)
	line(`echo; echo %s; cat -- %s 2> /dev/null`, q(codexRequirementsMark), q(codexRequirementsFile))
	line(`exit 0`)
	return b.String()
}

// codexConfigRead is the part of Codex 0.171.0's `config/read` answer the
// guard reads (measured): every layer Codex loads for the start directory,
// each named by its kind and file, with its own configuration, and the reason
// one is not loaded (an untrusted project's).
type codexConfigRead struct {
	ID     json.RawMessage `json:"id"`
	Result *struct {
		Layers []struct {
			Name struct {
				Type           string `json:"type"`
				File           string `json:"file"`
				DotCodexFolder string `json:"dotCodexFolder"`
			} `json:"name"`
			Config struct {
				MCPServers map[string]json.RawMessage `json:"mcp_servers"`
				Skills     struct {
					Config []codexSkillEntry `json:"config"`
				} `json:"skills"`
				codexHookSwitches
			} `json:"config"`
			DisabledReason string `json:"disabledReason"`
		} `json:"layers"`
		Config codexHookSwitches `json:"config"`
	} `json:"result"`
}

// codexHookSwitches is the part of a configuration that can switch the run's
// nested hook off: the two names of the hooks feature, and each hook's state.
type codexHookSwitches struct {
	Features map[string]json.RawMessage `json:"features"`
	Hooks    struct {
		State map[string]struct {
			Enabled *bool `json:"enabled"`
		} `json:"state"`
	} `json:"hooks"`
}

// codexRequirementsFile is the machine's requirements Codex 0.171.0 reads,
// its top-level `allow_managed_hooks_only` running managed hooks alone (read
// in its ConfigRequirementsToml; root's alone to write, so not measured). The
// Codex home's own requirements.toml is not read (measured).
const codexRequirementsFile = "/etc/codex/requirements.toml"

// codexRequirementsMark opens [codexRequirementsFile]'s content in the
// guard's answer, after the app-server's.
const codexRequirementsMark = "crewlet-codex-requirements"

// codexHookFeatures is the two names Codex 0.171.0 reads its hooks feature
// by, either false switching every hook off (measured).
var codexHookFeatures = []string{"codex_hooks", "hooks"}

// codexHooksVerdict refuses the run where out's `config/read` answer switches
// the run's nested hook off: a hooks feature false in the configuration Codex
// settles on, named by the highest loaded layer setting it (a trusted
// project's counts), or the hook's own state `enabled = false` in a loaded
// layer other than a project's, which Codex ignores there (measured). home
// names the layer the guard's own home stands in for, the box home's file.
func codexHooksVerdict(out, home string) error {
	var read codexConfigRead
	for l := range strings.SplitSeq(out, "\n") {
		var r codexConfigRead
		if json.Unmarshal([]byte(l), &r) == nil && string(r.ID) == "2" && r.Result != nil {
			read = r
		}
	}
	var said []string
	if _, text, ok := strings.Cut(out, "\n"+codexRequirementsMark+"\n"); ok {
		var requirements struct {
			ManagedHooksOnly *bool `toml:"allow_managed_hooks_only"`
		}
		if _, err := toml.Decode(text, &requirements); err == nil && requirements.ManagedHooksOnly != nil &&
			*requirements.ManagedHooksOnly {
			said = append(said, codexRequirementsFile+" sets allow_managed_hooks_only = true, which runs the "+
				"machine's managed hooks alone")
		}
	}
	if read.Result == nil {
		return codexHooksRefusal(said)
	}
	file := func(i int) string {
		layer := read.Result.Layers[i]
		switch {
		case layer.Name.Type == "user":
			return home
		case layer.Name.DotCodexFolder != "":
			return layer.Name.DotCodexFolder + "/config.toml"
		case layer.Name.File != "":
			return layer.Name.File
		}
		return "Codex's " + layer.Name.Type + " configuration"
	}
	for _, feature := range codexHookFeatures {
		if string(read.Result.Config.Features[feature]) != "false" {
			continue
		}
		for i, layer := range read.Result.Layers {
			if v, ok := layer.Config.Features[feature]; ok && layer.DisabledReason == "" && string(v) != "null" {
				said = append(said, fmt.Sprintf("%s sets features.%s = false", file(i), feature))
				break
			}
		}
	}
	for i, layer := range read.Result.Layers {
		state, ok := layer.Config.Hooks.State[codexNestedHookKey]
		if ok && state.Enabled != nil && !*state.Enabled && layer.DisabledReason == "" && layer.Name.Type != "project" {
			said = append(said, fmt.Sprintf("%s sets hooks.state.%q.enabled = false", file(i), codexNestedHookKey))
		}
	}
	return codexHooksRefusal(said)
}

// codexSkillsVerdict refuses the run where out's `config/read` answer
// switches one of skills off in a layer beyond the home's own, which
// [codexSkillsOff] reads: the machine's /etc/codex/config.toml or a managed
// layer (root's to write, so read in Codex's answer, not measured). Every
// loaded layer but a project's (whose entries do nothing, measured) counts,
// lowest first, a later entry for the skill deciding ([codexSkillsSettled]).
func codexSkillsVerdict(out string, skills []sandbox.Skill, paths Paths) error {
	if len(skills) == 0 {
		return nil
	}
	var read codexConfigRead
	for l := range strings.SplitSeq(out, "\n") {
		var r codexConfigRead
		if json.Unmarshal([]byte(l), &r) == nil && string(r.ID) == "2" && r.Result != nil {
			read = r
		}
	}
	if read.Result == nil {
		return nil
	}
	var entries []codexSkillEntry
	for i := len(read.Result.Layers) - 1; i >= 0; i-- {
		layer := read.Result.Layers[i]
		if layer.DisabledReason == "" && layer.Name.Type != "project" {
			entries = append(entries, layer.Config.Skills.Config...)
		}
	}
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		names = append(names, s.Listed())
	}
	off := codexSkillsSettled(entries, names, paths)
	var said []string
	for _, name := range names {
		c, ok := off[name]
		if !ok {
			continue
		}
		where := "a layer of Codex's own configuration"
		for i := range read.Result.Layers {
			for _, e := range read.Result.Layers[i].Config.Skills.Config {
				if e.Name == c.Name && e.Path == c.Path && e.Enabled != nil && !*e.Enabled {
					where = codexLayerFile(read.Result.Layers[i].Name.Type, read.Result.Layers[i].Name.File,
						read.Result.Layers[i].Name.DotCodexFolder, paths)
				}
			}
		}
		said = append(said, fmt.Sprintf("%s holds a [[skills.config]] entry (name %q, path %q) with enabled = false "+
			"for the seat skill %s", where, c.Name, c.Path, name))
	}
	if len(said) == 0 {
		return nil
	}
	return fmt.Errorf("executor: Codex's configuration switches off a skill of the run's, which Codex would "+
		"then go without, with no word: %s; the run never switches back on what a person switched off, so "+
		"remove that entry", strings.Join(said, "; "))
}

// codexLayerFile names a `config/read` layer's file: the box home's for the
// user layer the guard's own home stands in for.
func codexLayerFile(kind, file, folder string, paths Paths) string {
	switch {
	case kind == "user":
		return paths.CodexHome() + "/config.toml"
	case folder != "":
		return folder + "/config.toml"
	case file != "":
		return file
	}
	return "Codex's " + kind + " configuration"
}

// codexHooksRefusal is the run's refusal for said, what switches its nested
// hook off; nil for nothing.
func codexHooksRefusal(said []string) error {
	if len(said) == 0 {
		return nil
	}
	return fmt.Errorf("executor: Codex would run without the run's hook that hands a checkout cloned during it "+
		"its instructions: %s, and the run never switches on a hook the box switched off; remove that key",
		strings.Join(said, "; "))
}

// codexGuardVerdict refuses the run where out, an app-server's stdout, holds
// a `config/read` answer in which a layer Codex loads, other than the run's
// own file (`user`) or its command line (`sessionFlags`), holds the entry of
// one of servers, any key of it, an empty one included: a project's (a
// parent's counts), the system's, a managed one. A layer Codex does not load
// (an untrusted project's) and another server's entry start the run. AN
// ANSWER WITHOUT LAYERS REFUSES IT: Codex lists the run's own at least, so a
// release that does not is one the guard cannot hold to the rule; a box with
// no codex ([codexGuardNoCodex]) is the script's to refuse.
func codexGuardVerdict(out string, servers []string) error {
	if len(servers) == 0 {
		return nil
	}
	read := codexConfigAnswer(out)
	if read.Result == nil || len(read.Result.Layers) == 0 {
		if strings.TrimSpace(out) == codexGuardNoCodex {
			return nil
		}
		return fmt.Errorf("executor: the box's Codex gave no layers of its configuration for the start directory "+
			"within %s (config/read), so the launch cannot tell whether a repository changes Crewlet's own "+
			"server; the box's codex must be Codex %s, which answers it", codexGuardAnswer, codexFloor())
	}
	var said []string
	var entries []words.Said
	for _, layer := range read.Result.Layers {
		if layer.DisabledReason != "" || layer.Name.Type == "user" || layer.Name.Type == "sessionFlags" {
			continue
		}
		file := layer.Name.File
		if layer.Name.DotCodexFolder != "" {
			file = layer.Name.DotCodexFolder + "/config.toml"
		}
		if file == "" {
			file = "Codex's " + layer.Name.Type + " configuration"
		}
		for _, name := range servers {
			raw, ok := layer.Config.MCPServers[name]
			if !ok || string(raw) == "null" {
				continue
			}
			entry := words.Said("[mcp_servers." + name + "]")
			if !slices.Contains(entries, entry) {
				entries = append(entries, entry)
			}
			var keys map[string]json.RawMessage
			if json.Unmarshal(raw, &keys) != nil || len(keys) == 0 {
				said = append(said, fmt.Sprintf("%s holds %s as %s", file, entry, codexShown(raw)))
				continue
			}
			for _, key := range slices.Sorted(maps.Keys(keys)) {
				said = append(said, fmt.Sprintf("%s sets mcp_servers.%s.%s = %s in %s", file, name, key,
					codexShown(keys[key]), entry))
			}
		}
	}
	if len(said) == 0 {
		return nil
	}
	return fmt.Errorf("executor: a repository's configuration may not change Crewlet's own server, which the "+
		"run's notes and tools go through, and Codex ranks it above the run's own: %s; remove %s from that "+
		"configuration", strings.Join(said, "; "), words.And(entries))
}

// codexConfigAnswer is the `config/read` answer out, an app-server's stdout,
// holds; one with no result where it holds none.
func codexConfigAnswer(out string) codexConfigRead {
	var read codexConfigRead
	for l := range strings.SplitSeq(out, "\n") {
		var r codexConfigRead
		if json.Unmarshal([]byte(l), &r) == nil && string(r.ID) == "2" && r.Result != nil {
			read = r
		}
	}
	return read
}

// codexDocLimit is the project_doc_max_bytes out's `config/read` answer gives
// the start directory, its effective configuration's, the home's or a trusted
// start directory's setting where one sets it and Codex's 32768 where none
// does (measured on 0.171.0); [codexProjectDocMaxBytes] where the answer holds
// no whole number of bytes, as for a box with no codex or no answer. Read
// apart from the verdict's decode, so an odd value never costs its layers.
func codexDocLimit(out string) int {
	limit := codexProjectDocMaxBytes
	for l := range strings.SplitSeq(out, "\n") {
		var r struct {
			ID     json.RawMessage `json:"id"`
			Result *struct {
				Config map[string]json.RawMessage `json:"config"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(l), &r) != nil || string(r.ID) != "2" || r.Result == nil {
			continue
		}
		var n int
		if raw, ok := r.Result.Config["project_doc_max_bytes"]; ok && json.Unmarshal(raw, &n) == nil && n >= 0 {
			limit = n
		} else {
			limit = codexProjectDocMaxBytes
		}
	}
	return limit
}

// codexShown is a value of a layer's configuration as the refusal shows it:
// compact JSON on one line, cut short.
func codexShown(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return firstLine(string(raw), 200)
	}
	return firstLine(b.String(), 200)
}
