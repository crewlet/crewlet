package sandbox

import (
	"fmt"
	"slices"
	"strings"
)

// How a box is given a repository on the engine's own code host.
//
// THE ONE WAY, for every caller: a coding run's launch and, later, a check's
// both ask [CloneRecipe] for the step, so what a box's git is asked to do, and
// the floor that git is held to ([GitVersionStep]), are written once. Each is
// an ordinary [SetupStep] — one shell command, run with the launch's own
// environment, where the box's shell expands the code host's base URL
// ("$CREWLET_GIT_URL") — so a step fails the launch loudly, naming itself,
// like any provisioning step, rather than leaving the coding agent to find a
// missing checkout.
//
// THE CHECKOUT IS RELATIVE TO WHERE THE BOX'S COMMANDS START: a setup command
// and the coding agent are both given no directory of their own, so they run
// in the one default their backend has (the box's workspace on the local
// ones), and `./<repo>` is the same directory to the clone and to the agent —
// whichever directory that default is.

// ClonePurpose is what a box is given a repository FOR, which decides how much
// of the repository the step fetches. A named string with Valid, like every
// enum of the package.
type ClonePurpose string

const (
	// CloneCoding is a coding run: a blobless clone of the repository's
	// default branch — every commit and tree of its history, each file's
	// contents fetched as git needs them, because the agent rebases, logs
	// and blames — and of the base, wherever it is; no other branch; with an
	// optional cone. The branch is created at the base and the
	// commit-trailer hook written ([TrailerHook]).
	CloneCoding ClonePurpose = "coding"
	// CloneSingleSHA is a check: depth one at one commit, blobless, with an
	// optional cone, because a build that deletes its checkout afterwards
	// needs the tree at one commit and nothing of its history. A check box
	// commits nothing, so no hook is written.
	CloneSingleSHA ClonePurpose = "single-sha"
	// CloneBorrowed is a check whose box BORROWS the objects of a git node's
	// copy of the repository, mounted read-only into it ([Mount],
	// [RunRepo.Objects]): its one commit checked out, with the cone, of
	// objects read in place as git's alternates, and NOTHING FETCHED — the
	// code host serves the check none of its bytes, since the node it runs
	// on already holds every one. The checkout reads the copy's whole history
	// through the alternates as it reads the commit, and writes nothing into
	// it. Like a single-commit checkout it commits nothing, so no hook is
	// written; and the code host is its remote, as there, so a check reads
	// the same remote whichever of the two its cell gave it.
	CloneBorrowed ClonePurpose = "borrowed"
)

// ClonePurposes is every [ClonePurpose] this build knows.
func ClonePurposes() []ClonePurpose {
	return []ClonePurpose{CloneCoding, CloneSingleSHA, CloneBorrowed}
}

// Valid reports whether p is one of [ClonePurposes].
func (p ClonePurpose) Valid() bool { return slices.Contains(ClonePurposes(), p) }

// ClientGitFloor is the oldest git a box may reach the code host with: the
// code host's CLIENT floor, internal/code/gitproc's ClientMinVersion, as
// text. Spelled here because this package imports nothing of the code host,
// and held equal to the floor by TestTheSandboxClientFloorIsGitprocs, so the
// two cannot drift. That test is this package's rather than gitproc's: it
// costs gitproc's suite nothing of this package's dependencies, and the
// sandbox's suite reads gitproc already through its test git.
const ClientGitFloor = "2.40.0"

// GitVersionStep is the step every box given repositories runs FIRST: it fails,
// naming the floor, where the box's template or image has no git or one below
// [ClientGitFloor]. ONE step for every repository, not one per repository.
//
// LOUD, ON PURPOSE: a git below the floor would otherwise fail later and
// obscurely — a clone refusing a filter, a by-id fetch the protocol it speaks
// cannot ask for — and the run would spend its box and its model's tokens
// finding that out. Its failure reaches the launch as a [SetupError] naming
// the step, with the box's own sentence, and the box is reclaimed.
//
// The version is read off the first line `git version` prints that is git's
// own — a wrapper such as hub prints its own version on a line before it —
// from its third field, cut at its first byte that is neither a digit nor a
// dot, so a vendor's suffix (2.39.3 (Apple Git-145), 2.40.0.windows.1) reads
// as its release, and compared as numbers: the reading gitproc.ParseVersion
// makes of the same line, in the box's own awk. awk's "/dev/stderr" is
// understood by gawk, mawk and busybox's awk alike.
func GitVersionStep() SetupStep {
	program := versionNumber + `
/^git version / { seen = 1; v = $3; sub(/[^0-9.].*$/, "", v)
  if (n(v) < n(want)) {
    print "git " $3 " in this box is below " want ", the oldest git a coding box may reach the code host with; use a template or image with a newer git" > "/dev/stderr"
    exit 3
  }
  exit 0 }
END { if (!seen) { print "the git in this box did not say its version, so it cannot be held to " want > "/dev/stderr"; exit 3 } }`
	return SetupStep{
		Name: "git-version",
		Commands: []string{
			"command -v git >/dev/null 2>&1 || { echo " + ShellQuote("this box has no git: the code host needs git "+
				ClientGitFloor+" or newer in the box's template or image") + " >&2; exit 3; }\n" +
				"git version | awk -v want=" + ClientGitFloor + " " + ShellQuote(program),
		},
	}
}

// versionNumber is the awk function both readings of a box's git version
// compare releases by: a release's three numbers as one, so 2.40.10 is above
// 2.40.9.
const versionNumber = `function n(s,  a) { split(s, a, "."); return a[1] * 1000000 + a[2] * 1000 + a[3] }`

// CodingSteps is every step the engine runs to give a coding run's box its
// repositories on the engine's own code host, in order: [GitVersionStep],
// each repository's [CloneRecipe] for [CloneCoding] in the order given, and,
// in a cell whose boxes have a kernel of their own ([Placement.OwnsKernel]),
// the step that turns git's file-system monitor on in them where the box can
// keep it ([MonitorStep]). Refused for a repository the coding recipe
// refuses, naming it.
//
// THE CELL DECIDES THE MONITOR, never the box alone: a monitor's daemon takes
// a watch of the kernel's for every directory of its checkout, and on the
// engine host every box spends the engine user's ([Placement.OwnsKernel]),
// beside the other seats' coding CLIs and their own file watchers — a budget
// no box can see the rest of, so no box may decide to spend it.
func CodingSteps(p Placement, repos []RunRepo) ([]SetupStep, error) {
	steps := []SetupStep{GitVersionStep()}
	for _, r := range repos {
		step, err := CloneRecipe(r, CloneCoding)
		if err != nil {
			return nil, fmt.Errorf("sandbox: the clone of repository %s: %w", r.Repo, err)
		}
		steps = append(steps, step)
	}
	if p.OwnsKernel() && len(repos) > 0 {
		steps = append(steps, MonitorStep(repos))
	}
	return steps, nil
}

// monitorGitFloor is the oldest git whose file-system monitor [MonitorStep]
// turns on: 2.55.0, the oldest git the code host runs on its own nodes
// (internal/code/gitproc's NodeMinVersion), whose monitor watches a Linux
// checkout through inotify as the node pin's does — the git the engine's
// measurements read it on (2.56.0, internal/code/host's
// BenchmarkBoxWorktree). A box's git below it keeps git's default.
const monitorGitFloor = "2.55.0"

// The kernel's per-user inotify budgets a monitor spends, read where Linux
// says them: the host's, and the box's own user namespace's, which may only
// be lower — what a box's process may take is the least of every level's.
// A daemon takes one INSTANCE and one WATCH for every directory of its work
// tree.
const (
	inotifyWatches    = "/proc/sys/fs/inotify/max_user_watches"
	nsInotifyWatches  = "/proc/sys/user/max_inotify_watches"
	inotifyInstances  = "/proc/sys/fs/inotify/max_user_instances"
	nsInotifyInstance = "/proc/sys/user/max_inotify_instances"
)

// MonitorStep is the step that turns git's FILE-SYSTEM MONITOR on in the
// checkouts of repos, each in its own configuration, where the box's git is
// [monitorGitFloor] or newer and every checkout at the top of the box's
// working directory takes at most HALF of each budget the monitors spend
// from: their directories together half the watches the box's user may hold,
// and their count half its instances. Otherwise it turns the monitor off in
// each, stopping a daemon an earlier launch of the run started. Never a
// failure of the step: the monitor is speed, never a part of the checkout.
// Run AFTER the clones, every launch — a reused box's included — since the
// decision is the box's whole: [CodingSteps] runs it only in a cell whose
// boxes own their kernel.
//
// EVERY CHECKOUT AT THE TOP, NOT ONLY THIS CALL'S: a reused box keeps an
// earlier job's checkouts, and their daemons, beside the ones this launch
// names, so they are counted, watched or not.
//
// HALF, BECAUSE THE COUNT IS TAKEN ONCE AND THE TREE GROWS: the daemon
// watches every directory the work tree comes to hold — a build's output,
// `node_modules`, a `.venv`, ignored or not — and a daemon whose kernel
// refuses it a watch DIES, after which every git command starts it again,
// walks the tree adding watches until the kernel refuses one, and falls back
// to reading the tree: worse than no monitor at every command, and taking
// the user's whole remaining budget each time (internal/code/host's
// BenchmarkBoxWorktree, its fsmonitor=starved arm, measured in
// docs/concepts/code-sandbox.md's "What a clone costs"). The other half is
// what the tree may grow by, and what the box's other watchers — its coding
// CLI's — are left.
//
// ADOPTED ON ITS MEASUREMENT, by the rule a box-side setting is
// (docs/concepts/code-sandbox.md, "What a clone costs"): at 100 000 files a
// warm `git status` took 62 ms with it against 142 ms without (17 ms beside
// the engine's index settings, 99 without it), and a one-file commit 88 ms
// against 172, while the checkout is not touched — the monitor's daemon is
// started by the first command that asks it, whose status took 417 ms.
// THE CHECKOUT'S OWN CONFIGURATION, never the box's environment: a monitor
// is a daemon a repository runs, and the environment would start one for
// every repository the box's git touches, every clone its agent makes of
// another host's included.
//
// Every number it reads is held to digits before the shell's arithmetic
// sees it, since dash ends the whole step at an arithmetic error: a budget
// it cannot read is none, and the monitor stays off.
func MonitorStep(repos []RunRepo) SetupStep {
	dirs := make([]string, len(repos))
	for i, r := range repos {
		dirs[i] = ShellQuote(r.Repo)
	}
	program := versionNumber + `
/^git version / { seen = 1; v = $3; sub(/[^0-9.].*$/, "", v); ok = n(v) >= n(want); exit }
END { exit !(seen && ok) }`
	command := strings.Join([]string{
		"least() {",
		"  a=$(cat \"$1\" 2>/dev/null); case $a in ''|*[!0-9]*) a=0 ;; esac",
		"  b=$(cat \"$2\" 2>/dev/null); case $b in ''|*[!0-9]*) b=$a ;; esac",
		"  [ \"$b\" -lt \"$a\" ] && a=$b; echo \"$a\"",
		"}",
		"watches=$(least " + inotifyWatches + " " + nsInotifyWatches + ")",
		"instances=$(least " + inotifyInstances + " " + nsInotifyInstance + ")",
		"checkouts=0; held=0",
		"for g in */.git; do",
		"  [ -e \"$g\" ] || continue",
		"  n=$(find \"${g%/.git}\" -type d | wc -l | tr -d ' '); case $n in ''|*[!0-9]*) n=$watches ;; esac",
		"  checkouts=$((checkouts + 1)); held=$((held + n))",
		"done",
		"on=false",
		"if git version | awk -v want=" + monitorGitFloor + " " + ShellQuote(program) + " &&",
		"  [ \"$held\" -le $((watches / 2)) ] && [ \"$checkouts\" -le $((instances / 2)) ]; then on=true; fi",
		"for d in " + strings.Join(dirs, " ") + "; do",
		"  if $on; then git -C \"$d\" config core.fsmonitor true",
		"  else git -C \"$d\" config --unset core.fsmonitor; git -C \"$d\" fsmonitor--daemon stop; fi >/dev/null 2>&1",
		"done",
		"exit 0",
	}, "\n")
	return SetupStep{Name: "git monitor", Commands: []string{command}}
}

// CloneRecipe is the ONE way a box is given a repository: the setup step that
// checks r out for purpose, into `./<r.Repo>`, from
// "$CREWLET_GIT_URL/<r.Repo>.git" — or, borrowed, from the objects r names
// in the box. Every value it interpolates is quoted ([ShellQuote]); the base
// URL is the launch environment's, expanded by the box's shell. Refused for
// an r [RunRepo.ValidateFor] refuses for purpose, and so for a purpose this
// build does not know.
//
// THE WHOLE STEP IS SKIPPED WHERE `./<r.Repo>/.git` EXISTS, so a reused box
// keeps its checkout and the work in it, and a second launch in one turn is
// given only the repositories its box lacks.
//
// NEVER TREELESS (`--filter=tree:0`): a treeless clone carries every commit of
// the history — millions at the rate agents push — and fetches each tree a
// walk touches one request at a time. NEVER A BUNDLE URI: a client downloads a
// whole bundle even for a filtered clone. No `--reference --dissociate`: a
// coding box holds no cache to borrow from, and a remote one never can — and a
// copy borrowed and dissociated costs the box a repack of the whole history,
// 47.8 s against the recipe's 13.0 at a gigabyte (internal/code/host's
// BenchmarkBoxDissociate). A check's box on a git node, which holds such a
// cache, is given that cache's objects to READ ([CloneBorrowed]), never to
// copy.
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
	case CloneBorrowed:
		command = borrowedClone(r)
	}
	return SetupStep{
		Name:     cloneStepName(r, purpose),
		Commands: []string{command},
		Brief:    cloneBrief(r, purpose),
	}, nil
}

// cloneStepName is the name a step's failure is reported under: a coding
// run's clone, or a check's checkout of its one commit however its objects
// arrive.
func cloneStepName(r RunRepo, purpose ClonePurpose) string {
	if purpose == CloneCoding {
		return "clone " + r.Repo
	}
	return "checkout " + r.Repo
}

// cloneWords are the quoted words every recipe writes: the directory, the
// remote, the branch's name and ref, the base, the cone, and the objects a
// borrowed checkout reads.
type cloneWords struct {
	dir, url, branch, ref, base, cone, objects string
}

// fetchBase is the command that fetches w's base BY ITS ID, at depth when depth
// is set.
//
// ALWAYS, rather than only where the clone lacks it: a base off the default
// branch — the tip of the seat's own branch a run continues, or a commit of a
// branch deleted since, which protocol v2 serves by id for any object the host
// holds — is fetched with the clone's refs negotiated, so only what the
// default branch's history lacks travels; a base the clone holds costs the one
// capability request the transport opens with and nothing more, since git
// sends no fetch for wants it already holds. Asking first whether the clone holds it does
// not save that: in a partial clone a lookup of a missing object (`cat-file
// -e`) FETCHES it, with no negotiation at all — every commit and tree of the
// base's history sent again — and the gits that can be told not to lazily
// fetch (2.44, GIT_NO_LAZY_FETCH) are above the 2.40 floor.
//
// --no-tags, so the fetch lists no refs: an exact id is fetched as itself,
// and tag following would ask the host for its whole advertisement to name
// none.
func (w cloneWords) fetchBase(depth string) string {
	return "  git -C " + w.dir + " fetch --quiet --no-tags" + depth + " --filter=blob:none origin " + w.base + " &&"
}

// sparseCone is the command that narrows w's checkout to its cone, "" for a
// whole tree.
//
// --skip-checks, because a cone names DIRECTORIES and nothing else
// ([RunRepo.Paths]), which is what the checks it skips exist to ask: in cone
// mode git otherwise refuses a directory whose name starts with '!' or holds
// any of *?[] as a pattern a person mistyped, and a repository may hold such
// a directory. Skipped, git takes each argument as the directory it names and
// escapes those bytes itself when it writes the cone. The third check, a path
// that is a FILE in the index, never fires here: the clone checked nothing
// out, so the index is empty when the cone is set.
func (w cloneWords) sparseCone() string {
	if w.cone == "" {
		return ""
	}
	return "  git -C " + w.dir + " sparse-checkout set --cone --skip-checks -- " + w.cone + " &&"
}

// wordsOf quotes r's values for a recipe, each through [ShellQuote]. The
// remote is the one unquoted expansion, the launch environment's base URL,
// itself in double quotes so whatever it holds is one word.
func wordsOf(r RunRepo) cloneWords {
	cone := make([]string, len(r.Paths))
	for i, p := range r.Paths {
		cone[i] = ShellQuote(p)
	}
	return cloneWords{
		dir:     ShellQuote(r.Repo),
		url:     `"$` + GitURLVar + `"/` + ShellQuote(r.Repo+".git"),
		branch:  ShellQuote(r.Branch),
		ref:     ShellQuote("refs/heads/" + r.Branch),
		base:    ShellQuote(r.Base),
		cone:    strings.Join(cone, " "),
		objects: ShellQuote(r.Objects),
	}
}

// codingClone is [CloneCoding]'s command: a blobless clone of the default
// branch alone with no checkout, the cone, the base fetched by id
// ([cloneWords.fetchBase]), the branch made at the base and checked out —
// only the cone's files, fetched as the checkout needs them — and the
// commit-trailer hook written. An EMPTY repository (no base) has its branch
// named as the unborn HEAD instead. git's file-system monitor is no clone's:
// whether a box may keep one is its cell's and every checkout's together
// ([CodingSteps], [MonitorStep]).
//
// ONE BRANCH (--single-branch), BECAUSE A REPOSITORY'S BRANCHES ARE MOSTLY
// OTHER SEATS' WORK. Every full_access seat that reads a repository pushes
// its work there under refs/heads/seats/<its handle>/ (code.SeatNamespace),
// a branch for each work item it is given, and the code host advertises
// every ref it holds, up to code.MaxRefsPerRepo live ones. git's default
// clone wants every branch and tag it is shown, so a box would be sent the
// commits and trees of every seat's branch on top of its base's history,
// keep each as a remote-tracking ref, and carry a fetch refspec that pulls
// every other seat's next push into it on the agent's own `git fetch` or
// `git pull`: the bytes a box clone moves, the host's walk over the tips it
// is asked for and how long the clone holds a transfer slot of the runs'
// share would all grow with the company's live seat branches rather than
// with the run's base. --single-branch wants the one branch the host's HEAD
// names — the tags that point into its history follow, as a clone's do — and
// writes the refspec for that branch alone, so a later fetch moves the
// default branch and nothing else. What it does not narrow is the LISTING:
// git's clone still asks the host for every branch's and tag's name and id
// (protocol v2's ls-refs, by the prefixes refs/heads/ and refs/tags/), about
// eighty bytes a ref, once a clone — and no object of a branch it does not
// want: beside ten thousand seats' branches the clone receives the objects
// it receives beside none, and 0.8 MB more of listing (internal/code/host's
// BenchmarkBoxClone, its seats arms). A repository whose HEAD names a branch nobody has pushed yet —
// one that holds only seats' branches — gives the clone no branch at all,
// and the base, a seat's tip, arrives by its id like any base off the
// default branch.
//
// A CLONE, rather than `git init` and a fetch of the base by id, which would
// list no ref at all: the agent is given the default branch as a branch of its
// own and as origin/<default>, under a remote whose HEAD it knows, to diff,
// log and rebase against — what a person's clone gives them, and what an
// agent reaches for first — where a fetch by id leaves a repository holding
// one commit's history and no name for anything in it but the run's branch.
//
// The hook is written only where no entry of its name exists — a box's git
// whose template directory put a prepare-commit-msg there keeps its own —
// and never through core.hooksPath, which would take a project's own hooks
// away ([TrailerHook]).
func codingClone(r RunRepo) string {
	w := wordsOf(r)
	hook := w.dir + "/.git/hooks/prepare-commit-msg"
	lines := []string{
		"if [ ! -e " + w.dir + "/.git ]; then",
		"  git clone --quiet --single-branch --filter=blob:none --no-checkout -- " + w.url + " " + w.dir + " &&",
	}
	if cone := w.sparseCone(); cone != "" {
		lines = append(lines, cone)
	}
	if r.Base == "" {
		lines = append(lines, "  git -C "+w.dir+" symbolic-ref HEAD "+w.ref+" &&")
	} else {
		lines = append(lines,
			w.fetchBase(""),
			"  git -C "+w.dir+" checkout --quiet -B "+w.branch+" "+w.base+" &&")
	}
	lines = append(lines,
		"  { [ -e "+hook+" ] || [ -L "+hook+" ] ||",
		"    { mkdir -p "+w.dir+"/.git/hooks && printf '%s' "+ShellQuote(TrailerHook)+" > "+hook+" &&",
		"      chmod 755 "+hook+"; }; }",
		"fi")
	return strings.Join(lines, "\n")
}

// singleSHAClone is [CloneSingleSHA]'s command: a repository initialised with
// the code host as its remote, the one commit fetched by id at depth one
// (which a git of the 2.40 floor asks for under protocol v2, where `clone
// --revision` needs 2.49) with the blobless filter — which makes the remote
// the repository's PROMISOR, as a filtered clone's is, so the files the
// checkout needs are fetched as for a blobless clone — the cone, and the
// commit checked out detached.
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

// borrowedClone is [CloneBorrowed]'s command: a repository initialised with the
// code host as its remote — added, never fetched from — the borrowed objects
// named as its one ALTERNATE, the cone, and the commit checked out detached.
//
// NO FETCH, AND NO PROMISOR: the remote is a plain one, so a checkout missing
// an object FAILS, naming it, rather than fetching it lazily from the code
// host. The copy the box borrows holds every object of the repository —
// it is a git node's whole copy, held for the box's run — so a missing one is
// a fault of that copy, which a quiet fetch per object would hide while
// costing the code host the bytes the borrow exists to save.
//
// THE ALTERNATES FILE IS WRITTEN, never `GIT_ALTERNATE_OBJECT_DIRECTORIES`: the
// check's own command runs git in the checkout after this step, in an
// environment the engine does not compose, and must read the same objects;
// `git init` makes the `objects/info` it is written into. Read-only: git
// reads an alternate's packs, indexes and commit-graph in place and writes
// nothing into an alternate's directory, which a read-only mount would refuse
// anyway.
func borrowedClone(r RunRepo) string {
	w := wordsOf(r)
	lines := []string{
		"if [ ! -e " + w.dir + "/.git ]; then",
		"  git init --quiet " + w.dir + " && git -C " + w.dir + " remote add origin " + w.url + " &&",
		"  printf '%s\\n' " + w.objects + " > " + w.dir + "/.git/objects/info/alternates &&",
	}
	if cone := w.sparseCone(); cone != "" {
		lines = append(lines, cone)
	}
	lines = append(lines,
		"  git -C "+w.dir+" checkout --quiet --detach "+w.base,
		"fi")
	return strings.Join(lines, "\n")
}

// cloneBrief is what the coding agent is told the step made true: where the
// repository is, at what, how much of it, and how it pushes — "Your
// environment" is where a run learns it, so it never spends rounds
// rediscovering how its git reaches the code host.
func cloneBrief(r RunRepo, purpose ClonePurpose) string {
	var b strings.Builder
	switch {
	case purpose == CloneSingleSHA:
		fmt.Fprintf(&b, "Repository `%s` is checked out at `./%s` at commit %s, with no history "+
			"(file contents are fetched as git needs them)", r.Repo, r.Repo, shortCommit(r.Base))
	case purpose == CloneBorrowed:
		fmt.Fprintf(&b, "Repository `%s` is checked out at `./%s` at commit %s, its history and file "+
			"contents read from this node's copy of the repository at `%s`, which is read-only", r.Repo, r.Repo,
			shortCommit(r.Base), r.Objects)
	case r.Base == "":
		fmt.Fprintf(&b, "Repository `%s` is at `./%s` on branch `%s`, which has no commits yet: the "+
			"repository is empty", r.Repo, r.Repo, r.Branch)
	default:
		// THE DEFAULT BRANCH ALONE IS SAID, because it is all `git branch -r`
		// shows: an agent asked to work against another branch is told how
		// to reach it rather than left to conclude the host does not have it
		// — and where the fetch leaves it, since the clone's refspec names
		// no remote-tracking branch for it.
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
		// NO PUSH INSTRUCTION for a repository given to read: the code host
		// refuses its push, and telling the agent how to make one is telling
		// it to spend a round on that refusal.
		fmt.Fprintf(&b, " Your git is configured for this company's code host, and this run was given "+
			"`%s` to read: a push to it is refused.", r.Repo)
	default:
		fmt.Fprintf(&b, " Your git is configured for this company's code host: push with `git push origin %s`, "+
			"and every push is recorded under this run and reported back for you.", r.Branch)
	}
	return b.String()
}

// shortCommit is a commit id as a person reads one: its first twelve digits.
func shortCommit(oid string) string {
	return oid[:min(len(oid), 12)]
}
