package sandbox

import (
	"fmt"
	"strconv"
	"strings"
)

// A CODING BOX'S GIT ON GITLET (ADR-0093): where it dials and the
// configuration it runs with, environment the engine hands the launch, and the
// credential it presents, a file the engine writes in the box ([GitAccessFile])
// that the environment names by its path alone, never an operator's setup
// step. Out of the environment, the credential reaches no process that prints
// or passes on its environment: a model's tool shell, a harness's hook.

// BoxGitURLVar is the base a box dials for Gitlet: a Gitlet node's public
// listener and the /box/git prefix (https://git.example.com/box/git), or a
// balancer over every node's, read through [ParseBoxURL] by every node that
// launches executors, in the engine host's network: a container box dials a
// loopback one under its runtime's alias ([BoxGit.EngineHost]). Unlike the
// tool bridge's URL, which must address the launching node, ANY Gitlet node
// serves any box: every node reads the run's admitting row.
const BoxGitURLVar = "CREWLET_BOX_GIT_URL"

// The variables an executor's git reaches Gitlet by: the launching node's
// [BoxGitURLVar] as the box dials it ([BoxGit.Env]), which every clone step's
// remote expands ([CloneRecipe]), and the absolute path in the box of the file
// holding the run's credential, which the engine's helper reads to answer git
// ([BoxGit]). Both are the ENGINE'S: config.ReservedSandboxEnv lists them, so
// no operator's value is ever beside them. Neither name says credential, so no
// harness's redaction of such names takes them from the shell the box's git
// runs in.
const (
	GitURLVar        = "CREWLET_GIT_URL"
	GitAccessFileVar = "CREWLET_GIT_ACCESS_FILE"
)

// GitAccessFile is where the run's Gitlet credential is written, relative to
// the box's home ([GitAccessStep]), whose absolute path [GitAccessFileVar]
// holds. Each cell writes it readable by the box's user alone (0600).
const GitAccessFile = ".crewlet/git-access"

// GitAccessStep is the setup step writing token, a box's Gitlet credential, to
// [GitAccessFile]: before a check's checkout, and once an executor's box is
// made or reattached, before its job starts ([Coordinator.Launch]). A file the
// step writes is never logged, only counted.
func GitAccessStep(token string) SetupStep {
	return SetupStep{Name: "gitlet-access", Files: map[string]string{GitAccessFile: token}}
}

// gitConfigCount and its two siblings are git's own variables for
// configuration handed through the environment (git-config(1), "ENVIRONMENT"):
// a count, and a key and a value for each index below it.
const (
	gitConfigCount = "GIT_CONFIG_COUNT"
	gitConfigKey   = "GIT_CONFIG_KEY_"
	gitConfigValue = "GIT_CONFIG_VALUE_"
)

// GitConfigParametersVar is the variable `git -c` hands its settings to the
// commands it runs in, which git reads AFTER every GIT_CONFIG_COUNT entry: the
// one place a setting outranks entries a harness appends to the count (Gemini
// CLI's shell empties credential.helper and core.hooksPath there).
const GitConfigParametersVar = "GIT_CONFIG_PARAMETERS"

// GitConfigParameter is one [GitConfigParametersVar] item setting key to
// value, single-quoted as git's sq_quote_buf quotes one, which steps a quote
// or a ! out of the quotes behind a backslash. In git's first form,
// 'key=value', which every git reads: the 'key'='value' form git 2.31 writes
// is refused by an older git as a bogus format, failing every command, so a
// key holding '=' cannot be written ([BoxGit.Env] refuses such a base).
func GitConfigParameter(key, value string) string {
	return "'" + strings.NewReplacer("'", `'\''`, "!", `'\!'`).Replace(key+"="+value) + "'"
}

// BoxGit is the engine's git configuration of one launch of a run given
// repositories on Gitlet: what [BoxGit.Env] puts in the launch's
// environment.
type BoxGit struct {
	// Base is Gitlet's base in the engine host's network: [BoxGitURLVar] as
	// [ParseBoxURL] read it, so with no trailing '/'.
	Base string
	// EngineHost is the name the engine host answers to inside the box
	// ([Manager.EngineHost] of its cell), under which the box dials a loopback
	// Base; "" for a box on the host's network, which dials Base as written.
	EngineHost EngineHost
	// User is the Basic user name Gitlet takes a box's token under (Gitlet's
	// BoxUser), and Token the run's credential for this launch.
	User, Token string
	// Name and Email are the seat's commit identity, the one its engine-built
	// commits take too, BOTH or neither (one alone is refused): a CHECK'S box (user
	// `check`) acts as nobody and gets none, so a commit its command makes fails
	// rather than naming somebody who never wrote it.
	Name, Email string
}

// Launch is the box's git environment over run ([BoxGit.Env]) with the
// credential its helper answers with, which the launch writes into the box
// ([LaunchEnv.GitAccess]).
func (g BoxGit) Launch(run map[string]string) (LaunchEnv, error) {
	env, err := g.Env(run)
	if err != nil {
		return LaunchEnv{}, err
	}
	return LaunchEnv{Env: env, GitAccess: g.Token}, nil
}

// Env is the box's git environment over run, the run environment assembled
// from the operator's configuration, answering only what it adds: Gitlet's
// base as the box dials it ([GitURLVar]) and git configuration through the
// environment (git-config(1), "ENVIRONMENT"), the operator's entries kept and
// the engine's appended LAST, so its single-valued keys win: a seat's identity
// is role.email's to change, never an environment variable's. Every repository
// the box's git touches has them, one cloned from another host included.
// Never the credential, nor [GitAccessFileVar], whose path is the box's,
// joined by whoever writes the file there ([GitAccessStep]). An operator's
// GIT_CONFIG_COUNT git would refuse is refused here, naming it
// ([operatorGitConfig]), rather than failing every clone in git's words.
func (g BoxGit) Env(run map[string]string) (map[string]string, error) {
	switch {
	case g.Base == "" || strings.HasSuffix(g.Base, "/"):
		return nil, fmt.Errorf("sandbox: a box's git base %q is not one ParseBoxURL answers", g.Base)
	case !plainWord(g.User):
		// Spelled into the helper's shell text, so held to what needs no
		// quoting there.
		return nil, fmt.Errorf("sandbox: a box's git user %q is not a plain word", g.User)
	case g.Token == "":
		return nil, fmt.Errorf("sandbox: a box's git is given no credential")
	case strings.Contains(g.Base, "="):
		return nil, fmt.Errorf("sandbox: %s=%q holds an '=', which no %s item a box's git reads can "+
			"name in a credential key; serve Gitlet's box path at a URL without one",
			BoxGitURLVar, g.Base, GitConfigParametersVar)
	case (g.Name == "") != (g.Email == ""):
		return nil, fmt.Errorf("sandbox: a box's git identity is a name and an email together, or none for " +
			"a box that acts as nobody")
	}
	n, err := operatorGitConfig(run)
	if err != nil {
		return nil, err
	}
	// THE BASE THE BOX DIALS, which the helper's scope is spelled from too: git
	// matches a scope against the URL it dials, so a scope on the engine host's
	// loopback would answer no clone a container box makes.
	base := g.EngineHost.Address(g.Base)
	// The trailing '/' scopes the helper to Gitlet's paths: git matches a URL's
	// path as a prefix ending at a '/', so never a sibling path of the same host.
	// In GIT_CONFIG_PARAMETERS, after the run environment's own items: read
	// after every count entry, so a harness's appended empty credential.helper
	// (Gemini CLI's shell) cannot reset the list past it.
	scope := "credential." + base + "/.helper"
	credential := strings.Join([]string{
		// Empty RESETS the helper list (git-config(1), credential.helper), so an
		// operator's `credential.helper store` is neither asked for the token nor
		// told to save it.
		GitConfigParameter(scope, ""),
		// Answers `get` from the file the box's environment names, read at each
		// ask, so the credential is in no variable; one that cannot be read
		// answers nothing and fails the ask. `store` and `erase` do nothing.
		GitConfigParameter(scope, `!f() { test "$1" = get || return 0; t=$(cat "$`+GitAccessFileVar+
			`") || return 1; printf 'username=`+g.User+`\npassword=%s\n' "$t"; }; f`),
	}, " ")
	if prior := run[GitConfigParametersVar]; prior != "" {
		credential = prior + " " + credential
	}
	var entries []struct{ key, value string }
	// The seat's identity, as an engine-built commit has it. A convention a
	// cooperating box keeps, never a check: the push's row is the provenance.
	if g.Name != "" {
		entries = append(entries, struct{ key, value string }{"user.name", g.Name},
			struct{ key, value string }{"user.email", g.Email})
	}
	entries = append(entries, []struct{ key, value string }{
		// Gitlet serves partial clones and fetches by id under v2.
		{"protocol.version", "2"},
		// feature.manyFiles's two index settings without its index.skipHash, whose
		// index older libgit2 refuses; measured in by BenchmarkBoxWorktree
		// (internal/gitlet/host; docs/concepts/code-sandbox.md "What a clone costs").
		{"index.version", "4"},
		{"core.untrackedCache", "true"},
	}...)
	out := map[string]string{GitURLVar: base, GitConfigParametersVar: credential,
		gitConfigCount: strconv.Itoa(n + len(entries))}
	for i, e := range entries {
		out[gitConfigKey+strconv.Itoa(n+i)] = e.key
		out[gitConfigValue+strconv.Itoa(n+i)] = e.value
	}
	return out, nil
}

// operatorGitConfig is how many configuration entries run already hands git
// through the environment: its GIT_CONFIG_COUNT, held to git's own rule —
// a non-negative integer, and a key and a value for every index below it —
// and zero where it sets none.
func operatorGitConfig(run map[string]string) (int, error) {
	raw, set := run[gitConfigCount]
	if !set {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("sandbox: the run environment (role.sandbox.env or a setup step's env) sets "+
			"%s=%q, which is not a count: a box's git refuses it, so every clone of Gitlet's "+
			"repositories would fail", gitConfigCount, raw)
	}
	for i := range n {
		for _, name := range []string{gitConfigKey + strconv.Itoa(i), gitConfigValue + strconv.Itoa(i)} {
			if _, ok := run[name]; !ok {
				return 0, fmt.Errorf("sandbox: the run environment (role.sandbox.env or a setup step's env) "+
					"sets %s=%d but no %s: a box's git refuses a configuration with a hole in it, so every "+
					"clone of Gitlet's repositories would fail", gitConfigCount, n, name)
			}
		}
	}
	return n, nil
}

// plainWord reports s is lowercase letters alone: a word a shell reads as
// written, inside single quotes or out.
func plainWord(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}
