package sandbox

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/shellword"
)

// How a box is given a repository on Gitlet.
//
// THE ONE WAY: an executor's launch and a check's both take their step from
// [CloneRecipe], so what a box's git is asked to do, and the floor it is held
// to ([GitVersionStep]), are written once. Each is an ordinary [SetupStep] run
// with the launch's environment (the box's shell expands "$CREWLET_GIT_URL"),
// so a failure fails the launch naming the step rather than leaving the
// harness to find a missing checkout.
//
// THE CHECKOUT IS RELATIVE: setup commands and the harness both run in their
// backend's default directory, whichever it is, so `./<repo>` is the same
// directory to the clone and to the agent.

// ClonePurpose is what a box is given a repository FOR, which decides how much
// of the repository the step fetches.
type ClonePurpose string

const (
	// CloneCoding is an executor's: a blobless clone of the default branch
	// (every commit and tree, contents fetched as needed, since the agent
	// rebases, logs and blames) plus the base wherever it is, no other branch,
	// an optional cone, and the branch created at the base. No hook: a commit's
	// attribution is the run's ([RunRequest.Attribution]), not this checkout's.
	// One in `.git/hooks` would be skipped wherever the repository sets
	// core.hooksPath (husky's), and write Crewlet-Run a second time.
	CloneCoding ClonePurpose = "coding"
	// CloneSingleSHA is a check's: depth one at one commit, blobless, with an
	// optional cone, since a build that deletes its checkout needs no history.
	CloneSingleSHA ClonePurpose = "single-sha"
)

// ClonePurposes is every [ClonePurpose] this build knows.
func ClonePurposes() []ClonePurpose {
	return []ClonePurpose{CloneCoding, CloneSingleSHA}
}

// Valid reports whether p is one of [ClonePurposes].
func (p ClonePurpose) Valid() bool { return slices.Contains(ClonePurposes(), p) }

// ClientGitFloor is the oldest git a box may reach Gitlet with: gitproc's
// ClientMinVersion as text, since this package imports nothing of Gitlet,
// held equal to it by TestTheSandboxClientFloorIsGitprocs. That test lives
// here so gitproc's suite takes none of this package's dependencies.
const ClientGitFloor = "2.40.0"

// GitVersionStep is the step every box given repositories runs FIRST, once
// for all of them: it fails, naming the floor, where the box has no git or
// one below [ClientGitFloor]. LOUD ON PURPOSE: a git below the floor fails
// later and obscurely (a clone refusing a filter, a by-id fetch its protocol
// cannot ask for), after spending the box and the model's tokens; this fails
// the launch as a [SetupError] naming the step, and the box is reclaimed.
//
// It reads the version as gitproc.ParseVersion does: the first line that is
// git's own (a wrapper such as hub prints its own first), its third field cut
// at the first byte neither digit nor dot (2.39.3 (Apple Git-145),
// 2.40.0.windows.1), compared as numbers. gawk, mawk and busybox's awk all
// understand "/dev/stderr".
func GitVersionStep() SetupStep {
	program := versionNumber + `
/^git version / { seen = 1; v = $3; sub(/[^0-9.].*$/, "", v)
  if (n(v) < n(want)) {
    print "git " $3 " in this box is below " want ", the oldest git a coding box may reach Gitlet with; use an image or host with a newer git" > "/dev/stderr"
    exit 3
  }
  exit 0 }
END { if (!seen) { print "the git in this box did not say its version, so it cannot be held to " want > "/dev/stderr"; exit 3 } }`
	return SetupStep{
		Name: "git-version",
		Commands: []string{
			"command -v git >/dev/null 2>&1 || { echo " + shellword.Quote("this box has no git: Gitlet needs git "+
				ClientGitFloor+" or newer in the box's image or on its host") + " >&2; exit 3; }\n" +
				"git version | awk -v want=" + ClientGitFloor + " " + shellword.Quote(program),
		},
	}
}

// versionNumber is the awk function [GitVersionStep] compares a box's git
// version by: a release's three numbers as one, so 2.40.10 is above 2.40.9.
const versionNumber = `function n(s,  a) { split(s, a, "."); return a[1] * 1000000 + a[2] * 1000 + a[3] }`

// CodingSteps is every step that gives an executor's box its repositories on
// Gitlet, in order: [GitVersionStep], then each repository's [CloneRecipe] for
// [CloneCoding] in the order given. Refused, naming it, for a repository the
// coding recipe refuses.
//
// NO STEP TURNS GIT'S FILE-SYSTEM MONITOR ON: its daemon takes an inotify
// instance and a watch per directory, every box spends the engine user's
// budget (a rootless runtime's user namespace included) beside the other
// seats' harnesses, and a monitor the kernel starves is slower than none
// (docs/concepts/code-sandbox.md, "What a clone costs").
func CodingSteps(repos []RunRepo) ([]SetupStep, error) {
	steps := []SetupStep{GitVersionStep()}
	for _, r := range repos {
		step, err := CloneRecipe(r, CloneCoding)
		if err != nil {
			return nil, fmt.Errorf("sandbox: the clone of repository %s: %w", r.Repo, err)
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// CloneRecipe is the ONE way a box is given a repository: the setup step that
// checks r out for purpose into `./<r.Repo>` from
// "$CREWLET_GIT_URL/<r.Repo>.git", every value quoted ([shellword.Quote]), refused
// where [RunRepo.ValidateFor] refuses r for purpose, an unknown one included.
// Skipped whole where `./<r.Repo>/.git` exists, keeping a reused box's work.
//
// Never treeless (`--filter=tree:0`): it carries every commit and fetches each
// tree a walk touches one request at a time. Never a bundle URI: a filtered
// clone still downloads the whole bundle. No `--reference --dissociate`: a box
// has no cache to borrow, and dissociating repacks the whole history
// (internal/gitlet/host's BenchmarkBoxDissociate). A check's box, even on a
// Gitlet node, fetches as a remote one does (a borrow is unbuilt: #496).
func CloneRecipe(r RunRepo, purpose ClonePurpose) (SetupStep, error) {
	if err := r.ValidateFor(purpose); err != nil {
		return SetupStep{}, err
	}
	var command string
	switch purpose {
	case CloneCoding:
		command = codingClone(r)
	case CloneSingleSHA:
		command = singleSHAClone(r)
	}
	return SetupStep{
		Name:     cloneStepName(r, purpose),
		Commands: []string{command},
		Brief:    cloneBrief(r, purpose),
	}, nil
}

// cloneStepName is the name a step's failure is reported under: an
// executor's clone, or a check's checkout of its one commit.
func cloneStepName(r RunRepo, purpose ClonePurpose) string {
	if purpose == CloneCoding {
		return "clone " + r.Repo
	}
	return "checkout " + r.Repo
}

// cloneWords are the quoted words every recipe writes: the directory, the
// remote, the branch's name and ref, the base and the cone.
type cloneWords struct {
	dir, url, branch, ref, base, cone string
}

// fetchBase is the command that fetches w's base BY ITS ID, at depth when set.
//
// ALWAYS, not only where the clone lacks it: with the clone's refs negotiated,
// a base off the default branch (a seat's tip, a deleted branch's commit, which
// protocol v2 serves by id) sends only what the default branch lacks, and one
// the clone holds costs only the capability request. Checking first saves
// nothing: in a partial clone `cat-file -e` of a missing object FETCHES it
// un-negotiated, the base's whole history again, and GIT_NO_LAZY_FETCH needs
// 2.44, above the 2.40 floor.
//
// --no-tags: an exact id needs no ref listing, and tag following would fetch
// the host's whole advertisement to name none.
func (w cloneWords) fetchBase(depth string) string {
	return "  git -C " + w.dir + " fetch --quiet --no-tags" + depth + " --filter=blob:none origin " + w.base + " &&"
}

// sparseCone is the command that narrows w's checkout to its cone, "" for a
// whole tree.
//
// --skip-checks, because a cone names only directories ([RunRepo.Paths]): the
// checks would refuse a real directory starting with '!' or holding any of *?[]
// as a mistyped pattern; skipped, git takes each argument as a directory and
// escapes those bytes itself. The third check, a path that is a file in the
// index, cannot fire: the clone checked nothing out, so the index is empty.
func (w cloneWords) sparseCone() string {
	if w.cone == "" {
		return ""
	}
	return "  git -C " + w.dir + " sparse-checkout set --cone --skip-checks -- " + w.cone + " &&"
}

// wordsOf quotes r's values for a recipe, each through [shellword.Quote]. The
// remote is the one unquoted expansion, the launch environment's base URL,
// itself in double quotes so whatever it holds is one word.
func wordsOf(r RunRepo) cloneWords {
	cone := make([]string, len(r.Paths))
	for i, p := range r.Paths {
		cone[i] = shellword.Quote(p)
	}
	return cloneWords{
		dir:    shellword.Quote(r.Repo),
		url:    `"$` + GitURLVar + `"/` + shellword.Quote(r.Repo+".git"),
		branch: shellword.Quote(r.Branch),
		ref:    shellword.Quote("refs/heads/" + r.Branch),
		base:   shellword.Quote(r.Base),
		cone:   strings.Join(cone, " "),
	}
}

// codingClone is [CloneCoding]'s command: a blobless, no-checkout clone of
// the default branch alone, the cone, the base fetched by id
// ([cloneWords.fetchBase]), and the branch made at the base and checked out;
// an EMPTY repository (no base) names the branch as its unborn HEAD instead.
//
// A CLONE rather than `git init` and a fetch by id, which lists no ref: the
// agent gets the default branch, as a branch and as origin/<default> under a
// remote whose HEAD it knows, to diff, log and rebase against, as a person's
// clone gives them.
func codingClone(r RunRepo) string {
	w := wordsOf(r)
	// --single-branch, because a repository's branches are mostly other seats'
	// work (gitlet.SeatNamespace, up to gitlet.MaxRefsPerRepo live refs): a
	// default clone would fetch and track every seat's branch and pull each
	// later push on the agent's fetch, so its bytes, the host's walk and its
	// transfer slot would grow with the company's seats, not the run's base.
	// The branch's tags still follow; only the listing still names every ref,
	// ~80 bytes each, once (internal/gitlet/host's BenchmarkBoxClone, its seats
	// arms). A HEAD naming a branch nobody pushed gives no branch; the base, a
	// seat's tip, still arrives by id.
	lines := []string{
		"if [ ! -e " + w.dir + "/.git ]; then",
		"  git clone --quiet --single-branch --filter=blob:none --no-checkout -- " + w.url + " " + w.dir + " &&",
	}
	if cone := w.sparseCone(); cone != "" {
		lines = append(lines, cone)
	}
	if r.Base == "" {
		lines = append(lines, "  git -C "+w.dir+" symbolic-ref HEAD "+w.ref)
	} else {
		lines = append(lines,
			w.fetchBase(""),
			"  git -C "+w.dir+" checkout --quiet -B "+w.branch+" "+w.base)
	}
	lines = append(lines, "fi")
	return strings.Join(lines, "\n")
}

// singleSHAClone is [CloneSingleSHA]'s command: a repository initialised with
// Gitlet as its remote, the one commit fetched by id at depth one with the
// blobless filter (which makes the remote its PROMISOR, so the checkout's
// files are fetched as for a blobless clone), the cone, and the commit checked
// out detached. A fetch by id, since the 2.40 floor asks for one under
// protocol v2 and `clone --revision` needs 2.49.
func singleSHAClone(r RunRepo) string {
	w := wordsOf(r)
	lines := []string{
		"if [ ! -e " + w.dir + "/.git ]; then",
		"  git init --quiet " + w.dir + " && git -C " + w.dir + " remote add origin " + w.url + " &&",
		w.fetchBase(" --depth=1"),
	}
	if cone := w.sparseCone(); cone != "" {
		lines = append(lines, cone)
	}
	lines = append(lines,
		"  git -C "+w.dir+" checkout --quiet --detach "+w.base,
		"fi")
	return strings.Join(lines, "\n")
}

// cloneBrief is what the harness is told the step made true: where the
// repository is, at what, how much of it, and how it pushes, so a run never
// spends rounds rediscovering how its git reaches Gitlet ("Your environment").
func cloneBrief(r RunRepo, purpose ClonePurpose) string {
	var b strings.Builder
	switch {
	case purpose == CloneSingleSHA:
		fmt.Fprintf(&b, "Repository `%s` is checked out at `./%s` at commit %s, with no history "+
			"(file contents are fetched as git needs them)", r.Repo, r.Repo, shortCommit(r.Base))
	case r.Base == "":
		fmt.Fprintf(&b, "Repository `%s` is at `./%s` on branch `%s`, which has no commits yet: the "+
			"repository is empty", r.Repo, r.Repo, r.Branch)
	default:
		// HOW TO FETCH ANOTHER BRANCH, and where it lands, since `git branch -r`
		// shows the default branch alone: otherwise an agent concludes the host
		// lacks it.
		fmt.Fprintf(&b, "Repository `%s` is checked out at `./%s` on branch `%s`, from %s (a blobless "+
			"clone of the default branch alone: file contents are fetched as git needs them, and another "+
			"branch with `git fetch origin <branch>`, which leaves it at `FETCH_HEAD`)", r.Repo, r.Repo,
			r.Branch, shortCommit(r.Base))
	}
	if len(r.Paths) > 0 {
		cone := make([]string, len(r.Paths))
		for i, p := range r.Paths {
			cone[i] = "`" + p + "`"
		}
		fmt.Fprintf(&b, ", with only %s checked out (git sparse-checkout)", strings.Join(cone, ", "))
	}
	b.WriteString(".")
	switch {
	case purpose != CloneCoding:
	case r.Access == RepoRead:
		// NO PUSH INSTRUCTION for a repository given to read: Gitlet refuses
		// the push, and the agent would spend a round on that refusal.
		fmt.Fprintf(&b, " Your git is configured for Gitlet, and this run was given "+
			"`%s` to read: a push to it is refused.", r.Repo)
	default:
		fmt.Fprintf(&b, " Your git is configured for Gitlet: push with `git push origin %s`, "+
			"and every push is recorded under this run and reported back for you.", r.Branch)
	}
	return b.String()
}

// shortCommit is a commit id as a person reads one: its first twelve digits.
func shortCommit(oid string) string {
	return oid[:min(len(oid), 12)]
}
