package wire

import (
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/words"
)

// DEEP WORK AND FILES (ADR-0054): the values the drawer reads about a turn that
// escalated into a harness, and about the files a person
// attaches and a run delivers.
//
// A deep run is ONE STEP of its turn ([StepEvent.Deep]), re-sent as it
// progresses: its state, its clock, the share of its step and budget caps it
// has used and the last lines of what it is doing. Each Crewlet tool it calls is
// a step of the turn like any other, nested under it ([StepEvent.Parent]). The
// browser is never sent the run's token count, only the share of its cap.

// DeepState is where a deep run stands.
type DeepState string

const (
	// DeepStarting is a run whose box is being prepared.
	DeepStarting DeepState = "starting"
	// DeepRunning is a run at work.
	DeepRunning DeepState = "running"
	// DeepWaiting is a run that proposed a change and stopped for the
	// person's decision; the approval continues it.
	DeepWaiting DeepState = "waiting"
	// DeepFinished is a run that ended with its report.
	DeepFinished DeepState = "finished"
	// DeepStopped is a run the person stopped.
	DeepStopped DeepState = "stopped"
	// DeepFailed is a run that ended without its report: a limit reached, a
	// box that could not start, a harness that failed.
	DeepFailed DeepState = "failed"
)

// DeepStates are the six, in declaration order.
var DeepStates = []DeepState{DeepStarting, DeepRunning, DeepWaiting, DeepFinished, DeepStopped, DeepFailed}

// Valid reports whether s is a state this build knows.
func (s DeepState) Valid() bool { return slices.Contains(DeepStates, s) }

// DeepActivityLines is the most lines of a run's own account a step carries:
// what it is doing now, never its history, which the run's report says.
const DeepActivityLines = 8

// DeepProgress is a deep run as its step shows it.
type DeepProgress struct {
	State DeepState `json:"state"`
	// Goal is the task the run was given, in the assistant's words.
	Goal string `json:"goal"`
	// StartedAt and DeadlineAt are the run's clock: when it began and the
	// latest it may run until.
	StartedAt  time.Time `json:"started_at"`
	DeadlineAt time.Time `json:"deadline_at"`
	// Steps and MaxSteps are the run's Crewlet calls so far and its cap.
	Steps    int `json:"steps"`
	MaxSteps int `json:"max_steps"`
	// BudgetPercent is the share of the run's token cap it has used, 0..100.
	BudgetPercent int `json:"budget_percent"`
	// Activity is the last lines of what the run says it is doing, redacted.
	Activity []string `json:"activity,omitempty"`
	// Reason is why a failed run ended, in the engine's words.
	Reason string `json:"reason,omitempty"`
}

// validate reports what is wrong with a run's progress.
func (d DeepProgress) validate() error {
	switch {
	case !d.State.Valid():
		return fmt.Errorf("%w: deep state %q is not one of %s", ErrInvalid, d.State,
			words.Or(DeepStates))
	case d.BudgetPercent < 0 || d.BudgetPercent > 100:
		return fmt.Errorf("%w: a deep run's budget share is %d%%", ErrInvalid, d.BudgetPercent)
	case len(d.Activity) > DeepActivityLines:
		return fmt.Errorf("%w: a deep step carries %d activity lines, over %d", ErrInvalid,
			len(d.Activity), DeepActivityLines)
	}
	return nil
}

// FileMaxBytes is the largest file a person attaches or a run delivers: five
// mebibytes, a screenshot at full resolution or a brand sheet, and well inside
// what one download answers.
const FileMaxBytes = 5 << 20

// AttachmentsPerTurn is the most files one question carries.
const AttachmentsPerTurn = 4

// DeliveredFilesPerRun is the most files one deep run hands back.
const DeliveredFilesPerRun = 10

// AttachmentMediaTypes are what a person may attach: images, which the run
// reads with its image reader. Sniffed from the bytes, never taken from the
// browser's claim.
var AttachmentMediaTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

// FileMediaTypes are what a run may deliver: images, vector art, tables,
// documents and text. Never HTML or a script: a download is served as an
// attachment, and nothing a run wrote is ever rendered as a page.
var FileMediaTypes = []string{
	"image/png", "image/jpeg", "image/webp", "image/gif", "image/svg+xml",
	"text/csv", "text/markdown", "text/plain", "application/json", "application/pdf",
}

// FileOrigin is where a stored file came from.
type FileOrigin string

const (
	// FileAttached is a file the person attached to a question.
	FileAttached FileOrigin = "attached"
	// FileDelivered is a file a deep run handed back.
	FileDelivered FileOrigin = "delivered"
)

// FileOrigins are the two.
var FileOrigins = []FileOrigin{FileAttached, FileDelivered}

// Valid reports whether o is an origin this build knows.
func (o FileOrigin) Valid() bool { return slices.Contains(FileOrigins, o) }

// FileRef is one stored file as a message or a card names it.
type FileRef struct {
	FileID    uuid.UUID `json:"file_id"`
	Name      string    `json:"name"`
	MediaType string    `json:"media_type"`
	Size      int64     `json:"size"`
	// Download is the owner's route for its bytes, served as an attachment.
	Download string `json:"download"`
}

// FileCard is the files a deep run delivered, each downloadable by the
// person. An image previews in the drawer through an `<img>` element, which
// runs no script an SVG carries.
type FileCard struct {
	Files []FileRef `json:"files"`
}

// FileStored answers an attachment's upload:
// `POST /assistant/conversations/{id}/files`.
type FileStored struct {
	File FileRef `json:"file"`
}

// FilePath is the owner's route for one stored file's bytes.
func FilePath(conversation, file uuid.UUID) string {
	return "/assistant/conversations/" + conversation.String() + "/files/" + file.String()
}

// validate reports what is wrong with a file card.
func (c FileCard) validate() error {
	if len(c.Files) == 0 || len(c.Files) > DeliveredFilesPerRun {
		return fmt.Errorf("%w: a file card names %d files, want 1..%d", ErrInvalid,
			len(c.Files), DeliveredFilesPerRun)
	}
	for _, f := range c.Files {
		if !slices.Contains(FileMediaTypes, f.MediaType) {
			return fmt.Errorf("%w: a file card names a %q file", ErrInvalid, f.MediaType)
		}
		if f.Size < 0 || f.Size > FileMaxBytes {
			return fmt.Errorf("%w: a file card names a file of %d bytes", ErrInvalid, f.Size)
		}
	}
	return nil
}
