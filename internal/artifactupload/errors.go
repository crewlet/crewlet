package artifactupload

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
)

// The sentinels. Each is a fact a surface answers differently, which is why
// they are separate. Every refusal of the domain's writer
// (internal/artifactsdomain) passes through unchanged beside them.
var (
	// ErrInvalid reports a request this package refuses, naming the field
	// and what to change (400 or 422).
	ErrInvalid = errors.New("artifactupload: invalid")

	// ErrMediaType reports a file whose type an artifact may not publish
	// (415 `media_type_refused`).
	ErrMediaType = errors.New("artifactupload: the file's type is not one an artifact may publish")

	// ErrUploadNotFound reports an upload this principal never began, or one
	// whose record is gone: another principal's upload answers exactly this,
	// so an upload id is no oracle (404 `upload_not_found`).
	ErrUploadNotFound = errors.New("artifactupload: no such upload")

	// ErrUploadExpired reports an upload past its transfer window: begin
	// another (410 `upload_expired`).
	ErrUploadExpired = errors.New("artifactupload: the upload's window has closed; begin another upload")

	// ErrUploadConsumed reports an upload a finalize has claimed or used,
	// or a file already transferred: a name is put once (409
	// `upload_consumed`).
	ErrUploadConsumed = errors.New("artifactupload: the upload is already used")

	// ErrCapabilityUnknown reports a capability this build does not serve
	// (422 `capability_unknown`).
	ErrCapabilityUnknown = errors.New("artifactupload: the capability is not one this build serves")

	// ErrContractUnknown reports a runtime contract this build does not
	// serve (422 `contract_unknown`).
	ErrContractUnknown = errors.New("artifactupload: the runtime contract is not one this build serves")

	// ErrCredential reports a credential shape in what an agent publishes
	// (422 `credential_refused`): never silently redacted, which would
	// ship a broken page.
	ErrCredential = errors.New("artifactupload: the content carries a credential")
)

// Sentence is a refusal of the publish pipeline, as a person and a model read
// it: the sentence, without the package names and the class words a Go error
// carries in front of it — this package's, those of the packages a publish
// passes through (a media type, the policy linter, an edit, a vendored file, a
// format's reader and its starter), and the domain's writer's
// ([artifactsdomain.Sentence]), since every refusal of that writer passes
// through here unchanged.
//
// THE ONE FUNCTION EVERY SURFACE WORDS AN ARTIFACT'S REFUSAL BY: the tools
// and the artifacts API each kept a list of package names to strip, which knew
// no class word, so a refusal read "publish_artifact refused that: invalid:
// kind …".
func Sentence(err error) string {
	text := artifactsdomain.Sentence(err)
	for _, cut := range sentenceCuts {
		text = strings.ReplaceAll(text, cut, "")
	}
	return text
}

// sentenceCuts are what [Sentence] removes beyond the domain's own, the class
// words before the package names they open with.
var sentenceCuts = []string{
	ErrInvalid.Error() + ": ", canvasformat.ErrInvalid.Error() + ": ",
	"artifactupload: ", "mediatype: ", "lint: ", "edits: ", "vendor: ",
	"slideformat: ", "canvasformat: ", "dashspec: ", "docindex: ",
}

// UploadMismatch is a file whose stored bytes are not the ones its upload
// declared (422 `upload_mismatch`): its object is left for the collector, and
// the path is uploaded again in a new upload.
type UploadMismatch struct {
	Path   string
	Reason string
}

func (e *UploadMismatch) Error() string {
	return fmt.Sprintf("artifactupload: %q is not the bytes its upload declared: %s; upload it again",
		e.Path, e.Reason)
}

// UploadMissing is a publish naming files whose bytes have not arrived (409
// `upload_missing`): transfer them, then publish again.
type UploadMissing struct {
	Paths []string
}

func (e *UploadMissing) Error() string {
	return fmt.Sprintf("artifactupload: the bytes of %s have not arrived; transfer them, then "+
		"publish again", strings.Join(e.Paths, ", "))
}

// UploadsOutstanding is an intent the reservation budget refuses (429
// `uploads_outstanding`): Scope is whose budget (`principal` or `company`),
// Limit and Held the figure in bytes or, when Uploads is set, in uploads, and
// Until when the earliest hold in that scope lapses on its own.
type UploadsOutstanding struct {
	Scope   string
	Limit   int64
	Held    int64
	Uploads bool
	Until   time.Time
}

func (e *UploadsOutstanding) Error() string {
	unit := "bytes"
	if e.Uploads {
		unit = "uploads"
	}
	return fmt.Sprintf("artifactupload: the %s already holds %d of its %d %s reserved by uploads "+
		"no publish has finished; finish or abandon them (the earliest is released at %s)",
		e.Scope, e.Held, e.Limit, unit, e.Until.UTC().Format(time.RFC3339))
}

// TurnQuota is an agent's publish past a per-turn guardrail (`turn_quota`):
// the turn has done all it should, and the model is told to stop and report.
type TurnQuota struct {
	// What is the guardrail: `publishes`, `publishes_on_artifact` or
	// `bytes`.
	What  string
	Limit int64
	Used  int64
}

func (e *TurnQuota) Error() string {
	return fmt.Sprintf("artifactupload: this turn has used %d of its %d %s; stop publishing and "+
		"report what was done", e.Used, e.Limit, strings.ReplaceAll(e.What, "_", " "))
}

// ContentRefused is a file whose bytes are not what its type promises (422
// `content_refused`): text that is not UTF-8, or binary bytes that do not begin
// as their type's do, which is how a polyglot passes for a benign file.
type ContentRefused struct {
	Path   string
	Reason string
}

func (e *ContentRefused) Error() string {
	return fmt.Sprintf("artifactupload: %q: %s", e.Path, e.Reason)
}
