package assistant

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE DEEP-WORK SEAMS (ADR-0053), declared here by their consumer and satisfied
// by the engine: the coding CLI's run in a box (internal/engine's deep work over
// internal/sandbox and the MCP bridge, which this package may not import), and
// the store a conversation's files are kept in.

// DeepWork runs a deep-work run: the assistant's coding CLI with its own tools
// (shell, files, web search and fetch, image reading) in a box, whose only way
// into Crewlet is [DeepRun.Surface], served to it over the engine's bridge.
type DeepWork interface {
	// Ready is why a run cannot start on this node now (no deep_work entry,
	// no placement, a host that cannot confine one), nil when it can.
	Ready(ctx context.Context) error
	// Run launches the run, watches it and collects it, and returns when it
	// ends, reaches a limit, or ctx ends, which kills it. The error is a run
	// that could not be started or watched at all; a run that started and
	// failed is an outcome ([DeepOutcome.End]).
	Run(ctx context.Context, r DeepRun) (DeepOutcome, error)
	// Keep holds a finished run's box for its continuation after the
	// person's decision: paused where its backend pauses, and reaped by its
	// own deadline if nobody continues it.
	Keep(ctx context.Context, b DeepBox) error
	// Discard ends a box no run will continue. Best effort: a box it cannot
	// reach is reaped by its backend's deadline.
	Discard(ctx context.Context, b DeepBox)
}

// DeepRun is one deep-work run as the turn hands it over.
type DeepRun struct {
	// ID names the run: its bridge session and its box's label. Unique per
	// run, and never a secret.
	ID    string
	Owner uuid.UUID
	Turn  uuid.UUID
	// System is the run's standing instructions and Brief its ask: the goal
	// and plan, or for a continuation the person's decision.
	System, Brief string
	// Continue is the box and CLI session a parked run left, nil for a new
	// run.
	Continue *DeepBox
	// Inputs are files written into the box's inputs directory before the
	// run starts: the images the person attached.
	Inputs []DeepFile
	// Surface is the run's only way into Crewlet: the person's own
	// assistant tools, every call the person's request (ADR-0049).
	Surface DeepSurface
	// Timeout, Steps and Tokens are the run's caps: its wall clock, its
	// CLI's agentic rounds, and the tokens it may spend before it is
	// stopped.
	Timeout time.Duration
	Steps   int
	Tokens  int
	// Progress is told what the run is doing while it works. Never
	// blocking, and never called after Run returns.
	Progress func(DeepProgress)
}

// DeepSurface is what the bridge serves a run: the four things it asks of a
// live tool surface (internal/api/mcpbridge's Surface, which this package may
// not import, so it is restated here and satisfied structurally).
type DeepSurface interface {
	Revision() uint64
	ToolDefs() []llm.ToolDef
	AnnotationsOf(name string) mcp.Annotations
	Execute(ctx context.Context, call llm.ToolCall) (toolloop.ToolResult, error)
}

// DeepProgress is one live reading of a run.
type DeepProgress struct {
	// Lines are what the run said since the last reading, redacted.
	Lines []string
	// Spent is every token the run has reported so far.
	Spent int
}

// DeepEnd is how a run ended.
type DeepEnd string

const (
	// DeepEndFinished is a run that ended on its own with its report.
	DeepEndFinished DeepEnd = "finished"
	// DeepEndFailed is a run whose CLI failed, or whose box would not start.
	DeepEndFailed DeepEnd = "failed"
	// DeepEndTimeout is a run stopped at its wall clock.
	DeepEndTimeout DeepEnd = "timeout"
	// DeepEndTokens is a run stopped at its token cap.
	DeepEndTokens DeepEnd = "tokens"
	// DeepEndStopped is a run whose turn was stopped, or whose node is
	// shutting down.
	DeepEndStopped DeepEnd = "stopped"
)

// DeepOutcome is what a run ended with.
type DeepOutcome struct {
	End DeepEnd
	// Report is the run's own account of what it did, redacted; Reason is
	// why a run that did not finish ended, in the engine's words.
	Report string
	Reason string
	// Box is where the run's files and CLI session are, for a continuation.
	Box DeepBox
	// Files are what the run left in its outbox, at most
	// [wire.DeliveredFilesPerRun], each at most [wire.FileMaxBytes].
	Files []DeepFile
	// InputTokens and OutputTokens are what the run spent, the input's
	// cached share included.
	InputTokens, OutputTokens int
}

// DeepBox is a run's box and its CLI session: everything a continuation
// needs, persisted on the parked turn.
type DeepBox struct {
	Placement string `json:"placement"`
	Sandbox   string `json:"sandbox"`
	Agent     string `json:"agent"`
	Session   string `json:"session,omitempty"`
}

// DeepFile is one file going into a box or coming out of one.
type DeepFile struct {
	Name    string
	Content []byte
}

// Files is where a conversation's files are kept: the images a person attaches
// and the files a deep run delivers, sealed and owner-keyed like the rest of a
// conversation (ADR-0052), and purged with it.
type Files interface {
	// Store keeps one file, refusing one past [wire.FileMaxBytes].
	Store(ctx context.Context, w Write, f NewFile) (wire.FileRef, error)
	// Open is one file and its bytes, read as its owner: another person's,
	// or one of another conversation, is [ErrNotFound].
	Open(ctx context.Context, owner, conversation, file uuid.UUID) (StoredFile, []byte, error)
}

// NewFile is one file to keep.
type NewFile struct {
	ID           uuid.UUID
	Conversation uuid.UUID
	Turn         uuid.UUID
	Origin       wire.FileOrigin
	Name         string
	// MediaType is the type [SniffFile] answered for Content.
	MediaType string
	Content   []byte
}

// StoredFile is a kept file's record.
type StoredFile struct {
	wire.FileRef
	Owner        uuid.UUID
	Conversation uuid.UUID
	Turn         uuid.UUID
	Origin       wire.FileOrigin
	CreatedAt    time.Time
}
