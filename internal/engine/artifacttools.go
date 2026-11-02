package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE ARTIFACTS TOOLS' ENGINE WIRING: the same deps for a seat's registry and
// for the operator catalogue, over the artifacts estate through the router (a
// node without `data` acts through a data node), the node's one publish
// pipeline, the running chart, the identity directory for whoever a turn is
// acting for, and the coding run's box a publish reads.

// ArtifactToolDeps are the artifacts tools' dependencies on this node, or the
// zero value (which serves none of them) where the node takes no artifact
// uploads: it runs no object store, no coordination store or no router.
//
// EXPORTED for the operator surface, which builds its catalogue outside this
// package; a seat's registry takes the same value at every apply ([equip]).
func ArtifactToolDeps(e *Engine) builtin.ArtifactDeps {
	if e == nil || e.router == nil {
		return builtin.ArtifactDeps{}
	}
	publisher, err := e.ArtifactUploads()
	if err != nil {
		// A NODE THAT TAKES NO UPLOADS SERVES NO ARTIFACT TOOL rather than
		// a catalogue it cannot complete: the reason is the pipeline's, and
		// was logged where it was built.
		log.Debug("artifact_tools_omitted", "reason", err.Error())
		return builtin.ArtifactDeps{}
	}
	estate := e.router.Artifacts()
	deps := builtin.ArtifactDeps{
		Reader: estate, Writer: estate, Publisher: publisher,
		Chart:        ChartAuthorityOf(e),
		Requesters:   turnRequesters{engine: e},
		Limits:       e.artifactLimits,
		ManageOthers: e.artifactsManageOthers,
		Link:         e.artifactLink,
		Org:          e.currentOrg,
		Await:        e.router.Await,
		KitVersion:   kitVersion,
		FolderTurns:  e.folderTurns,
	}
	if e.sandbox.Load() != nil {
		deps.Boxes = artifactBoxes{engine: e}
	}
	// THE PRESENCE HUB, so a seat reading or writing an artifact (or its
	// datastore, whose tools share this half) appears in the artifact's
	// room. A nil hub stays a nil interface, which tells nobody.
	if e.presence != nil {
		deps.Presence = e.presence
	}
	return deps
}

// artifactsManageOthers is the company's `artifacts.agents_manage_others` as
// the running revision says it: false, the closed end, on a node with none.
func (e *Engine) artifactsManageOthers() bool {
	c := e.Company()
	return c != nil && c.Config != nil && c.Config.Artifacts.AgentsManageOthers
}

// artifactLink is the dashboard's address of an artifact, on
// `api.external_url`, or empty where the deployment names none: the one
// function every tool answer's `url` is built by, so an agent forwarding a
// link forwards a working dashboard address.
func (e *Engine) artifactLink(id artifacts.ID) string {
	if e.externalBase == "" {
		return ""
	}
	return e.externalBase + auth.PathDashboard + "#/artifacts/" + id.String()
}

// currentOrg is the running company's chart, or nil on a node with none.
func (e *Engine) currentOrg() *org.Organization {
	if c := e.Company(); c != nil {
		return c.Org
	}
	return nil
}

// ---- the coding run's box ------------------------------------------------ //

// artifactBoxes attaches the box of the coding run working in a turn, for a
// publish reading its build.
//
// THE RUN READS ITS OWN BOX, WHILE IT WORKS. A seat's executor is its coding
// CLI's own run in that box, and every call that names a box source reaches
// here from its CLI over the tool bridge — a worker it delegates to holds no
// artifact write — while the run's record reads running: a collected run's
// resumed segment calls no artifact tool (its reviewer holds one submission
// tool, and the delivery check none). So a running record is the one a box is
// read for, and the run says by calling when its build is ready. A record past
// it has no reader left: one parked on a question has its job paused, which a
// read would wake; a collected one is over; a reaped pause has no box.
type artifactBoxes struct{ engine *Engine }

func (b artifactBoxes) Box(ctx context.Context, t *turnctx.Turn) (builtin.ArtifactBox, error) {
	rt := b.engine.sandbox.Load()
	if rt == nil || t == nil || t.RunID == "" {
		return nil, builtin.ErrNoBox
	}
	row, found, err := rt.pending.Get(ctx, t.RunID)
	switch {
	case err != nil:
		return nil, err
	case !found || row.SandboxID == "":
		return nil, builtin.ErrNoBox
	case row.AgentHandle != "" && row.AgentHandle != t.Handle():
		// THE ROW IS ANOTHER SEAT'S: a turn reads its own run's box only.
		return nil, builtin.ErrNoBox
	case row.Status != sandbox.StatusRunning:
		// NOT THE RUN WORKING IN IT: see [artifactBoxes].
		return nil, fmt.Errorf("%w: the coding run is %s", builtin.ErrNoBox, row.Status)
	}
	manager := rt.coordinator.Manager()
	if manager == nil {
		return nil, builtin.ErrNoBox
	}
	if manager.OnLaunchHost(row) && row.LaunchNode != b.engine.id {
		// ANOTHER NODE'S HOST: the seat moved since the run launched, and
		// its box is a process tree or a container there, which only that
		// node touches (internal/sandbox, boxhost.go).
		return nil, fmt.Errorf("%w: the coding run's box is on node %s's host, and this turn "+
			"runs on another node", builtin.ErrNoBox, row.LaunchNode)
	}
	// ATTACHED, NEVER RECONNECTED: the job is working in the box, and a
	// reconnect resumes and re-times it — E2B's /connect moves a box's kill
	// timer to its own, under the waiter that keeps it — where a reader
	// changes nothing. A box paused under a running record is a collection
	// that has not moved the record on yet: its run is over.
	box, _, err := manager.Attach(ctx, sandbox.Placement(row.Placement), row.SandboxID, row.CodingAgent)
	switch {
	case errors.Is(err, sandbox.ErrBoxGone), errors.Is(err, sandbox.ErrBoxPaused):
		return nil, fmt.Errorf("%w: %w", builtin.ErrNoBox, err)
	case err != nil:
		return nil, err
	}
	return &runBox{box: box, root: path.Join(box.Home(), sandbox.WorkspaceSubdir),
		listed: map[string]bool{}}, nil
}

// runBox is one reattached box, read under its workspace.
type runBox struct {
	box  sandbox.Sandbox
	root string

	mu     sync.Mutex
	listed map[string]bool
}

// boxListTimeout bounds the listing command: a find over a build directory,
// which a box under load answers in seconds.
const boxListTimeout = 60

// listEntryBytes bounds the listing's output per file it may name: a path of
// at most [artifacts.MaxPathBytes] below a directory of the same bound, and
// its terminator.
const listEntryBytes = 2*artifacts.MaxPathBytes + 1

// The listing command's own exits, each one no exit of the tools it runs nor a
// box's own (124 for its timeout, 125 for a runtime's failure, 126 and 127 for
// a command it can't run, above 128 for a signal) is.
const (
	// listNotADirectory is a dir the workspace does not hold as a
	// directory: missing, a file or a link.
	listNotADirectory = 3
	// listNoWorkspace is a workspace that is gone from the box, which no
	// dir the caller names can list.
	listNoWorkspace = 4
	// listIncomplete is find exiting non-zero: it walked part of the dir
	// and could not read the rest (a directory the box's user can't open,
	// a read error), so what it printed is not the whole listing.
	listIncomplete = 5
)

// Files lists every regular file under dir through the box itself: find
// prints NUL-terminated paths and never follows a link (`-type f` is false for
// one), and the output is cut at what max entries can weigh, so a box cannot
// hand the engine an unbounded answer.
//
// A DIR THE WORKSPACE DOES NOT HOLD AS A DIRECTORY IS THE CALLER'S
// ([builtin.ErrNotADirectory]), asked of the box before find runs, and a
// workspace that is gone is said as that ([builtin.ErrNoWorkspace]). find is
// handed the dir as `./dir`, never bare, because a dir named `-delete` is a
// find expression and ran over the whole workspace.
//
// FIND'S OWN EXIT IS CARRIED OUT OF THE PIPELINE, whose status is head's: a
// walk that failed partway printed what it reached, and the listing was taken
// as the whole build. A find that exits non-zero is [builtin.ErrListIncomplete]
// unless head cut its output first, which killed it, and a find or a head the
// box can't run is its image ([boxExited]). Any other exit is the box's.
func (b *runBox) Files(ctx context.Context, dir string, max int) ([]string, error) {
	limit := max * listEntryBytes
	start := "."
	if dir != "." {
		start = "./" + dir
	}
	q := sandbox.ShellQuote(start)
	cmd := "cd " + sandbox.ShellQuote(b.root) + " 2>/dev/null || exit " + strconv.Itoa(listNoWorkspace) +
		"; [ -d " + q + " ] && [ ! -L " + q + " ] || exit " + strconv.Itoa(listNotADirectory) +
		"; exec 3>&1; st=$( { { find " + q + " -type f -print0; echo $? >&4; } | head -c " +
		strconv.Itoa(limit) + " >&3; } 4>&1 ) || exit $?; case $st in 0) ;; 126|127) exit $st ;; " +
		"*) exit " + strconv.Itoa(listIncomplete) + " ;; esac"
	res, err := b.box.Exec(ctx, cmd, sandbox.ExecOptions{TimeoutSec: boxListTimeout})
	cut := len(res.Stdout) >= limit
	switch {
	case err != nil:
		return nil, fmt.Errorf("engine: list %s in the box: %w", dir, err)
	case res.ExitCode == listNotADirectory:
		return nil, fmt.Errorf("engine: list %s in the box: %w", dir, builtin.ErrNotADirectory)
	case res.ExitCode == listNoWorkspace:
		return nil, fmt.Errorf("engine: list %s in the box: %w", dir, builtin.ErrNoWorkspace)
	case res.ExitCode == listIncomplete && !cut:
		return nil, fmt.Errorf("engine: list %s in the box: %w: %s", dir,
			builtin.ErrListIncomplete, strings.TrimSpace(clipTail(res.Stderr)))
	case res.ExitCode != 0 && res.ExitCode != listIncomplete:
		return nil, boxExited("list "+dir, res)
	}
	if cut {
		// CUT, SO THE LAST NAME MAY BE HALF OF ONE: answer one more entry
		// than asked for, which the caller refuses as too many. A find
		// head's cut killed exits non-zero, which says nothing more.
		out := make([]string, 0, max+1)
		for i := 0; i <= max; i++ {
			out = append(out, "")
		}
		return out, nil
	}
	var out []string
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range strings.Split(res.Stdout, "\x00") {
		if p == "" {
			continue
		}
		p = strings.TrimPrefix(p, "./")
		out = append(out, p)
		b.listed[p] = true
	}
	return out, nil
}

// checkNotARegularFile is the file check's own exit for a path that names no
// regular file of the workspace, which no box's own exit is: test's own false
// is 1, which is also what a container runtime's CLI exits with when it fails
// itself, and read as the caller's path it told a box the runtime could not
// reach as a file the caller named wrong.
const checkNotARegularFile = 3

// Open streams one regular file. A path the listing did not name is held to
// being a regular file first, because a box streams a missing path as an
// empty file and a link to wherever it points.
func (b *runBox) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	b.mu.Lock()
	listed := b.listed[p]
	b.mu.Unlock()
	full := path.Join(b.root, p)
	if !listed {
		q := sandbox.ShellQuote(full)
		res, err := b.box.Exec(ctx, "test -f "+q+" && test ! -L "+q+" || exit "+
			strconv.Itoa(checkNotARegularFile), sandbox.ExecOptions{TimeoutSec: boxListTimeout})
		if err != nil {
			return nil, fmt.Errorf("engine: check %s in the box: %w", p, err)
		}
		switch res.ExitCode {
		case 0:
		case checkNotARegularFile:
			// THE CALLER'S PATH, which names nothing a publish can read:
			// [sandbox.ErrNotRegularFile] is what the tool refuses it by,
			// as the argument it is, where an unmarked error was told as a
			// fault of this node. The check's own exit and nothing else: a
			// check the box timed out, killed or couldn't run is the box's,
			// never a path the caller named wrong.
			return nil, fmt.Errorf("engine: %s is missing from the coding run's workspace, "+
				"or %w", p, sandbox.ErrNotRegularFile)
		default:
			return nil, boxExited("check "+p, res)
		}
	}
	return b.box.OpenFile(ctx, full)
}

// boxExited is a command the engine runs in a box exiting with a status the
// command itself never answers with, classed by what clears it ([sandbox.Why]):
// 126 or 127 is a box that cannot run its shell, or find or head (a listing
// carries find's own status out for this), which is its image, a setup a
// person changes; 124 (its timeout), above 128 (a signal ended it) and
// anything else, a runtime's own failure among it, is the box failing on its
// side, which clears when it answers again. Unclassed, every one was told as
// a fault of this node. Its stderr goes to the log with the error, never to a
// reader.
func boxExited(what string, res sandbox.ExecResult) error {
	err := fmt.Errorf("engine: %s in the box exited %d: %s", what, res.ExitCode,
		strings.TrimSpace(clipTail(res.Stderr)))
	if res.ExitCode == 126 || res.ExitCode == 127 {
		return sandbox.Because(sandbox.WhyConfiguration, err)
	}
	return sandbox.Because(sandbox.WhyProviderUnavailable, err)
}

// clipTail is the end of a command's error output, enough to name the error.
func clipTail(s string) string {
	const keep = 512
	if len(s) <= keep {
		return s
	}
	return s[len(s)-keep:]
}
