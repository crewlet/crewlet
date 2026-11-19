package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/words"
)

// `crewlet gitlet import -local` pushes another host's repository from this
// machine into an existing repository Gitlet serves, for a source only this
// machine reaches; without -local the NODE imports (codeimportnode.go). The operator's git pushes
// as the token's person, so every push rule holds.
//
// A step is MEASURED, never estimated: the estimate only picks where it ends, and
// the measured copy is the pushed one, so no margin below max_push_bytes is
// needed (importSendConfig, importPlanner). Steps push `--no-thin` (the node
// would complete a thin pack with whole bases, storing more than measured),
// fast-forward, and are `--atomic` so a re-run resumes; a ref held where the
// source's history does not reach is refused. The total stored is held
// to repo_max_bytes (importPlanner.hold).

// codeImportUsage is the import's two synopses: the node's, and -local's.
const codeImportUsage = "usage: crewlet gitlet import <source-url> <name> [<config.yaml>] [-api URL] " +
	"-owner unit:KEY|seat:HANDLE [-credential VAR] [-mirror] [-op-id ID]\n" +
	"       crewlet gitlet import <source-url> <name> -local [<config.yaml>] [-api URL] [-git PATH] [-dry-run]"

// importMeasureCap is how far a build goes: twice the limit, past which the step
// is cut to under half its length anyway (a range of small deltas of large files
// can pack to many times its estimate).
func importMeasureCap(limit int64) int64 { return 2 * limit }

// importOrderSlack is the most a step's copy ([importer.sent]) can differ from
// its build: four bytes per object, since reordering moves only each OFS_DELTA
// base offset's varint (1 to 5 bytes under 32 GiB). A build further past the
// limit is cut on its own size, never copied.
func importOrderSlack(objects int64) int64 { return 4 * objects }

// importCutShare is how far below its proportional point a step past the limit
// is cut, so a range that packs a little worse than the built one still fits at
// the first rebuild.
const importCutShare = 15.0 / 16.0

// importRatioFloor is the smallest step whose measurement feeds the next plan:
// an eighth of the limit. A smaller pack is mostly header, trailer and entry
// headers, which says nothing about how the history packs.
func importRatioFloor(budget int64) int64 { return budget / 8 }

// importNamed is how many items one list names; the rest are counted, or a
// forge's hundred thousand refs would bury the remedy.
const importNamed = 20

// maxImportLine bounds one line of git's output as the import reads it: 1 MiB is
// far past any object id, path or ref, so a longer line is not git's answer.
const maxImportLine = 1 << 20

// oidHexLen is an object id's length in hex. The source is held to SHA-1 before
// anything is read by length ([importer.inspect]).
const oidHexLen = 40

// gitletImport is `crewlet gitlet import`.
func gitletImport(environ config.Source, args []string, stdout, stderr io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	call, err := readImport(environ, args, stderr)
	if err != nil {
		return err
	}
	if !call.local {
		return gitletImportOnNode(ctx, call, stdout, stderr)
	}
	imp, err := call.importer(ctx, stdout, stderr)
	if err != nil {
		return err
	}
	return imp.run(ctx, call.dryRun)
}

// importCall is one `crewlet gitlet import` command line.
type importCall struct {
	source string // the source's URL, as the operator gave it
	name   string // the repository on the node
	client *nodeClient
	// local is -local; unset, the node creates the repository and fetches the
	// source itself.
	local bool
	// gitPath and dryRun are -local's.
	gitPath string
	dryRun  bool
	// owner, credential, mirror and opID are the node's import's.
	owner, credential, opID string
	mirror                  bool
}

// readImport reads an import's command line, keeping the two imports' flags apart.
func readImport(environ config.Source, args []string, stderr io.Writer) (importCall, error) {
	// THE TWO SUBJECTS LEAD, before any flag: the node client's optional config
	// document is a third positional, and only order tells the three apart.
	source, rest := splitSubject(args)
	name, rest := splitSubject(rest)
	if source == "" || name == "" {
		fmt.Fprintln(stderr, codeImportUsage)
		return importCall{}, errors.New("name the repository to import and Gitlet's repository to " +
			"import it into, before any flag")
	}
	var (
		local, dryRun, mirror            *bool
		gitPath, owner, credential, opID *string
	)
	client, err := nodeClientFor(environ, rest, "gitlet import", stderr, func(fs *flag.FlagSet) {
		local = fs.Bool("local", false, "import on THIS machine with its own git and network: push the "+
			"source into a repository that exists, in steps under code.limits.max_push_bytes; the same "+
			"command again resumes it")
		gitPath = fs.String("git", "", "-local: the git binary to run; empty is the first on PATH")
		dryRun = fs.Bool("dry-run", false, "-local: plan the import and push nothing")
		owner = fs.String("owner", "", "the new project's owner: unit:<key> or seat:<handle>")
		credential = fs.String("credential", "", "the NAME of the variable that holds the source's token, "+
			"in the node's secret store or environment (crewlet secrets set NAME), which "+
			"code.remote_credentials declares for the source's host; empty fetches anonymously")
		mirror = fs.Bool("mirror", false, "keep the repository in step with the source as a pull mirror, "+
			"rather than import it once")
		opID = opIDFlag(fs, "retry an `unknown` outcome with the id it printed, so the retry is "+
			"the same create; empty starts a new one")
	})
	if err != nil {
		return importCall{}, err
	}
	call := importCall{source: source, name: name, client: client, local: *local, gitPath: *gitPath,
		dryRun: *dryRun, owner: *owner, credential: *credential, opID: *opID, mirror: *mirror}
	// Each import reads only its own flags, so the other's would be silently
	// dropped: a -dry-run that runs, a -mirror that imports once.
	given := map[string]bool{"-owner": call.owner != "", "-credential": call.credential != "",
		"-mirror": call.mirror, "-op-id": call.opID != ""}
	if !call.local {
		given = map[string]bool{"-git": call.gitPath != "", "-dry-run": call.dryRun}
	}
	// Unquoted: a flag is copied onto a command, not read as a value somebody typed.
	var misplaced []words.Said
	for flag, set := range given {
		if set {
			misplaced = append(misplaced, words.Said(flag))
		}
	}
	if len(misplaced) == 0 {
		return call, nil
	}
	slices.Sort(misplaced)
	fmt.Fprintln(stderr, codeImportUsage)
	if call.local {
		return importCall{}, fmt.Errorf("%s: the node's import alone, which creates the repository and "+
			"fetches the source, where -local pushes from this machine into one that exists: drop them, or "+
			"-local", words.And(misplaced))
	}
	return importCall{}, fmt.Errorf("%s: -local's alone, which pushes from this machine with its own git "+
		"into a repository that exists: add -local, or drop them for the node to fetch the source",
		words.And(misplaced))
}

// importer is -local's run of call: the target repository and the operator's git,
// read before anything is pushed.
func (call importCall) importer(ctx context.Context, stdout, stderr io.Writer) (*importer, error) {
	repo, err := gitletImportTarget(ctx, call.client, call.name)
	if err != nil {
		return nil, err
	}
	principal, err := gitletImportPrincipal(ctx, call.client)
	if err != nil {
		return nil, err
	}
	g, err := newOperatorGit(call.gitPath, call.client, os.Environ())
	if err != nil {
		return nil, err
	}
	if err = g.atClientFloor(ctx); err != nil {
		return nil, err
	}
	imp := &importer{g: g, client: call.client, source: call.source, name: call.name, repo: repo,
		principal: principal, budget: repo.Limits.MaxPushBytes, maxStored: gitlet.MaxPackBytes,
		stdout: stdout, stderr: stderr}
	if isTerminal(stderr) {
		imp.live = stderr
	}
	return imp, nil
}

// gitletImportTarget is the repository an import pushes into, read at the log's
// head (never "not declared" from an old copy), with the limits the node holds
// its pushes to and what its packs hold now.
func gitletImportTarget(ctx context.Context, client *nodeClient, name string) (codeRepoView, error) {
	var answer codeReadView
	if err := client.forCodeReads().get(ctx, "/gitlet/repos/"+url.PathEscape(name)+"?read_level=linearizable",
		&answer); err != nil {
		// The remedy only where it is one: a company that hosts no code
		// (not_hosted) gains nothing by declaring a repository.
		var refusal *nodeRefusal
		if errors.As(err, &refusal) && refusal.Code == "not_found" && refusal.Reason == "not_declared" {
			return codeRepoView{}, fmt.Errorf("%w\n\nAdd %s to code.repos, wait for the activation to apply, then "+
				"import", err, name)
		}
		return codeRepoView{}, codeReadRefused(client, err)
	}
	if answer.Repo == nil {
		return codeRepoView{}, fmt.Errorf("the node's answer for %s carries no repository", name)
	}
	r := *answer.Repo
	switch r.Status {
	case "active":
	case "archived":
		return codeRepoView{}, fmt.Errorf("%s is archived and takes no push: declare it without archived: true in "+
			"code.repos to import into it", name)
	case "deleted":
		return codeRepoView{}, fmt.Errorf("%s is deleted: declare it again in code.repos to restore it, then import",
			name)
	default:
		// purging — which nothing undoes — or a status a newer node added.
		return codeRepoView{}, fmt.Errorf("%s is %s, and takes no push", name, r.Status)
	}
	if r.Limits == nil || r.Limits.MaxPushBytes <= 0 || r.Limits.MaxBlobBytes <= 0 || r.Limits.RepoMaxBytes <= 0 {
		return codeRepoView{}, fmt.Errorf("the node's answer for %s says no code.limits, which an import cuts its "+
			"pushes to", name)
	}
	return r, nil
}

// gitletImportPrincipal is who the node records the import's pushes as, the
// token's id and its bound seat, asked of the node (GET /viewer). Every push's
// record carries it, so it counts against the record ([importer.recordHolds]);
// neither id nor handle has a length bound, so only asking says how much room it takes.
func gitletImportPrincipal(ctx context.Context, client *nodeClient) (gitlet.Principal, error) {
	var viewer struct {
		OperatorID string `json:"operator_id"`
		Handle     string `json:"handle"`
	}
	if err := client.get(ctx, "/viewer", &viewer); err != nil {
		return gitlet.Principal{}, fmt.Errorf("ask the node who this token is: %w", err)
	}
	return gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: viewer.OperatorID, Seat: viewer.Handle}, nil
}

// importer is one run of `crewlet gitlet import`.
type importer struct {
	g      operatorGit
	client *nodeClient
	source string // the source's URL, as the operator gave it
	name   string // the repository on the node
	repo   codeRepoView
	// principal is who the node records each step's push as.
	principal gitlet.Principal
	// budget is the most one step's pack carries: code.limits.max_push_bytes,
	// past which receive.maxInputSize refuses a push.
	budget int64
	// maxStored is the most one push may STORE (pack, .idx and .rev together):
	// gitlet.MaxPackBytes, past which the node refuses pack_too_large.
	maxStored int64
	// work is the import's working directory, clone the source's bare clone in
	// it; each step's repository ([importer.stepRepo]) is made there too.
	work, clone string
	// live is stderr where a person is watching, for git's progress; else nil.
	live           io.Writer
	stdout, stderr io.Writer
}

// sourceRef is one ref of the source's clone: name, value, and the value's kind.
type sourceRef struct {
	ref, oid, kind string
}

// isBranch reports a refs/heads/ ref whose value is a commit: one that can be walked.
func (r sourceRef) isBranch() bool {
	return strings.HasPrefix(r.ref, "refs/heads/") && r.kind == "commit"
}

// run is the import, from the source's advertisement to the proof.
func (imp *importer) run(ctx context.Context, dryRun bool) error {
	// 1 REFUSALS BEFORE ANYTHING IS DOWNLOADED: the advertisement, and whether
	// this token may push at all.
	if err := imp.listSource(ctx); err != nil {
		return err
	}
	// $TMPDIR needs room for the source and two steps' packs at once.
	tmp, err := os.MkdirTemp("", "crewlet-import-")
	if err != nil {
		return fmt.Errorf("make the import's working directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	imp.work = tmp
	if err = imp.checkPushAccess(ctx, filepath.Join(tmp, "probe.git")); err != nil {
		return err
	}

	// 2 THE CLONE, in the operator's own environment: their credential helpers
	// reach the source, else the person at the terminal answers git's prompt.
	imp.clone = filepath.Join(tmp, "source.git")
	fmt.Fprintf(imp.stdout, "cloning %s into %s\n", imp.source, tmp)
	clone := []string{"clone", "--bare", "--no-local", "--quiet"}
	if imp.live != nil {
		clone = []string{"clone", "--bare", "--no-local", "--progress"}
	}
	if _, err = imp.g.call(ctx, gitCall{env: imp.g.environ, live: imp.live, asks: true},
		append(clone, "--", imp.source, imp.clone)...); err != nil {
		return err
	}

	// 3 EVERY OTHER REFUSAL, listed together, pushing nothing.
	src, err := imp.inspect(ctx)
	if err != nil {
		return err
	}

	// 4 WHAT THE REPOSITORY ALREADY HOLDS.
	queue, remote, err := imp.resume(ctx, src)
	if err != nil {
		return err
	}
	if len(queue) == 0 {
		fmt.Fprintf(imp.stdout, "nothing to push: %s holds every ref of the source\n", imp.name)
		return imp.prove(ctx, src, importDone{})
	}

	// 5 THE STEPS.
	p := &importPlanner{imp: imp, remote: remote, budget: imp.budget, exact: !dryRun, ratio: 1, queue: queue,
		held: imp.repo.LiveBytes}
	if dryRun {
		return imp.plan(ctx, p)
	}
	fmt.Fprintf(imp.stdout, "importing %s in steps of at most %s (code.limits.max_push_bytes), "+
		"each measured before it is pushed\n", counted(len(queue), "ref", "refs"), humanBytes(imp.budget))
	done, err := imp.execute(ctx, p)
	if err != nil {
		return err
	}
	done.recut = p.recut
	return imp.prove(ctx, src, done)
}

// ---- 1: the source's advertisement -----------------------------------------

// importKindOf classifies a source ref by the domain's own rule
// (gitlet.StatelessRefusal), so the import keeps exactly what a push may carry.
// SKIPPED: outside refs/heads/ and refs/tags/ (a forge's pull refs). BAD: inside
// them but no ref. RESERVED: a seat branch (gitlet.SeatNamespaceRoot), named for
// what it is since check-ref-format has nothing against it.
func importKindOf(ref string) importKind {
	refusal := gitlet.StatelessRefusal(gitlet.Command{Ref: ref, Old: gitlet.ZeroOID, New: strings.Repeat("1", oidHexLen)})
	switch {
	case refusal == "":
		return importKept
	case !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/"):
		return importSkipped
	case refusal == gitlet.RefusalReservedRef:
		return importReserved
	default:
		return importBad
	}
}

type importKind int

const (
	importKept importKind = iota
	importSkipped
	importBad
	importReserved
)

// importNotHeld is the source's refs Gitlet takes no push of (bad, reserved),
// each counted, the first few named.
type importNotHeld struct {
	bad, reserved           []string
	badCount, reservedCount int
}

// add counts ref under its kind; other kinds are ignored.
func (n *importNotHeld) add(kind importKind, ref string) {
	switch kind {
	case importBad:
		n.badCount++
		if len(n.bad) < importNamed {
			n.bad = append(n.bad, fmt.Sprintf("%q", ref))
		}
	case importReserved:
		n.reservedCount++
		if len(n.reserved) < importNamed {
			n.reserved = append(n.reserved, fmt.Sprintf("%q", ref))
		}
	}
}

func (n *importNotHeld) findings() []string {
	var out []string
	if n.badCount > 0 {
		out = append(out, fmt.Sprintf("%s Gitlet takes no push of (git's check-ref-format "+
			"rules, at most %d bytes of UTF-8): %s. Rename them in the source", counted(n.badCount, "ref", "refs"),
			gitlet.MaxRefNameBytes, listed(n.bad, n.badCount)))
	}
	if n.reservedCount > 0 {
		out = append(out, fmt.Sprintf("%s Gitlet keeps for its seats' own branches, which live under "+
			"%s<handle>/ (%s, and %s<name> as a branch itself, would block them, since no ref is created "+
			"beside a directory of its name): %s. Rename them in the source",
			counted(n.reservedCount, "branch", "branches"), gitlet.SeatNamespaceRoot,
			strings.TrimSuffix(gitlet.SeatNamespaceRoot, "/"), gitlet.SeatNamespaceRoot,
			listed(n.reserved, n.reservedCount)))
	}
	return out
}

// namespaceOf names a skipped ref's namespace: refs/pull/1/head is refs/pull/*.
func namespaceOf(ref string) string {
	parts := strings.SplitN(ref, "/", 3)
	if len(parts) < 3 {
		return ref
	}
	return parts[0] + "/" + parts[1] + "/*"
}

// listSource reads the source's advertisement BEFORE anything is downloaded, so a
// source refused for its refs fails in seconds and skipped refs are still named.
func (imp *importer) listSource(ctx context.Context) error {
	var (
		kept, skipped int
		spaces        []string
		notHeld       importNotHeld
	)
	seen := map[string]bool{}
	w := &lineWriter{max: maxImportLine, fn: func(line []byte) error {
		_, ref, ok := bytes.Cut(line, []byte{'\t'})
		if !ok {
			return fmt.Errorf("git ls-remote of the source printed a line that is no ref: %q", line)
		}
		name := string(ref)
		if name == "HEAD" || strings.HasSuffix(name, "^{}") {
			return nil
		}
		switch kind := importKindOf(name); kind {
		case importKept:
			kept++
		case importSkipped:
			skipped++
			if ns := namespaceOf(name); !seen[ns] {
				seen[ns] = true
				spaces = append(spaces, ns)
			}
		default:
			notHeld.add(kind, name)
		}
		return nil
	}}
	if _, err := imp.g.call(ctx, gitCall{env: imp.g.environ, stdout: w, asks: true}, "ls-remote",
		imp.source); err != nil {
		return err
	}
	if err := w.flush(); err != nil {
		return err
	}
	findings := notHeld.findings()
	if kept > gitlet.MaxRefsPerRepo {
		findings = append(findings, fmt.Sprintf("the source holds %d branches and tags, past the %d one repository "+
			"holds", kept, gitlet.MaxRefsPerRepo))
	}
	if len(findings) > 0 {
		return refuseImport(findings)
	}
	if kept == 0 {
		return fmt.Errorf("%s serves no branch and no tag, so there is nothing to import", imp.source)
	}
	if skipped > 0 {
		fmt.Fprintf(imp.stdout, "skipping %s outside refs/heads/ and refs/tags/ (%s): a forge's, not the "+
			"repository's\n", counted(skipped, "ref", "refs"), listed(spaces, len(spaces)))
	}
	return nil
}

// importProbeRef is the ref the push-access probe asks git to delete in a dry
// run, which sends nothing, so whatever the repository holds there stays.
const importProbeRef = "refs/heads/crewlet-import-probe"

// importProbeWait is how long the probe waits for the node's answer: the node
// client's own call bound, in which a node that refuses a push refuses it (it
// refuses before reading anything).
const importProbeWait = nodeRequestTimeout

// checkPushAccess asks the node, before the source is cloned, whether this token
// may push (not if bound to nobody, or api.auth.disabled), so a refusal comes in
// seconds in the node's words.
//
// ASKED THROUGH GIT with the steps' credential: a `git push --dry-run` of a
// deletion from an empty repository makes the push's first request and sends
// nothing; auth.GitAccess says which requests are pushes. A node with no copy
// yet may sync past importProbeWait, which is no refusal (the first step asks
// again). Any other failure is QUOTED AS GIT SAID IT, never read as the token's.
func (imp *importer) checkPushAccess(ctx context.Context, probe string) error {
	if _, err := imp.g.run(ctx, imp.g.environ, "", "init", "--bare", "--quiet", probe); err != nil {
		return err
	}
	asking, cancel := context.WithTimeout(ctx, importProbeWait)
	defer cancel()
	out, err := imp.g.call(asking, gitCall{env: imp.g.targetEnv(), gitDir: probe, config: importPushConfig},
		append(slices.Clone(importPushFlags), "--dry-run", imp.g.repoURL(imp.name), ":"+importProbeRef)...)
	if _, answered := porcelainLines(out)[importProbeRef]; answered {
		// git planned the push against the node's advertisement: it takes
		// this token's pushes here.
		return nil
	}
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case asking.Err() != nil:
		return nil
	case err == nil:
		return fmt.Errorf("git pushed to %s without saying what became of the push", imp.name)
	}
	return fmt.Errorf("nothing was imported: before downloading the source, the import asks %s whether it takes "+
		"a push to %s (a dry run of one, as every step is pushed), and git answered:\n%w", imp.client.base,
		imp.name, err)
}

// importPushConfig and importPushFlags PIN every push (each step and the probe)
// to the measured push, whatever the operator's config says. push.pushOption is
// cleared (Gitlet reads a change from push options; a step is never a proposal);
// push.negotiate=false (a fetch first could shrink the pack below the
// measurement); --no-verify (pre-push hooks such as git-lfs's would fail every
// step); --no-thin (the stored pack is the sent one); --porcelain, which
// [importer.push] reads. pack.* is not pinned: it shapes only the one build
// ([importSendConfig]). A step's repository holds no ref, so no tag is added.
var (
	importPushConfig = []string{"push.pushOption=", "push.negotiate=false"}
	importPushFlags  = []string{"push", "--porcelain", "--no-verify", "--no-thin", "--no-signed",
		"--no-recurse-submodules"}
)

// importSendConfig is what every COPY of a step's pack (measured and pushed) runs
// under. A pack built twice differs (delta-search threads), so a step is built
// ONCE and both copies must match byte for byte. pack.window=0 means no delta
// search, so every object is taken as built; core.multiPackIndex,
// pack.useBitmaps and pack.allowPackReuse false take each object from the step's
// own pack, not the borrowed clone's (which may hold a delta against an unsent
// object). On the command line, so they outrank the operator's configuration.
var importSendConfig = []string{"pack.window=0", "core.multiPackIndex=false", "pack.useBitmaps=false",
	"pack.allowPackReuse=false"}

// ---- 3: the source's clone -------------------------------------------------

// importSource is the source's clone as the import plans from it.
type importSource struct {
	// refs are the kept refs in for-each-ref (refname) order, the order every
	// batch takes them in.
	refs []sourceRef
	// head is the source's HEAD branch, or "" where HEAD names none.
	head string
}

// inspect holds the clone to every rule a refusal before the first push needs
// (whole history, SHA-1, pushable refs, ref count, blob and repository size) and
// refuses with every finding at once, so the source is fixed once, not once a run.
func (imp *importer) inspect(ctx context.Context) (importSource, error) {
	var findings []string
	out, err := imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone}, "rev-parse",
		"--is-shallow-repository", "--show-object-format")
	if err != nil {
		return importSource{}, err
	}
	answers := strings.Fields(string(out))
	if len(answers) != 2 {
		return importSource{}, fmt.Errorf("git rev-parse answered %q about the clone", out)
	}
	if answers[0] == "true" {
		// Its history stops at commits whose parents it lacks, which
		// receive-pack refuses (receive.shallowUpdate).
		findings = append(findings, "the source is a shallow clone, whose history stops part way: import from a "+
			"full clone of it")
	}
	if answers[1] != "sha1" {
		// Nothing past here may read an id by its length.
		return importSource{}, refuseImport([]string{fmt.Sprintf("the source's objects are %s, and Gitlet "+
			"keeps SHA-1 repositories: a repository cannot change its object format by being pushed", answers[1])})
	}

	var src importSource
	var notHeld importNotHeld
	refs := &lineWriter{max: maxImportLine, fn: func(line []byte) error {
		oid, rest, ok1 := bytes.Cut(line, []byte{' '})
		kind, ref, ok2 := bytes.Cut(rest, []byte{' '})
		if !ok1 || !ok2 || len(oid) != oidHexLen {
			return fmt.Errorf("git for-each-ref printed a line that is no ref: %q", line)
		}
		switch which := importKindOf(string(ref)); which {
		case importKept:
			src.refs = append(src.refs, sourceRef{ref: string(ref), oid: string(oid), kind: string(kind)})
		case importSkipped:
			// The advertisement already said what is skipped.
		default:
			notHeld.add(which, string(ref))
		}
		return nil
	}}
	if _, err = imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone, stdout: refs}, "for-each-ref",
		"--format=%(objectname) %(objecttype) %(refname)"); err != nil {
		return importSource{}, err
	}
	if err = refs.flush(); err != nil {
		return importSource{}, err
	}
	findings = append(findings, notHeld.findings()...)
	if n := len(src.refs); n > gitlet.MaxRefsPerRepo {
		findings = append(findings, fmt.Sprintf("the source holds %d branches and tags, past the %d one repository "+
			"holds", n, gitlet.MaxRefsPerRepo))
	}
	if len(src.refs) == 0 {
		return importSource{}, fmt.Errorf("%s holds no branch and no tag once cloned, so there is nothing to "+
			"import", imp.source)
	}

	blobs, err := imp.oversizedBlobs(ctx)
	if err != nil {
		return importSource{}, err
	}
	findings = append(findings, blobs...)

	out, err = imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone}, "count-objects", "-v")
	if err != nil {
		return importSource{}, err
	}
	packed, err := sizePack(out)
	if err != nil {
		return importSource{}, err
	}
	// One pack of the whole history with every delta it offers (a step may only
	// delta against what it carries), so the steps store at least about this
	// much; a source already past the limit is refused before any push. What the
	// repository holds is not added: on a resume it is these same objects. The
	// steps' own bound is [importPlanner.hold].
	if limit := imp.repo.Limits.RepoMaxBytes; packed > limit {
		findings = append(findings, fmt.Sprintf("the source's history packs to %s as one pack, past "+
			"code.limits.repo_max_bytes (%s) before the steps of an import store more than one pack does: raise "+
			"it, or import a smaller history", humanBytes(packed), humanBytes(limit)))
	}
	if len(findings) > 0 {
		return importSource{}, refuseImport(findings)
	}

	head, err := imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone}, "symbolic-ref", "--quiet", "HEAD")
	var exit *exec.ExitError
	switch {
	case err == nil:
		src.head = strings.TrimSpace(string(head))
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		// A detached HEAD names no branch.
	default:
		return importSource{}, err
	}
	branches := 0
	for _, r := range src.refs {
		if strings.HasPrefix(r.ref, "refs/heads/") {
			branches++
		}
	}
	fmt.Fprintf(imp.stdout, "cloned %s: %s (%s, %s), %s packed\n", imp.source,
		counted(len(src.refs), "ref", "refs"), counted(branches, "branch", "branches"),
		counted(len(src.refs)-branches, "tag", "tags"), humanBytes(packed))
	return src, nil
}

// oversizedBlobs is a finding for every blob past code.limits.max_blob_bytes,
// named by id, first path and size: one `cat-file --batch-all-objects` streamed
// (a large history is millions of lines) and, only on a hit, one `rev-list
// --objects --all` streamed until every named blob has a path.
func (imp *importer) oversizedBlobs(ctx context.Context) ([]string, error) {
	limit := imp.repo.Limits.MaxBlobBytes
	type blob struct {
		oid  string
		size int64
		path string
	}
	var (
		found []blob
		over  int
	)
	all := &lineWriter{max: maxImportLine, fn: func(line []byte) error {
		fields := bytes.Fields(line)
		if len(fields) != 3 {
			return fmt.Errorf("git cat-file printed a line that is no object: %q", line)
		}
		if string(fields[1]) != "blob" {
			return nil
		}
		size, err := strconv.ParseInt(string(fields[2]), 10, 64)
		if err != nil {
			return fmt.Errorf("git cat-file printed a size that is no number: %q", line)
		}
		if size > limit {
			over++
			if len(found) < importNamed {
				found = append(found, blob{oid: string(fields[0]), size: size})
			}
		}
		return nil
	}}
	// --unordered reads the packs in their own order, the fast one.
	if _, err := imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone, stdout: all}, "cat-file",
		"--batch-all-objects", "--unordered", "--batch-check=%(objectname) %(objecttype) %(objectsize)"); err != nil {
		return nil, err
	}
	if err := all.flush(); err != nil {
		return nil, err
	}
	if over == 0 {
		return nil, nil
	}
	unnamed := len(found)
	index := make(map[string]int, len(found))
	for i, b := range found {
		index[b.oid] = i
	}
	paths := &lineWriter{max: maxImportLine, fn: func(line []byte) error {
		oid, path, ok := bytes.Cut(line, []byte{' '})
		if !ok {
			return nil
		}
		if i, named := index[string(oid)]; named && found[i].path == "" {
			found[i].path = string(path)
			if unnamed--; unnamed == 0 {
				return errStopReading
			}
		}
		return nil
	}}
	_, err := imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone, stdout: paths}, "rev-list",
		"--objects", "--all")
	if err == nil {
		err = paths.flush()
	}
	if err != nil && !errors.Is(err, errStopReading) {
		return nil, err
	}
	findings := make([]string, 0, len(found)+1)
	for _, b := range found {
		path := b.path
		if path == "" {
			path = "no path"
		}
		findings = append(findings, fmt.Sprintf("blob %s (%s) is %s, past code.limits.max_blob_bytes (%s): large "+
			"binaries belong in a Worklet project's files", b.oid, path, humanBytes(b.size), humanBytes(limit)))
	}
	if more := over - len(found); more > 0 {
		findings = append(findings, fmt.Sprintf("and %s past code.limits.max_blob_bytes",
			counted(more, "more blob", "more blobs")))
	}
	return findings, nil
}

// sizePack is `git count-objects -v`'s size-pack in bytes (git reports KiB).
func sizePack(out []byte) (int64, error) {
	for line := range strings.Lines(string(out)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "size-pack: "); ok {
			kib, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("git count-objects printed a size-pack that is no number: %q", v)
			}
			return kib << 10, nil
		}
	}
	return 0, fmt.Errorf("git count-objects printed no size-pack:\n%s", out)
}

// refuseImport is the one refusal of an import that pushed nothing.
func refuseImport(findings []string) error {
	return fmt.Errorf("nothing was imported:\n  %s", strings.Join(findings, "\n  "))
}

// counted is n with the noun one names it by, or many where n is not one.
func counted(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// listed is the first of n names, the rest counted.
func listed(names []string, n int) string {
	s := strings.Join(names, ", ")
	if more := n - len(names); more > 0 {
		s += fmt.Sprintf(" and %d more", more)
	}
	return s
}

// ---- 4: what the repository already holds ----------------------------------

// remoteRefs is the repository's refs once every step planned so far has landed:
// what git push's pack-objects is told the node holds (send-pack's negatives:
// every remote ref whose object the clone holds), so every estimate and
// measurement is made against it and a measurement is the size of the push.
type remoteRefs struct {
	refs map[string]string // ref → its value
	held map[string]bool   // a value the clone holds
	negs []byte            // the negatives as rev-list --stdin reads them; nil once stale
}

// set records ref's value once a planned step lands.
func (r *remoteRefs) set(ref, oid string) {
	r.refs[ref] = oid
	r.held[oid] = true
	r.negs = nil
}

// negatives is `^<oid>` for every distinct held value, sorted so two runs over
// one state ask git one question, built once a state.
func (r *remoteRefs) negatives() []byte {
	if r.negs == nil {
		oids := make([]string, 0, len(r.refs))
		seen := make(map[string]bool, len(r.refs))
		for _, oid := range r.refs {
			if r.held[oid] && !seen[oid] {
				seen[oid] = true
				oids = append(oids, oid)
			}
		}
		slices.Sort(oids)
		b := make([]byte, 0, len(oids)*(oidHexLen+2))
		for _, oid := range oids {
			b = append(b, '^')
			b = append(b, oid...)
			b = append(b, '\n')
		}
		r.negs = b
	}
	return r.negs
}

// revs is news, then every negative, as rev-list --stdin and pack-objects --revs read.
func (r *remoteRefs) revs(news []string) []byte {
	neg := r.negatives()
	b := make([]byte, 0, len(news)*(oidHexLen+1)+len(neg))
	for _, oid := range news {
		b = append(b, oid...)
		b = append(b, '\n')
	}
	return append(b, neg...)
}

// queuedRef is a ref still to push. walk marks a branch advanced along its
// first-parent history in steps of its own rather than batched.
type queuedRef struct {
	sourceRef
	walk bool
}

// resume answers what is left to push (default branch first, then the source's
// order) and the refs the plan starts from. A ref held at the source's value is
// done; a BRANCH held at an ancestor is a landed step, so it continues. A tag
// never moves and a value outside the source's history is somebody else's push:
// either is refused, naming the first.
func (imp *importer) resume(ctx context.Context, src importSource) ([]queuedRef, *remoteRefs, error) {
	theirs, err := imp.targetRefs(ctx)
	if err != nil {
		return nil, nil, err
	}
	distinct := map[string]bool{}
	for _, oid := range theirs {
		distinct[oid] = true
	}
	held, err := imp.held(ctx, distinct)
	if err != nil {
		return nil, nil, err
	}
	remote := &remoteRefs{refs: theirs, held: held}

	defaultRef := "refs/heads/" + imp.repo.DefaultBranch
	byRef := make(map[string]bool, len(src.refs))
	for _, r := range src.refs {
		byRef[r.ref] = true
	}
	if !byRef[defaultRef] {
		switch {
		case src.head != "" && byRef[src.head]:
			branch := strings.TrimPrefix(src.head, "refs/heads/")
			fmt.Fprintf(imp.stdout, "the source's default branch is %s; code.repos says %s: importing %s as %s; "+
				"push a %s for the repository's HEAD\n", branch, imp.repo.DefaultBranch, branch, branch,
				imp.repo.DefaultBranch)
			defaultRef = src.head
		default:
			defaultRef = ""
		}
	}

	descends, err := imp.descendants(ctx, src.refs, theirs, held)
	if err != nil {
		return nil, nil, err
	}
	var (
		queue     []queuedRef
		already   int
		continued []string
	)
	for i, r := range src.refs {
		at, holds := theirs[r.ref]
		switch {
		case !holds:
		case at == r.oid:
			already++
			continue
		case !r.isBranch() || !held[at] || !descends[i]:
			return nil, nil, diverged(imp.name, r, at, held[at])
		default:
			continued = append(continued, r.ref)
		}
		q := queuedRef{sourceRef: r, walk: r.ref == defaultRef && r.isBranch()}
		if q.walk {
			queue = slices.Insert(queue, 0, q)
		} else {
			queue = append(queue, q)
		}
	}
	var theirsOnly []string
	for ref := range theirs {
		if !byRef[ref] {
			theirsOnly = append(theirsOnly, ref)
		}
	}
	slices.Sort(theirsOnly)
	if n := len(src.refs) + len(theirsOnly); n > gitlet.MaxRefsPerRepo {
		return nil, nil, refuseImport([]string{fmt.Sprintf("%s would hold %d refs (the source's %d and %d of its "+
			"own), past the %d one repository holds", imp.name, n, len(src.refs), len(theirsOnly),
			gitlet.MaxRefsPerRepo)})
	}
	if already > 0 {
		fmt.Fprintf(imp.stdout, "already on %s: %d of the source's refs, at the source's values\n", imp.name,
			already)
	}
	if len(continued) > 0 {
		fmt.Fprintf(imp.stdout, "continuing %s from the earlier commit %s holds it at\n",
			listed(continued[:min(len(continued), importNamed)], len(continued)), imp.name)
	}
	if len(theirsOnly) > 0 {
		fmt.Fprintf(imp.stdout, "%s holds %s the source does not, left as they are: %s\n", imp.name,
			counted(len(theirsOnly), "ref", "refs"),
			listed(theirsOnly[:min(len(theirsOnly), importNamed)], len(theirsOnly)))
	}
	return queue, remote, nil
}

// diverged is the refusal of a repository holding r at a value that is not an
// earlier state of the source's; held is whether the clone holds that value.
func diverged(name string, r sourceRef, at string, held bool) error {
	why := "this is not a resumed import of the same history"
	switch {
	case !held:
		why = "the source does not hold that commit, so something else was pushed there: " + why
	case strings.HasPrefix(r.ref, "refs/tags/"):
		why = "a tag never moves: " + why
	}
	return fmt.Errorf("nothing was imported: %s is at %s on %s and %s in the source: %s", r.ref, at, name, r.oid,
		why)
}

// targetRefs is every ref the repository holds, read through the node's git
// routes as a person's git reads them.
func (imp *importer) targetRefs(ctx context.Context) (map[string]string, error) {
	refs := map[string]string{}
	w := &lineWriter{max: maxImportLine, fn: func(line []byte) error {
		oid, ref, ok := bytes.Cut(line, []byte{'\t'})
		if !ok {
			return fmt.Errorf("git ls-remote of %s printed a line that is no ref: %q", imp.name, line)
		}
		if name := string(ref); name != "HEAD" && !strings.HasSuffix(name, "^{}") {
			refs[name] = string(oid)
		}
		return nil
	}}
	if _, err := imp.g.call(ctx, gitCall{env: imp.g.targetEnv(), stdout: w}, "ls-remote",
		imp.g.repoURL(imp.name)); err != nil {
		return nil, err
	}
	return refs, w.flush()
}

// held is which of oids the clone holds, in one `cat-file --batch-check`.
func (imp *importer) held(ctx context.Context, oids map[string]bool) (map[string]bool, error) {
	present := make(map[string]bool, len(oids))
	if len(oids) == 0 {
		return present, nil
	}
	in := make([]byte, 0, len(oids)*(oidHexLen+1))
	for oid := range oids {
		in = append(in, oid...)
		in = append(in, '\n')
	}
	w := &lineWriter{max: maxImportLine, fn: func(line []byte) error {
		if !bytes.HasSuffix(line, []byte(" missing")) {
			present[string(line)] = true
		}
		return nil
	}}
	if _, err := imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone, stdin: in, stdout: w}, "cat-file",
		"--buffer", "--batch-check=%(objectname)"); err != nil {
		return nil, err
	}
	return present, w.flush()
}

// descendants reports, per source ref the repository holds at an earlier commit
// the clone has, whether the source's value descends from it.
//
// ASKED BOUNDED IN PARALLEL: git has no many-pair merge-base --is-ancestor, and
// up to gitlet.MaxRefsPerRepo branches at 5-10 ms each is slow in series. A ref
// found not to descend stops questions about LATER refs (the refusal names the
// first); earlier ones are still answered; nothing runs on past return.
func (imp *importer) descendants(ctx context.Context, refs []sourceRef, theirs map[string]string,
	held map[string]bool) ([]bool, error) {

	descends := make([]bool, len(refs))
	var first atomic.Int64 // the first ref in order found not to descend
	first.Store(int64(len(refs)))
	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(runtime.GOMAXPROCS(0))
	for i, r := range refs {
		at, holds := theirs[r.ref]
		if !holds || at == r.oid || !r.isBranch() || !held[at] {
			continue
		}
		if int64(i) > first.Load() {
			break
		}
		group.Go(func() error {
			if int64(i) > first.Load() {
				return nil
			}
			ok, err := imp.isAncestor(gctx, at, r.oid)
			if err != nil {
				return err
			}
			descends[i] = ok
			for !ok {
				cur := first.Load()
				if int64(i) >= cur || first.CompareAndSwap(cur, int64(i)) {
					break
				}
			}
			return nil
		})
	}
	return descends, group.Wait()
}

// isAncestor reports whether commit a is b or one of its ancestors.
func (imp *importer) isAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone}, "merge-base", "--is-ancestor", a, b)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil
	default:
		return false, err
	}
}

// ---- 5: the steps ----------------------------------------------------------

// importStep is one push of the import: the refs it moves and the bytes its
// pack carries.
type importStep struct {
	cmds []importCmd
	// estimate is what the source's own packs hold of its objects; size is the
	// pack's measured bytes and objects its header's count (in a dry run, the
	// estimate and no count).
	estimate, size, objects int64
	exact                   bool
	// alone is a step no smaller step can carry (one commit, one ref): past the
	// limit it is refused, or has a merge's history stepped first; any other
	// step is cut shorter.
	alone bool
	// past is a measurement stopped at importMeasureCap: size is then a floor.
	past bool
	// repo is the step's own repository holding its built pack
	// ([importer.stepRepo]), kept only while the step fits, until its push or
	// [importStep.drop].
	repo string
	// overRepo marks a dry run's step whose estimate takes the repository past
	// code.limits.repo_max_bytes; a real run measures it and refuses if so.
	overRepo bool
	// note is why the plan took the step out of its turn ([importPlanner.bringIn]).
	note string
}

// importCmd is one ref a step moves, to the value it moves it to.
type importCmd struct{ oid, ref string }

// describe is the step as the import prints it.
func (s *importStep) describe() string {
	if len(s.cmds) == 1 {
		return s.cmds[0].ref + " → " + shortOID(s.cmds[0].oid)
	}
	return fmt.Sprintf("%d refs, %s … %s", len(s.cmds), s.cmds[0].ref, s.cmds[len(s.cmds)-1].ref)
}

// sized is the step's size as the import prints it: measured, or an estimate.
func (s *importStep) sized() string {
	if s.exact {
		return humanBytes(s.size)
	}
	return "≈ " + humanBytes(s.size)
}

// stored is what the node keeps for the step: the pack as sent plus the .idx and
// .rev, sized from the object count (gitlet.IndexBytesFor, ReverseBytesFor). The
// node holds that bundle to gitlet.MaxPackBytes. In a dry run, the estimate.
func (s *importStep) stored() int64 {
	if !s.exact {
		return s.size
	}
	return s.size + gitlet.IndexBytesFor(s.objects) + gitlet.ReverseBytesFor(s.objects)
}

// news is the values the step moves its refs to, IN THE ORDER GIT PUSH HANDS
// THEM TO ITS OWN pack-objects: refs remote holds first, by name, then the refs
// the step creates, in step order. The copy equals the pushed pack only given
// the same order ([importer.sent]): a walk meets two tips of one commit time in
// the order handed (git 2.53 made different checksums), and an order moving a
// delta from its base changes the size.
func (s *importStep) news(remote *remoteRefs) []string {
	news := make([]string, 0, len(s.cmds))
	updated := make([]importCmd, 0, len(s.cmds))
	for _, c := range s.cmds {
		if _, held := remote.refs[c.ref]; held {
			updated = append(updated, c)
		}
	}
	slices.SortFunc(updated, func(a, b importCmd) int { return strings.Compare(a.ref, b.ref) })
	for _, c := range updated {
		news = append(news, c.oid)
	}
	for _, c := range s.cmds {
		if _, held := remote.refs[c.ref]; !held {
			news = append(news, c.oid)
		}
	}
	return news
}

// drop removes the step's repository and pack once it is pushed or the plan will
// not push it, so a long import never holds every step it built.
func (s *importStep) drop() {
	if s.repo != "" {
		_ = os.RemoveAll(s.repo)
		s.repo = ""
	}
}

// oidList is object ids as one run of bytes, oidHexLen a piece: a first-parent
// history can be a million commits, and a string each would take three times the bytes.
type oidList []byte

func (l oidList) len() int        { return len(l) / oidHexLen }
func (l oidList) at(i int) string { return string(l.raw(i)) }
func (l oidList) raw(i int) []byte {
	return l[i*oidHexLen : (i+1)*oidHexLen]
}

// firstParentWalk is a branch advanced along a first-parent history, its own or
// one a merge brings in ([importPlanner.bringIn]): list is every commit of it the
// repository will not hold, oldest first, and at the first not yet planned.
type firstParentWalk struct {
	ref  string
	list oidList
	at   int
	// carrier is the queued branch a merge's history is stepped on, taken out of
	// the queue at slot and put back when the walk is done, to be pushed on from
	// where the walk left it (unless it reached its own value). Nil for a
	// branch's own history.
	carrier *queuedRef
	slot    int
}

// importPlanner cuts the refs still to push into steps ONE AT A TIME, each
// planned against the refs the repository will hold once earlier steps land
// (remote), so the next is planned while this one is pushed.
//
// A step is cut by estimate to fit the limit scaled by ratio (how much larger
// than estimates the last steps packed, at least one), searched from the last
// step's length (furthest). Unless a dry run it is then MEASURED: past the limit
// or one push's store, it is cut back to where the measurement fits and measured
// again, until it fits or is one commit or ref, refused past the limit (a merge
// instead has its brought-in history stepped first, as a dry run does by estimate).
type importPlanner struct {
	imp    *importer
	remote *remoteRefs
	budget int64 // what a step's pack is held to: code.limits.max_push_bytes
	exact  bool  // measure each step (a dry run plans from estimates alone)
	ratio  float64
	queue  []queuedRef
	walk   *firstParentWalk
	// suspended are the walks a merge paused, innermost last ([importPlanner.bringIn]).
	suspended []*firstParentWalk
	// note is said of the next step taken ([importStep.note]).
	note string
	// held is what the repository's packs will hold once every planned step
	// lands ([importPlanner.hold]).
	held int64
	// lastWalk and lastBatch are the last steps' sizes: where the next search starts.
	lastWalk, lastBatch int
	// recut counts the steps a measurement cut shorter.
	recut int
}

// next is the next step, its refs recorded in remote as if landed, or nil when
// nothing is left.
func (p *importPlanner) next(ctx context.Context) (*importStep, error) {
	step, err := p.take(ctx)
	if step != nil && p.note != "" {
		step.note, p.note = p.note, ""
	}
	return step, err
}

// take is [importPlanner.next] before the step carries its note.
func (p *importPlanner) take(ctx context.Context) (*importStep, error) {
	for {
		if p.walk != nil && p.walk.at < p.walk.list.len() {
			return p.walkStep(ctx)
		}
		if w := p.walk; w != nil && w.carrier != nil && p.remote.refs[w.ref] != w.carrier.oid {
			// The carrier goes back to its turn. Walks are a stack and
			// the queue's head is taken only when none is left, so slot
			// is where it stood: later carriers are already back.
			p.queue = slices.Insert(p.queue, min(w.slot, len(p.queue)), *w.carrier)
		}
		p.walk = nil
		if n := len(p.suspended); n > 0 {
			// The merge's history is in: its paused walk resumes.
			p.walk, p.suspended = p.suspended[n-1], p.suspended[:n-1]
			continue
		}
		if len(p.queue) == 0 {
			return nil, nil
		}
		if head := p.queue[0]; head.walk {
			p.queue = p.queue[1:]
			w, err := p.walkTo(ctx, head.ref, head.oid)
			if err != nil {
				return nil, err
			}
			if p.walk = w; w == nil {
				// The repository will hold every commit already: the ref
				// alone moves, in a batch (an empty pack, so no batch
				// sends it back here).
				p.queue = slices.Insert(p.queue, 0, queuedRef{sourceRef: head.sourceRef})
			}
			continue
		}
		return p.batchStep(ctx)
	}
}

// walkTo is ref advanced along tip's first-parent history from what the
// repository will hold, or nil when there is nothing to advance through.
//
// A BRANCH ALREADY HELD is advanced only through commits that DESCEND from its
// held value, since every step fast-forwards. Where that value reached tip
// through a merge's second parent, the first-parent history BEGINS with commits
// that do not descend from it (non-fast-forward every run); the first
// descendant's step carries them ([importer.firstDescendant]). The held value
// must be an ancestor of tip ([importer.resume], [importPlanner.carrier]).
func (p *importPlanner) walkTo(ctx context.Context, ref, tip string) (*firstParentWalk, error) {
	var list oidList
	w := &lineWriter{max: maxImportLine, fn: func(line []byte) error {
		if len(line) != oidHexLen {
			return fmt.Errorf("git rev-list printed a line that is no commit: %q", line)
		}
		list = append(list, line...)
		return nil
	}}
	if _, err := p.imp.g.call(ctx, gitCall{env: p.imp.g.environ, gitDir: p.imp.clone,
		stdin: p.remote.revs([]string{tip}), stdout: w}, "rev-list", "--first-parent", "--reverse",
		"--stdin"); err != nil {
		return nil, err
	}
	if err := w.flush(); err != nil {
		return nil, err
	}
	if list.len() == 0 {
		return nil, nil
	}
	if at, holds := p.remote.refs[ref]; holds {
		from, err := p.imp.firstDescendant(ctx, at, tip, list, p.remote)
		if err != nil {
			return nil, err
		}
		list = list[from*oidHexLen:]
	}
	return &firstParentWalk{ref: ref, list: list}, nil
}

// firstDescendant is the index of the first commit of list (first-parent history
// to tip, oldest first) that descends from at, where the repository holds the
// branch; every later one does too.
//
// ONE TRAVERSAL: rev-list --ancestry-path=<at> (git 2.38) streams every
// descendant of at, looked up among list's sorted commits. Never with
// --first-parent, which misses a descendant reached through a second parent.
func (imp *importer) firstDescendant(ctx context.Context, at, tip string, list oidList,
	remote *remoteRefs) (int, error) {

	n := list.len()
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return bytes.Compare(list.raw(a), list.raw(b)) })
	first := n
	w := &lineWriter{max: maxImportLine, fn: func(line []byte) error {
		j, found := slices.BinarySearchFunc(order, line, func(i int, oid []byte) int {
			return bytes.Compare(list.raw(i), oid)
		})
		if found && order[j] < first {
			first = order[j]
		}
		return nil
	}}
	if _, err := imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone, stdin: remote.revs([]string{tip}),
		stdout: w}, "rev-list", "--ancestry-path="+at, "--stdin"); err != nil {
		return 0, err
	}
	if err := w.flush(); err != nil {
		return 0, err
	}
	if first == n {
		return 0, fmt.Errorf("no commit of %s's first-parent history descends from %s, which %s holds it at, "+
			"though git said %s is an ancestor of %s", shortOID(tip), at, imp.name, at, shortOID(tip))
	}
	return first, nil
}

// walkStep is the walk's next step: its branch fast-forwarded to the furthest
// commit the step can carry.
func (p *importPlanner) walkStep(ctx context.Context) (*importStep, error) {
	w := p.walk
	estimates := map[int]int64{}
	estimate := func(i int) (int64, error) {
		if e, ok := estimates[i]; ok {
			return e, nil
		}
		e, err := p.imp.estimate(ctx, []string{w.list.at(i)}, p.remote)
		estimates[i] = e
		return e, err
	}
	end, err := furthest(w.at, w.list.len()-1, w.at+max(p.lastWalk, 1)-1, under(estimate, p.target()))
	if err != nil {
		return nil, err
	}
	shorter := false
	for {
		end = max(end, w.at)
		est, err := estimate(end)
		if err != nil {
			return nil, err
		}
		step := &importStep{cmds: []importCmd{{oid: w.list.at(end), ref: w.ref}}, estimate: est,
			alone: end == w.at}
		cut, err := p.settle(ctx, step)
		if err != nil {
			return nil, err
		}
		if cut == 0 && !p.exact && step.alone && step.size > p.budget {
			// A dry run measures nothing, so it plans the bring-in by
			// estimate; only a merge nothing can carry is planned as it
			// is, marked past the limit.
			var stepped bool
			if stepped, _, err = p.bringIn(ctx, step); err != nil {
				return nil, err
			}
			if stepped {
				return p.take(ctx)
			}
		}
		if cut == 0 {
			if err = p.hold(ctx, step); err != nil {
				return nil, err
			}
			if shorter {
				p.recut++
			}
			p.lastWalk = end - w.at + 1
			w.at = end + 1
			p.remote.set(w.ref, step.cmds[0].oid)
			return step, nil
		}
		if step.alone {
			var (
				stepped bool
				lacking []string
			)
			switch stepped, lacking, err = p.bringIn(ctx, step); {
			case err != nil:
				return nil, err
			case stepped:
				return p.take(ctx)
			case len(lacking) > 0:
				return nil, p.mergeTooLarge(ctx, step, lacking)
			}
			return nil, p.tooLarge(step, true)
		}
		shorter = true
		// Cut back to the furthest commit whose estimate is under the measurement's fit.
		if end, err = furthest(w.at, end-1, w.at+(end-w.at)/2, under(estimate, cut)); err != nil {
			return nil, err
		}
	}
}

// bringIn steps the history a merge brings in BEFORE the merge, where the merge
// alone is past a step: a first-parent walk meets another line of history as the
// ONE commit that merges it, which no step can split. It steps the first-parent
// history of a parent the repository will not hold, on a still-to-push branch
// reaching it ([importPlanner.carrier]), suspending the walk at the merge. It
// walks THE PARENT'S HISTORY, never the branch's own, which can meet this merge
// again. It answers whether it took one, else the parents the repository lacks:
// all held, nothing can split the commit; one without a carrier, the refusal
// names it ([importPlanner.mergeTooLarge]).
func (p *importPlanner) bringIn(ctx context.Context, step *importStep) (bool, []string, error) {
	merge := step.cmds[0]
	parents, err := p.imp.parents(ctx, merge.oid)
	if err != nil {
		return false, nil, err
	}
	var lacking []string
	for _, parent := range parents {
		held, err := p.imp.reaches(ctx, parent, p.remote)
		if err != nil {
			return false, nil, err
		}
		if !held {
			lacking = append(lacking, parent)
		}
	}
	for _, parent := range lacking {
		i, err := p.carrier(ctx, parent, merge.oid)
		if err != nil {
			return false, nil, err
		}
		if i < 0 {
			continue
		}
		carrier := p.queue[i]
		walk, err := p.walkTo(ctx, carrier.ref, parent)
		if err != nil {
			return false, nil, err
		}
		if walk == nil {
			return false, nil, fmt.Errorf("%s will not hold %s, yet git lists no commit of its history to push",
				p.imp.name, parent)
		}
		walk.carrier, walk.slot = &carrier, i
		p.queue = slices.Delete(p.queue, i, i+1)
		p.suspended = append(p.suspended, p.walk)
		p.walk = walk
		p.note = fmt.Sprintf("%s first, to %s: commit %s of %s brings in its history, which is past one step on "+
			"its own", carrier.ref, shortOID(parent), shortOID(merge.oid), merge.ref)
		return true, nil, nil
	}
	return false, lacking, nil
}

// carrier is the queue index of the branch parent's history is stepped on, or -1
// where none can: a branch whose source value reaches parent and which the
// repository does not hold or holds at an ancestor of parent
// ([importPlanner.walkTo]). One reaching parent WITHOUT the merge goes first
// (vendor, where main merged vendor), so the history lands where it belongs;
// else one reaching it through the merge. Either is the first in source order,
// so a resumed run takes the one the failed run took. Held ones are asked
// BOUNDED IN PARALLEL as in [importer.descendants].
func (p *importPlanner) carrier(ctx context.Context, parent, merge string) (int, error) {
	reach, err := p.imp.branchesContaining(ctx, parent, "")
	if err != nil {
		return -1, err
	}
	without, err := p.imp.branchesContaining(ctx, parent, merge)
	if err != nil {
		return -1, err
	}
	var candidates []int // indexes into the queue, in the order they are preferred
	for _, own := range []bool{true, false} {
		for k, q := range p.queue {
			if q.isBranch() && reach[q.ref] && without[q.ref] == own {
				candidates = append(candidates, k)
			}
		}
	}
	var first atomic.Int64 // the first candidate in order found able to carry it
	first.Store(int64(len(candidates)))
	found := func(k int) {
		for {
			cur := first.Load()
			if int64(k) >= cur || first.CompareAndSwap(cur, int64(k)) {
				return
			}
		}
	}
	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(runtime.GOMAXPROCS(0))
	for k, i := range candidates {
		if int64(k) > first.Load() {
			break
		}
		at, holds := p.remote.refs[p.queue[i].ref]
		if !holds {
			found(k)
			break
		}
		group.Go(func() (err error) {
			if int64(k) > first.Load() {
				return nil
			}
			var ok bool
			if ok, err = p.imp.isAncestor(gctx, at, parent); ok {
				found(k)
			}
			return err
		})
	}
	if err = group.Wait(); err != nil {
		return -1, err
	}
	if k := first.Load(); k < int64(len(candidates)) {
		return candidates[k], nil
	}
	return -1, nil
}

// mergeTooLarge is the refusal of a commit past the push limit because of the
// history its missing parents bring in, which no branch carries
// ([importPlanner.carrier]).
func (p *importPlanner) mergeTooLarge(ctx context.Context, step *importStep, lacking []string) error {
	brings, err := p.imp.estimate(ctx, lacking, p.remote)
	if err != nil {
		return err
	}
	c := step.cmds[0]
	size := humanBytes(step.size)
	if step.past {
		size = "more than " + size
	}
	branches := make([]string, len(lacking))
	// Unquoted: an object name is copied into a command, not read as a value
	// somebody typed.
	parents := make([]words.Said, len(lacking))
	for i, parent := range lacking {
		branches[i] = fmt.Sprintf("git --git-dir=mirror.git branch import-%s %s", shortOID(parent), parent)
		parents[i] = words.Said(parent)
	}
	return fmt.Errorf("commit %s of %s alone packs to %s, past code.limits.max_push_bytes (%s): it brings in "+
		"the history of %s (about %s in the source's own packs), and no other branch of the source can carry "+
		"it, so nothing can push that history in steps before it. Give it a branch in a mirror of the source "+
		"(git clone --mirror %s mirror.git, then %s) and import from the mirror: the import steps that history "+
		"on it first, and the commit is then a small step. Delete the branch from %s after, if it is not wanted",
		c.oid, c.ref, size, humanBytes(p.budget), words.And(parents), humanBytes(brings), p.imp.source,
		strings.Join(branches, "; "), p.imp.name)
}

// batchStep is the next batch: as many queue refs, in order, as a step can carry,
// within gitlet.MaxCommandsPerPush and one push's record ([importer.recordHolds]).
// A branch alone past the limit is walked instead.
func (p *importPlanner) batchStep(ctx context.Context) (*importStep, error) {
	candidates := p.queue[:min(len(p.queue), gitlet.MaxCommandsPerPush)]
	held, err := p.imp.recordHolds(candidates)
	if err != nil {
		return nil, err
	}
	candidates = candidates[:held]
	estimates := map[int]int64{}
	estimate := func(n int) (int64, error) {
		if e, ok := estimates[n]; ok {
			return e, nil
		}
		news := make([]string, n)
		for i, r := range candidates[:n] {
			news[i] = r.oid
		}
		e, estimateErr := p.imp.estimate(ctx, news, p.remote)
		estimates[n] = e
		return e, estimateErr
	}
	n, err := furthest(1, len(candidates), max(p.lastBatch, 1), under(estimate, p.target()))
	if err != nil {
		return nil, err
	}
	if n < 1 && candidates[0].isBranch() {
		return p.walkInstead(ctx)
	}
	shorter := false
	for {
		n = max(n, 1)
		est, err := estimate(n)
		if err != nil {
			return nil, err
		}
		// Only a non-branch ref is alone: a branch past the limit is walked.
		step := &importStep{estimate: est, alone: n == 1 && !candidates[0].isBranch()}
		for _, r := range candidates[:n] {
			step.cmds = append(step.cmds, importCmd{oid: r.oid, ref: r.ref})
		}
		cut, err := p.settle(ctx, step)
		if err != nil {
			return nil, err
		}
		if cut == 0 {
			if err = p.hold(ctx, step); err != nil {
				return nil, err
			}
			if shorter {
				p.recut++
			}
			p.lastBatch = n
			p.queue = p.queue[n:]
			for _, c := range step.cmds {
				p.remote.set(c.ref, c.oid)
			}
			return step, nil
		}
		switch {
		case n == 1 && candidates[0].isBranch():
			return p.walkInstead(ctx)
		case n == 1:
			return nil, p.tooLarge(step, false)
		}
		shorter = true
		if n, err = furthest(1, n-1, n/2, under(estimate, cut)); err != nil {
			return nil, err
		}
	}
}

// importWidestNode is the widest node id config admits (config.ValidNodeID): any
// node may receive a step's push, and its id is part of the push's record.
var importWidestNode = strings.Repeat("n", 64)

// recordHolds is how many of refs, in order, one push carries by the room its
// record has: at least one, up to all.
//
// THE NODE HOLDS A PUSH'S REF UPDATES TO ITS RECORD (gitlet.Proposal.
// ValidateCommands): a push's code-log entry takes at most gitlet.EntryRoom,
// about 1 MiB, or the push is refused whole. Refs of characters the log spells
// in six bytes (`&`, `<`, `>`) fill it at some six hundred, so counting is not
// enough. The batch is cut by the domain's own rule: the recorded person
// ([gitletImportPrincipal]), the widest node id, each command as a create via
// git (gitlet.ViaGit). None fitting is no ref's fault: the person's id or handle
// fills the record.
func (imp *importer) recordHolds(refs []queuedRef) (int, error) {
	var refusal error
	fits := func(n int) (bool, error) {
		cmds := make([]gitlet.Command, n)
		for i, r := range refs[:n] {
			cmds[i] = gitlet.Command{Ref: gitlet.RecordedRef(r.ref), Old: gitlet.ZeroOID, New: r.oid}
		}
		err := gitlet.Proposal{Principal: imp.principal, Via: gitlet.ViaGit, Node: importWidestNode,
			Commands: cmds}.ValidateCommands()
		var wide *gitlet.TooWideError
		switch {
		case err == nil:
			return true, nil
		case errors.As(err, &wide):
			refusal = err
			return false, nil
		}
		return false, fmt.Errorf("the batch from %s: %w", refs[0].ref, err)
	}
	// The whole list first: one question for every batch of plain refs.
	n, err := furthest(1, len(refs), len(refs), fits)
	switch {
	case err != nil:
		return 0, err
	case n < 1:
		return 0, fmt.Errorf("a push of %s alone, as %s, is wider than one push's record may be (%w): the "+
			"token's id or the seat bound to it fills the record, and no push by this token can land. "+
			"Import with a token whose id, and whose seat's handle, are shorter", refs[0].ref,
			principalName(imp.principal), refusal)
	}
	return n, nil
}

// principalName is the seat's handle and the token's id.
func principalName(p gitlet.Principal) string {
	if p.Seat == "" {
		return "token " + p.OperatorID
	}
	return p.Seat + " (token " + p.OperatorID + ")"
}

// walkInstead walks the queue's first ref, a branch no batch can carry.
func (p *importPlanner) walkInstead(ctx context.Context) (*importStep, error) {
	p.queue[0].walk = true
	return p.take(ctx)
}

// target is the estimate a step is planned to: the limit over the pack ratio.
func (p *importPlanner) target() float64 { return float64(p.budget) / p.ratio }

// settle measures step (in a dry run, takes its estimate) and answers 0 when it
// may be pushed, else the estimate its range must come under: its own estimate
// scaled down by how far it passed the bound it passed furthest (limit, or
// importer.maxStored), less importCutShare.
//
// MEASURED IS COPIED: the pack is built once ([importer.build]) and a build that
// may fit is measured as the copy the push sends ([importer.sent]); a fitting
// step keeps its repository for the push, else it is dropped. A build too far
// past the limit to fit as its copy ([importOrderSlack]), or stopped at its cap,
// is cut on its own size, never copied.
func (p *importPlanner) settle(ctx context.Context, step *importStep) (float64, error) {
	if !p.exact {
		step.size = step.estimate
		return 0, nil
	}
	size, objects, past, err := p.imp.build(ctx, step, p.remote)
	if err != nil {
		step.drop()
		return 0, err
	}
	step.size, step.objects, step.past, step.exact = size, objects, past, true
	if !past && size <= p.budget+importOrderSlack(objects) {
		if step.size, err = p.imp.sent(ctx, step, p.remote); err != nil {
			step.drop()
			return 0, err
		}
	}
	if !past && step.size <= p.budget && step.stored() <= p.imp.maxStored {
		if step.estimate >= importRatioFloor(p.budget) {
			p.ratio = max(1, float64(step.size)/float64(step.estimate))
		}
		return 0, nil
	}
	step.drop()
	over := max(float64(step.size)/float64(p.budget), float64(step.stored())/float64(p.imp.maxStored))
	return float64(step.estimate) / over * importCutShare, nil
}

// hold adds step's stored bytes to what the repository's packs will hold,
// REFUSING THE STEP BEFORE IT IS PUSHED where they would pass
// code.limits.repo_max_bytes (the node refuses repo_size only after the whole
// upload), saying by how much and what is left. A dry run only notes the crossing.
func (p *importPlanner) hold(ctx context.Context, step *importStep) error {
	stored, limit := step.stored(), p.imp.repo.Limits.RepoMaxBytes
	if p.held+stored <= limit || !p.exact {
		step.overRepo = p.held+stored > limit
		p.held += stored
		return nil
	}
	step.drop()
	rest, err := p.rest(ctx)
	if err != nil {
		return err
	}
	return fmt.Errorf("%s would hold %s of packs once the next step lands (%s before it, and %s that step "+
		"stores), past code.limits.repo_max_bytes (%s): raise it, and run the same command again. What is "+
		"left of the source, that step included, holds about %s in the source's own packs, and its steps "+
		"store more than that: each keeps whole what its deltas cannot reach in an earlier step", p.imp.name,
		humanBytes(p.held+stored), humanBytes(p.held), humanBytes(stored), humanBytes(limit), humanBytes(rest))
}

// rest is what is left to push, in the source's own packs: every ref the plan
// has not taken (the walks', carriers out of the queue, the queue's) against
// what the repository will hold.
func (p *importPlanner) rest(ctx context.Context) (int64, error) {
	var tips []string
	for _, w := range append(slices.Clone(p.suspended), p.walk) {
		if w != nil && w.at < w.list.len() {
			tips = append(tips, w.list.at(w.list.len()-1))
		}
		if w != nil && w.carrier != nil {
			tips = append(tips, w.carrier.oid)
		}
	}
	for _, q := range p.queue {
		tips = append(tips, q.oid)
	}
	if len(tips) == 0 {
		return 0, nil
	}
	return p.imp.estimate(ctx, tips, p.remote)
}

// tooLarge refuses a step that cannot be smaller and is past the push limit;
// commit says it is one commit of a branch's history, else one ref.
func (p *importPlanner) tooLarge(step *importStep, commit bool) error {
	size := humanBytes(step.size)
	if step.past {
		size = "more than " + size
	}
	c := step.cmds[0]
	if !step.past && step.size <= p.budget {
		// Many small objects: the indexes take it past gitlet.MaxPackBytes,
		// which no setting raises.
		return fmt.Errorf("%s (%s) alone stores %s with its indexes (%s objects), past the %s one push may store, "+
			"and cannot be pushed in smaller steps", c.ref, c.oid, humanBytes(step.stored()),
			strconv.FormatInt(step.objects, 10), humanBytes(p.imp.maxStored))
	}
	remedy := fmt.Sprintf("raise code.limits.max_push_bytes (%s now, at most %s)", humanBytes(p.budget),
		humanBytes(config.CodeMaxPushBytesCeiling))
	if p.budget >= config.CodeMaxPushBytesCeiling {
		remedy = fmt.Sprintf("code.limits.max_push_bytes is already its ceiling of %s",
			humanBytes(config.CodeMaxPushBytesCeiling))
	}
	if commit {
		return fmt.Errorf("commit %s of %s alone packs to %s, past code.limits.max_push_bytes (%s), and one "+
			"commit is the smallest step there is: %s, or split the history into smaller commits", c.oid, c.ref,
			size, humanBytes(p.budget), remedy)
	}
	return fmt.Errorf("%s (%s) alone packs to %s, past code.limits.max_push_bytes (%s), and no branch's history "+
		"reaches it, so it cannot be pushed in steps: %s", c.ref, c.oid, size, humanBytes(p.budget), remedy)
}

// under asks whether the range ending at i estimates under target.
func under(estimate func(int) (int64, error), target float64) func(int) (bool, error) {
	return func(i int) (bool, error) {
		e, err := estimate(i)
		return float64(e) <= target, err
	}
}

// furthest is the largest i in [lo, hi] for which fits(i) holds (true up to some
// index, false past it), or lo-1 for none. It asks guess, gallops by doubling to
// bracket the answer, then halves the bracket: a guess near the last step's end
// costs the log of how far off it was, not of the whole history.
//
// WHERE THE GUESS DOES NOT FIT, lo IS ASKED NEXT: every range holds lo's, so a
// lo that does not fit means none does, saving a walk down that re-traverses a
// past-limit branch's whole history at each halving.
func furthest(lo, hi, guess int, fits func(int) (bool, error)) (int, error) {
	if hi < lo {
		return lo - 1, nil
	}
	guess = min(max(guess, lo), hi)
	ok, err := fits(guess)
	if err != nil {
		return 0, err
	}
	// fits(good) holds (or good is lo-1); fits(bad) does not (or bad is hi+1).
	good, bad := lo-1, hi+1
	if ok {
		good = guess
		for step := 1; good < hi; step *= 2 {
			next := min(good+step, hi)
			if ok, err = fits(next); err != nil {
				return 0, err
			}
			if !ok {
				bad = next
				break
			}
			good = next
		}
	} else {
		bad = guess
		if guess > lo {
			if ok, err = fits(lo); err != nil {
				return 0, err
			}
			if !ok {
				return lo - 1, nil
			}
			good = lo
		}
		for step := 1; ; step *= 2 {
			next := bad - step
			if next <= good {
				break
			}
			if ok, err = fits(next); err != nil {
				return 0, err
			}
			if ok {
				good = next
				break
			}
			bad = next
		}
	}
	for bad-good > 1 {
		mid := good + (bad-good)/2
		if ok, err = fits(mid); err != nil {
			return 0, err
		}
		if ok {
			good = mid
		} else {
			bad = mid
		}
	}
	return good, nil
}

// estimate is what the source's own packs hold of the objects news reach that
// the repository will not hold (rev-list --disk-usage).
func (imp *importer) estimate(ctx context.Context, news []string, remote *remoteRefs) (int64, error) {
	out, err := imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone, stdin: remote.revs(news)},
		"rev-list", "--objects", "--disk-usage", "--stdin")
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("git rev-list --disk-usage answered %q, which is no size", out)
	}
	return n, nil
}

// parents is commit's parents, first parent first.
func (imp *importer) parents(ctx context.Context, commit string) ([]string, error) {
	out, err := imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone}, "rev-list", "--parents",
		"--no-walk", commit)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 || fields[0] != commit {
		return nil, fmt.Errorf("git rev-list --parents answered %q about %s", out, commit)
	}
	return fields[1:], nil
}

// reaches reports whether the repository will hold commit and its whole history,
// by one rev-list that stops at the first commit its refs do not reach.
func (imp *importer) reaches(ctx context.Context, commit string, remote *remoteRefs) (bool, error) {
	out, err := imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone, stdin: remote.revs([]string{commit})},
		"rev-list", "--max-count=1", "--stdin")
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) == 0, nil
}

// branchesContaining is every source branch whose history holds commit and, where
// without is set, not without (for-each-ref --no-contains, git 2.13).
func (imp *importer) branchesContaining(ctx context.Context, commit, without string) (map[string]bool, error) {
	reach := map[string]bool{}
	w := &lineWriter{max: maxImportLine, fn: func(line []byte) error {
		reach[string(line)] = true
		return nil
	}}
	args := []string{"for-each-ref", "--contains=" + commit, "--format=%(refname)"}
	if without != "" {
		args = append(args, "--no-contains="+without)
	}
	if _, err := imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone, stdout: w},
		append(args, "refs/heads/")...); err != nil {
		return nil, err
	}
	return reach, w.flush()
}

// build builds step's pack ONCE (its one delta search) and answers its bytes and
// header object count: pack-objects --delta-base-offset (the node advertises
// ofs-delta), whole as pushed, under the operator's own pack settings. It is
// written into step.repo as counted, and stopped at importMeasureCap.
func (imp *importer) build(ctx context.Context, step *importStep, remote *remoteRefs) (int64, int64, bool,
	error) {

	repo, err := imp.stepRepo(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	step.repo = repo
	f, err := os.OpenFile(stepPack(repo), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, 0, false, fmt.Errorf("write a step's pack: %w", err)
	}
	counted := &countingWriter{limit: importMeasureCap(imp.budget), to: f}
	_, err = imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone, stdin: remote.revs(step.news(remote)),
		stdout: counted}, "pack-objects", "--revs", "--stdout", "--delta-base-offset", "-q")
	if closeErr := f.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("write a step's pack: %w", closeErr)
	}
	past := errors.Is(err, errPastCap)
	if err != nil && !past {
		return 0, 0, false, err
	}
	objects, err := counted.objects()
	return counted.n, objects, past, err
}

// sent is the bytes of the pack a push of step sends: the build indexed in its
// repository, only then given the clone's objects ([importer.borrow]), COPIED
// under importSendConfig, counted and thrown away. The copy is the push's own
// pack byte for byte: neither has anything to choose and both get the tips in
// one order ([importStep.news]). Uncapped: it is at most importOrderSlack past
// the build, which settle copies only within the limit.
func (imp *importer) sent(ctx context.Context, step *importStep, remote *remoteRefs) (int64, error) {
	if _, err := imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: step.repo}, "index-pack",
		stepPack(step.repo)); err != nil {
		return 0, err
	}
	if err := imp.borrow(step.repo); err != nil {
		return 0, err
	}
	counted := &countingWriter{limit: math.MaxInt64}
	if _, err := imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: step.repo, config: importSendConfig,
		stdin: remote.revs(step.news(remote)), stdout: counted}, "pack-objects", "--revs", "--stdout",
		"--delta-base-offset", "-q"); err != nil {
		return 0, err
	}
	return counted.n, nil
}

// stepRepo makes a step's own bare repository, with no ref and no pack. The build
// is written there and indexed before it borrows the clone's objects
// ([importer.borrow]): pack-objects takes local packs first, and the clone's are
// what its walk reads below the step. SHA-1 whatever init.defaultObjectFormat
// says, since a repository borrows only its own format's objects.
func (imp *importer) stepRepo(ctx context.Context) (string, error) {
	dir, err := os.MkdirTemp(imp.work, "step-*.git")
	if err != nil {
		return "", fmt.Errorf("make a step's repository: %w", err)
	}
	if _, err = imp.g.run(ctx, imp.g.environ, "", "init", "--bare", "--quiet", "--object-format=sha1",
		dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// borrow has a step's repository borrow every object of the clone
// (objects/info/alternates), for the copy's walk and the push.
//
// ONLY ONCE THE BUILD IS INDEXED: index-pack compares each object it shares with
// the repository byte for byte, and borrowing first would read the whole build
// back from the clone for a comparison that cannot fail.
//
// BY ABSOLUTE PATH: git reads a relative alternate against the borrowing
// repository's objects directory, and $TMPDIR may be relative.
func (imp *importer) borrow(repo string) error {
	objects, err := filepath.Abs(filepath.Join(imp.clone, "objects"))
	if err == nil {
		err = os.WriteFile(filepath.Join(repo, "objects", "info", "alternates"), []byte(objects+"\n"), 0o600)
	}
	if err != nil {
		return fmt.Errorf("have a step's repository borrow the clone's objects: %w", err)
	}
	return nil
}

// stepPack is where a step's build is written; git finds it by its .idx.
func stepPack(repo string) string { return filepath.Join(repo, "objects", "pack", "step.pack") }

// errPastCap is a measurement stopped at its cap.
var errPastCap = errors.New("the pack is past what its measurement builds")

// packHeaderBytes is a pack's header: "PACK", the version, the object count.
const packHeaderBytes = 12

// countingWriter counts what is written to it, refusing a write past limit, and
// keeps the pack's header; it passes everything on to to, if set.
type countingWriter struct {
	n, limit int64
	to       io.Writer
	head     [packHeaderBytes]byte
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if c.n < packHeaderBytes {
		copy(c.head[c.n:], p)
	}
	c.n += int64(len(p))
	if c.n > c.limit {
		return 0, errPastCap
	}
	if c.to != nil {
		return c.to.Write(p)
	}
	return len(p), nil
}

// objects is the object count the pack's header carries.
func (c *countingWriter) objects() (int64, error) {
	if c.n < packHeaderBytes || string(c.head[:4]) != "PACK" {
		return 0, fmt.Errorf("git pack-objects wrote %d bytes that are no pack", c.n)
	}
	if v := binary.BigEndian.Uint32(c.head[4:8]); v != 2 && v != 3 {
		return 0, fmt.Errorf("git pack-objects wrote a pack of version %d, which no git reads", v)
	}
	return int64(binary.BigEndian.Uint32(c.head[8:12])), nil
}

// plan prints a dry run's steps, planned from estimates alone.
func (imp *importer) plan(ctx context.Context, p *importPlanner) error {
	var (
		steps []*importStep
		total int64
	)
	for {
		step, err := p.next(ctx)
		if err != nil {
			return err
		}
		if step == nil {
			break
		}
		steps = append(steps, step)
		total += step.size
	}
	fmt.Fprintf(imp.stdout, "plan: %s, ≈ %s, estimated from how the source stores its objects; an import "+
		"measures each step before it pushes it, and cuts one that packs past %s shorter\n",
		counted(len(steps), "step", "steps"), humanBytes(total), humanBytes(imp.budget))
	for i, s := range steps {
		if s.note != "" {
			fmt.Fprintf(imp.stdout, "  stepping %s\n", s.note)
		}
		note := ""
		switch {
		case s.size > p.budget:
			note = ", past the push limit by its estimate: measured when it is pushed, and refused if it packs " +
				"that large"
		case s.overRepo:
			note = ", past code.limits.repo_max_bytes by its estimate, with what the repository holds: measured " +
				"when it is pushed, and refused if it stores that much"
		}
		fmt.Fprintf(imp.stdout, "  step %d/%d: %s (%s)%s\n", i+1, len(steps), s.describe(), s.sized(), note)
	}
	_, err := fmt.Fprintln(imp.stdout, "dry run: nothing was pushed")
	return err
}

// stepOutcome is what became of one step's push.
type stepOutcome struct {
	err error
	// rejected is git's own line for every ref the push did not land.
	rejected []string
}

// importDone is what one run pushed: steps, bytes sent, bytes the node stores,
// and how many steps a measurement cut shorter (recut).
type importDone struct {
	steps        int
	sent, stored int64
	recut        int
}

// planned is one planning of the next step, as its goroutine hands it back.
type planned struct {
	step *importStep
	err  error
}

// execute pushes every step, planning each while the one before is pushed. One
// push at a time (each fast-forwards from the one before); the step in flight is
// waited for on every path out, its outcome reported before a plan's refusal
// since it may have landed. The next step is planned on a goroutine under its own
// context; a refused push cancels it and is reported once planning has returned,
// so no git outlives the import.
func (imp *importer) execute(ctx context.Context, p *importPlanner) (importDone, error) {
	var (
		done     importDone
		flying   <-chan stepOutcome
		inFlight *importStep
	)
	landed := func() {
		done.steps++
		done.sent += inFlight.size
		done.stored += inFlight.stored()
		fmt.Fprintf(imp.stdout, "imported step %d: %s (%s)\n", done.steps, inFlight.describe(), inFlight.sized())
	}
	for {
		planning, stopPlanning := context.WithCancel(ctx)
		next := make(chan planned, 1)
		go func() {
			step, err := p.next(planning)
			next <- planned{step: step, err: err}
		}()
		var plan planned
		if flying == nil {
			plan = <-next
		} else {
			select {
			case out := <-flying:
				if out.err != nil {
					stopPlanning()
					if unpushed := <-next; unpushed.step != nil {
						unpushed.step.drop()
					}
					return done, imp.stopped(done.steps, out)
				}
				landed()
				plan = <-next
			case plan = <-next:
				if out := <-flying; out.err != nil {
					stopPlanning()
					if plan.step != nil {
						plan.step.drop()
					}
					return done, imp.stopped(done.steps, out)
				}
				landed()
			}
		}
		stopPlanning()
		if plan.err != nil {
			return done, fmt.Errorf("%w\n\nthe import stopped before step %d; %s. Run the same command again "+
				"once the cause is fixed: it resumes", plan.err, done.steps+1, landedBefore(done.steps))
		}
		step := plan.step
		if step == nil {
			return done, nil
		}
		if step.note != "" {
			fmt.Fprintf(imp.stdout, "stepping %s\n", step.note)
		}
		fmt.Fprintf(imp.stdout, "pushing step %d: %s (%s)\n", done.steps+1, step.describe(), step.sized())
		pushed := make(chan stepOutcome, 1)
		go func() { pushed <- imp.push(ctx, step) }()
		flying, inFlight = pushed, step
	}
}

// push is one step's `git push --porcelain --atomic`, pinned to the measured
// push (importPushConfig, importPushFlags), sent from the step's own repository
// (importSendConfig), and read from its own lines: every named ref landed, or the
// step did not. The repository goes when the push ends.
//
// --atomic so a step lands whole or not at all, which lets a re-run resume. The
// refspecs are on the command line: at most gitlet.MaxCommandsPerPush of 296
// bytes each is under 300 KB, inside the argument limit (1 MiB darwin, 2 MiB
// linux). A line for an unnamed ref is a push that carried more than its plan
// (an unpinned operator setting), and is said.
func (imp *importer) push(ctx context.Context, step *importStep) stepOutcome {
	defer step.drop()
	args := append(slices.Clone(importPushFlags), "--atomic")
	if imp.live != nil {
		args = append(args, "--progress")
	}
	args = append(args, imp.g.repoURL(imp.name))
	named := make(map[string]bool, len(step.cmds))
	for _, c := range step.cmds {
		args = append(args, c.oid+":"+c.ref)
		named[c.ref] = true
	}
	out, err := imp.g.call(ctx, gitCall{env: imp.g.targetEnv(), gitDir: step.repo,
		config: append(slices.Clone(importSendConfig), importPushConfig...), live: imp.live}, args...)
	lines := porcelainLines(out)
	var rejected, unplanned []string
	unsaid := 0
	for _, c := range step.cmds {
		l, ok := lines[c.ref]
		switch {
		case !ok:
			unsaid++
		case l.flag == '!':
			rejected = append(rejected, l.text)
		}
	}
	for ref := range lines {
		if !named[ref] {
			unplanned = append(unplanned, ref)
		}
	}
	slices.Sort(unplanned)
	switch {
	case len(rejected) > 0 && err != nil:
		// Git's own words for why go beside its ref lines: a line names a class
		// alone (`(unpacker error)` for every object the node's fsck refuses),
		// and the reason is on stderr, which only a live terminal showed.
		return stepOutcome{err: fmt.Errorf("git refused it: %w", err), rejected: rejected}
	case len(rejected) > 0:
		return stepOutcome{err: errors.New("git refused it"), rejected: rejected}
	case err != nil:
		return stepOutcome{err: err}
	case unsaid > 0:
		return stepOutcome{err: fmt.Errorf("git pushed without saying what became of %d of its refs", unsaid)}
	case len(unplanned) > 0:
		return stepOutcome{err: fmt.Errorf("git pushed %s the step did not name (%s), which the plan never "+
			"measured", counted(len(unplanned), "ref", "refs"), listed(unplanned[:min(len(unplanned), importNamed)],
			len(unplanned)))}
	}
	return stepOutcome{}
}

// porcelainLine is one ref's line of `git push --porcelain`: its flag (' ', '+',
// '-', '*', '=' landed or up to date, '!' rejected) and the line as printed.
type porcelainLine struct {
	flag byte
	text string
}

// porcelainLines reads `git push --porcelain` status lines
// ("<flag>\t<from>:<to>\t<summary>") by destination ref.
func porcelainLines(out []byte) map[string]porcelainLine {
	lines := map[string]porcelainLine{}
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSuffix(line, "\n")
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || len(parts[0]) != 1 {
			continue
		}
		_, dst, ok := strings.Cut(parts[1], ":")
		if !ok {
			continue
		}
		lines[dst] = porcelainLine{flag: parts[0][0], text: strings.Join(parts, " ")}
	}
	return lines
}

// stopped ends an import whose next step did not land: git's lines for the
// refused refs on stderr, and what to do.
func (imp *importer) stopped(landed int, out stepOutcome) error {
	for _, l := range out.rejected {
		fmt.Fprintln(imp.stderr, l)
	}
	why := out.err.Error()
	hint := ""
	for _, l := range out.rejected {
		if strings.Contains(l, string(gitlet.RefusalProtectedDirect)) {
			hint = "\na branch whose code.repos rule says direct_push: none takes no push, an import's included: " +
				"lift the rule for the import and restore it after"
			break
		}
	}
	return fmt.Errorf("the import stopped at step %d (%s); %s. Fix the cause and run the same command again: "+
		"it resumes%s", landed+1, why, landedBefore(landed), hint)
}

// landedBefore says how many steps landed before the one that stopped the import.
func landedBefore(n int) string {
	switch n {
	case 0:
		return "no step of this run landed before it"
	case 1:
		return "the 1 step of this run before it landed"
	}
	return fmt.Sprintf("the %d steps of this run before it landed", n)
}

// prove holds the repository to the source once the steps landed: every kept ref
// at the source's value as git lists it from the node (an annotated tag's peeled
// value with it, since a tag's id names the object it peels to). Then the summary.
func (imp *importer) prove(ctx context.Context, src importSource, done importDone) error {
	theirs, err := imp.targetRefs(ctx)
	if err != nil {
		return err
	}
	var differ []string
	count := 0
	for _, r := range src.refs {
		if at := theirs[r.ref]; at != r.oid {
			count++
			if len(differ) < importNamed {
				if at == "" {
					at = "missing"
				}
				differ = append(differ, fmt.Sprintf("%s is %s on %s and %s in the source", r.ref, at, imp.name, r.oid))
			}
		}
	}
	if count > 0 {
		return fmt.Errorf("the import pushed every step, and %s of %s still differ from the source:\n  %s",
			counted(count, "ref", "refs"), imp.name, strings.Join(differ, "\n  ")+moreThan(count, len(differ)))
	}
	cut := ""
	if done.recut > 0 {
		cut = fmt.Sprintf("; %s packed past what a step may carry at the length its estimate chose, and was cut "+
			"shorter before it was pushed", counted(done.recut, "step", "steps"))
		if done.recut > 1 {
			cut = fmt.Sprintf("; %d steps packed past what a step may carry at the lengths their estimates chose, "+
				"and were cut shorter before they were pushed", done.recut)
		}
	}
	_, err = fmt.Fprintf(imp.stdout, "imported %s: %s, %s sent in %s, %s stored with the packs' indexes%s\n",
		imp.name, counted(len(src.refs), "ref", "refs"), humanBytes(done.sent), counted(done.steps, "step", "steps"),
		humanBytes(done.stored), cut)
	return err
}

// moreThan is "and N more" beneath a list that named shown of n.
func moreThan(n, shown int) string {
	if n > shown {
		return fmt.Sprintf("\n  and %d more", n-shown)
	}
	return ""
}

// ---- reading git's output --------------------------------------------------

// errStopReading is a reader that has all it needed: git is stopped, no failure.
var errStopReading = errors.New("read all it needed")

// lineWriter hands fn each line git writes, without its newline, holding only
// the current line (an object listing is millions of lines). fn must not keep
// the slice. A line past max is not git's answer, and is refused.
type lineWriter struct {
	fn      func(line []byte) error
	max     int
	partial []byte
}

func (l *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			i = len(p)
		}
		// The bound holds for a line whole in one write or split across many.
		if len(l.partial)+i > l.max {
			return 0, fmt.Errorf("git wrote a line longer than %s", humanBytes(int64(l.max)))
		}
		if i == len(p) {
			l.partial = append(l.partial, p...)
			break
		}
		line := p[:i]
		if len(l.partial) > 0 {
			l.partial = append(l.partial, line...)
			line = l.partial
		}
		if err := l.fn(line); err != nil {
			return 0, err
		}
		l.partial = l.partial[:0]
		p = p[i+1:]
	}
	return n, nil
}

// flush hands fn a last line git ended without a newline.
func (l *lineWriter) flush() error {
	if len(l.partial) == 0 {
		return nil
	}
	line := l.partial
	l.partial = nil
	return l.fn(line)
}
