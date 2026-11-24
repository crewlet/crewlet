package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A JOB'S NEWEST SUMMARY, KEPT APART (ADR-0073's amendment "a session's newest
// summary is kept apart"). Every box read asks the job's main thread for its
// newest compaction summary in the one read it makes of it
// ([Coordinator.readMainThread]). The take reads the values its capture is
// scrubbed of once, for itself and the summary ([readSummary]), and the summary
// is kept beside the session, never inside it, and named on the run's row in a
// stamp the take writes anyway ([SessionCapture.Summary]): uploaded in the
// session's keep where the take spools it ([Coordinator.keepSummaryBeside]),
// before the refusal's stamp where it does not ([Coordinator.keepFound]). The
// turn's end names it on the executor log ([Coordinator.summaryOf]).

// readSummary is the newest summary a box read found on its job's main thread
// as its harness wrote it, on its way to being kept: by its runner, once the
// take has read the values its capture is scrubbed of (scrub, scrubbed), and
// ONCE A READ ([Coordinator.keepFound]), its answer kept for a keep that
// follows a refused stamp.
type readSummary struct {
	found    ThreadSummary
	keeper   summaryKeeper
	scrub    []string
	scrubbed bool

	mu    sync.Mutex
	tried bool
	kept  *SummaryStamp
}

// asksSummary reports whether a read of runner's box asks its job's newest
// summary: a runner that reads and keeps one, on a node that keeps sessions
// ([Coordinator.capturing]).
func (c *Coordinator) asksSummary(runner Runner) bool {
	_, keeps := runner.(summaryKeeper)
	return keeps && ThreadTraitsOf(runner).Summaries && c.capturing()
}

// foundSummary is what run's box read takes of the summary its main-thread
// read found: nil where it asked none, the thread holds none, or the read could
// not say, which is said (`executor_session_summary_unread`).
func (c *Coordinator) foundSummary(ctx context.Context, run PendingRun, runner Runner, asked bool,
	found ThreadSummary) *readSummary {

	keeper, keeps := runner.(summaryKeeper)
	switch {
	case !asked || !keeps:
		return nil
	case found.Unread != "":
		log.InfoContext(ctx, "executor_session_summary_unread", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"detail", found.Unread)
		return nil
	case found.Text == "":
		return nil
	}
	return &readSummary{found: found, keeper: keeper}
}

// keepFound keeps rs, the summary run's box read found, once scrubbed and once
// a read: kept by its runner, sealed and uploaded as an object of its own
// ([SessionKeep.PutSummary]) within [Coordinator.summaryRoom], held to its
// reason once the coordinator stops. Nil where it keeps none: unscrubbed, a
// summary run's row names already under a key still fresh
// ([SummaryStamp.names]), or one that could not be kept, said
// (`executor_session_summary_unkept`) and changing nothing else.
func (c *Coordinator) keepFound(ctx context.Context, run PendingRun, rs *readSummary) *SummaryStamp {
	if rs == nil {
		return nil
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.tried || !rs.scrubbed {
		return rs.kept
	}
	rs.tried = true
	rs.kept = c.keepScrubbed(ctx, run, rs)
	return rs.kept
}

// keepScrubbed is [Coordinator.keepFound]'s keep of rs, scrubbed.
func (c *Coordinator) keepScrubbed(ctx context.Context, run PendingRun, rs *readSummary) *SummaryStamp {
	kept := rs.keeper.KeepSummary(rs.found, rs.scrub)
	if kept.Text == "" {
		return nil
	}
	digest := summaryDigest(kept.Text)
	if run.Summary != nil && run.Summary.names(digest, c.now()) {
		return nil
	}
	upload, uploaded := c.stopHeld(ctx, c.recordBudget, c.summaryRoom())
	sealed, err := c.keep.PutSummary(upload, run.AgentHandle, run.TurnID, strings.NewReader(kept.Text))
	uploaded()
	if err != nil {
		log.WarnContext(ctx, "executor_session_summary_unkept", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"bytes", kept.Bytes, "detail", fmt.Sprintf("the summary could not be sealed and uploaded: %v", err))
		return nil
	}
	stamp := &SummaryStamp{LaunchSeq: run.LaunchSeq, Object: sealed.Object.Key.String(),
		ObjectHash: string(sealed.Object.Hash), ObjectSize: sealed.Object.Size, ContentKey: sealed.ContentKey,
		At: c.now().UTC(), Bytes: kept.Bytes, Kept: int64(len(kept.Text)), Cut: kept.Cut, Harness: run.Harness,
		Session: run.SessionRan(), Digest: digest}
	if err := stamp.Bounded(); err != nil {
		log.WarnContext(ctx, "executor_session_summary_unkept", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"detail", "the summary kept is past what the run's record names: "+err.Error())
		return nil
	}
	log.InfoContext(ctx, "executor_session_summary_kept", "turn_id", run.TurnID, "launch_id", run.LaunchID,
		"bytes", kept.Bytes, "cut", kept.Cut, "object", stamp.Object)
	return stamp
}

// keepSummaryBeside keeps the summary spooled's take found beside its
// session's upload ([Coordinator.keepCapture]), on a goroutine of its own, so
// neither upload waits on the other: the answer is its stamp, waited for once,
// which every stamp of the keep names ([SessionCapture.Summary]).
func (c *Coordinator) keepSummaryBeside(ctx context.Context, spooled *spooledCapture) func() *SummaryStamp {
	if spooled.summary == nil {
		return func() *SummaryStamp { return nil }
	}
	done := make(chan *SummaryStamp, 1)
	go func() { done <- c.keepFound(ctx, spooled.run, spooled.summary) }()
	return sync.OnceValue(func() *SummaryStamp { return <-done })
}

// keepSummaryRefused keeps rs where its take was refused, before the refusal's stamp
// names it: no keep follows a refusal, and the turn's end may follow at once.
// UNDER THE READ'S OWN CONTEXT, up to where the take's records begin
// ([Coordinator.takeBy]): a take refused at its deadline leaves its own context
// ended, and it ended a summary's room early for this ([Coordinator.summaryBy]).
// row reads, within it, the scrub's values a take refused before them had not,
// and answers the run's row.
func (c *Coordinator) keepSummaryRefused(read context.Context, by time.Time, rs *readSummary,
	row func(context.Context) PendingRun) *SummaryStamp {

	ctx, cancel := before(read, func() {}, c.takeBy(by))
	defer cancel()
	return c.keepFound(ctx, row(ctx), rs)
}

// summaryRoom is a summary's upload: a record's reason and the object store's
// pace for one at its bound.
func (c *Coordinator) summaryRoom() time.Duration {
	return c.recordBudget + objstore.PaceFor(executorkeep.SummarySealedMaxBytes)
}

// summaryBy is when a take that found a summary ends, where the read ends by
// by: a summary's room before the take's records ([Coordinator.takeBy]), so a
// refused take keeps it before its stamp ([Coordinator.keepSummaryRefused]). Zero
// where by is.
func (c *Coordinator) summaryBy(by time.Time) time.Time {
	last := c.takeBy(by)
	if last.IsZero() {
		return last
	}
	return last.Add(-c.summaryRoom())
}

// summaryUploadRoom is what keeping a summary adds to a box read
// ([BoxReadBudget]): [Coordinator.summaryRoom] at the record's default reason,
// given a refused take's keep, which comes before its stamp. A spooled take's
// keep comes after the read, its scrub's values are the take's and its name
// rides a stamp the read makes anyway, so none adds anything.
var summaryUploadRoom = sessionRecordBudget + objstore.PaceFor(executorkeep.SummarySealedMaxBytes)

// summaryFresh is how long after its key was minted a summary the row names
// stands for the same one found again ([SummaryStamp.names]), and how old one
// the turn's end names may be before it is copied under a fresh key
// ([Coordinator.summaryOf]): HALF of objstore.RecordWithin, past which the
// executor log's end names no object.
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

	// Bytes is the whole summary's size as kept before its bound, Kept the
	// bytes kept, and Cut says they are its last part.
	Bytes int64 `json:"bytes"`
	Kept  int64 `json:"kept"`
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

// Named is s as the turn's end names it ([executorkeep.Ending.Summary]), or the
// error that keeps it off: a stamp a newer build wrote past what this one
// reads, which an end would carry to a refusal of the whole end.
func (s SummaryStamp) Named() (executorkeep.Summary, error) {
	key, err := objstore.ParseKey(s.Object)
	if err != nil {
		return executorkeep.Summary{}, err
	}
	hash, err := objstore.ParseHash(s.ObjectHash)
	if err != nil {
		return executorkeep.Summary{}, err
	}
	kept := executorkeep.Summary{Object: objstore.Object{Key: key, Hash: hash, Size: s.ObjectSize},
		ContentKey: s.ContentKey, At: s.At.UTC(), Bytes: s.Bytes, Kept: s.Kept, Cut: s.Cut, Harness: s.Harness,
		Session: s.Session}
	return kept, kept.Validate()
}

// Bounded refuses a stamp past what the run's row carries for it: every bound
// [SummaryStamp.Named] holds the end's to, and a digest that is none.
func (s SummaryStamp) Bounded() error {
	if _, err := s.Named(); err != nil {
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
// the row names none, or one the end could not carry, which is said. ONE MINTED
// PAST [summaryFresh] IS COPIED UNDER A FRESH KEY first ([SessionKeep.CopySummary]),
// within [summaryCopyRoom]: the executor log names no object minted past
// objstore.RecordWithin, and a turn parked past it would otherwise end naming
// none in exactly the case a summary is kept for. A copy that fails names the
// one the row names, which the log's decide leaves out where it is too old.
func (c *Coordinator) summaryOf(ctx context.Context, run PendingRun) *executorkeep.Summary {
	if run.Summary == nil {
		return nil
	}
	kept, err := run.Summary.Named()
	if err != nil {
		log.WarnContext(ctx, "executor_session_summary_unkept", "turn_id", run.TurnID,
			"detail", "the run's record names a summary the turn's end cannot carry, so the end names none: "+
				err.Error())
		return nil
	}
	if c.now().Sub(kept.Object.Key.Minted()) <= summaryFresh {
		return &kept
	}
	copying, copied := c.stopHeld(ctx, c.recordBudget, summaryCopyRoom)
	fresh, err := c.keep.CopySummary(copying, run.AgentHandle, run.TurnID, kept.Object)
	copied()
	if err != nil {
		log.WarnContext(ctx, "executor_session_summary_unkept", "turn_id", run.TurnID, "object", run.Summary.Object,
			"detail", fmt.Sprintf("the summary minted past %s could not be copied under a fresh key, so the "+
				"turn's end names it as it is: %v", summaryFresh, err))
		return &kept
	}
	kept.Object, kept.ContentKey = fresh.Object, fresh.ContentKey
	return &kept
}

// summaryCopyRoom bounds a summary's copy under a fresh key: its read back
// (objstore.ReadBudget) and its upload, each at the object store's pace for a
// summary at its bound, beside a record's reason.
var summaryCopyRoom = objstore.ReadBudget(executorkeep.SummarySealedMaxBytes) + summaryUploadRoom

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
