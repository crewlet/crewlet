package evals

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Files is a conversation's file store in memory, keyed on the owner as the
// real one is: another person's file, or one of another conversation, is not
// found.
type Files struct {
	now func() time.Time

	mu    sync.Mutex
	files map[uuid.UUID]storedFile
	// refuse, when set, is what every later Store answers.
	refuse error
}

type storedFile struct {
	assistant.StoredFile
	content []byte
}

// NewFiles is an empty store.
func NewFiles(now func() time.Time) *Files {
	return &Files{now: now, files: map[uuid.UUID]storedFile{}}
}

var _ assistant.Files = (*Files)(nil)

// Store keeps one file under the writer's owner.
func (f *Files) Store(_ context.Context, w assistant.Write, n assistant.NewFile) (wire.FileRef, error) {
	if len(n.Content) > wire.FileMaxBytes {
		return wire.FileRef{}, fmt.Errorf("%w: %d bytes", assistant.ErrFileRefused, len(n.Content))
	}
	ref := wire.FileRef{FileID: n.ID, Name: n.Name, MediaType: n.MediaType,
		Size: int64(len(n.Content)), Download: wire.FilePath(n.Conversation, n.ID)}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse != nil {
		return wire.FileRef{}, f.refuse
	}
	f.files[n.ID] = storedFile{StoredFile: assistant.StoredFile{FileRef: ref, Owner: w.Owner,
		Conversation: n.Conversation, Turn: n.Turn, Origin: n.Origin, CreatedAt: f.now()},
		content: slices.Clone(n.Content)}
	return ref, nil
}

// Open is one file and its bytes, as its owner.
func (f *Files) Open(ctx context.Context, owner, conversation, file uuid.UUID) (assistant.StoredFile, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.files[file]
	if !ok || held.Owner != owner || held.Conversation != conversation {
		return assistant.StoredFile{}, nil, assistant.ErrNotFound
	}
	return held.StoredFile, slices.Clone(held.content), nil
}

// Stat is one file's record, as its owner.
func (f *Files) Stat(ctx context.Context, owner, conversation, file uuid.UUID) (assistant.StoredFile, error) {
	held, _, err := f.Open(ctx, owner, conversation, file)
	return held, err
}

// RefuseStores makes every later Store fail with err, as a store that keeps
// nothing more does.
func (f *Files) RefuseStores(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse = err
}

// Attach keeps an image the person uploaded, as the upload route does.
func (f *Files) Attach(owner, conversation uuid.UUID, name string, content []byte) (wire.FileRef, error) {
	mediaType, kept, err := assistant.SniffFile(name, content, wire.AttachmentMediaTypes)
	if err != nil {
		return wire.FileRef{}, err
	}
	return f.Store(context.Background(), assistant.Write{Owner: owner}, assistant.NewFile{
		ID: ID("attachment/" + kept), Conversation: conversation, Origin: wire.FileAttached,
		Name: kept, MediaType: mediaType, Content: content})
}

// DeepRunScript is what one scripted deep-work run does: it reports its
// lines, makes its Crewlet calls in order through the run's surface (a call
// the surface answers with a refusal or a waiting note ends the calls), leaves
// its files in its outbox, and ends.
type DeepRunScript struct {
	Lines  []string
	Calls  []llm.ToolCall
	Files  []assistant.DeepFile
	Report string
	End    assistant.DeepEnd
	Reason string
	// Input and Output are the tokens the run reports spending.
	Input, Output int
	// Hold, when set, keeps the run going until it is stopped.
	Hold bool
	// Err is a run that could not start.
	Err error
}

// DeepScript is a scripted deep-work backend: each run takes the next script,
// and every run, every answer its surface gave and every box kept or discarded
// is recorded for a suite to read.
type DeepScript struct {
	// Unready is what Ready answers.
	Unready error

	mu        sync.Mutex
	scripts   []DeepRunScript
	runs      []assistant.DeepRun
	answers   [][]toolloop.ToolResult
	kept      []assistant.DeepBox
	discarded []assistant.DeepBox
	boxes     int
	// Started is told each run's id as it starts, when set.
	Started chan string
}

// NewDeepScript scripts runs, in order.
func NewDeepScript(runs ...DeepRunScript) *DeepScript { return &DeepScript{scripts: runs} }

var _ assistant.DeepWork = (*DeepScript)(nil)

// ErrDeepScriptEnded is a run asked for past the script.
var ErrDeepScriptEnded = errors.New("evals: the deep-work script has ended")

// Ready answers the scripted readiness.
func (d *DeepScript) Ready(context.Context) error { return d.Unready }

// Run plays the next script.
func (d *DeepScript) Run(ctx context.Context, r assistant.DeepRun) (assistant.DeepOutcome, error) {
	d.mu.Lock()
	d.runs = append(d.runs, r)
	if len(d.scripts) == 0 {
		d.mu.Unlock()
		return assistant.DeepOutcome{}, ErrDeepScriptEnded
	}
	s := d.scripts[0]
	d.scripts = d.scripts[1:]
	box := assistant.DeepBox{Placement: "fake", Agent: "claude-code"}
	if r.Continue != nil {
		box = *r.Continue
	} else {
		d.boxes++
		box.Sandbox, box.Session = "box-"+strconv.Itoa(d.boxes), "session-"+strconv.Itoa(d.boxes)
	}
	started := d.Started
	d.mu.Unlock()
	if s.Err != nil {
		return assistant.DeepOutcome{}, s.Err
	}
	if started != nil {
		started <- r.ID
	}
	if r.Progress != nil && len(s.Lines) > 0 {
		r.Progress(assistant.DeepProgress{Lines: s.Lines, Spent: s.Input + s.Output})
	}
	var answers []toolloop.ToolResult
	for _, call := range s.Calls {
		res, err := r.Surface.Admit(call.Name, call.Arguments)(ctx, call)
		if err != nil {
			break
		}
		answers = append(answers, res)
		if res.Failed || res.Output == "" {
			break
		}
	}
	d.mu.Lock()
	d.answers = append(d.answers, answers)
	d.mu.Unlock()
	out := assistant.DeepOutcome{End: s.End, Report: s.Report, Reason: s.Reason, Box: box,
		Files: s.Files, InputTokens: s.Input, OutputTokens: s.Output}
	if s.Hold {
		<-ctx.Done()
		out.End = assistant.DeepEndStopped
	}
	if out.End == "" {
		out.End = assistant.DeepEndFinished
	}
	return out, nil
}

// Keep records a kept box.
func (d *DeepScript) Keep(_ context.Context, b assistant.DeepBox) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.kept = append(d.kept, b)
	return nil
}

// Discard records a discarded box.
func (d *DeepScript) Discard(_ context.Context, b assistant.DeepBox) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.discarded = append(d.discarded, b)
}

// Runs are the runs started so far.
func (d *DeepScript) Runs() []assistant.DeepRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.runs)
}

// Answers are what each run's calls were answered, by run.
func (d *DeepScript) Answers() [][]toolloop.ToolResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.answers)
}

// Kept and Discarded are the boxes kept for a continuation and the ones
// ended.
func (d *DeepScript) Kept() []assistant.DeepBox {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.kept)
}

// Discarded is the boxes ended.
func (d *DeepScript) Discarded() []assistant.DeepBox {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.discarded)
}
