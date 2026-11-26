package executor

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// Commit attribution: what a run asked to credit Crewlet on its commits
// ([sandbox.CommitAttribution]) is added by git in its box, not by the model:
// a standing instruction holds only while the model remembers it, and a
// prepare-commit-msg hook runs on every commit, `--no-verify` included.
// The run's hooks directory replaces the repository's, so the one script
// linked under every hook name ([gitHooks]) hands each hook on to the
// repository's, whose refusals still refuse (attribution_hook.sh). Only a
// commit authored as the run's identity is credited: a person's commit the run
// amends, cherry-picks or rebases keeps its author and gains no co-author.
// Not reached, by construction: a commit made through a code host's API, which
// runs no git in the box, and a command run with GIT_CONFIG_PARAMETERS
// stripped from its environment ([attributionExports]).

// attributionHookScript is the hook, with [attributionTrailersMark] where the
// run's trailers go. Embedded so the hook a run is handed is the one this
// build tests.
//
//go:embed attribution_hook.sh
var attributionHookScript string

// attributionTrailersMark is where [attributionHookScript] takes the run's
// trailers, as `--if-exists` and `--trailer` arguments to git
// interpret-trailers, and attributionEntryMark where it takes the run's
// GIT_CONFIG_PARAMETERS item ([hooksParameter]), each quoted for sh
// ([attributionHook]).
const (
	attributionTrailersMark = "@TRAILERS@"
	attributionEntryMark    = "@HOOKS_ENTRY@"
)

// CoAuthor is who a run's commits credit: Crewlet, at an address verified on
// the GitHub account that renders it, so the commit lists Crewlet among its
// participants. Not `Signed-off-by`, which certifies the Developer Certificate
// of Origin (only the party answerable for the code can) and GitHub renders
// as nothing.
const CoAuthor = "Crewlet <noreply@crewlet.com>"

// The keys of the Crewlet trailers: the seat that made a commit, the run that
// wrote it, and the work item its turn is charged to. A trailer is a claim the
// box can forge, never evidence: nothing in the engine reads a pushed commit's
// trailers (a box push's provenance is stamped from the run's row at
// admission), and they carry it only where the row does not reach, a mirror
// or a commit past Gitlet's push horizon. So the keys are unexported and no
// other Go string of the binary spells one
// (TestNothingButTheAttributionSpellsATrailerKey).
const (
	seatTrailer     = "Crewlet-Seat"
	runTrailer      = "Crewlet-Run"
	workItemTrailer = "Crewlet-Work-Item"
)

// noWorkItem is the work-item trailer of a seat's run on no work item.
// Written, never left out: the trailer replaces a rewritten commit's, so a run
// on no item amending another run's commit would otherwise keep that item.
const noWorkItem = "none"

// gitHooks is every hook git runs by name out of a hooks directory
// (githooks(5)); a name missing here leaves the repository's hook of that name
// unrun while the run's entry stands. fsmonitor-watchman is listed for
// completeness only: git runs it from core.fsmonitor, not by name.
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
// the attribution names one. Each Crewlet trailer replaces the one a rewritten
// commit carries, as git replaces the committer; the co-author is added only
// where absent, so a person the commit already credits stays credited.
//
// A run that is not one line of printable text is refused: a line break would
// end the trailer and add the rest to the commit message. The seat and the
// item are encoded instead ([trailerValue]): a tracker's key is somebody
// else's text, and a label must not fail a run. A work item with no seat or no
// backend is the engine's own bug, refused like any failure of the run's
// environment.
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
// (the space and '%' among them) written as upper-case %XX: one line of
// printable ASCII no value can turn into a second trailer, which
// url.PathUnescape turns back into s. "ENG-412", "acme/api#88", handles and
// UUIDs pass unchanged. It never cuts: git bounds no message line, and a cut
// key could not be recovered.
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

// attributionHook is the hook script crediting trailers from the hooks
// directory hooks: each trailer one argument quoted for sh, after the
// --if-exists action it is added under, which git interpret-trailers applies
// to every --trailer that follows it.
func attributionHook(trailers []attributionTrailer, hooks string) []byte {
	args := make([]string, len(trailers))
	for i, trailer := range trailers {
		args[i] = "--if-exists " + trailer.ifExists + " --trailer " + shellword.Quote(trailer.line)
	}
	return []byte(strings.NewReplacer(attributionTrailersMark, strings.Join(args, " "),
		attributionEntryMark, shellword.Quote(hooksParameter(hooks))).Replace(attributionHookScript))
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
	// A program the box runs, so git's exec of it is never refused as busy
	// ([writeProgram]): a refused hook would be a refused commit.
	if err = writeProgram(ctx, box, paths.AttributionHook(), attributionHook(trailers, paths.GitHooks())); err != nil {
		return fmt.Errorf("executor: writing the commit attribution hook: %w", err)
	}
	// The links are relative and made in the box: a host-made link names a host
	// path a container lacks.
	link := "cd " + shellword.Quote(paths.CommitAttribution()) + " && chmod 700 hook && mkdir -p hooks && " +
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
// hooks: its GIT_CONFIG_PARAMETERS item ([hooksParameter]) appended to any the
// run's environment, image or profile holds. An environment entry, not a
// configuration file: git ranks it above every file, whereas written to the
// box's global configuration it would collide with the operator's setup
// recipe, and a repository's own core.hooksPath (husky's) would silently
// switch it off. GIT_CONFIG_PARAMETERS rather than a GIT_CONFIG_COUNT entry
// because git reads it after every COUNT entry: Gemini CLI's shell appends
// COUNT entries blanking core.hooksPath, which outrank an earlier COUNT entry.
// Run in the wrapper, after the login profile, for [Output.Unset]'s reason.
func attributionExports(hooks string) string {
	return `export GIT_CONFIG_PARAMETERS="${GIT_CONFIG_PARAMETERS:+$GIT_CONFIG_PARAMETERS }"` +
		shellword.Quote(hooksParameter(hooks)) + "; "
}

// hooksParameter is the GIT_CONFIG_PARAMETERS item setting core.hooksPath to
// hooks ([sandbox.GitConfigParameter]).
func hooksParameter(hooks string) string { return sandbox.GitConfigParameter("core.hooksPath", hooks) }
