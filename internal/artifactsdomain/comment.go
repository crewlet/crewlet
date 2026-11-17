package artifactsdomain

import (
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/commentanchor"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// COMMENT THREADS (ADR-0154) ride their own subject beneath the artifact
// ([CommentSubject]) and write no history row: the writer reads its verdict
// back from the positions on the thread's rows.

// ThreadStatus is whether a thread still asks for something.
type ThreadStatus string

const (
	// ThreadOpen is a thread nobody has resolved, or one reopened.
	ThreadOpen ThreadStatus = "open"

	// ThreadResolved is a thread somebody marked addressed, naming the
	// version that addressed it.
	ThreadResolved ThreadStatus = "resolved"
)

// ThreadStatuses are both.
var ThreadStatuses = []ThreadStatus{ThreadOpen, ThreadResolved}

// Valid reports whether a status off the wire is one this build knows.
func (s ThreadStatus) Valid() bool { return slices.Contains(ThreadStatuses, s) }

// NotifyReason is why a seat is woken or told about a comment record.
type NotifyReason string

const (
	// NotifyOwner is the artifact's owner.
	NotifyOwner NotifyReason = "owner"

	// NotifyAssignee is the assignee of a work item the artifact is linked
	// to, read when the record was written.
	NotifyAssignee NotifyReason = "assignee"

	// NotifyMention is a seat the remark @-mentions.
	NotifyMention NotifyReason = "mention"

	// NotifyParticipant is a seat that already remarked in the thread.
	NotifyParticipant NotifyReason = "participant"
)

// NotifyReasons are the four.
var NotifyReasons = []NotifyReason{NotifyOwner, NotifyAssignee, NotifyMention, NotifyParticipant}

// Valid reports whether a reason off the wire is one this build knows.
func (r NotifyReason) Valid() bool { return slices.Contains(NotifyReasons, r) }

// Recipient is one seat a comment record wakes or tells, and why.
type Recipient struct {
	// Seat is the seat handle.
	Seat string `json:"seat"`

	// Reason is why the seat is told.
	Reason NotifyReason `json:"reason"`

	// Addressed is a recipient the remark speaks to rather than one kept
	// informed: an agent a person mentioned, or the owner when a person
	// wrote.
	Addressed bool `json:"addressed,omitempty"`
}

// CommentNotify is a comment record's routing snapshot: who is woken (Agents)
// or told (People), computed when the record is formed so delivery reads only
// the record (ADR-0012). The applier writes no row of it.
type CommentNotify struct {
	Agents []Recipient `json:"agents,omitempty"`
	People []Recipient `json:"people,omitempty"`

	// About is what a wake says of where the comment was made.
	About *CommentAbout `json:"about,omitempty"`

	// WorkingFor is whom an agent's turn wrote the record for, so the turn
	// it wakes works for them too (ADR-0109); nil for anybody else's.
	WorkingFor *types.WorkingFor `json:"working_for,omitempty"`
}

// CommentAbout is what a comment's wake says of where it was made, read when the
// record was formed: the artifact's title, kind and head, and the thread's
// version, label and quote.
type CommentAbout struct {
	Title string         `json:"title,omitempty"`
	Kind  artifacts.Kind `json:"kind,omitempty"`
	Head  int            `json:"head,omitempty"`

	// Version is the version the thread was made on, Label where on it as a
	// person reads it, and Quote the page's text it quotes.
	Version int    `json:"version,omitempty"`
	Label   string `json:"label,omitempty"`
	Quote   string `json:"quote,omitempty"`
}

// MaxCommentLabelBytes bounds a wake's label of a thread's place: a section's
// name or a slide's number and the first words of an element, with room to spare.
const MaxCommentLabelBytes = 512

// Empty reports a snapshot naming nobody.
func (n CommentNotify) Empty() bool { return len(n.Agents) == 0 && len(n.People) == 0 }

// MaxNotifyRecipients bounds one routing snapshot's recipients on each side:
// the owner, a work item's assignees, every mention and every participant of
// a thread at its bound, with room to spare.
const MaxNotifyRecipients = artifacts.MaxRemarksPerThread + artifacts.MaxCommentMentions + 16

// CommentOpenPayload opens a thread with its first remark, on the thread's
// subject, create-only. The thread's id is the subject's.
type CommentOpenPayload struct {
	V int `json:"v"`

	// Remark is the opening remark's id, derived from the record's
	// operation in a namespace of its own.
	Remark string `json:"remark"`

	// Body is the remark's Markdown, stored verbatim.
	Body string `json:"body"`

	// Version is the version the thread was made on, and Anchor where on it.
	Version int                  `json:"version"`
	Anchor  commentanchor.Anchor `json:"anchor"`

	// Quote is the text the commenter selected, and Crop a picture of the
	// region, CropType its media type; each empty for none.
	Quote    string `json:"quote,omitempty"`
	Crop     []byte `json:"crop,omitempty"`
	CropType string `json:"crop_type,omitempty"`

	// Mentions are the seat handles the remark names, sorted and unique.
	Mentions []string `json:"mentions,omitempty"`

	// MaxThreads is the per-artifact thread bound the writer decided under,
	// stamped: two opens on their own subjects may race past its snapshot.
	MaxThreads int `json:"max_threads"`

	Notify *CommentNotify `json:"notify,omitempty"`
}

// CommentReplyPayload adds one remark to a thread.
type CommentReplyPayload struct {
	V      int    `json:"v"`
	Remark string `json:"remark"`
	Body   string `json:"body"`

	// ReplyTo is the remark this one answers, empty for the thread itself.
	ReplyTo string `json:"reply_to,omitempty"`

	Mentions []string `json:"mentions,omitempty"`

	// MaxRemarks is the bound the writer decided under
	// ([artifacts.MaxRemarksPerThread]), stamped as [CommentOpenPayload.MaxThreads] is.
	MaxRemarks int `json:"max_remarks"`

	Notify *CommentNotify `json:"notify,omitempty"`
}

// CommentEditPayload replaces one remark's body and mentions, by its author.
type CommentEditPayload struct {
	V        int      `json:"v"`
	Remark   string   `json:"remark"`
	Body     string   `json:"body"`
	Mentions []string `json:"mentions,omitempty"`
}

// CommentRemovePayload removes one remark: its row stays, marked removed, with
// its words cleared.
type CommentRemovePayload struct {
	V      int    `json:"v"`
	Remark string `json:"remark"`
}

// CommentStatusPayload resolves or reopens a thread. Version is the version
// that addressed it, on a resolve; zero on a reopen.
type CommentStatusPayload struct {
	V       int            `json:"v"`
	Version int            `json:"version,omitempty"`
	Notify  *CommentNotify `json:"notify,omitempty"`
}

// The namespaces a thread's and a remark's ids are derived in from their
// creating operation. FIXED for the life of the format, for [AssetNamespace]'s
// reason.
var (
	ThreadNamespace = uuid.MustParse("3d5c8e21-7b4a-5f90-a1c6-2e8f4b7d9a13")
	RemarkNamespace = uuid.MustParse("9b1e4f7a-2c6d-5a38-b0e9-7f3a1c5d8e24")
)

// threadIDOf is the thread an open under opID makes.
func threadIDOf(opID string) uuid.UUID { return uuid.NewSHA1(ThreadNamespace, []byte(opID)) }

// remarkIDOf is the remark an open or a reply under opID makes.
func remarkIDOf(opID string) uuid.UUID { return uuid.NewSHA1(RemarkNamespace, []byte(opID)) }

// ThreadInput opens a thread on an artifact.
type ThreadInput struct {
	Artifact artifacts.ID

	// Body is the opening remark's Markdown, stored verbatim.
	Body string

	// Version is the version the thread is made on; zero is the head.
	Version int

	// Anchor is where on that version; the zero anchor is the whole version.
	Anchor commentanchor.Anchor

	// Quote is the selected text, and Crop a picture of the region in
	// CropType (image/png, image/jpeg or image/webp); each optional.
	Quote    string
	Crop     []byte
	CropType string

	// Mentions are the seat handles the remark names.
	Mentions []string

	// Notify is the routing snapshot the caller computed.
	Notify CommentNotify
}

// ReplyInput adds a remark to a thread.
type ReplyInput struct {
	Artifact artifacts.ID
	Thread   uuid.UUID
	Body     string

	// ReplyTo is the remark this one answers; the zero id answers the
	// thread.
	ReplyTo uuid.UUID

	Mentions []string
	Notify   CommentNotify
}

// RemarkRef names one remark of one thread.
type RemarkRef struct {
	Artifact artifacts.ID
	Thread   uuid.UUID
	Remark   uuid.UUID
}

// RemarkEdit replaces one remark's body and mentions.
type RemarkEdit struct {
	RemarkRef
	Body     string
	Mentions []string
}

// RemarkRemoval removes one remark. Moderated is set by the API for a caller
// who may remove somebody else's remark (an administrator); without it only
// the author may.
type RemarkRemoval struct {
	RemarkRef
	Moderated bool
}

// ResolveInput resolves a thread.
type ResolveInput struct {
	Artifact artifacts.ID
	Thread   uuid.UUID

	// Version is the version that addressed the thread, between the version
	// it was made on and the head; zero is the head.
	Version int

	Notify CommentNotify
}

// ReopenInput reopens a resolved thread.
type ReopenInput struct {
	Artifact artifacts.ID
	Thread   uuid.UUID
	Notify   CommentNotify
}

// Commented is a comment write that made a remark: the thread and the remark
// it made, and the write's result.
type Commented struct {
	Thread uuid.UUID
	Remark uuid.UUID
	Result statelog.Result
}

// CommentThread is one thread as the rows hold it, its remarks oldest first.
type CommentThread struct {
	Artifact artifacts.ID `json:"artifact"`
	ID       uuid.UUID    `json:"id"`

	// Version is the version the thread was made on, and Anchor where on
	// it, exactly as made: where it is on another version is found at read.
	Version int                  `json:"version"`
	Anchor  commentanchor.Anchor `json:"anchor"`

	Quote    string `json:"quote,omitempty" untrusted:"fence"`
	Crop     []byte `json:"crop,omitempty"`
	CropType string `json:"crop_type,omitempty"`

	Status ThreadStatus `json:"status"`

	// ResolvedBy, ResolvedAt and ResolvedVersion are the last resolve's,
	// zero while the thread is open.
	ResolvedBy      iam.Actor `json:"resolved_by"`
	ResolvedAt      time.Time `json:"resolved_at"`
	ResolvedVersion int       `json:"resolved_version,omitempty"`

	CreatedBy iam.Actor `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Position is the packed position of the last record that wrote the
	// thread's row.
	Position int64 `json:"position"`

	Remarks []Remark `json:"remarks"`
}

// Remark is one remark of a thread. A removed remark keeps its place and
// author, and holds no words.
type Remark struct {
	ID       uuid.UUID `json:"id"`
	Author   iam.Actor `json:"author"`
	Body     string    `json:"body" untrusted:"fence"`
	Mentions []string  `json:"mentions,omitempty"`

	// ReplyTo is the remark this one answers; zero for the thread itself.
	ReplyTo uuid.UUID `json:"reply_to,omitzero"`

	CreatedAt time.Time `json:"created_at"`
	EditedAt  time.Time `json:"edited_at,omitzero"`
	Removed   bool      `json:"removed,omitempty"`

	// Position is the packed position of the last record that wrote it.
	Position int64 `json:"position"`
}

// checkNotify refuses a routing snapshot a record could not carry.
func checkNotify(n CommentNotify) error {
	if a := n.About; a != nil {
		switch {
		case len(a.Title) > artifacts.MaxTitleBytes:
			return fmt.Errorf("%w: the comment's wake names a title of %d bytes and the most is %d",
				ErrInvalid, len(a.Title), artifacts.MaxTitleBytes)
		case len(a.Label) > MaxCommentLabelBytes:
			return fmt.Errorf("%w: the comment's wake names a place of %d bytes and the most is %d",
				ErrInvalid, len(a.Label), MaxCommentLabelBytes)
		case int64(len(a.Quote)) > artifacts.MaxCommentQuoteBytes:
			return fmt.Errorf("%w: the comment's wake quotes %d bytes and the most is %d",
				ErrInvalid, len(a.Quote), artifacts.MaxCommentQuoteBytes)
		case a.Head < 0 || a.Version < 0:
			return fmt.Errorf("%w: the comment's wake names no version", ErrInvalid)
		}
	}
	for side, list := range map[string][]Recipient{"agents": n.Agents, "people": n.People} {
		if len(list) > MaxNotifyRecipients {
			return fmt.Errorf("%w: the comment notifies %d %s and the most is %d", ErrInvalid,
				len(list), side, MaxNotifyRecipients)
		}
		for _, r := range list {
			if err := checkName("a notified seat", r.Seat); err != nil {
				return err
			}
			if !r.Reason.Valid() {
				return fmt.Errorf("%w: notify reason %q is not one of %s", ErrInvalid, r.Reason,
					words.Or(NotifyReasons))
			}
		}
	}
	return nil
}

// notifyOf is a snapshot as a payload carries it: nil for nobody.
func notifyOf(n CommentNotify) *CommentNotify {
	if n.Empty() {
		return nil
	}
	return &n
}
