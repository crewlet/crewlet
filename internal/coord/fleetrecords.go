package coord

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/iam"
)

// WHAT THE FLEET'S SMALL RECORDS NAME: every bucket but the runs' and a few
// declared classes holds records of a few fields read at the read term, a
// small value ([SmallValueBytes], internal/coord/kv's holdings). Each text
// field such a record takes from its writer is bounded here, as the store
// encodes it (a byte JSON escapes counted as six), and every backend refuses
// one past its bound with [ErrValueTooLarge] before anything is written: each
// is an identifier or a word the engine chose, which a cut would turn into one
// naming nothing, so it is refused, never cut.

// MaxLedgerDetailBytes is the most a completion's detail holds
// ([Ledger.Record]): the turn's id that did the work, a UUID, or a short tag a
// watcher names its touches by ("confluence_touch").
const MaxLedgerDetailBytes = 128

// MaxSeenMarkBytes is the most a seen-through mark holds ([SeenMarks]): a
// state log position's token, a stream name of at most 64 bytes, a generation
// and a sequence (internal/statelog's Position.String), under a hundred.
const MaxSeenMarkBytes = 128

// MaxFollowReasonBytes is the most a follow's reason holds ([Follows.Follow]):
// a word of internal/notify's FollowReason, the longest a dozen bytes.
const MaxFollowReasonBytes = 64

// MaxApplyNameBytes is the most each name a node's apply status holds
// ([NodeApply]): the revisions it attempted, serves and boots, each a UUID the
// store minted, and its status, a word of internal/configplane's ApplyStatus.
const MaxApplyNameBytes = 64

// MaxSessionClaimPartBytes is the most each part of a session claim holds
// ([SessionClaim]): the session's id, held where a box names it to
// internal/executorkeep's SessionIDMaxBytes, which a test holds this to, and
// its run's turn and its job, each a UUID.
const MaxSessionClaimPartBytes = 128

// holdField refuses record's field holding value past max bytes as stored,
// with [ErrValueTooLarge] naming both and the size.
func holdField(record, field, value string, max int) error {
	if size := encodedText(value); size > max {
		return fmt.Errorf("%w: %s's %s of %d bytes as stored, past the %d it holds", ErrValueTooLarge,
			record, field, size, max)
	}
	return nil
}

// CheckLedgerDetail refuses a completion's detail past [MaxLedgerDetailBytes].
func CheckLedgerDetail(detail string) error {
	return holdField("a completion", "detail", detail, MaxLedgerDetailBytes)
}

// CheckSeenMark refuses a seen-through mark past [MaxSeenMarkBytes].
func CheckSeenMark(mark string) error {
	return holdField("a seen-through mark", "position", mark, MaxSeenMarkBytes)
}

// CheckFollowReason refuses a follow's reason past [MaxFollowReasonBytes].
func CheckFollowReason(reason string) error {
	return holdField("a follow", "reason", reason, MaxFollowReasonBytes)
}

// Check refuses a channel whose parties are not seat handles' width: each is
// at most [iam.MaxLogin] as stored.
func (c Channel) Check() error {
	if err := holdField("a channel", "requester", c.Requester, iam.MaxLogin); err != nil {
		return err
	}
	return holdField("a channel", "target", c.Target, iam.MaxLogin)
}

// Check refuses a mailbox record whose handle is past a seat handle's width,
// [iam.MaxLogin] as stored.
func (r MailboxRecord) Check() error {
	return holdField("a mailbox record", "handle", r.Handle, iam.MaxLogin)
}

// Check refuses an apply status whose revisions or status are past
// [MaxApplyNameBytes]. Its error is cut where it is written
// ([TruncateApplyError]), since it is a failure's words, not a name.
func (a NodeApply) Check() error {
	for _, f := range []struct{ field, value string }{
		{"revision", a.RevisionID}, {"serving revision", a.Serving}, {"boot revision", a.Boots},
		{"status", a.Status},
	} {
		if err := holdField("an apply status", f.field, f.value, MaxApplyNameBytes); err != nil {
			return err
		}
	}
	return nil
}
