package executor

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// Commit attribution: what a run asked to credit Crewlet on its commits
// ([sandbox.CommitAttribution]) is given in its box.
//
// GIT ADDS THE CREDIT, NOT THE MODEL. An instruction in the run's standing
// text — which is how a harness adds its own co-author line — holds only as
// long as the model remembers it, and nothing would notice the commit that
// went out without it. A prepare-commit-msg hook runs on every commit git
// makes, `git commit --no-verify` included, which skips pre-commit and
// commit-msg but not this one.
//
// THE HOOK IS REACHED THROUGH THE RUN'S ENVIRONMENT, never a git
// configuration file. The wrapper appends `core.hooksPath` to the run's
// GIT_CONFIG_COUNT entries ([attributionExports]), which git ranks above
// every configuration file: written into the box's global configuration
// instead, it would collide with the operator's setup recipe, which writes
// that file, and a repository's own core.hooksPath (husky's, say) would
// switch it off without a word. Appended IN THE WRAPPER, after the login
// shell has sourced its profile, for the reason [Output.Unset] is applied
// there: a profile that sets the count would otherwise replace the entry.
//
// A hooks directory replaces the repository's own for the run, so the one
// script linked under every hook name ([gitHooks]) hands each hook on to the
// repository's: the repository's hooks run as they always do, and a hook
// that refuses a commit still refuses it (attribution_hook.sh).
//
// ONLY THE RUN'S OWN COMMITS: the hook credits a commit whose author is the
// identity the run commits as. A person's commit the run amends, rewords,
// cherry-picks or carries through a rebase keeps its author and gains no
// co-author, because Crewlet did not write it.
//
// Not reached, by construction: a commit made through a code host's API (a
// tool that writes a file through GitHub's), which runs no git in the box;
// and a box whose git predates GIT_CONFIG_COUNT (2.31), which ignores it.

// attributionHookScript is the hook, with [attributionTrailersMark] where the
// run's trailers go. Embedded rather than shipped beside the binary, so the
// hook a run is handed is the one this build tests.
//
//go:embed attribution_hook.sh
var attributionHookScript string

// attributionTrailersMark is where [attributionHookScript] takes the run's
// trailers, as `--trailer` arguments to git interpret-trailers.
const attributionTrailersMark = "@TRAILERS@"

// CoAuthor is who a run's commits credit: Crewlet, at an address verified on
// the GitHub account that renders it — so the commit lists Crewlet among its
// participants beside the seat that authored it. NOT `Signed-off-by`, which
// is no attribution at all: it certifies the Developer Certificate of Origin,
// which only the party answerable for the code can do, and GitHub renders it
// as nothing.
const CoAuthor = "Crewlet <noreply@crewlet.ai>"

// RunTrailer is the key of the trailer naming the run a commit came from.
const RunTrailer = "Crewlet-Run"

// gitHooks is every hook git runs by name out of a hooks directory
// (githooks(5)). The script is linked under each of them, because a
// directory that lacked one would leave the repository's hook of that name
// unrun for as long as the run's entry stands. fsmonitor-watchman is listed
// for completeness only: git runs it from core.fsmonitor rather than by name.
var gitHooks = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch",
	"pre-commit", "pre-merge-commit", "prepare-commit-msg", "commit-msg", "post-commit",
	"pre-rebase", "post-checkout", "post-merge", "pre-push",
	"pre-receive", "update", "proc-receive", "post-receive", "post-update",
	"reference-transaction", "push-to-checkout", "pre-auto-gc", "post-rewrite",
	"sendemail-validate", "fsmonitor-watchman",
	"p4-changelist", "p4-prepare-changelist", "p4-post-changelist", "p4-pre-submit",
	"post-index-change",
}

// attributionTrailers is the trailers a run's commits carry: the co-author,
// and the run where the attribution names one.
//
// A RUN THAT IS NOT ONE LINE OF PRINTABLE TEXT IS REFUSED rather than
// written: a line break in a trailer's value would end the trailer there and
// add whatever followed as a line of the commit message.
func attributionTrailers(a sandbox.CommitAttribution) ([]string, error) {
	trailers := []string{"Co-authored-by: " + CoAuthor}
	if a.Run == "" {
		return trailers, nil
	}
	if strings.TrimSpace(a.Run) != a.Run || strings.ContainsFunc(a.Run, unicode.IsControl) {
		return nil, fmt.Errorf("executor: the run's commits would name it %q, which is not "+
			"one line of printable text a %s trailer can carry", a.Run, RunTrailer)
	}
	return append(trailers, RunTrailer+": "+a.Run), nil
}

// attributionHook is the hook script crediting trailers, each one argument
// quoted for sh.
func attributionHook(trailers []string) []byte {
	args := make([]string, len(trailers))
	for i, trailer := range trailers {
		args[i] = "--trailer " + shellQuote(trailer)
	}
	return []byte(strings.Replace(attributionHookScript, attributionTrailersMark, strings.Join(args, " "), 1))
}

// installAttribution writes the hook into the box and links it under every
// hook name in the directory the run's core.hooksPath names
// ([Paths.GitHooks]). The directory is the run's own, removed whole before
// every run ([Runner.ClearArtifacts]), so a run that credits nothing leaves
// no hook behind, and one that names another run never credits the last one.
func installAttribution(ctx context.Context, box sandbox.Sandbox, paths Paths, a sandbox.CommitAttribution) error {
	trailers, err := attributionTrailers(a)
	if err != nil {
		return err
	}
	source := paths.AttributionHook() + ".source"
	if err = box.WriteFile(ctx, source, attributionHook(trailers)); err != nil {
		return fmt.Errorf("executor: writing the commit attribution hook: %w", err)
	}
	// THE HOOK IS PUT IN PLACE BY THE BOX'S OWN PROCESS, as a copy of what
	// the engine wrote beside it. On a box on the engine host the engine's
	// own write is a descriptor a fork of the engine's can inherit, and
	// Linux refuses to execute a file any process holds open for writing
	// (ETXTBSY) — a commit's hook refused as busy is a commit refused. A
	// copy made by a process that has exited is held open by nobody.
	//
	// RELATIVE LINKS, made in the box too: a link the engine made on the
	// host would name the host's path, which a container does not have.
	link := "cd " + shellQuote(paths.CommitAttribution()) + " && cp hook.source hook && rm hook.source && " +
		"chmod 700 hook && mkdir -p hooks && " +
		"for hook in " + strings.Join(gitHooks, " ") + `; do ln -sf ../hook "hooks/$hook" || exit; done`
	res, err := box.Exec(ctx, link, sandbox.ExecOptions{})
	if err != nil {
		return fmt.Errorf("executor: linking the commit attribution hook: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("executor: linking the commit attribution hook exited %d: %s",
			res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// attributionExports is the wrapper's statement pointing git at the run's
// hooks: one more GIT_CONFIG_COUNT entry, appended after any the run's
// environment, its image or its profile already holds, so theirs still hold.
//
// A COUNT THAT IS NOT A NUMBER IS LEFT AS IT IS: git refuses every command
// under one ("bogus count in GIT_CONFIG_COUNT"), which says what is wrong far
// better than an entry written over the run's own first one would.
func attributionExports(hooks string) string {
	return `_crewlet_n=${GIT_CONFIG_COUNT:-0}; case $_crewlet_n in *[!0-9]*) ;; *) ` +
		`export "GIT_CONFIG_KEY_$_crewlet_n=core.hooksPath" "GIT_CONFIG_VALUE_$_crewlet_n="` + shellQuote(hooks) +
		` "GIT_CONFIG_COUNT=$((_crewlet_n + 1))" ;; esac; `
}
