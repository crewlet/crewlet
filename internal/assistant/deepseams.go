package assistant

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE DEEP-WORK SEAMS (ADR-0054), declared here by their consumer and satisfied
// by the engine: the harness's run in a box (internal/engine's deep work over
// internal/sandbox and the MCP bridge, which this package may not import), and
// the store a conversation's files are kept in.

// DeepWork runs a deep-work run: the assistant's harness with its own tools
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
	// Seat is the person's own seat handle, the spend history's name for
	// whose run it was.
	Seat string
	Turn uuid.UUID
	// System is the run's standing instructions and Brief its ask: the goal
	// and plan, or for a continuation the person's decision.
	System, Brief string
	// Continue is the box and harness session a parked run left, nil for a new
	// run.
	Continue *DeepBox
	// Inputs are files written into the box's inputs directory before the
	// run starts: the images the person attached.
	Inputs []DeepFile
	// Surface is the run's only way into Crewlet: the person's own
	// assistant tools, every call the person's request (ADR-0050).
	Surface DeepSurface
	// Timeout, Steps and Tokens are the run's caps: its wall clock, its
	// harness's agentic rounds, and the tokens it may spend before it is
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
	Admit(name string, args map[string]any) toolloop.Admitted
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
	// DeepEndFailed is a run whose harness failed, or whose box would not start.
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
	// Box is where the run's files and harness session are, for a continuation.
	Box DeepBox
	// Files are what the run left in its outbox, at most
	// [wire.DeliveredFilesPerRun], each at most [wire.FileMaxBytes].
	Files []DeepFile
	// InputTokens and OutputTokens are what the run spent, the input's
	// cached share included.
	InputTokens, OutputTokens int
}

// DeepBox is a run's box and its harness session: everything a continuation
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
// conversation (ADR-0053), and purged with it. A draft change that names a
// file is held to it too ([turn.draftChange]).
type Files interface {
	// Store keeps one file, refusing one past [wire.FileMaxBytes]. A file a
	// condition of the node kept out is answered as [*Unkept].
	Store(ctx context.Context, w Write, f NewFile) (wire.FileRef, error)
	// Open is one file and its bytes, read as its owner: another person's,
	// or one of another conversation, is [ErrNotFound].
	Open(ctx context.Context, owner, conversation, file uuid.UUID) (StoredFile, []byte, error)
	// Stat is one file's record without its bytes, by the same rule as Open.
	Stat(ctx context.Context, owner, conversation, file uuid.UUID) (StoredFile, error)
}

// Unkept is a file [Files.Store] did not keep because of a condition of the
// node: the company's file storage full, a store that did not take the bytes,
// a log that refused the file's record. Said is that condition as a model is
// told it, a clause with nothing of the error's own text, and Cause is the
// failure itself, beneath for the log and for the surface that tells a person
// (the upload's route answers it through the cause).
//
// WORDED BY WHOEVER STORES THE FILE (internal/engine, through the condition
// table internal/agent/builtin keeps), because nothing under
// internal/assistant may reach that table, and a deep run's delivery note
// said only that a file "could not be kept" whatever kept it out, so the model
// could tell the person neither why nor whether a later run would fare better.
type Unkept struct {
	Said  string
	Cause error
}

func (e *Unkept) Error() string { return fmt.Sprintf("assistant: file not kept: %v", e.Cause) }

// Unwrap makes [Unkept] errors.Is-comparable with its cause.
func (e *Unkept) Unwrap() error { return e.Cause }

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
