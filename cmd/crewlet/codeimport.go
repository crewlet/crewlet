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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
)

// `crewlet code import` — another host's repository, pushed into one the code
// host serves.
//
// # Client-side, and as the person
//
// The operator's git clones the source with the operator's own credentials,
// proxy and network, and pushes it to the node as the person the token is
// bound to, through the ordinary push path: every rule a push is held to — the
// limits, the protections, the blob limit, git's fsck — holds for an import
// because an import IS pushes, and the repository's push history names that
// person for each. The engine dials nothing, so there is no server-side fetch
// of a URL somebody typed to guard (a server-side import is M6's).
//
// # Steps, each MEASURED before it is pushed
//
// One push carries at most code.limits.max_push_bytes — receive.maxInputSize
// refuses a pack past it, having read it — so a larger history goes in steps:
// the default branch first, along its first-parent history, each step a
// FAST-FORWARD of the branch itself, so no temporary ref ever exists; then
// every other branch and tag in batches, each as many refs as one push's
// record holds ([importer.recordHolds]). Each step is held to the limit itself:
// what a step pushes is the pack it was measured as, byte for byte, so no
// margin below it is left for anything to fill (below, "The pack a step
// measured is the pack it pushes").
//
// A STEP IS SENT WHOLE (`git push --no-thin`), SO WHAT IT SENDS IS WHAT THE
// NODE STORES. A thin pack — git's default — holds deltas against objects the
// node already has, and the node's receive-pack completes it (index-pack
// --fix-thin) by appending every such base WHOLE: the stored pack is larger
// than the one sent, by an amount the client never sees. The node holds what
// it STORES to two limits — a push's bundle (the pack, its .idx and its .rev)
// to code.MaxPackBytes, refused pack_too_large, and the repository's packs to
// code.limits.repo_max_bytes, refused repo_size — so a plan that measured the
// thin pack could not keep either: measured on this repository's own history
// cut into eleven steps, the thin packs sent 40.1 MB and the node stored
// 74.4 MB. Sent whole, a step's pack is the stored pack byte for byte —
// index-pack has nothing to append — and its .idx and .rev follow from its
// object count, so the import knows what each step stores before it is sent
// ([importStep.stored]). It is also SMALLER stored: a whole pack keeps the
// object a delta could not reach whole, where --fix-thin appends that delta's
// base again though an earlier step already stored it — the same eleven steps
// whole store 68.5 MB. The price is twofold. The bytes the node would have
// appended are uploaded instead (68.5 MB sent against 40.1 MB), and the node
// no longer spends its own cores compressing them. And a step stores at most
// what it may send — the limit — where a thin step sent under the limit
// could store up to code.MaxPackBytes: whole steps are smaller and more of
// them, fewest with code.limits.max_push_bytes at its ceiling, which an import
// of a large history should run under. A JUDGEMENT for whole steps: exact
// rather than approximate accounting — a thin step's completion appends
// objects compressed by the node's zlib, which the operator's need not be —
// and no base stored twice, against the steps a thin plan saves between the
// limit and code.MaxPackBytes, a quarter of a gibibyte at the ceiling.
//
// A STEPPED HISTORY STORES MORE THAN ONE PACK OF IT, whatever the steps send:
// every step keeps whole what its deltas cannot reach in the steps before it,
// so the files a step touches are stored again in full at each step. This
// repository's history, 34.0 MB as one pack, stores 68.5 MB in eleven steps
// and 78.6 MB in forty-two — so the import takes the largest steps the limit
// allows, and holds the repository's stored total to repo_max_bytes as it
// goes ([importPlanner.hold]).
//
// A STEP'S SIZE IS MEASURED, NEVER ESTIMATED. `git rev-list --disk-usage`
// sizes a range by the bytes its objects take in the source's own pack, which
// is a different number from the pack a push of the range sends. A history a
// forge has packed stores the newest version of a file whole and each older
// version as a delta against the newer one — deltas the push of an older
// range cannot use, since their bases are not sent yet — so the steps of such
// a history pack to MORE than their estimate. Measured on this repository's
// own history, packed by `git gc --aggressive` and cloned as a forge serves
// it: its first twenty commits estimate at 9.0 MB and pack to 20.2 MB, 2.24
// times as much — so a plan cut on estimates, with a tenth of headroom, has
// its first push refused.
// The estimate is a traversal, a hundred times cheaper than building a pack
// (0.03 s against 2.4 s for one 12 MB range), so it CHOOSES where a step
// ends; then the step's pack is BUILT — pack-objects --revs, against the refs
// the node will hold, under the operator's own pack settings, its threads
// among them — into a repository of the step's own ([importer.stepRepo]); and
// a step past the limit is cut shorter in proportion and built again
// ([importPlanner]). Every push is PINNED to the pack it was measured as,
// whatever the operator's push configuration says ([importPushConfig],
// [importSendConfig]).
//
// # The pack a step measured is the pack it pushes
//
// git push builds the pack it sends, and a pack built twice is not the same
// pack: pack-objects' delta search splits its work among threads that take
// work from each other as each finishes, and a file's versions split across
// two threads cost one more of them stored whole. Built ten times at four
// threads, one range of a history packed as a forge packs one — sixteen files
// of 40 to 55 KiB — came to 895 KiB five times and to 1003–1057 KiB the
// other five, four of them past 1 MiB (git 2.56, two cores, measured): a step
// measured under a code.limits.max_push_bytes of 1 MiB was pushed past it,
// and the node's index-pack refused it having read it all, `[remote rejected]
// (unpacker error)` for its `pack exceeds maximum allowed size`. No margin
// covers that — one split costs a whole version of the largest file the step
// touches — and the tenth below the limit this held each step to did not.
//
// So a step's pack is built ONCE, and what the import measures and what its
// push sends are two COPIES of that build: pack-objects again, from the
// step's repository, with no delta search (pack.window=0), which takes every
// object as the build stored it and has nothing left to choose, on any
// number of threads — the same pack, byte for byte, whichever process copies
// it, the push's own included, so long as each is handed the step's tips in
// the order git push hands them ([importStep.news]; importSendConfig says how
// the copy is kept to the step's pack). The step is pushed from its
// repository, which is then removed. The copy is the build's objects in
// another order, so it is the copy that is measured — the order moves each
// delta's offset, written in one to five bytes ([importOrderSlack]).
//
// Each step's delta search runs once, where the push used to run a second:
// the copies are a pass over bytes already compressed, and indexing the
// build — what lets a copy find its objects — resolves its deltas without
// searching for any, and reads nothing of the clone ([importer.borrow]). The
// price is room: the import's working directory holds a step's pack until it
// is pushed, beside the clone.
//
// THE NEXT STEP IS BUILT WHILE THIS ONE IS PUSHED, in a repository of its own:
// a push's upload, the node's indexing of it and its commit leave this
// machine's cores idle while the next build uses them — and a push that fails
// ends the planning at once ([importer.execute]). So the working directory
// holds at most two steps' packs at a time: the one being pushed, at most the
// limit, and the next one's build, at most twice it ([importMeasureCap]).
//
// A MERGE THAT BRINGS IN MORE THAN A STEP is stepped from the other side: a
// first-parent walk meets the whole history a merge's other parent brings in
// as that one commit, so the walk pauses there and steps that parent's own
// history first, on a branch of the source that reaches it
// ([importPlanner.bringIn]).
//
// # Resuming
//
// A failed step is safe to run again: each is `--atomic`, so it lands whole or
// not at all, and the same command reads what the repository already holds and
// pushes only the rest. A branch the node holds at an earlier commit of the
// source's history is a step that already landed, continued from there; a ref
// the node holds at a value the source's history does not reach is not a
// resumed import of the same history, and is refused.

// codeImportUsage is the import's synopsis.
const codeImportUsage = "usage: crewlet code import <source-url> <name> [<config.yaml>] [-url URL] " +
	"[-git PATH] [-dry-run]"

// importMeasureCap is how far a step's build goes before it stops: twice the
// limit. Past it the step is cut to under half its length whatever the rest
// of its pack holds, so building the rest would be thrown away — and a range
// whose objects are stored as small deltas of large files can pack to many
// times its estimate.
func importMeasureCap(limit int64) int64 { return 2 * limit }

// importOrderSlack is the most a step's copy ([importer.sent]) can differ from
// its build in bytes: four for each of its objects. The copy writes the
// build's objects — each its type, its size and its data exactly as the build
// stored them — in an order of its own, which moves only the offset of each
// delta's base, an OFS_DELTA's varint of seven bits a byte: one byte at the
// least, and five for any offset under 32 GiB, which every pack a push may
// send is under. A build further than that past the limit cannot fit as its
// copy either, so it is cut on its own size, never indexed or copied.
func importOrderSlack(objects int64) int64 { return 4 * objects }

// importCutShare is how far below its proportional point a step past the
// limit is cut: to fifteen sixteenths of the estimate at which its build says
// the limit falls, so a range that packs a little worse than the one that was
// built still fits at the first rebuild.
const importCutShare = 15.0 / 16.0

// importRatioFloor is the smallest step whose measurement is carried into the
// next step's plan: an eighth of the limit. A smaller pack is mostly what
// every pack carries whatever its objects — its header, its trailer, each
// object's entry header — so how its bytes compare with its estimate says
// nothing about how the history packs.
func importRatioFloor(budget int64) int64 { return budget / 8 }

// importNamed is how many items one list of an import's sentences names; the
// rest are counted. A forge's repository can hold a hundred thousand refs, and
// a refusal that printed every one would bury the remedy.
const importNamed = 20

// maxImportLine bounds one line of git's output as the import reads it: an
// object id and a path, a ref and its value — 1 MiB is far past any of them,
// so a longer line is not git's answer.
const maxImportLine = 1 << 20

// oidHexLen is an object id's length in hex. The source is held to SHA-1
// before anything is read by length ([importer.inspect]): the code host stores
// SHA-1 repositories.
const oidHexLen = 40

// codeImport is `crewlet code import`.
func codeImport(environ config.Source, args []string, stdout, stderr io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	imp, dryRun, err := prepareImport(ctx, environ, args, stdout, stderr)
	if err != nil {
		return err
	}
	return imp.run(ctx, dryRun)
}

// prepareImport reads the command line, the repository the import fills and
// the operator's git, and answers the run they make and whether it is a dry
// one.
func prepareImport(ctx context.Context, environ config.Source, args []string, stdout, stderr io.Writer) (
	*importer, bool, error) {

	// THE TWO SUBJECTS LEAD, before any flag (`work purge`'s idiom): the
	// optional config document a node client takes after them is a third
	// positional, and only their order tells the three apart.
	source, rest := splitSubject(args)
	name, rest := splitSubject(rest)
	if source == "" || name == "" {
		fmt.Fprintln(stderr, codeImportUsage)
		return nil, false, errors.New("name the repository to import and the code host's repository to import " +
			"it into, before any flag")
	}
	var (
		gitPath *string
		dryRun  *bool
	)
	client, err := nodeClientFor(environ, rest, "code import", stderr, func(fs *flag.FlagSet) {
		gitPath = fs.String("git", "", "the git binary to run; empty is the first on PATH")
		dryRun = fs.Bool("dry-run", false, "plan the import and push nothing")
	})
	if err != nil {
		return nil, false, err
	}
	repo, err := codeImportTarget(ctx, client, name)
	if err != nil {
		return nil, false, err
	}
	principal, err := codeImportPrincipal(ctx, client)
	if err != nil {
		return nil, false, err
	}
	g, err := newOperatorGit(*gitPath, client, os.Environ())
	if err != nil {
		return nil, false, err
	}
	if err = g.atClientFloor(ctx); err != nil {
		return nil, false, err
	}
	imp := &importer{g: g, client: client, source: source, name: name, repo: repo, principal: principal,
		budget: repo.Limits.MaxPushBytes, maxStored: code.MaxPackBytes,
		stdout: stdout, stderr: stderr}
	if isTerminal(stderr) {
		imp.live = stderr
	}
	return imp, *dryRun, nil
}

// codeImportTarget is the repository an import pushes into, read at the log's
// head — never "not declared" concluded from an old copy — with the limits of
// the node that will hold its pushes to them, and what its packs hold now.
func codeImportTarget(ctx context.Context, client *nodeClient, name string) (codeRepoView, error) {
	var answer codeReadView
	if err := client.forCodeReads().get(ctx, "/code/repos/"+url.PathEscape(name)+"?read_level=linearizable",
		&answer); err != nil {
		// THE REMEDY ONLY WHERE IT IS ONE: a repository nobody declared is
		// filled once code.repos declares it; a company that hosts no code
		// (not_hosted) has nothing declaring one changes, and is answered
		// in the node's words alone.
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

// codeImportPrincipal is who the node records the import's pushes as — the
// token's id and the seat bound to it, asked of the node (GET /viewer, the one
// question every personal surface asks) rather than assumed. Every push's
// record carries it beside the push's ref updates, so it is part of what one
// push's record holds ([importer.recordHolds]); neither a token's id nor a
// handle has a length bound of its own, so nothing short of asking says how
// much of the record it takes.
func codeImportPrincipal(ctx context.Context, client *nodeClient) (code.Principal, error) {
	var viewer struct {
		OperatorID string `json:"operator_id"`
		Handle     string `json:"handle"`
	}
	if err := client.get(ctx, "/viewer", &viewer); err != nil {
		return code.Principal{}, fmt.Errorf("ask the node who this token is: %w", err)
	}
	return code.Principal{Kind: code.PrincipalOperator, OperatorID: viewer.OperatorID, Seat: viewer.Handle}, nil
}

// importer is one run of `crewlet code import`.
type importer struct {
	g      operatorGit
	client *nodeClient
	source string // the source's URL, as the operator gave it
	name   string // the repository on the node
	repo   codeRepoView
	// principal is who the node records each step's push as
	// ([codeImportPrincipal]).
	principal code.Principal
	// budget is the most one step's pack carries: code.limits.max_push_bytes,
	// past which the node's receive.maxInputSize refuses a push.
	budget int64
	// maxStored is the most one push may STORE — its pack, .idx and .rev
	// together: code.MaxPackBytes, past which the node refuses the push
	// pack_too_large.
	maxStored int64
	// work is the import's working directory, and clone the source's bare
	// clone in it; each step's own repository is made there too
	// ([importer.stepRepo]).
	work, clone string
	// live is stderr where a person is watching it, for git's progress and
	// the remote's messages as they come; nil when nobody is.
	live           io.Writer
	stdout, stderr io.Writer
}

// sourceRef is one ref of the source's clone: its name, its value and what
// that value is (commit, tag, tree or blob).
type sourceRef struct {
	ref, oid, kind string
}

// isBranch reports a ref under refs/heads/ whose value is a commit: one that
// can be advanced along its first-parent history.
func (r sourceRef) isBranch() bool {
	return strings.HasPrefix(r.ref, "refs/heads/") && r.kind == "commit"
}

// run is the import, from the source's advertisement to the proof.
func (imp *importer) run(ctx context.Context, dryRun bool) error {
	// 1 WHAT CAN BE REFUSED BEFORE ANYTHING IS DOWNLOADED: the source's own
	// advertisement, and whether this token may push at all.
	if err := imp.listSource(ctx); err != nil {
		return err
	}
	// The import's working directory is the system's temporary one
	// ($TMPDIR), which needs room for the source and for two steps' packs
	// at once (the package doc's "The pack a step measured is the pack it
	// pushes").
	tmp, err := os.MkdirTemp("", "crewlet-import-")
	if err != nil {
		return fmt.Errorf("make the import's working directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	imp.work = tmp
	if err = imp.checkPushAccess(ctx, filepath.Join(tmp, "probe.git")); err != nil {
		return err
	}

	// 2 THE CLONE, with the operator's environment untouched: their own
	// credential helpers are what reach the source, and where none holds a
	// credential, the person at the terminal answers git's prompt.
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

// importKindOf is what an import does with a ref of the source: a ref the
// code host takes a push of is KEPT; one outside refs/heads/ and refs/tags/ is
// SKIPPED — a forge's pull and merge-request refs, its notes and remotes —
// since the domain refuses it as reserved_ref; one inside them that is no ref
// at all is BAD, the domain's bad_ref; and a branch the domain reserves for
// its seats — refs/heads/seats, or refs/heads/seats/<name> as a branch itself
// (code.SeatNamespaceRoot) — is RESERVED, refused like a bad one but named for
// what it is, since check-ref-format has nothing against it. Asked of the
// domain's own rule (code.StatelessRefusal), so the import keeps exactly what
// a push may carry.
func importKindOf(ref string) importKind {
	refusal := code.StatelessRefusal(code.Command{Ref: ref, Old: code.ZeroOID, New: strings.Repeat("1", oidHexLen)})
	switch {
	case refusal == "":
		return importKept
	case !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/"):
		return importSkipped
	case refusal == code.RefusalReservedRef:
		return importReserved
	default:
		return importBad
	}
}

// importKind is the closed set [importKindOf] answers.
type importKind int

const (
	importKept importKind = iota
	importSkipped
	importBad
	importReserved
)

// importNotHeld is the refs of the source the code host takes no push of: the
// ones git's rules or the engine's bound refuse (bad), and the branches the
// engine keeps for its seats (reserved) — each counted, the first few named.
type importNotHeld struct {
	bad, reserved           []string
	badCount, reservedCount int
}

// add counts ref under its kind; any other kind is not one of the refused.
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

// findings is the refusal's sentence for each kind of ref counted.
func (n *importNotHeld) findings() []string {
	var out []string
	if n.badCount > 0 {
		out = append(out, fmt.Sprintf("%s the code host takes no push of (git's check-ref-format "+
			"rules, at most %d bytes of UTF-8): %s — rename them in the source", counted(n.badCount, "ref", "refs"),
			code.MaxRefNameBytes, listed(n.bad, n.badCount)))
	}
	if n.reservedCount > 0 {
		out = append(out, fmt.Sprintf("%s the code host keeps for its seats' own branches, which live under "+
			"%s<handle>/ — %s, and %s<name> as a branch itself, would block them, since no ref is created "+
			"beside a directory of its name: %s — rename them in the source",
			counted(n.reservedCount, "branch", "branches"), code.SeatNamespaceRoot,
			strings.TrimSuffix(code.SeatNamespaceRoot, "/"), code.SeatNamespaceRoot,
			listed(n.reserved, n.reservedCount)))
	}
	return out
}

// namespaceOf is a skipped ref's namespace as the import names it:
// refs/pull/1/head is refs/pull/*.
func namespaceOf(ref string) string {
	parts := strings.SplitN(ref, "/", 3)
	if len(parts) < 3 {
		return ref
	}
	return parts[0] + "/" + parts[1] + "/*"
}

// listSource reads the source's advertisement — every ref it serves — BEFORE
// anything is downloaded, so a source the import would refuse for its refs is
// refused in seconds rather than after its whole history arrived, and the
// refs it skips are named although the clone never fetches them: a bare clone
// takes the branches and the tags, and a forge's pull refs are history the
// import would only throw away.
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
	if kept > code.MaxRefsPerRepo {
		findings = append(findings, fmt.Sprintf("the source holds %d branches and tags, past the %d one repository "+
			"holds", kept, code.MaxRefsPerRepo))
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
// run: a dry run sends nothing, so whatever the repository holds there is
// left as it is.
const importProbeRef = "refs/heads/crewlet-import-probe"

// importProbeWait is how long the push-access probe waits for the node's
// answer before going on without it: the node client's own bound on a call
// (nodeRequestTimeout), the time in which a node that refuses a push refuses
// it, since it refuses before it reads anything.
const importProbeWait = nodeRequestTimeout

// checkPushAccess asks the node, before the source is cloned, what a push's
// first request asks — whether this token may push to the repository — so a
// token that may not (one bound to nobody, or every caller where
// api.auth.disabled is set) is refused in seconds, in the node's own words,
// rather than at the first step after the whole source has been downloaded.
//
// ASKED THROUGH GIT, as every step is: a `git push --dry-run` from an empty
// repository (probe) of a deletion, which makes the push's first request —
// the node's advertisement of what it would take, or the refusal git prints
// as `remote error: …` — and sends nothing. Through the same credential as
// the steps, so the answer is the one they will get; and through git rather
// than an HTTP request of this command's own, because which requests are
// pushes is the API's one classifier's to say (auth.GitAccess) and git's to
// spell.
//
// BEST EFFORT IN TIME: the node answers an advertisement once its copy of the
// repository is current, which for a large repository on a node holding no
// copy yet is a sync of every pack, longer than the probe waits
// (importProbeWait). An answer that does not come in time is no refusal: the
// first step's push asks again and waits as git waits, and the sync the probe
// began has warmed the copy it is answered from.
//
// ANY OTHER FAILURE STOPS THE IMPORT, QUOTED AS GIT SAID IT — and never read
// as the token's. The node's own refusal reaches git as `remote error:` with
// its sentence (a token bound to nobody, an archived repository); a draining
// node's 503, a node serving no git routes, a TLS or proxy failure on git's
// side are git's own lines. Every one of them would refuse the first step
// too, after the whole source had been downloaded, and git's words already
// say which it is, in the operator's language: telling them apart here would
// mean reading git's messages, which git translates.
func (imp *importer) checkPushAccess(ctx context.Context, probe string) error {
	if _, err := imp.g.run(ctx, imp.g.environ, "", "init", "--bare", "--quiet", probe); err != nil {
		return err
	}
	asking, cancel := context.WithTimeout(ctx, importProbeWait)
	defer cancel()
	out, err := imp.g.call(asking, gitCall{env: imp.g.targetEnv(), gitDir: probe, config: importPushConfig},
		append(slices.Clone(importPushFlags), "--dry-run", imp.g.repoURL(imp.name), ":"+importProbeRef)...)
	if _, answered := porcelainLines(out)[importProbeRef]; answered {
		// git read the node's advertisement and planned the push against
		// it: the node takes this token's pushes here.
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
		"a push to %s — a dry run of one, as every step is pushed — and git answered:\n%w", imp.client.base,
		imp.name, err)
}

// importPushConfig and importPushFlags PIN every push of an import — each
// step and the probe — to the push its plan measured, whatever the
// operator's own push configuration says. Each one names a setting that
// changes what a push carries, or whether git will make it at all:
//
//   - push.pushOption, cleared (an empty value empties the list): the code
//     host reads a change from a push's options, so a `change` the operator
//     keeps there for their own pushes would open a change from every step
//     that pushes one branch alone — an import's steps are history, never a
//     proposal — and a forge's (ci.skip) means nothing here;
//   - push.negotiate=false: a negotiation is a fetch from the node before
//     each push, which can only shrink the pack against the measurement and
//     costs the node an upload-pack a step;
//   - --no-signed: push.gpgSign asks for a push certificate the code host does
//     not advertise, which git refuses;
//   - --no-recurse-submodules: push.recurseSubmodules looks for each pushed
//     gitlink's commit on a submodule's remote, which a bare clone has none
//     of;
//   - --no-thin: the pack the node stores is the pack sent (the package doc);
//   - --porcelain: the lines a step's outcome is read from ([importer.push]);
//     --no-verify: the operator's pre-push hooks are for their own pushes, and
//     one that uploads elsewhere (git-lfs's) would fail every step.
//
// A step's push also runs under [importSendConfig], which makes the pack it
// sends a copy of the pack its step was built as. The operator's own pack.*
// settings are deliberately NOT pinned beyond that: they shape the one build
// of a step's pack — its delta search's window, depth, threads and memory —
// and every copy of it after is the same pack whatever they are.
//
// push.followTags needs no pin: it adds the annotated tags the PUSHING
// repository's refs hold at a commit the push carries — refs the plan never
// batched, and 1 001 tags on one commit took a step past the 1 000 commands a
// push may carry, refused on every run — and a step is pushed from a
// repository of its own, which holds no ref at all ([importer.stepRepo]), as
// the probe's does.
var (
	importPushConfig = []string{"push.pushOption=", "push.negotiate=false"}
	importPushFlags  = []string{"push", "--porcelain", "--no-verify", "--no-thin", "--no-signed",
		"--no-recurse-submodules"}
)

// importSendConfig is what every COPY of a step's pack runs under — the one
// its measurement counts ([importer.sent]) and the one its push sends:
//
//   - pack.window=0: NO DELTA SEARCH, so pack-objects takes every object as
//     the step's build stored it and has nothing left to choose, on any number
//     of threads — which is what makes the copy the measurement counts and
//     the push's own the same pack, byte for byte (the package doc's "The
//     pack a step measured is the pack it pushes" says why a second search
//     is not);
//   - core.multiPackIndex=false, pack.useBitmaps=false and
//     pack.allowPackReuse=false: the copy takes each object from the step's
//     own pack, the one its search built, rather than from the clone the
//     step's repository borrows from — whose multi-pack-index, read before any
//     pack, and whose bitmapped pack, walked or copied whole, would each hand
//     pack-objects the clone's copy of an object first. Each of those is a
//     copy no less exact, but of an object a forge stored as a delta against
//     one the step does not send, so stored whole: the pack the step's search
//     shrank is the one sent.
//
// On the command line, so they outrank whatever the operator's configuration
// or environment sets.
var importSendConfig = []string{"pack.window=0", "core.multiPackIndex=false", "pack.useBitmaps=false",
	"pack.allowPackReuse=false"}

// ---- 3: the source's clone -------------------------------------------------

// importSource is the source's clone as the import plans from it.
type importSource struct {
	// refs are the kept refs, in for-each-ref order — refname order, which
	// is the order every batch takes them in.
	refs []sourceRef
	// head is the source's HEAD branch, refs/heads/…, or "" where its HEAD
	// names none.
	head string
}

// inspect holds the clone to every rule a refusal before the first push
// needs — its history whole, SHA-1, every ref one a push may carry, no more of
// them than a repository holds, no blob past the limit, no more bytes than a
// repository holds — and refuses with every finding at once, so an operator
// fixes the source once rather than once a run.
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
		// A clone of a shallow repository is shallow too, and its history
		// stops at commits whose parents it lacks — which receive-pack
		// refuses to be pushed (receive.shallowUpdate).
		findings = append(findings, "the source is a shallow clone, whose history stops part way: import from a "+
			"full clone of it")
	}
	if answers[1] != "sha1" {
		// Nothing past here may read an id by its length.
		return importSource{}, refuseImport([]string{fmt.Sprintf("the source's objects are %s, and the code host "+
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
			// A bare clone fetches heads and tags alone; the
			// advertisement already said what is skipped.
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
	if n := len(src.refs); n > code.MaxRefsPerRepo {
		findings = append(findings, fmt.Sprintf("the source holds %d branches and tags, past the %d one repository "+
			"holds", n, code.MaxRefsPerRepo))
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
	// THE SOURCE'S OWN PACKING, against what the repository may hold: one
	// pack of the whole history, built by the pack-objects every step runs —
	// the clone is the source's upload-pack's — with every delta the history
	// offers, where a step may only delta against what it carries itself. So
	// the steps store at least about this much, and in practice more (the
	// package doc), and a source already past the limit here is refused before
	// anything is pushed. What the repository holds already is not added: on a
	// resumed import it is these objects, stored by the steps that landed. Not
	// a bound the steps are held to: that is [importPlanner.hold], step by
	// step, on what each stores.
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

// oversizedBlobs is a finding for every blob of the clone past
// code.limits.max_blob_bytes, each named by its id, its first path and its
// size: one `cat-file --batch-all-objects` streamed a line at a time — a large
// history is millions of lines, never held — and, only where it found one,
// one `rev-list --objects --all` streamed until every blob it names has a
// path.
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
	// --unordered reads the packs in their own order, the fast one for every
	// object of a repository.
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
			"binaries belong in tracker files", b.oid, path, humanBytes(b.size), humanBytes(limit)))
	}
	if more := over - len(found); more > 0 {
		findings = append(findings, fmt.Sprintf("and %s past code.limits.max_blob_bytes",
			counted(more, "more blob", "more blobs")))
	}
	return findings, nil
}

// sizePack is `git count-objects -v`'s size-pack, in bytes: the bytes the
// clone's packs hold, which git reports in KiB.
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

// refuseImport is the one refusal of an import that pushed nothing, every
// finding beneath it.
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

// remoteRefs is the repository's refs as they will be once every step planned
// so far has landed — what git push's own pack-objects is told the node holds
// (send-pack's negatives: every remote ref whose object the clone holds), and
// so what every estimate and every measurement is made against, which is what
// makes a measurement the size of the push.
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

// negatives is `^<oid>` for every distinct value the repository's refs will
// have that the clone holds — one line each, sorted, so two runs over one
// state ask git one question — built once a state.
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

// revs is news, then every negative: what rev-list --stdin and
// pack-objects --revs read.
func (r *remoteRefs) revs(news []string) []byte {
	neg := r.negatives()
	b := make([]byte, 0, len(news)*(oidHexLen+1)+len(neg))
	for _, oid := range news {
		b = append(b, oid...)
		b = append(b, '\n')
	}
	return append(b, neg...)
}

// queuedRef is a ref still to push. walk marks the default branch, which is
// advanced along its first-parent history in steps of its own rather than
// batched.
type queuedRef struct {
	sourceRef
	walk bool
}

// resume compares the repository's refs with the source's and answers what is
// left to push — the default branch first, then the rest in the source's
// order — and the repository's refs as the plan starts from them; it refuses
// a repository that does not hold an earlier state of the same history.
//
// A RESUMED IMPORT, NEVER AN EQUAL ONE ALONE: a ref the repository holds at
// the source's value is done, and a BRANCH it holds at a commit the source's
// branch descends from is a step that landed before a later one failed — the
// first-parent steps leave the default branch exactly there — so it is
// continued. A tag is never at an earlier value, since a tag does not move,
// and a value the source's history does not hold at all is somebody else's
// push: either is not this import, and is refused naming the first.
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
			fmt.Fprintf(imp.stdout, "the source's default branch is %s; code.repos says %s — importing %s as %s; "+
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
	if n := len(src.refs) + len(theirsOnly); n > code.MaxRefsPerRepo {
		return nil, nil, refuseImport([]string{fmt.Sprintf("%s would hold %d refs — the source's %d and %d of its "+
			"own — past the %d one repository holds", imp.name, n, len(src.refs), len(theirsOnly),
			code.MaxRefsPerRepo)})
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
// earlier state of the source's: held reports whether the clone holds that
// value at all.
func diverged(name string, r sourceRef, at string, held bool) error {
	why := "this is not a resumed import of the same history"
	switch {
	case !held:
		why = "the source does not hold that commit, so something else was pushed there: " + why
	case strings.HasPrefix(r.ref, "refs/tags/"):
		why = "a tag never moves: " + why
	}
	return fmt.Errorf("nothing was imported: %s is at %s on %s and %s in the source — %s", r.ref, at, name, r.oid,
		why)
}

// targetRefs is every ref the repository holds (ref → value), read through
// the node's git routes as a person's git reads them.
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

// held is which of oids the clone holds: one `cat-file --batch-check` for all
// of them.
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

// descendants reports, for each kept ref of the source that the repository
// holds at an earlier commit the clone has, whether the source's value
// descends from it: a branch an import continues, rather than one something
// else was pushed to.
//
// ASKED BOUNDED IN PARALLEL: git has no form that asks merge-base
// --is-ancestor of many pairs at once, and an import run again to bring over
// what a source still in use has gained finds every branch somebody pushed to
// at an earlier value — up to code.MaxRefsPerRepo of them, which one process
// after another at 5–10 ms each made most of a minute before the first
// estimate. Each question reads the clone alone, so they are independent:
// GOMAXPROCS at a time, the cores this command may use. A ref found not to
// descend stops every question about a ref AFTER it in the source's order —
// the refusal names the first — while those before it are still answered,
// since one of them may be the first; and nothing is left running on return.
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
	// estimate is what the source's own packs hold of its objects; size is
	// the pack's measured bytes and objects the count its header carries —
	// or, in a dry run, which measures nothing, the estimate again and no
	// count.
	estimate, size, objects int64
	exact                   bool
	// alone is a step no smaller step can carry — one commit, one ref — so
	// past the limit it is refused, or has the history a merge brings in
	// stepped first, where any other step is cut shorter.
	alone bool
	// past is a measurement that stopped at importMeasureCap: size is then a
	// floor.
	past bool
	// repo is the step's own repository, holding the pack it was built as
	// ([importer.stepRepo]) — set once it is measured, and kept only while it
	// fits, until its push or until the plan drops it ([importStep.drop]).
	repo string
	// overRepo marks a dry run's step whose estimate takes the repository's
	// packs past code.limits.repo_max_bytes, which a real run measures and,
	// measured past it, refuses.
	overRepo bool
	// note is why the step is taken where it is, where the plan took it out
	// of its turn ([importPlanner.bringIn]).
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

// stored is what the node keeps for the step: the pack it is sent — whole
// (--no-thin), so receive-pack's index-pack appends nothing to it — then the
// .idx and the .rev receive-pack writes beside it, whose sizes follow from
// the pack's object count (code.IndexBytesFor, code.ReverseBytesFor). That
// bundle is what the node holds to code.MaxPackBytes and adds to the
// repository's bytes. In a dry run, the estimate.
func (s *importStep) stored() int64 {
	if !s.exact {
		return s.size
	}
	return s.size + code.IndexBytesFor(s.objects) + code.ReverseBytesFor(s.objects)
}

// news is the values the step moves its refs to, as pack-objects --revs reads
// them, IN THE ORDER GIT PUSH HANDS THEM TO ITS OWN: the refs remote holds
// first, by name, then the refs the step creates, in the step's order.
// send-pack feeds its pack-objects the new value of each ref it updates as it
// walks the refs the node advertised — in the node's order, which is by name —
// and then of each ref it creates, in the order the push names them.
//
// The order is the copy's to match, since the copy is the pack the push sends
// only when it is handed the same revisions in the same order ([importer.sent]):
// a copy writes the objects in the order its walk meets them, and the walk
// meets two tips of one commit time in the order it was handed them. A step
// that updated b, which the node held, and created a, which names before it,
// at two commits of one second, copied in the step's own order and in the
// push's, made two packs of one size and two checksums (git 2.53, measured);
// and where the order moves a delta further from its base, the size moves too.
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

// drop removes the step's repository, and its pack with it: once the step is
// pushed, or once the plan will not push it — cut shorter, refused, or left
// unpushed by an import that stopped. The import's working directory goes
// with the import whatever is left in it; this is what keeps a long import
// from holding every step it ever built until then.
func (s *importStep) drop() {
	if s.repo != "" {
		_ = os.RemoveAll(s.repo)
		s.repo = ""
	}
}

// oidList is a list of object ids held as one run of bytes, oidHexLen a
// piece: a first-parent history can be a million commits, and a string each
// would hold three times the bytes.
type oidList []byte

func (l oidList) len() int        { return len(l) / oidHexLen }
func (l oidList) at(i int) string { return string(l.raw(i)) }
func (l oidList) raw(i int) []byte {
	return l[i*oidHexLen : (i+1)*oidHexLen]
}

// firstParentWalk is a branch advanced along a first-parent history — its
// own, or the history a merge brings in ([importPlanner.bringIn]): list is
// every commit of it the repository will not hold, oldest first, and at the
// first not yet planned.
type firstParentWalk struct {
	ref  string
	list oidList
	at   int
	// carrier is the queued branch a merge's history is stepped on, taken
	// out of the queue at slot: once the walk is done it goes back there,
	// to be pushed on in its turn from where the walk left it — unless the
	// walk took it to its own value. Nil for a walk of a branch's own
	// history, which takes it all the way.
	carrier *queuedRef
	slot    int
}

// importPlanner cuts the refs still to push into steps, ONE AT A TIME, each
// planned against the refs the repository will hold once every step before it
// lands (remote), so the next step can be planned while this one is pushed.
//
// HOW A STEP IS CUT: by estimate, as far as the estimate fits the limit —
// scaled by ratio, how much larger than their estimates the last steps packed
// (never below one: a step is never planned past the limit on the strength
// of an estimate that overstated an earlier one) — found by a search that
// starts from the last step's length (furthest), because a history packs much
// alike from one step to the next. Then, unless this is a dry run, MEASURED:
// a step whose pack is past the limit, or whose stored bundle is past what
// one push may store, is cut back to where its own measurement says it fits,
// and measured again, until it fits or is one commit or one ref, which is
// refused past the limit — or, for a merge, has the history it brings in
// stepped first. A dry run, which measures nothing, takes that last turn
// where a merge's estimate is past the limit, so it plans what the import
// would push.
type importPlanner struct {
	imp    *importer
	remote *remoteRefs
	budget int64 // what a step's pack is held to: code.limits.max_push_bytes
	exact  bool  // measure each step (a dry run plans from estimates alone)
	ratio  float64
	queue  []queuedRef
	walk   *firstParentWalk
	// suspended are the walks a merge paused, the innermost last: each goes
	// on from its merge once the history stepped for it is done
	// ([importPlanner.bringIn]).
	suspended []*firstParentWalk
	// note is said of the next step taken ([importStep.note]).
	note string
	// held is what the repository's packs will hold once every step planned
	// so far lands: its live bytes when the import read it, and each planned
	// step's stored bytes after ([importPlanner.hold]).
	held int64
	// lastWalk and lastBatch are the commits the last first-parent step took
	// and the refs the last batch took: where the next search starts.
	lastWalk, lastBatch int
	// recut counts the steps whose measurement cut them shorter, for the
	// summary.
	recut int
}

// next is the next step, its refs recorded in the plan's remote state as if
// it had landed — or nil when nothing is left.
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
			// The branch a merge's history was stepped on goes back
			// to its turn, to go on from there. The walks are a
			// stack and the queue's head is taken only when none is
			// left, so slot is where it stood: every carrier taken
			// after it has been put back.
			p.queue = slices.Insert(p.queue, min(w.slot, len(p.queue)), *w.carrier)
		}
		p.walk = nil
		if n := len(p.suspended); n > 0 {
			// The history a merge waited for is in: the walk the
			// merge paused goes on from the merge.
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
				// Every commit of it is one the repository will hold
				// already: the ref alone moves, in a batch — whose pack
				// is a header with no object in it, so no batch sends it
				// back here.
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
// A BRANCH THE REPOSITORY HOLDS ALREADY is advanced only through the commits
// that DESCEND from the value it holds, since every step is a fast-forward of
// it. Where that value reached tip through a merge's second parent — a branch
// merged in, a source that moved on with a merge since the last run — the
// first-parent history past what the repository holds BEGINS with commits
// that do not descend from it, and a step to one of them was refused as
// non-fast-forward on every run. The first descendant's step carries them
// instead ([importer.firstDescendant]). The value it holds must be an
// ancestor of tip, which [importer.resume] holds of the source's own value
// and [importPlanner.carrier] of a merge's parent.
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

// firstDescendant is the index of the first commit of list — a first-parent
// history to tip, oldest first — that descends from at, which the repository
// holds the branch at. Every commit after it descends from at too, since each
// has the one before it as its first parent, so the commits the branch can be
// fast-forwarded through are list's suffix from there.
//
// ONE TRAVERSAL: rev-list --ancestry-path=<at> (git 2.38) streams every commit
// of the range to tip that descends from at, each looked up among list's
// commits, sorted once — never with --first-parent beside it, which misses a
// descendant whose path from at runs through a second parent further down
// (measured: at a commit one below the tip of a merged branch, it listed
// none).
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
// commit of its first-parent history the step can carry.
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
			// A DRY RUN PLANS THE BRING-IN THE IMPORT WOULD MAKE: it
			// measures nothing, so a merge whose estimate is past the
			// limit has the history it brings in stepped first here
			// too, and only one that nothing can carry is planned as
			// it is — marked past the limit by its estimate.
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
		// Cut back to where the measurement says the step fits: the
		// furthest commit before this one whose estimate is under it.
		if end, err = furthest(w.at, end-1, w.at+(end-w.at)/2, under(estimate, cut)); err != nil {
			return nil, err
		}
	}
}

// bringIn steps the history a merge brings in BEFORE the merge, where the
// merge alone is past what a step may carry. A first-parent walk meets a
// whole other line of history — a long-lived branch merged back, a `git merge
// --allow-unrelated-histories` or a `git subtree add` of another repository —
// as the ONE commit that merges it, which no step of the walk can split, and
// the commit itself is usually small: its size is that history's. So the
// planner steps THAT HISTORY — the first-parent history of a parent the
// repository will not hold, to that parent and no further — on a branch of
// the source still to be pushed that reaches it ([importPlanner.carrier]),
// taken out of its turn: the walk waits at the merge (suspended), the history
// is walked in steps of its own, the merge is then a step of what is left,
// and the branch goes back to its turn to be pushed on from the parent. Every
// step is a fast-forward of that branch, since the branch's own value
// descends from the parent; the repository gets no ref it would not have got,
// and none it would have got later than the import's end.
//
// WHAT IS WALKED IS THE PARENT'S HISTORY, NEVER THE BRANCH'S OWN. A branch's
// first-parent history can meet the very merge it was taken for — every
// branch created after the merge reaches the parent THROUGH it — or a merge of
// its own of what the paused walk is about to push: walked to its own value,
// each branch above the merge met the merge again, measured once more at up to
// twice the limit, until the import was refused naming the last of them. No
// first-parent history of the parent holds the merge, which descends from it,
// and nothing on it is what the paused walk is about to push.
//
// It answers whether it took one, and otherwise the parents the repository
// lacks. Where every parent is held, the commit's own bytes are the step's
// and nothing can split it; where a parent the repository lacks has no branch
// to carry it, the caller's refusal names it ([importPlanner.mergeTooLarge]).
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

// carrier is where the queue holds the branch the history of parent — a
// parent of merge the repository will not hold — is stepped on, or -1 where
// no branch can carry it. A carrier is a branch, since its steps are
// fast-forwards; one whose value in the source reaches parent, so it goes on
// from there by fast-forwards too; and one the repository does not hold, or
// holds at an ancestor of parent, which its steps then descend from
// ([importPlanner.walkTo]).
//
// ONE THAT REACHES PARENT WITHOUT THE MERGE IS TAKEN FIRST — vendor, where
// main merged vendor — so the history lands on the branch whose history it
// is. Only where none does is it carried by one that reaches it through the
// merge, as every branch created after the merge does: such a branch goes on
// to its own value from the parent all the same, and a source with no branch
// left on the merged-in history imports without one. Either is the first in
// the source's order, so a run resumed after a failed step takes the one the
// failed run took.
//
// THE HELD ONES ARE ASKED BOUNDED IN PARALLEL, as [importer.descendants] asks
// its questions and for its reason: an import run again to bring over what a
// source still in use has gained finds every branch at an earlier value, and
// every branch since that merged main back reaches the parent while the
// repository holds it at a commit from before the merge — up to
// code.MaxRefsPerRepo questions ahead of the first that can carry it, at
// 5–10 ms each one after another. An eligible candidate stops every question
// about a candidate after it, those before it are still answered, since one
// of them may be the first, and nothing is left running on return.
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

// mergeTooLarge is the refusal of a commit whose step is past the push limit
// because of the history its parents the repository lacks bring in, which no
// branch of the source still to be pushed can carry ([importPlanner.carrier]):
// a branch on each in a mirror of the source carries it.
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
	for i, parent := range lacking {
		branches[i] = fmt.Sprintf("git --git-dir=mirror.git branch import-%s %s", shortOID(parent), parent)
	}
	return fmt.Errorf("commit %s of %s alone packs to %s, past code.limits.max_push_bytes (%s): it brings in "+
		"the history of %s — about %s in the source's own packs — and no other branch of the source can carry "+
		"it, so nothing can push that history in steps before it. Give it a branch in a mirror of the source "+
		"(git clone --mirror %s mirror.git, then %s) and import from the mirror: the import steps that history "+
		"on it first, and the commit is then a small step. Delete the branch from %s after, if it is not wanted",
		c.oid, c.ref, size, humanBytes(p.budget), strings.Join(lacking, " and "), humanBytes(brings), p.imp.source,
		strings.Join(branches, "; "), p.imp.name)
}

// batchStep is the next batch of the queue: as many of its refs, in order, at
// most code.MaxCommandsPerPush and no more than one push's record holds
// ([importer.recordHolds]), as the step can carry. A branch that alone is past
// the limit is advanced along its first-parent history instead.
func (p *importPlanner) batchStep(ctx context.Context) (*importStep, error) {
	candidates := p.queue[:min(len(p.queue), code.MaxCommandsPerPush)]
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
		// ONE REF IS ALONE ONLY WHEN IT IS NOT A BRANCH: a branch past the
		// limit is walked to it rather than refused.
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

// importWidestNode is the widest node id config admits (config.ValidNodeID):
// a step's push is received by whichever node its URL reaches, and that
// node's id is part of what the push's record holds.
var importWidestNode = strings.Repeat("n", 64)

// recordHolds is how many of refs, in order, one push carries by the room its
// record has for them: at least one, and up to all of them.
//
// THE NODE HOLDS A PUSH'S REF UPDATES TO ITS RECORD before anything of the push
// runs (code.Proposal.ValidateCommands): one push's entry in the code log's
// record takes at most code.EntryRoom, about 1 MiB, with every field the
// push's work could add at its largest, and a push past it is refused whole —
// "push them in parts". A thousand plain refs of the longest take under half
// of that, but a ref whose characters the log spells in six bytes each (`&`,
// `<`, `>`) costs up to six times its length, so some six hundred such refs
// fill a push's record — and an import that batched them by count alone had
// every such batch refused, on every run. So the batch is cut by the domain's
// own rule, asked as the node will ask it: the person the node records
// ([codeImportPrincipal]) and the widest node id that may receive it. A
// command's two ids are spelled at one width whatever they hold, so each
// command is asked as a create of its value; the rule takes every command
// forced and refused at its widest, so what the node decides of it never
// widens it past what this answered. Every step is a push git sends, so it is
// asked as one (code.ViaGit), which its record's entry names too.
//
// None fitting is no ref's fault: one update of the longest ref a push may
// carry takes under two kibibytes, so the person's own id or handle fills the
// record, and no push of theirs can land.
func (imp *importer) recordHolds(refs []queuedRef) (int, error) {
	var refusal error
	fits := func(n int) (bool, error) {
		cmds := make([]code.Command, n)
		for i, r := range refs[:n] {
			cmds[i] = code.Command{Ref: code.RecordedRef(r.ref), Old: code.ZeroOID, New: r.oid}
		}
		err := code.Proposal{Principal: imp.principal, Via: code.ViaGit, Node: importWidestNode,
			Commands: cmds}.ValidateCommands()
		var wide *code.TooWideError
		switch {
		case err == nil:
			return true, nil
		case errors.As(err, &wide):
			refusal = err
			return false, nil
		}
		return false, fmt.Errorf("the batch from %s: %w", refs[0].ref, err)
	}
	// The whole list first, which is the answer for every batch of plain
	// refs: one question, never a search.
	n, err := furthest(1, len(refs), len(refs), fits)
	switch {
	case err != nil:
		return 0, err
	case n < 1:
		return 0, fmt.Errorf("a push of %s alone, as %s, is wider than one push's record may be (%w): the "+
			"token's id or the seat bound to it fills the record, and no push by this token can land — "+
			"import with a token whose id, and whose seat's handle, are shorter", refs[0].ref,
			principalName(imp.principal), refusal)
	}
	return n, nil
}

// principalName is the person a principal is, as the import names it: the
// seat's handle and the token's id.
func principalName(p code.Principal) string {
	if p.Seat == "" {
		return "token " + p.OperatorID
	}
	return p.Seat + " (token " + p.OperatorID + ")"
}

// walkInstead advances the queue's first ref — a branch no batch can carry —
// along its first-parent history.
func (p *importPlanner) walkInstead(ctx context.Context) (*importStep, error) {
	p.queue[0].walk = true
	return p.take(ctx)
}

// target is the estimate a step is planned to: the limit, over how much
// larger than their estimates the last steps packed.
func (p *importPlanner) target() float64 { return float64(p.budget) / p.ratio }

// settle measures step — in a dry run, takes its estimate — and answers 0
// when it may be pushed, or else the estimate its range must come under to
// fit: the step's own estimate scaled down by how far its measurement passed
// the bound it passed furthest — its pack past the limit, or its stored
// bundle past what one push may store (importer.maxStored) — and a sixteenth
// below that.
//
// MEASURED IS COPIED: the step's pack is built once ([importer.build]), and a
// build that may fit is measured as the copy its push will send
// ([importer.sent]); a step that fits keeps its repository for that push, and
// one that does not is dropped. A build too far past the limit to fit as its
// copy ([importOrderSlack]), or stopped at its cap, is cut on its own size,
// and never indexed or copied.
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
// code.limits.repo_max_bytes: the node refuses such a push repo_size after
// its whole upload, and an import whose next step crosses the limit stops
// there run after run until the limit is raised, so the refusal says by how
// much and what is left. In a dry run, which measures nothing, the estimate is
// added and the crossing only noted.
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
	return fmt.Errorf("%s would hold %s of packs once the next step lands — %s before it, and %s that step "+
		"stores — past code.limits.repo_max_bytes (%s): raise it, and run the same command again. What is "+
		"left of the source, that step included, holds about %s in the source's own packs, and its steps "+
		"store more than that: each keeps whole what its deltas cannot reach in an earlier step", p.imp.name,
		humanBytes(p.held+stored), humanBytes(p.held), humanBytes(stored), humanBytes(limit), humanBytes(rest))
}

// rest is what is left to push, in the source's own packs: every ref the
// plan has not yet taken — the walks', the branches out of the queue while a
// merge's history is stepped on them, and the queue's — against what the
// repository will hold.
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

// tooLarge is the refusal of a step that cannot be made smaller and whose pack
// is past the push limit; commit says it is one commit of a branch's history,
// otherwise one ref no branch's steps reach.
func (p *importPlanner) tooLarge(step *importStep, commit bool) error {
	size := humanBytes(step.size)
	if step.past {
		size = "more than " + size
	}
	c := step.cmds[0]
	if !step.past && step.size <= p.budget {
		// Its pack fits the limit and what it stores does not: a step of
		// a great many small objects, whose indexes take it past
		// code.MaxPackBytes. No setting raises that.
		return fmt.Errorf("%s (%s) alone stores %s with its indexes — %s objects — past the %s one push may store, "+
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

// under is a step search's question — does the range ending at i estimate
// under target? — over a memoised estimate.
func under(estimate func(int) (int64, error), target float64) func(int) (bool, error) {
	return func(i int) (bool, error) {
		e, err := estimate(i)
		return float64(e) <= target, err
	}
}

// furthest is the largest i in [lo, hi] for which fits(i) holds — fits being
// true up to some index and false past it, as an estimate of a growing range
// is — or lo-1 where it holds for none. It asks guess first, gallops from it
// by doubling until it brackets the answer, then halves the bracket: a search
// that starts near where the last step ended asks a few times the logarithm
// of how far off it was, rather than of the whole history.
//
// WHERE THE GUESS DOES NOT FIT, lo IS ASKED NEXT: every range holds lo's, so
// a lo that does not fit means none does — a batch whose first ref is a
// branch past the limit on its own, which after a full batch of a thousand
// was asked as eleven traversals of that branch's whole history on the way
// down, each proving what this one question proves.
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

// estimate is what the source's own packs hold of the objects news reach and
// the repository will not hold: rev-list --disk-usage, a traversal.
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

// reaches reports whether the repository will hold commit and its whole
// history: whether the refs it will hold reach it — one rev-list that stops
// at the first commit they do not.
func (imp *importer) reaches(ctx context.Context, commit string, remote *remoteRefs) (bool, error) {
	out, err := imp.g.call(ctx, gitCall{env: imp.g.environ, gitDir: imp.clone, stdin: remote.revs([]string{commit})},
		"rev-list", "--max-count=1", "--stdin")
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) == 0, nil
}

// branchesContaining is every branch of the source whose history holds
// commit — and, where without is a commit, does not hold without
// (for-each-ref --no-contains, git 2.13).
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

// build builds step's pack ONCE — its one delta search — and answers its
// bytes and the objects its header counts: pack-objects --revs --stdout
// --delta-base-offset (the node's receive-pack advertises ofs-delta), whole,
// as every step is pushed (--no-thin), against what the repository will hold,
// under the operator's own pack settings, threads and all — written into a
// repository of the step's own (step.repo) as it is counted, and stopped at
// importMeasureCap, past which past is set and the size is a floor.
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

// sent is the bytes of the pack a push of step sends: the step's build,
// indexed in its repository — index-pack resolves its deltas and writes the
// .idx pack-objects finds its objects by, searching for none — and only then
// given the clone's objects to borrow ([importer.borrow]), and COPIED under
// importSendConfig, counted and thrown away. That copy is the
// pack the push's own pack-objects writes, byte for byte, since neither has
// anything to choose and both are handed the step's tips in one order
// ([importStep.news]; the package doc's "The pack a step measured is the
// pack it pushes"). Uncapped: it is the build's objects, at most
// importOrderSlack past the build, which settle copies only within the limit.
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

// stepRepo makes a repository of one step's own in the import's working
// directory: bare, with no ref and no pack of its own. The step's build is
// written there as its one pack ([importer.build]) and indexed, and only then
// does the repository borrow the clone's objects ([importer.borrow]):
// pack-objects takes each object from the step's own pack before any pack it
// borrows — the local packs first — and the clone's are what its walk reads
// below what the step sends. SHA-1, as the clone is held to
// ([importer.inspect]), whatever the operator's init.defaultObjectFormat
// says: a repository borrows the objects of its own format only.
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
// (objects/info/alternates), for the copy's walk and the push to read below
// what the step sends.
//
// ONCE THE BUILD IS INDEXED, NEVER BEFORE: index-pack checks every object of
// the pack it indexes that its repository already holds against that copy,
// byte for byte, reading it whole — a check for a SHA-1 collision — and one
// object at a time, under a lock its threads share. Every object of a step's
// build is the clone's, so a repository that borrowed the clone first had its
// index read the whole build back out of the clone's packs, delta chains and
// all, for a comparison that cannot fail.
//
// BY ITS ABSOLUTE PATH: git reads a relative alternate against the borrowing
// repository's own objects directory, never against the directory the import
// runs in, and the working directory is under $TMPDIR, which an operator
// points at a volume with room for the import and may name relative to where
// they run it.
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

// stepPack is where a step's build is written in its repository: a pack git
// finds there by the .idx beside it, whatever its name.
func stepPack(repo string) string { return filepath.Join(repo, "objects", "pack", "step.pack") }

// errPastCap is a measurement stopped at its cap.
var errPastCap = errors.New("the pack is past what its measurement builds")

// packHeaderBytes is a pack's header: "PACK", the version, the object count.
const packHeaderBytes = 12

// countingWriter counts what is written to it, refusing a write that takes it
// past limit, and keeps the pack's header — writing everything on to to,
// where there is one, and otherwise keeping nothing else.
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

// plan prints a dry run's steps: every one planned from estimates, nothing
// measured and nothing pushed.
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
	fmt.Fprintf(imp.stdout, "plan: %s, ≈ %s — estimated from how the source stores its objects; an import "+
		"measures each step before it pushes it, and cuts one that packs past %s shorter\n",
		counted(len(steps), "step", "steps"), humanBytes(total), humanBytes(imp.budget))
	for i, s := range steps {
		if s.note != "" {
			fmt.Fprintf(imp.stdout, "  stepping %s\n", s.note)
		}
		note := ""
		switch {
		case s.size > p.budget:
			note = " — past the push limit by its estimate: measured when it is pushed, and refused if it packs " +
				"that large"
		case s.overRepo:
			note = " — past code.limits.repo_max_bytes by its estimate, with what the repository holds: measured " +
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

// importDone is what one run of an import pushed: its steps, the bytes their
// packs sent and what the node stores of them, and how many steps a
// measurement cut shorter (recut).
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

// execute pushes every step, planning and measuring each while the one before
// it is pushed, and answers what it pushed.
//
// ONE PUSH AT A TIME, and none of it left running: each step is a
// fast-forward from the one before it, so it waits for that one's outcome; and
// the step in flight is waited for on every path out, a refusal of the plan
// included — its outcome is reported before the plan's, since it may have
// landed.
//
// A PUSH THAT FAILS ENDS THE PLANNING AT ONCE. The next step is planned on a
// goroutine of its own, under a context of its own, while this one is pushed;
// whichever of the two answers first is read first. A push refused in a
// second — a protected branch, a token bound to nobody, the network — cancels
// the planning, which ends its git processes (operatorGit.call), and is
// reported once the planning has returned, so no git outlives the import: the
// refusal used to wait for the next step's estimates and every build of its
// pack, minutes of CPU at a gibibyte's step, spent on a step never pushed.
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
			return done, fmt.Errorf("%w\n\nthe import stopped before step %d; %s — run the same command again "+
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

// push is one step's `git push --porcelain --atomic`, pinned to the push its
// plan measured (importPushConfig, importPushFlags) and sent from the step's
// own repository as a copy of the pack it was built as (importSendConfig), and
// read from its own lines: every ref it names landed, or the step did not. The
// step's repository goes once the push has ended, however it ended.
//
// --atomic so a step lands whole or not at all, which is what lets a re-run
// read the repository and resume. The refspecs are on the command line: at
// most code.MaxCommandsPerPush of at most 296 bytes each (an id, a colon, a
// ref of code.MaxRefNameBytes) is under 300 KB, inside the argument limit of
// both platforms this binary is built for (1 MiB on darwin, 2 MiB on linux).
//
// A LINE FOR A REF THE STEP DID NOT NAME is a push that carried more than its
// plan — what an unpinned setting of the operator's did — and is said, though
// what it moved has landed.
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
		// GIT'S OWN WORDS FOR WHY, beside its lines for the refs. A ref's
		// line names a class alone — `(unpacker error)` for every object
		// the node's fsck refuses — and the reason is on git's stderr,
		// which only a person at a terminal (live) saw go by: a script's
		// or a CI job's import stopped telling its operator to fix a cause
		// nothing had named.
		return stepOutcome{err: fmt.Errorf("git refused it: %w", err), rejected: rejected}
	case len(rejected) > 0:
		return stepOutcome{err: errors.New("git refused it"), rejected: rejected}
	case err != nil:
		return stepOutcome{err: err}
	case unsaid > 0:
		return stepOutcome{err: fmt.Errorf("git pushed without saying what became of %d of its refs", unsaid)}
	case len(unplanned) > 0:
		return stepOutcome{err: fmt.Errorf("git pushed %s the step did not name — %s — which the plan never "+
			"measured", counted(len(unplanned), "ref", "refs"), listed(unplanned[:min(len(unplanned), importNamed)],
			len(unplanned)))}
	}
	return stepOutcome{}
}

// porcelainLine is one ref's line of `git push --porcelain`: its flag — ' ',
// '+', '-', '*' or '=' landed or up to date, '!' rejected — and the line as git
// printed it.
type porcelainLine struct {
	flag byte
	text string
}

// porcelainLines reads `git push --porcelain`'s status lines,
// "<flag>\t<from>:<to>\t<summary>", by destination ref.
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

// stopped is the end of an import whose step n+1 did not land: git's own
// lines for the refs it refused, on stderr, and what to do.
func (imp *importer) stopped(landed int, out stepOutcome) error {
	for _, l := range out.rejected {
		fmt.Fprintln(imp.stderr, l)
	}
	why := out.err.Error()
	hint := ""
	for _, l := range out.rejected {
		if strings.Contains(l, string(code.RefusalProtectedDirect)) {
			hint = "\na branch whose code.repos rule says direct_push: none takes no push, an import's included: " +
				"lift the rule for the import and restore it after"
			break
		}
	}
	return fmt.Errorf("the import stopped at step %d (%s); %s — fix the cause and run the same command again: "+
		"it resumes%s", landed+1, why, landedBefore(landed), hint)
}

// landedBefore says how many steps of this run landed before the one an
// import stopped at.
func landedBefore(n int) string {
	switch n {
	case 0:
		return "no step of this run landed before it"
	case 1:
		return "the 1 step of this run before it landed"
	}
	return fmt.Sprintf("the %d steps of this run before it landed", n)
}

// prove holds the repository to the source once the steps have landed: every
// kept ref of the source at the source's value, as git lists it from the node
// — an annotated tag's peeled value with it, since a tag's id names the
// object it peels to. Then the summary: the steps this run pushed, the bytes
// their packs sent and the node stores of them, and how many a measurement
// cut shorter.
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

// moreThan is ", and N more" beneath a list that named shown of n.
func moreThan(n, shown int) string {
	if n > shown {
		return fmt.Sprintf("\n  and %d more", n-shown)
	}
	return ""
}

// ---- reading git's output --------------------------------------------------

// errStopReading is a reader that has read all it needed: git is stopped, and
// that is not a failure.
var errStopReading = errors.New("read all it needed")

// lineWriter hands fn each line git writes, without its newline, holding only
// the line being read — a listing of every object of a large history is
// millions of lines. fn must not keep the slice it is handed. A line past max
// is not git's answer, and is refused.
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
		// THE BOUND HOLDS FOR EVERY LINE, whole in one write or split
		// across many: a line the write ends is as long as the part held
		// from writes before it and the part this one carries.
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
