package sandbox

import (
	"fmt"
	"strconv"
	"strings"
)

// A CODING BOX'S GIT ON GITLET (ADR-0093): where it dials, the credential it
// presents and the configuration it runs with, all environment the engine
// hands the launch, never a file in the box or an operator's setup step.

// BoxGitURLVar is the base a box dials for Gitlet: a Gitlet node's public
// listener and the /box/git prefix (https://git.example.com/box/git), or a
// balancer over every node's, read through [ParseBoxURL] by every node that
// launches executors. Unlike the tool bridge's URL, which must address the
// launching node, ANY Gitlet node serves any box: every node reads the run's
// admitting row.
const BoxGitURLVar = "CREWLET_BOX_GIT_URL"

// The variables an executor's git reaches Gitlet by: the launching node's
// [BoxGitURLVar], which every clone step's remote expands ([CloneRecipe]), and
// the run's credential, which the engine's helper answers git with ([BoxGit]).
// Both are the ENGINE'S: config.ReservedSandboxEnv lists them, so no
// operator's value is ever beside them.
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
// repositories on Gitlet: what [BoxGit.Env] puts in the launch's
// environment.
type BoxGit struct {
	// Base is Gitlet's base as a box dials it: [BoxGitURLVar] as [ParseBoxURL]
	// read it, so with no trailing '/'.
	Base string
	// User is the Basic user name Gitlet takes a box's token under (Gitlet's
	// BoxUser), and Token the run's credential for this launch.
	User, Token string
	// Name and Email are the seat's commit identity, the one its engine-built
	// commits take too, BOTH or neither (one alone is refused): a CHECK'S box (user
	// `check`) acts as nobody and gets none, so a commit its command makes fails
	// rather than naming somebody who never wrote it.
	Name, Email string
}

// Env is the box's git environment over run, the run environment assembled
// from the operator's configuration, answering only what it adds: the run's
// credential and base ([GitTokenVar], [GitURLVar]), and git configuration
// through the environment (git-config(1), "ENVIRONMENT"), the operator's
// entries kept and the engine's appended LAST, so its single-valued keys win:
// a seat's identity is role.email's to change, never an environment
// variable's. Through the environment, every repository the box's git
// touches has them, one its agent clones from another host included.
//
// An operator's GIT_CONFIG_COUNT git would refuse is refused here, naming it
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
	case (g.Name == "") != (g.Email == ""):
		return nil, fmt.Errorf("sandbox: a box's git identity is a name and an email together, or none for " +
			"a box that acts as nobody")
	}
	n, err := operatorGitConfig(run)
	if err != nil {
		return nil, err
	}
	// The trailing '/' scopes the helper to Gitlet's paths: git matches a URL's
	// path as a prefix ending at a '/', so never a sibling path of the same host.
	scope := "credential." + g.Base + "/.helper"
	entries := []struct{ key, value string }{
		// Empty RESETS the helper list (git-config(1), credential.helper), so an
		// operator's `credential.helper store` is neither asked for the token nor told
		// to save it.
		{scope, ""},
		// Answers `get` from the PROCESS environment, so the token is never in a
		// file; `store` and `erase` succeed doing nothing.
		{scope, `!f() { test "$1" = get || return 0; printf 'username=` + g.User + `\npassword=%s\n' "$` +
			GitTokenVar + `"; }; f`},
	}
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
