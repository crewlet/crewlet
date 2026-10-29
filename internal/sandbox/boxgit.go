package sandbox

import (
	"fmt"
	"strconv"
	"strings"
)

// A CODING BOX'S GIT ON THE ENGINE'S OWN CODE HOST (ADR-0063): where it dials,
// the credential it presents and the configuration it runs with — all of it
// environment the engine hands the launch, never a file in the box and never
// a setup step of the operator's.

// BoxGitURLVar is the base a box dials for the engine's code host: a git
// node's public listener and the /box/git prefix — https://git.example.com/box/git,
// or a balancer over every git node's. Read by every node that launches coding
// runs, through [ParseBoxURL] like the other two addresses a box dials back to
// the engine. Unlike the tool bridge's URL, which must address the launching
// node itself, ANY git node serves any box: the run is admitted by its row,
// which every node reads.
const BoxGitURLVar = "CREWLET_BOX_GIT_URL"

// The variables of a coding run's environment its git reaches the code host
// by: the launching node's [BoxGitURLVar], which every clone step's remote
// expands ([CloneRecipe]), and the run's own credential, which the engine's
// credential helper answers git with ([BoxGit]). Each is the ENGINE'S —
// config.ReservedSandboxEnv lists both, so no operator's value is ever beside
// them.
const (
	GitURLVar   = "CREWLET_GIT_URL"
	GitTokenVar = "CREWLET_GIT_TOKEN"
)

// gitConfigCount and its two siblings are git's own variables for
// configuration handed through the environment (git-config(1), "ENVIRONMENT"):
// a count, and a key and a value for each index below it.
const (
	gitConfigCount = "GIT_CONFIG_COUNT"
	gitConfigKey   = "GIT_CONFIG_KEY_"
	gitConfigValue = "GIT_CONFIG_VALUE_"
)

// BoxGit is the engine's git configuration of one launch of a run given
// repositories on the code host: what [BoxGit.Env] puts in the launch's
// environment.
type BoxGit struct {
	// Base is the code host's base as a box dials it — [BoxGitURLVar], as
	// [ParseBoxURL] read it, so with no trailing '/'.
	Base string
	// User is the Basic user name the code host takes a box's token under
	// (the code host's BoxUser), and Token the run's credential for this
	// launch.
	User, Token string
	// Name and Email are the seat's commit identity: the one identity rule a
	// seat's commits take, whether the engine builds them or its box does.
	Name, Email string
}

// Env is the box's git environment over run — the run environment the launch
// has assembled from the operator's configuration — answering only what it
// adds: the run's credential and the base ([GitTokenVar], [GitURLVar]), and
// git's configuration through the environment with the operator's own entries
// KEPT and the engine's appended after them.
//
// THE ENGINE'S ENTRIES, IN ORDER:
//
//  1. credential.<base>/.helper, empty — it RESETS the helper list for the
//     code host's URL (git-config(1), credential.helper: "an empty value
//     resets the list"), so a `credential.helper store` the operator
//     configured is neither asked for the run's token nor told to save it;
//  2. credential.<base>/.helper, a shell function answering git's `get` with
//     the user and the token, read from the PROCESS environment when git asks,
//     so the token is never written to a file — and `store` and `erase` are
//     no-ops that succeed, so nothing keeps it and git says nothing of them;
//  3. user.name and 4. user.email, the seat's identity — a commit the box
//     writes is the seat's as an engine-built one is. A CONVENTION a
//     cooperating box keeps, never a check: generated code sets
//     GIT_COMMITTER_* or `-c user.*` as it likes, and the push's row, never
//     a commit header, is the provenance;
//  5. protocol.version 2, which the code host serves partial clones and
//     fetches by id under, held against a box's own configuration.
//
// The URL key carries a trailing '/', so git's URL match (a path matched as a
// prefix ending at a '/') scopes the helper to the code host's paths and
// never to a sibling path of the same host. The engine's come LAST, so for
// the three single-valued keys its value wins: a seat's identity is
// role.email's to change, never an environment variable's.
//
// An operator's GIT_CONFIG_COUNT that is not a non-negative integer, or that
// names an index whose key or value the run environment does not set, is
// refused naming it: git refuses a configuration with a hole in it, and every
// clone would fail with git's words rather than the engine's.
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
	}
	n, err := operatorGitConfig(run)
	if err != nil {
		return nil, err
	}
	scope := "credential." + g.Base + "/.helper"
	entries := []struct{ key, value string }{
		{scope, ""},
		{scope, `!f() { test "$1" = get || return 0; printf 'username=` + g.User + `\npassword=%s\n' "$` +
			GitTokenVar + `"; }; f`},
		{"user.name", g.Name},
		{"user.email", g.Email},
		{"protocol.version", "2"},
	}
	out := map[string]string{GitTokenVar: g.Token, GitURLVar: g.Base,
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
			"%s=%q, which is not a count: a box's git refuses it, so every clone of the code host's "+
			"repositories would fail", gitConfigCount, raw)
	}
	for i := range n {
		for _, name := range []string{gitConfigKey + strconv.Itoa(i), gitConfigValue + strconv.Itoa(i)} {
			if _, ok := run[name]; !ok {
				return 0, fmt.Errorf("sandbox: the run environment (role.sandbox.env or a setup step's env) "+
					"sets %s=%d but no %s: a box's git refuses a configuration with a hole in it, so every "+
					"clone of the code host's repositories would fail", gitConfigCount, n, name)
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
