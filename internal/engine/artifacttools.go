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

// artifactBoxes reattaches the box of the coding run a turn launched, for a
// publish reading its build.
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
	case row.Status != sandbox.StatusResumed:
		// A JOB STILL WORKING (or waiting on a person) is writing into
		// the box: its build is not finished, and a turn is not reading
		// it from this Execute phase anyway.
		return nil, fmt.Errorf("%w: the coding run is %s", builtin.ErrNoBox, row.Status)
	}
	manager := rt.coordinator.Manager()
	if manager == nil {
		return nil, builtin.ErrNoBox
	}
	box, _, err := manager.Reconnect(ctx, sandbox.Placement(row.Placement), row.SandboxID, row.CodingAgent)
	switch {
	case errors.Is(err, sandbox.ErrBoxGone):
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

// Files lists every regular file under dir through the box itself: find
// prints NUL-terminated paths and never follows a link (`-type f` is false for
// one), and the output is cut at what max entries can weigh, so a box cannot
// hand the engine an unbounded answer.
func (b *runBox) Files(ctx context.Context, dir string, max int) ([]string, error) {
	limit := max * listEntryBytes
	cmd := "cd " + sandbox.ShellQuote(b.root) + " && find " + sandbox.ShellQuote(dir) +
		" -type f -print0 | head -c " + strconv.Itoa(limit)
	res, err := b.box.Exec(ctx, cmd, sandbox.ExecOptions{TimeoutSec: boxListTimeout})
	if err != nil {
		return nil, fmt.Errorf("engine: list %s in the box: %w", dir, err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("engine: list %s in the box exited %d: %s", dir, res.ExitCode,
			strings.TrimSpace(clipTail(res.Stderr)))
	}
	if len(res.Stdout) >= limit {
		// CUT, SO THE LAST NAME MAY BE HALF OF ONE: answer one more entry
		// than asked for, which the caller refuses as too many.
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
		res, err := b.box.Exec(ctx, "test -f "+q+" && test ! -L "+q,
			sandbox.ExecOptions{TimeoutSec: boxListTimeout})
		if err != nil {
			return nil, fmt.Errorf("engine: check %s in the box: %w", p, err)
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("engine: %s is not a regular file in the coding run's workspace", p)
		}
	}
	return b.box.OpenFile(ctx, full)
}

// clipTail is the end of a command's error output, enough to name the error.
func clipTail(s string) string {
	const keep = 512
	if len(s) <= keep {
		return s
	}
	return s[len(s)-keep:]
}
