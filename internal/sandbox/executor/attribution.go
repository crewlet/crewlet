package executor

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/events/types"
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
//
// A TRAILER IS A CLAIM, NEVER EVIDENCE. The run can delete the hook, unset
// the entry or type any trailer it likes, so nothing in the engine reads a
// pushed commit's trailers: a box push's provenance is stamped from the run's
// own row at its admission. The trailers carry that provenance where the row
// does not reach — a mirror of the repository, and a commit older than the
// code host's push horizon, after which the row is gone. No Go string of the
// binary but this file's spells a Crewlet trailer's key, and the keys are
// unexported, so a reader of them cannot be added unseen
// (TestNothingButTheAttributionSpellsATrailerKey).
//
// ONE OF EACH CREWLET TRAILER, REPLACED: a commit of the run's own that an
// amend, a rebase or a cherry-pick rewrote carries the trailers of the run
// that rewrote it, as git itself replaces the committer, never a second set
// beside the first. The co-author line is added only where it is not already
// there, never replacing one: a person the commit already credits stays
// credited.

// attributionHookScript is the hook, with [attributionTrailersMark] where the
// run's trailers go. Embedded rather than shipped beside the binary, so the
// hook a run is handed is the one this build tests.
//
//go:embed attribution_hook.sh
var attributionHookScript string

// attributionTrailersMark is where [attributionHookScript] takes the run's
// trailers, as `--if-exists` and `--trailer` arguments to git
// interpret-trailers ([attributionHook]).
const attributionTrailersMark = "@TRAILERS@"

// CoAuthor is who a run's commits credit: Crewlet, at an address verified on
// the GitHub account that renders it — so the commit lists Crewlet among its
// participants beside the seat that authored it. NOT `Signed-off-by`, which
// is no attribution at all: it certifies the Developer Certificate of Origin,
// which only the party answerable for the code can do, and GitHub renders it
// as nothing.
const CoAuthor = "Crewlet <noreply@crewlet.ai>"

// The keys of the Crewlet trailers: the seat that made a commit, the run that
// wrote it, and the work item its turn is charged to. UNEXPORTED, so no
// package can read a trailer by them ([attributionTrailers]).
const (
	seatTrailer     = "Crewlet-Seat"
	runTrailer      = "Crewlet-Run"
	workItemTrailer = "Crewlet-Work-Item"
)

// noWorkItem is the work-item trailer of a seat's run on no work item.
// WRITTEN, never left out: the trailer replaces the one a rewritten commit
// carried, so a run on no item that amended another run's commit would
// otherwise keep that run's item beside its own run.
const noWorkItem = "none"

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

// attributionTrailer is one trailer a run's commits carry, with the action
// git interpret-trailers takes where the message already holds one of its key.
type attributionTrailer struct {
	line     string
	ifExists string
}

// attributionTrailers is the trailers a run's commits carry: the co-author;
// the seat and its work item where the run is a seat's; and the run where
// the attribution names one.
//
// A RUN THAT IS NOT ONE LINE OF PRINTABLE TEXT IS REFUSED rather than
// written: a line break in a trailer's value would end the trailer there and
// add whatever followed as a line of the commit message. The seat and the
// item are ENCODED instead ([trailerValue]): a tracker's key is somebody
// else's text, and a label must not fail a run over a convention. A work
// item with no seat, or with no backend, is the engine's own bug, refused as
// for any failure of the run's environment.
func attributionTrailers(a sandbox.CommitAttribution) ([]attributionTrailer, error) {
	trailers := []attributionTrailer{{line: "Co-authored-by: " + CoAuthor, ifExists: "addIfDifferent"}}
	switch {
	case a.Seat == "" && a.WorkItem != nil:
		return nil, fmt.Errorf("executor: the run's commits would name work item %q but no seat", a.WorkItem.Key)
	case a.WorkItem != nil && a.WorkItem.Backend == "":
		return nil, fmt.Errorf("executor: the run's commits would name work item %q, which names no backend",
			a.WorkItem.Key)
	}
	if a.Seat != "" {
		trailers = append(trailers, attributionTrailer{line: seatTrailer + ": " + trailerValue(a.Seat),
			ifExists: "replace"})
	}
	if a.Run != "" {
		if strings.TrimSpace(a.Run) != a.Run || strings.ContainsFunc(a.Run, unicode.IsControl) {
			return nil, fmt.Errorf("executor: the run's commits would name it %q, which is not "+
				"one line of printable text a %s trailer can carry", a.Run, runTrailer)
		}
		trailers = append(trailers, attributionTrailer{line: runTrailer + ": " + a.Run, ifExists: "replace"})
	}
	if a.Seat != "" {
		trailers = append(trailers, attributionTrailer{line: workItemTrailer + ": " + workItemValue(a.WorkItem),
			ifExists: "replace"})
	}
	return trailers, nil
}

// workItemValue is the work-item trailer's value for item: `<backend> <key>`,
// the backend alone where the key is empty — a key alone is ambiguous between
// trackers — and [noWorkItem] for nil, each part through [trailerValue].
func workItemValue(item *types.WorkItem) string {
	if item == nil {
		return noWorkItem
	}
	work := trailerValue(string(item.Backend))
	if item.Key != "" {
		work += " " + trailerValue(item.Key)
	}
	return work
}

// trailerValue is s with every byte outside A-Z a-z 0-9 - . _ ~ / # ! @ +
// written as %XX in upper-case hex — the space and '%' among them — so the
// result is one line of printable ASCII with no space, which no value can turn
// into a second trailer line, and url.PathUnescape turns it back into s.
// "ENG-412", "acme/api#88", "group/sub/proj!12", every handle and every UUID
// pass through unchanged. It never cuts: git bounds no message line, and a
// cut key could not be recovered.
func trailerValue(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			strings.IndexByte("-._~/#!@+", c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// attributionHook is the hook script crediting trailers: each one argument
// quoted for sh, after the --if-exists action it is added under, which git
// interpret-trailers applies to every --trailer that follows it.
func attributionHook(trailers []attributionTrailer) []byte {
	args := make([]string, len(trailers))
	for i, trailer := range trailers {
		args[i] = "--if-exists " + trailer.ifExists + " --trailer " + sandbox.ShellQuote(trailer.line)
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
	link := "cd " + sandbox.ShellQuote(paths.CommitAttribution()) + " && cp hook.source hook && rm hook.source && " +
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
		`export "GIT_CONFIG_KEY_$_crewlet_n=core.hooksPath" "GIT_CONFIG_VALUE_$_crewlet_n="` + sandbox.ShellQuote(hooks) +
		` "GIT_CONFIG_COUNT=$((_crewlet_n + 1))" ;; esac; `
}
