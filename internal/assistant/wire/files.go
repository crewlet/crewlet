package wire

import (
	"slices"

	"github.com/google/uuid"
)

// FILES: the images a person attaches to a question, which the turn hands to
// its model, kept sealed and owner-keyed with the conversation (ADR-0053).

// FileMaxBytes is the largest file a person attaches: five mebibytes, a
// screenshot at full resolution, and well inside what one download answers.
const FileMaxBytes = 5 << 20

// AttachmentsPerTurn is the most files one question carries.
const AttachmentsPerTurn = 4

// AttachmentMediaTypes are what a person may attach: images, which the turn
// hands to its model. Sniffed from the bytes, never taken from the browser's
// claim.
var AttachmentMediaTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

// FileOrigin is where a stored file came from.
type FileOrigin string

// FileAttached is a file the person attached to a question.
const FileAttached FileOrigin = "attached"

// FileOrigins are the origins a stored file has.
var FileOrigins = []FileOrigin{FileAttached}

// Valid reports whether o is an origin this build knows.
func (o FileOrigin) Valid() bool { return slices.Contains(FileOrigins, o) }

// FileRef is one stored file as a message names it.
type FileRef struct {
	FileID    uuid.UUID `json:"file_id"`
	Name      string    `json:"name"`
	MediaType string    `json:"media_type"`
	Size      int64     `json:"size"`
	// Download is the owner's route for its bytes, served as an attachment.
	Download string `json:"download"`
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
