package engine

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE WAKE FEEDS' DEAD LETTERS, as one node keeps, replays and reads them.
//
// A wake feed keeps a record it handed back for an hour as a dead letter in the
// queue's dead-letter stream, and its start replays what it kept
// (internal/changefeed). Two things on a node touch those entries: its feeds,
// through [deadLetterLedger], and the alarm reading every beat takes, through
// [deadLetterWatch] — which raises `feed_dead_lettered` from the first entry,
// ages each one on every beat, and counts an entry the retention let go before
// anybody replayed it.

// deadLetterLedger is the queue's dead-letter verbs as this node's wake feeds
// call them, ACCOUNTING FOR every entry a feed's replay dropped.
//
// WHY IT ACCOUNTS: the reading counts an entry that vanished at its retention
// as EXPIRED — a wake lost for good — and an entry a replay dropped vanishes
// too, with its wake published. Told apart by age alone, a replay of an entry
// within a beat of its expiry would read as a lost wake. So a drop takes the
// claim the expiry count takes ([goneKey]) — whoever takes it first is the one
// account of where the entry went, on every node — and remembers the drop on
// this node besides, for a claim store that could not be reached.
type deadLetterLedger struct {
	store changefeed.DeadLetters

	// claims is where a drop is accounted for across the fleet, and now
	// the clock it is claimed on; nil claims nothing.
	claims expiryClaims
	now    func() time.Time

	mu      sync.Mutex
	dropped map[letterAt]struct{}
}

// letterAt names one dead letter: its subject and its sequence.
type letterAt struct {
	subject string
	seq     uint64
}

// newDeadLetterLedger is a ledger over store accounting for its drops in
// claims, or nil for no store.
func newDeadLetterLedger(store changefeed.DeadLetters, claims expiryClaims) *deadLetterLedger {
	if store == nil {
		return nil
	}
	return &deadLetterLedger{store: store, claims: claims,
		now:     func() time.Time { return time.Now().UTC() },
		dropped: map[letterAt]struct{}{}}
}

// KeepDeadLetter implements [changefeed.DeadLetters].
func (l *deadLetterLedger) KeepDeadLetter(ctx context.Context, subject string, body []byte) error {
	return l.store.KeepDeadLetter(ctx, subject, body)
}

// DeadLettersOn implements [changefeed.DeadLetters].
func (l *deadLetterLedger) DeadLettersOn(ctx context.Context, subject string) ([]queue.Kept, error) {
	return l.store.DeadLettersOn(ctx, subject)
}

// DropDeadLetter implements [changefeed.DeadLetters], accounting for the entry
// once it is gone.
func (l *deadLetterLedger) DropDeadLetter(ctx context.Context, subject string, seq uint64) error {
	if err := l.store.DropDeadLetter(ctx, subject, seq); err != nil {
		return err
	}
	at := letterAt{subject, seq}
	l.mu.Lock()
	l.dropped[at] = struct{}{}
	l.mu.Unlock()
	if l.claims != nil {
		// A CLAIM THAT FAILS ACCOUNTS FOR NOTHING, and the drop stands: a
		// node that then counts the entry as expired reports a wake lost
		// that was published, the side a count of losses may err on.
		if _, err := l.claims.Claim(ctx, goneKey(at), expiredClaimTTL, l.now()); err != nil {
			log.DebugContext(ctx, "changefeed_dead_letter_drop_unaccounted",
				"subject", subject, "seq", seq, "error", err.Error())
		}
	}
	return nil
}

// droppedHere reports whether this node dropped the entry.
func (l *deadLetterLedger) droppedHere(at letterAt) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, dropped := l.dropped[at]
	return dropped
}

// settle forgets every drop on subject that a listing which answered no
// longer holds: once the reading has read the subject without the entry, the
// drop has told it what it had to, and the ledger stays as small as what is
// still in flight.
func (l *deadLetterLedger) settle(subject string, listed []queue.Kept) {
	held := make(map[uint64]bool, len(listed))
	for _, k := range listed {
		held[k.Seq] = true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for at := range l.dropped {
		if at.subject == subject && !held[at.seq] {
			delete(l.dropped, at)
		}
	}
}

// feedDeadLetters is this node's ledger as a feed takes it, or no store at all
// where none is wired — never a nil pointer inside an interface.
func (e *Engine) feedDeadLetters() changefeed.DeadLetters {
	if e.deadLetterLedger == nil {
		return nil
	}
	return e.deadLetterLedger
}

// feedSources is the wake feed each of running declares, by domain: the source
// whose dead-letter subject the alarm reading lists for that log.
//
// THROUGH [Engine.feedFor], the one door a feed is built through, so the
// source is the one the feed keeps its dead letters under; a log that declares
// no feed, or whose feed disagrees with its declaration, starts none and keeps
// no dead letters, and has no entry. Read once, at the trim's start: what a
// domain declares does not change while the node runs.
func (e *Engine) feedSources(running []*runningLog) map[string]changefeed.Source {
	out := map[string]changefeed.Source{}
	for _, l := range running {
		if translator, _, err := e.feedFor(l); err == nil && translator != nil {
			out[l.domain.Name()] = translator.Source()
		}
	}
	return out
}

// expiredClaimTTL is how long the claim that accounts for one entry's going is
// held ([goneKey]).
//
// THE LONGEST ANY CLAIM IS HELD, [coord.MaxClaimTTL] — half an hour, which
// the store refuses to exceed rather than clamp. Every data node that saw the
// entry before its expiry judges its going on its next listing that answers,
// which is a beat later on a node whose reads are answering, so the window
// only has to cover a node whose reads failed across that moment. One whose
// reads failed for longer than half an hour can misjudge an entry that went
// while they did: count an expiry the fleet already counted, or count as
// expired one a replay dropped — a loss counted that was not one, on an outage
// that raised its own alarms long before, rather than a loss counted by
// nobody. The key names one entry on one subject, which is never kept again,
// so the longest window costs nothing but the row.
const expiredClaimTTL = coord.MaxClaimTTL

// expiryClaims is the fleet's claims, narrowed to the one verb an entry's
// account takes ([changefeed.Claims] fails open the same way).
type expiryClaims interface {
	Claim(ctx context.Context, key string, ttl time.Duration, now time.Time) (bool, error)
}

// deadLetterWatch is what the alarm reading knows of each wake feed's dead
// letters: the last listing that answered, per feed, re-read on every report.
//
// # Re-read on every beat
//
// So an entry's age and the time it has left move on every beat, and
// `feed_dead_lettered`'s count of what expires within a day is a reading of
// now rather than of the last replay. A listing that FAILED keeps the last one
// that answered, aged to now: an entry is not gone because one read did not
// reach the broker, and an alarm that cleared on a broker blip would raise
// again on the next beat for nothing.
//
// # An entry that vanished at its expiry is a wake lost
//
// The broker lets an entry go at its retention without telling anybody, and an
// entry that goes is either replayed — its wake published — or expired, its
// wake lost for good. So an entry the last listing held and this one does not
// is counted as expired (`crewlet.tracker.feed.dead_letter_expired`) when it
// is past its expiry by this listing, however many listings failed in between,
// or was within a beat ([statelog.AlarmInterval]) of it when last seen, since
// the broker's clock and this node's are not one. It is counted by whichever
// node first takes the claim on its going ([goneKey]), which a replay that
// dropped it took first ([deadLetterLedger]), so it is counted ONCE FLEET-WIDE
// and never for an entry somebody replayed, though every data node reads the
// subject on every beat. What is NOT counted is an expiry across a restart of
// every node that saw it: a node starts with no sight of what it had seen. And
// a replay whose claim could not be taken, as near its expiry, would be
// counted by a peer — a count of losses erring towards a loss.
type deadLetterWatch struct {
	// ledger is the store the listings are read through and the drops this
	// node made; nil reads nothing.
	ledger *deadLetterLedger

	// claims accounts for each entry's going once across the fleet; nil
	// counts an expiry on every node that saw it, and an unreachable store
	// counts it anyway, the feed's own fail-open rule.
	claims expiryClaims

	metrics *metrics.Recorder

	mu   sync.Mutex
	seen map[string]letterSight
}

// letterSight is one listing that answered: when, and what it held — each
// entry's sequence and instants, never its bytes, which the reading does not
// need and a large entry would hold in memory for nothing.
type letterSight struct {
	at   time.Time
	kept []queue.Kept
}

// newDeadLetterWatch is a watch over ledger's store.
func newDeadLetterWatch(ledger *deadLetterLedger, claims expiryClaims,
	rec *metrics.Recorder) *deadLetterWatch {

	return &deadLetterWatch{ledger: ledger, claims: claims, metrics: rec,
		seen: map[string]letterSight{}}
}

// read is src's feed's dead letters as the alarm reads them at now: the
// listing re-read, every entry that vanished at its expiry counted, and each
// entry's age and the time it has left.
func (w *deadLetterWatch) read(ctx context.Context, src changefeed.Source,
	now time.Time) []statelog.FeedDeadLetter {

	if w == nil || w.ledger == nil {
		return nil
	}
	subject := changefeed.DeadLetterSubject(src)
	kept, err := w.ledger.DeadLettersOn(ctx, subject)
	if err != nil {
		// QUIET, because the broker that did not answer raises its own
		// alarms, and this runs on every beat of every data node.
		log.DebugContext(ctx, "changefeed_dead_letters_unread", "source", src.Name,
			"subject", subject, "error", err.Error(),
			"detail", "the alarm reads the last listing that answered, aged to now")
		w.mu.Lock()
		defer w.mu.Unlock()
		return lettersAt(w.seen[subject].kept, now)
	}
	sight := letterSight{at: now, kept: make([]queue.Kept, len(kept))}
	for i, k := range kept {
		sight.kept[i] = queue.Kept{Seq: k.Seq, StoredAt: k.StoredAt, Expires: k.Expires}
	}
	w.mu.Lock()
	prior, seen := w.seen[subject]
	w.seen[subject] = sight
	w.mu.Unlock()
	if seen {
		w.countExpired(ctx, src, subject, prior, kept, now)
	}
	w.ledger.settle(subject, kept)
	return lettersAt(sight.kept, now)
}

// countExpired counts every entry prior held that kept does not, that this
// node did not drop, and that was within a beat of its expiry when prior saw
// it — once across the fleet, and never for one a replay accounted for.
func (w *deadLetterWatch) countExpired(ctx context.Context, src changefeed.Source,
	subject string, prior letterSight, kept []queue.Kept, now time.Time) {

	held := make(map[uint64]bool, len(kept))
	for _, k := range kept {
		held[k.Seq] = true
	}
	for _, k := range prior.kept {
		at := letterAt{subject, k.Seq}
		switch {
		case held[k.Seq], w.ledger.droppedHere(at):
			continue
		case k.Expires.IsZero():
			// KEPT BY NO RETENTION, so none let it go: a replay.
			continue
		case k.Expires.After(now) && prior.at.Add(statelog.AlarmInterval).Before(k.Expires):
			// GONE BEFORE ITS EXPIRY: not yet due by this listing, and more
			// than a beat from it when last seen — a replay, here or on a
			// peer. ONE DUE BY NOW IS JUDGED BY THE CLAIM however long ago
			// it was last seen, so an expiry while this node's listings
			// failed is still counted; the beat's slack covers a broker
			// whose clock runs a little ahead of this node's.
			continue
		}
		if !w.firstToCount(ctx, at, now) {
			continue
		}
		if w.metrics != nil {
			w.metrics.Add(metrics.TrackerFeedDeadLetterExpired, 1, metrics.Attrs{"source": src.Name})
		}
		log.WarnContext(ctx, "changefeed_dead_letter_expired", "source", src.Name,
			"subject", subject, "seq", k.Seq, "kept_at", k.StoredAt, "expired_at", k.Expires,
			"detail", "the dead-letter stream's retention let it go before any node could "+
				"read it, and the wake it held is lost: no node replayed it within "+
				"stream.event_retention_hours")
	}
}

// firstToCount reports whether this node counts the expiry of at: the claim's
// winner does — none, where a replay took it first — and so does every node
// when the claim store cannot answer.
func (w *deadLetterWatch) firstToCount(ctx context.Context, at letterAt, now time.Time) bool {
	if w.claims == nil {
		return true
	}
	won, err := w.claims.Claim(ctx, goneKey(at), expiredClaimTTL, now)
	if err != nil {
		log.WarnContext(ctx, "changefeed_dead_letter_expiry_claim_unavailable",
			"subject", at.subject, "seq", at.seq, "error", err.Error(),
			"detail", "counted here anyway: a lost wake counted twice is read; one "+
				"counted by nobody is not")
		return true
	}
	return won
}

// goneKey is the claim an entry's going is accounted for under: the replay
// that dropped it takes it, and so does the reading that counts its expiry, so
// whoever takes it first is the one account of where the entry went. The
// entry's subject and sequence, which no later entry ever carries again.
func goneKey(at letterAt) string {
	return changefeed.ClaimKey("dead_letter_gone", at.subject+"@"+strconv.FormatUint(at.seq, 10))
}

// lettersAt is each kept entry as the alarm reads it at now.
func lettersAt(kept []queue.Kept, now time.Time) []statelog.FeedDeadLetter {
	if len(kept) == 0 {
		return nil
	}
	out := make([]statelog.FeedDeadLetter, len(kept))
	for i, k := range kept {
		out[i] = statelog.FeedDeadLetter{Age: now.Sub(k.StoredAt), ExpiresIn: k.Expires.Sub(now)}
	}
	return out
}
