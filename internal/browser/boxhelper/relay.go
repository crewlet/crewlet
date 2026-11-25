package boxhelper

import (
	"path"
	"strings"

	"github.com/crewlet/crewlet/internal/shellword"
)

// RelayScript is how a job's script starts the job's relay, for the box whose
// home is home, on the wiring file at wiring: node running the helper's
// relay, as a POSIX sh script, every word quoted. node starts through
// /usr/bin/env -i, the env every #!/usr/bin/env script names, with the
// supervisor's environment alone ([supervisorEnv]): each of its names the
// script has set, as the script has it, set or empty, and nothing else. So
// none of the job's own variables reaches the relay: not NODE_OPTIONS or
// NODE_DEBUG, which change what node loads and what it logs, nor the run's
// keys, which the relay has no use for. env execs node, so the pid the
// script's $! names is the relay's own. It carries no token: the wiring file
// holds the route.
func RelayScript(home, wiring string) string {
	var b strings.Builder
	b.WriteString("exec /usr/bin/env -i")
	for _, name := range supervisorEnv() {
		b.WriteString(" ${" + name + `+"` + name + "=$" + name + `"}`)
	}
	b.WriteString(" node " + shellword.Quote(path.Join(Folder(home), "helper.mjs")) + " relay --wiring " +
		shellword.Quote(wiring))
	return b.String()
}
