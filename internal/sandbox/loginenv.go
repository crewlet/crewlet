package sandbox

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// A login shell, with the run environment over its profile.
//
// # The problem this exists for
//
// A coding CLI is commonly installed through nvm, asdf or a similar version
// manager whose PATH entries exist only in a profile a LOGIN shell sources, so
// the coding job and the setup steps that prepare its box run in one
// ([ExecOptions.Login]). But a login shell sources that profile AFTER it was
// started with the run's environment, and whatever the profile exports
// replaces what the engine set. An image's /etc/profile.d script, an E2B
// template's, or a ~/.profile a previous run left in a reused box could switch
// a coding agent's content capture back on, point its export at another
// collector, or replace a credential role.sandbox.env declares — measured with
// bash: `OTEL_LOG_USER_PROMPTS=false` in the environment and `export
// OTEL_LOG_USER_PROMPTS=true` in the profile left the command reading `true`.
// And the coding job's own `sh -lc` sourced the profile a second time, so a
// backend that got the order right for its shell would still have lost it to
// the runner's.
//
// # How the run environment ends up last
//
// The shell is started with each run variable TWICE: under its own name, so
// the profile sees the environment it always did, and under a STASH name the
// box reserves ([runEnvStash] and the variable's name). The script's first
// lines — after the profile, before the command — put every variable back from
// its stash and unset the stashes, so the command sees the run environment
// over the profile's, keeps the profile's PATH wherever the run sets none, and
// sees no stash.
//
// THE VALUES NEVER REACH THE COMMAND LINE. The script names variables and
// never their values, because a process's argv is world-readable — every ps
// on the engine host lists a container's or a direct box's — where its
// environment is not. Writing the values into a file the script sources would
// have kept them off the command line too, and left the run's credentials on
// the box's disk for as long as nothing removed them.
//
// A name that is not a shell name — one with a dot or a dash in it — is passed
// through unstashed: a shell cannot assign it, so neither can a profile.
//
// ONE LOGIN SHELL PER COMMAND, the backend's. A caller that wants one asks for
// it rather than starting its own, because a login shell inside a login shell
// sources the profile again, over the environment the outer one put back.

// runEnvStash is the prefix of the names a run variable is stashed under while
// a login shell's profile runs. The box reserves it: a run environment naming a
// variable that starts with it is refused, since its stash and that variable
// would be one name.
const runEnvStash = "CREWLET_RUN_ENV_"

// shellName is what a POSIX shell can assign and export.
var shellName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// loginScript is cmd as a login shell's -c script that puts env back over
// whatever the shell's profile exported, and the environment that shell must be
// started with for it to: env itself, and every shell-named variable of it
// stashed beside it.
func loginScript(cmd string, env map[string]string) (string, map[string]string, error) {
	start := make(map[string]string, 2*len(env))
	var restore, stashes []string
	for _, name := range slices.Sorted(maps.Keys(env)) {
		if strings.HasPrefix(name, runEnvStash) {
			return "", nil, fmt.Errorf("sandbox: the run environment names %s, and the box "+
				"reserves the %s prefix to carry the run environment past a login shell's "+
				"profile: rename the variable in role.sandbox.env or the setup step that sets it",
				name, runEnvStash)
		}
		start[name] = env[name]
		if !shellName.MatchString(name) {
			continue
		}
		stash := runEnvStash + name
		start[stash] = env[name]
		restore = append(restore, "export "+name+`="${`+stash+`}"`)
		stashes = append(stashes, stash)
	}
	if len(stashes) == 0 {
		return cmd, start, nil
	}
	return strings.Join(restore, "; ") + "; unset " + strings.Join(stashes, " ") + "; " + cmd, start, nil
}
