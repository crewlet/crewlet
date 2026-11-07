package engine

import (
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// BoxCredentialWarnings is an advisory at each line of the company document
// that sets a variable, or writes a file, that an integration sets in an agent
// seat's box to the seat's own credential ([seataccess.BoxSet]): the
// integration's wins at launch ([Engine.runEnvironment]), so the line is
// ignored for that seat.
//
// IGNORED, NEVER WINNING: the integration's state is the one authority on
// which identity the box acts as. A line that won could hand the box the
// company's provisioner token, whose role outranks every level the agent may
// hold, or one that outlives turning the agent off, while the Agents tab
// reports the level the agent's own credential holds.
func BoxCredentialWarnings(c *config.Company) []config.Warning {
	if c == nil {
		return nil
	}
	// A PROVIDER-WIDE STEP IS EVERY SEAT'S, so it is said once, naming the
	// seats it is ignored for.
	type shared struct {
		path  config.Path
		what  string
		seats []string
	}
	common := map[string]*shared{}
	var out []config.Warning
	for role, path := range c.EachRole() {
		handle := role.IdentityKey()
		if role.Kind == org.KindHuman || handle == "" {
			continue
		}
		env, files := boxCredentialsOf(c, handle)
		if len(env) == 0 && len(files) == 0 {
			continue
		}
		// A NAME THE SEAT'S EXECUTOR SIGNS IN BY is withheld at launch
		// ([newExecutorRun]), so the agent's tool token never signs its model in.
		signs := c.ExecutorSignIns(handle)
		for _, name := range slices.Sorted(maps.Keys(env)) {
			key, signsIn := signs[name]
			if !signsIn {
				continue
			}
			out = append(out, config.SeatAdvisory(append(slices.Clone(path), "integrations"), handle, fmt.Sprintf(
				"%s is how providers.llm.%s, which runs this agent's executor, signs in to its model, so "+
					"Crewlet does not put the agent's own %s credential there and its box has no %s. Give "+
					"this agent the %s MCP server, or run its executor on an entry that does not sign in by %s",
				name, key, env[name], name, env[name], name)))
		}
		ignored := func(here config.Path, what, kind string) {
			out = append(out, config.SeatAdvisory(here, handle, fmt.Sprintf(
				"%s is on at %s, so Crewlet sets %s in its box to the agent's own credential, and this "+
					"line is ignored there: remove it", handle, kind, what)))
		}
		if box := role.Sandbox; box != nil {
			for _, key := range slices.Sorted(maps.Keys(box.Env)) {
				if kind, ok := env[key]; ok {
					ignored(append(slices.Clone(path), "sandbox", "env", key), key, kind)
				}
			}
			for i, step := range box.Setup {
				at := append(slices.Clone(path), "sandbox", "setup", i)
				stepIgnored(step, env, files, func(field, key, what, kind string) {
					ignored(append(slices.Clone(at), field, key), what, kind)
				})
			}
		}
		if provider := c.Providers.Sandbox; provider != nil {
			for i, step := range provider.Setup {
				stepIgnored(step, env, files, func(field, key, what, kind string) {
					at := config.Path{"providers", "sandbox", "setup", i, field, key}
					id := at.String()
					if common[id] == nil {
						common[id] = &shared{path: at, what: what + " (" + kind + ")"}
					}
					common[id].seats = append(common[id].seats, handle)
				})
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(common)) {
		s := common[id]
		out = append(out, config.SeatAdvisory(s.path, s.seats[0], fmt.Sprintf(
			"Crewlet sets %s in the box of %s to each agent's own credential, so this line is ignored "+
				"for %s: keep it for the seats that are not on at that integration, or remove it",
			s.what, asWritten(s.seats), plural(s.seats, "that seat", "those seats"))))
	}
	return out
}

// boxCredentialsOf is the variables and credential files the integrations
// set in handle's box, each to the integration's name.
func boxCredentialsOf(c *config.Company, handle string) (env, files map[string]string) {
	env, files = map[string]string{}, map[string]string{}
	for _, d := range seataccess.BoxDefaults(c, handle) {
		switch kind := seataccess.KindName(d.Kind); {
		case d.Credential:
			env[d.Env] = kind
		case d.File != nil && d.File.Credential():
			files[boxPath(d.File.Path)] = kind
		}
	}
	return env, files
}

// stepIgnored calls ignored for each of step's env keys among env and each of
// its files among files, by the step's field, its key and what the
// integration sets there.
func stepIgnored(step config.SandboxSetupStep, env, files map[string]string,
	ignored func(field, key, what, kind string)) {
	for _, key := range slices.Sorted(maps.Keys(step.Env)) {
		if kind, ok := env[key]; ok {
			ignored("env", key, key, kind)
		}
	}
	for _, path := range slices.Sorted(maps.Keys(step.Files)) {
		if kind, ok := files[boxPath(path)]; ok {
			ignored("files", path, "the file "+path, kind)
		}
	}
}
