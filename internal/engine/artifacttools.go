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
	"github.com/crewlet/crewlet/internal/shellword"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE ARTIFACTS TOOLS' ENGINE WIRING: one set of deps for a seat's registry and
// the operator catalogue, over the artifacts estate through the router, the
// node's one publish pipeline, the chart, the identity directory and the
// executor's box a publish reads.

// ArtifactToolDeps are the artifacts tools' dependencies on this node, or the
// zero value (serving none) where the node takes no artifact uploads: no object
// store, coordination store or router.
//
// EXPORTED for the operator surface's catalogue; a seat's registry takes the
// same value at every apply ([equip]).
func ArtifactToolDeps(e *Engine) builtin.ArtifactDeps {
	if e == nil || e.router == nil {
		return builtin.ArtifactDeps{}
	}
	publisher, err := e.ArtifactUploads()
	if err != nil {
		// A NODE THAT TAKES NO UPLOADS SERVES NO ARTIFACT TOOL, never a
		// partial catalogue; the pipeline logged the reason when built.
		log.Debug("artifact_tools_omitted", "reason", err.Error())
		return builtin.ArtifactDeps{}
	}
	estate := e.router.Artifacts()
	deps := builtin.ArtifactDeps{
		Reader: estate, Writer: estate, Publisher: publisher, Comments: estate,
		Routing:      ArtifactCommentRouter(e),
		Chart:        ChartAuthorityOf(e),
		Requesters:   turnRequesters{engine: e},
		Limits:       e.artifactLimits,
		ManageOthers: e.artifactsManageOthers,
		Link:         e.ArtifactLink,
		Org:          e.currentOrg,
		Await:        e.router.Await,
		KitVersion:   kitVersion,
		FolderTurns:  e.folderTurns,
	}
	if e.sandbox.Load() != nil {
		deps.Boxes = artifactBoxes{engine: e}
	}
	// THE PRESENCE HUB, so a seat using an artifact or its datastore appears in
	// the artifact's room. A nil hub stays a nil interface.
	if e.presence != nil {
		deps.Presence = e.presence
	}
	return deps
}

// ArtifactCommentRouter decides who a comment on an artifact wakes and tells:
// the seats a remark names and the linked items' assignees, on the chart now.
// Exported for the artifacts API, which forms the same records.
func ArtifactCommentRouter(e *Engine) builtin.ArtifactCommentRouter {
	return builtin.ArtifactCommentRouter{Org: e.currentOrg, Mentions: LiveMentions(e),
		Assignees: artifactAssignees{engine: e}}
}

// artifactAssignees reads the assignee of every work item an artifact was made
// for or linked to, through the router; a company without Worklet adds nobody.
type artifactAssignees struct{ engine *Engine }

func (a artifactAssignees) Assignees(ctx context.Context, id artifacts.ID) ([]string, error) {
	if a.engine.router == nil || !a.engine.NativeStarted() {
		return nil, nil
	}
	work, ok := OperatorWork(a.engine)
	if !ok {
		return nil, nil
	}
	items, err := a.engine.router.Artifacts().WorkItems(ctx, id)
	if err != nil {
		return nil, err
	}
	fresh := statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)}
	var out []string
	for _, item := range items {
		detail, err := work.Task(ctx, item, worklet.DetailWants{}, fresh)
		switch {
		case errors.Is(err, worklet.ErrNoTask):
			continue
		case err != nil:
			return nil, fmt.Errorf("engine: read work item %s's assignee: %w", item, err)
		case detail.Task.Removed != nil || detail.Task.Assignee == "":
			continue
		}
		out = append(out, detail.Task.Assignee)
	}
	return out, nil
}

// artifactsManageOthers is the running revision's
// `artifacts.agents_manage_others`: false, the closed end, on a node with none.
func (e *Engine) artifactsManageOthers() bool {
	c := e.Company()
	return c != nil && c.Config != nil && c.Config.Artifacts.AgentsManageOthers
}

// ArtifactLink is an artifact's dashboard address on `api.external_url`, or
// empty where none is set: the one builder of every tool answer's `url`, so a
// forwarded link works, and of what a decision reads back as the artifact.
func (e *Engine) ArtifactLink(id artifacts.ID) string {
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

// ---- the executor's box -------------------------------------------------- //

// artifactBoxes attaches the box of the executor working in a turn, for a
// publish reading its build.
//
// THE RUN READS ITS OWN BOX, WHILE IT WORKS. Every call naming a box source
// comes from the seat's harness over the tool bridge while its record reads
// running (a delegated worker holds no artifact write; a collected run's
// resumed segment calls no artifact tool). So only a running record's box is
// read. Past it nobody reads: a parked job is paused and a read would wake it,
// a collected run is over, and a reaped pause has no box.
type artifactBoxes struct{ engine *Engine }

func (b artifactBoxes) Box(ctx context.Context, t *turnctx.Turn) (builtin.ArtifactBox, error) {
	box, err := b.attach(ctx, t)
	if err != nil {
		return nil, err
	}
	return &runBox{box: box, root: path.Join(box.Home(), sandbox.WorkspaceSubdir),
		listed: map[string]bool{}}, nil
}

// attach is the box of the executor working in turn t, attached: the one box
// a turn reads its build from and a render writes its screenshots to.
func (b artifactBoxes) attach(ctx context.Context, t *turnctx.Turn) (sandbox.Sandbox, error) {
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
		return nil, fmt.Errorf("%w: the executor is %s", builtin.ErrNoBox, row.Status)
	}
	manager := rt.coordinator.Manager()
	if manager == nil {
		return nil, builtin.ErrNoBox
	}
	if row.LaunchNode != b.engine.id {
		// ANOTHER NODE'S HOST: the seat moved since launch, and only that node
		// touches its box (internal/sandbox, boxhost.go).
		return nil, fmt.Errorf("%w: the executor's box is on node %s's host, and this turn "+
			"runs on another node", builtin.ErrNoBox, row.LaunchNode)
	}
	// ATTACHED, NEVER RECONNECTED: a reconnect resumes a paused box, where a
	// reader must change nothing. A box paused under a running record is a
	// collection not yet recorded: its run is over.
	box, _, err := manager.Attach(ctx, sandbox.Placement(row.Placement), row.SandboxID, row.Harness)
	switch {
	case errors.Is(err, sandbox.ErrBoxGone), errors.Is(err, sandbox.ErrBoxPaused):
		return nil, fmt.Errorf("%w: %w", builtin.ErrNoBox, err)
	case err != nil:
		return nil, err
	}
	return box, nil
}

// runBox is one reattached box, read under its workspace.
type runBox struct {
	box  sandbox.Sandbox
	root string

	mu     sync.Mutex
	listed map[string]bool
}

// boxListTimeout bounds the listing command, in seconds: a find over a build
// directory, which a box under load answers in seconds.
const boxListTimeout = 60

// listEntryBytes bounds the listing's output per file it may name: a path of
// at most [artifacts.MaxPathBytes] below a directory of the same bound, and
// its terminator.
const listEntryBytes = 2*artifacts.MaxPathBytes + 1

// The listing command's own exits, none of them an exit of the tools it runs
// or of a box (124 timeout, 125 runtime failure, 126/127 can't run, >128 signal).
const (
	// listNotADirectory is a dir the workspace does not hold as a
	// directory: missing, a file or a link.
	listNotADirectory = 3
	// listNoWorkspace is a workspace gone from the box.
	listNoWorkspace = 4
	// listIncomplete is find exiting non-zero: it could not read part of the
	// dir, so what it printed is not the whole listing.
	listIncomplete = 5
)

// Files lists every regular file under dir through the box itself: find prints
// NUL-terminated paths, never follows a link (`-type f`), and the output is cut
// at what max entries can weigh, so a box cannot hand back an unbounded answer.
// A non-directory dir is the caller's ([builtin.ErrNotADirectory]), a gone
// workspace [builtin.ErrNoWorkspace]. find gets `./dir`, never bare: a dir
// named `-delete` would be a find expression.
//
// FIND'S OWN EXIT IS CARRIED OUT OF THE PIPELINE (whose status is head's), or a
// walk failing partway passes as the whole build: non-zero is
// [builtin.ErrListIncomplete] unless head's cut killed it; 126/127 is the
// image ([boxExited]); any other exit is the box's.
func (b *runBox) Files(ctx context.Context, dir string, max int) ([]string, error) {
	limit := max * listEntryBytes
	start := "."
	if dir != "." {
		start = "./" + dir
	}
	q := shellword.Quote(start)
	cmd := "cd " + shellword.Quote(b.root) + " 2>/dev/null || exit " + strconv.Itoa(listNoWorkspace) +
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
		// CUT, SO THE LAST NAME MAY BE HALF OF ONE: answer one more entry than
		// asked for, which the caller refuses as too many.
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

// checkNotARegularFile is the file check's own exit for a path naming no
// regular file, never test's 1, which a failing container runtime CLI also
// exits with and would blame the caller's path for the runtime's fault.
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
		q := shellword.Quote(full)
		res, err := b.box.Exec(ctx, "test -f "+q+" && test ! -L "+q+" || exit "+
			strconv.Itoa(checkNotARegularFile), sandbox.ExecOptions{TimeoutSec: boxListTimeout})
		if err != nil {
			return nil, fmt.Errorf("engine: check %s in the box: %w", p, err)
		}
		switch res.ExitCode {
		case 0:
		case checkNotARegularFile:
			// THE CALLER'S PATH, refused as an argument
			// ([sandbox.ErrNotRegularFile]), not a node fault. Only this exit:
			// a check timed out, killed or unrunnable is the box's.
			return nil, fmt.Errorf("engine: %s is missing from the box's workspace, "+
				"or %w", p, sandbox.ErrNotRegularFile)
		default:
			return nil, boxExited("check "+p, res)
		}
	}
	return b.box.OpenFile(ctx, full)
}

// boxExited is a command in a box exiting with a status the command itself
// never answers with, classed by what clears it ([sandbox.Why]): 126 or 127
// (cannot run its shell, find or head) is the image, a setup a person changes;
// anything else (124 timeout, >128 signal, a runtime failure) is the box's
// side, clearing when it answers again. Unclassed it would read as this node's
// fault. Its stderr goes to the log, never to a reader.
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
