package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A JOB'S NEWEST SUMMARY, KEPT APART (ADR-0073's amendment "a session's newest
// summary is kept apart"). Every box read — a collection's, a stop's, a loss's —
// asks the job's main thread for its harness's newest compaction summary in
// the same one read ([Coordinator.readMainThread]), scrubbed of what the job's
// capture is scrubbed of ([Coordinator.askSummary]), and keeps it as a sealed
// object of its own beside the session's take, never inside it, so a session
// refused, too large or lost keeps it ([Coordinator.keepSummary]). The run's
// row names it ([PendingRun.Summary]) and the turn's end names it on the
// executor log ([Coordinator.publishEnd]), whose row releases it with its own.
// KEPT WITHIN THE READ, never after it: a stop's and a loss's ending publish
// the turn's end as soon as the read returns, and must find it named.

// summaryAsk is what a box read asks of its job's main thread's summary:
// whether, and the values it is scrubbed of.
type summaryAsk struct {
	asked bool
	scrub []string
}

// askSummary is what run's box read asks of its job's newest summary: asked
// where its runner reads summaries and this node keeps sessions
// ([Coordinator.capturing]), scrubbed of the values its capture is
// ([Coordinator.scrubValues]), read within a row read's room. Values that
// cannot be read ask none: a summary is never read where it cannot be scrubbed.
func (c *Coordinator) askSummary(ctx context.Context, run PendingRun, box Sandbox, runner Runner) summaryAsk {
	if !ThreadTraitsOf(runner).Summaries || !c.capturing() {
		return summaryAsk{}
	}
	read, cancel := c.rowReadContext(ctx, c.recordBudget)
	defer cancel()
	scrub, err := c.scrubValues(read, run, box)
	if err != nil {
		log.WarnContext(ctx, "executor_session_summary_unkept", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"detail", "the values the job's session is scrubbed of could not be read, so its newest summary was "+
				"not read: "+err.Error())
		return summaryAsk{}
	}
	return summaryAsk{asked: true, scrub: scrub}
}

// keepSummary keeps the summary found on run's job's main thread where ask
// asked one: sealed and uploaded as an object of its own
// ([SessionKeep.PutSummary]) and named on the run's row
// ([PendingStore.StampSummary]), unless the row names the same summary
// already, under a key still fresh ([SummaryStamp.names]). Each step within a
// record's room ([summaryKeepRoom]). Best effort: what fails is said
// (`executor_session_summary_unkept`) and changes nothing else of the read.
func (c *Coordinator) keepSummary(ctx context.Context, run PendingRun, ask summaryAsk, found ThreadSummary) {
	switch {
	case !ask.asked:
		return
	case found.Unread != "":
		log.InfoContext(ctx, "executor_session_summary_unread", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"detail", found.Unread)
		return
	case found.Text == "":
		return
	}
	digest := summaryDigest(found.Text)
	if run.Summary != nil && run.Summary.names(digest, c.now()) {
		return
	}
	unkept := func(why string) {
		log.WarnContext(ctx, "executor_session_summary_unkept", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"bytes", found.Bytes, "detail", why)
	}
	upload, uploaded := context.WithTimeout(ctx, c.recordBudget+objstore.PaceFor(executorkeep.SummarySealedMaxBytes))
	policy, err := c.keep.Policy(upload)
	if err != nil || !policy.Keep {
		uploaded()
		if err != nil {
			unkept(fmt.Sprintf("the company's session policy could not be read: %v", err))
		}
		return
	}
	sealed, err := c.keep.PutSummary(upload, run.AgentHandle, run.TurnID, strings.NewReader(found.Text))
	uploaded()
	if err != nil {
		unkept(fmt.Sprintf("the summary could not be sealed and uploaded: %v", err))
		return
	}
	stamp := SummaryStamp{LaunchSeq: run.LaunchSeq, Object: sealed.Object.Key.String(),
		ObjectHash: string(sealed.Object.Hash), ObjectSize: sealed.Object.Size, ContentKey: sealed.ContentKey,
		At: c.now().UTC(), Bytes: found.Bytes, Cut: found.Cut, Harness: run.Harness, Session: run.SessionRan(),
		Digest: digest}
	row, written := c.rowContext(ctx, c.recordBudget)
	took, err := c.pending.StampSummary(row, run.TurnID, stamp)
	written()
	switch {
	case err != nil:
		unkept(fmt.Sprintf("the run's record did not take the summary's name, so no row names it: %v", err))
	case !took:
		log.InfoContext(ctx, "executor_session_summary_passed_over", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "detail", "the run's record names a newer summary, or is gone")
	default:
		log.InfoContext(ctx, "executor_session_summary_kept", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"bytes", found.Bytes, "cut", found.Cut, "object", stamp.Object)
	}
}

// summaryKeepRoom is what keeping a summary adds to a box read
// ([BoxReadBudget]): the read of the values it is scrubbed of
// ([captureReadBudget]), its upload within a record's reason and the object
// store's pace for a summary at its bound, and its name's change of the row
// ([captureStampBudget]).
var summaryKeepRoom = captureReadBudget + sessionRecordBudget +
	objstore.PaceFor(executorkeep.SummarySealedMaxBytes) + captureStampBudget

// summaryFresh is how long after its key was minted a summary the row names
// stands for the same one found again ([SummaryStamp.names]): HALF of
// objstore.RecordWithin, past which the executor log's end names no object,
// so a long turn's box read uploads it afresh while its end can still name it.
const summaryFresh = objstore.RecordWithin / 2

// summaryDigest is the digest a summary as kept is told apart by.
func summaryDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// SummaryStamp names the newest summary a box read of the run kept, as the
// run's row names it ([PendingRun.Summary]): the object, never its words. A
// WIRE FORMAT like the rest of the row, with an Extra of its own (wirekeys.go).
type SummaryStamp struct {
	// LaunchSeq is the launch whose box read kept it.
	LaunchSeq int64 `json:"launch_seq"`

	// Object, ObjectHash and ObjectSize are the sealed object, and ContentKey
	// the keyring key that sealed it.
	Object     string `json:"object"`
	ObjectHash string `json:"object_hash"`
	ObjectSize int64  `json:"object_size"`
	ContentKey string `json:"content_key"`

	// At is when the read kept it, by the keeping node's clock.
	At time.Time `json:"at"`

	// Bytes and Cut are [ThreadSummary.Bytes] and [ThreadSummary.Cut].
	Bytes int64 `json:"bytes"`
	Cut   bool  `json:"cut,omitempty"`

	// Harness wrote it, and Session is the harness's session it summarises.
	Harness string `json:"harness"`
	Session string `json:"session"`

	// Digest tells the summary as kept apart ([summaryDigest]), so a read
	// finding the same one again keeps nothing new.
	Digest string `json:"digest"`

	Extra map[string]json.RawMessage `json:"-"`
}

// newerThan reports whether s was kept after o: by a later launch's read, or
// later within one launch.
func (s SummaryStamp) newerThan(o SummaryStamp) bool {
	return s.LaunchSeq > o.LaunchSeq || (s.LaunchSeq == o.LaunchSeq && s.At.After(o.At))
}

// names reports whether s names the summary digest tells apart under a key
// minted within [summaryFresh] of now.
func (s SummaryStamp) names(digest string, now time.Time) bool {
	key, err := objstore.ParseKey(s.Object)
	return err == nil && s.Digest == digest && now.Sub(key.Minted()) <= summaryFresh
}

// Kept is s as the turn's end names it ([executorkeep.Ending.Summary]), or the
// error that keeps it off: a stamp a newer build wrote past what this one
// reads, which an end would carry to a refusal of the whole end.
func (s SummaryStamp) Kept() (executorkeep.Summary, error) {
	key, err := objstore.ParseKey(s.Object)
	if err != nil {
		return executorkeep.Summary{}, err
	}
	hash, err := objstore.ParseHash(s.ObjectHash)
	if err != nil {
		return executorkeep.Summary{}, err
	}
	kept := executorkeep.Summary{Object: objstore.Object{Key: key, Hash: hash, Size: s.ObjectSize},
		ContentKey: s.ContentKey, At: s.At.UTC(), Bytes: s.Bytes, Cut: s.Cut, Harness: s.Harness,
		Session: s.Session}
	return kept, kept.Validate()
}

// Bounded refuses a stamp past what the run's row carries for it: every bound
// [SummaryStamp.Kept] holds the end's to, and a digest that is none.
func (s SummaryStamp) Bounded() error {
	if _, err := s.Kept(); err != nil {
		return err
	}
	if len(s.Digest) != hex.EncodedLen(sha256.Size) {
		return fmt.Errorf("sandbox: a summary's digest is %d bytes, and one is %d", len(s.Digest),
			hex.EncodedLen(sha256.Size))
	}
	if len(s.ContentKey) > secrets.MaxStreamKeyIDBytes {
		return fmt.Errorf("sandbox: a summary's content key is past %d bytes", secrets.MaxStreamKeyIDBytes)
	}
	return nil
}

// summaryOf is what the turn's end names of run's newest summary: none where
// the row names none, or one the end could not carry, which is said.
func summaryOf(ctx context.Context, run PendingRun) *executorkeep.Summary {
	if run.Summary == nil {
		return nil
	}
	kept, err := run.Summary.Kept()
	if err != nil {
		log.WarnContext(ctx, "executor_session_summary_unkept", "turn_id", run.TurnID,
			"detail", "the run's record names a summary the turn's end cannot carry, so the end names none: "+
				err.Error())
		return nil
	}
	return &kept
}

// MarshalJSON writes the stamp with the keys a later build added to it.
func (s SummaryStamp) MarshalJSON() ([]byte, error) {
	type plain SummaryStamp
	return marshalCarrying(plain(s), wireKeys(reflect.TypeFor[SummaryStamp]()), s.Extra)
}

// UnmarshalJSON reads the stamp and keeps the keys this build does not know.
func (s *SummaryStamp) UnmarshalJSON(raw []byte) error {
	type plain SummaryStamp
	p := plain(*s)
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[SummaryStamp]()))
	if err != nil {
		return err
	}
	*s = SummaryStamp(p)
	s.Extra = extra
	return nil
}
