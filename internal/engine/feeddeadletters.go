package engine

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE WAKE FEEDS' DEAD LETTERS, as one node keeps, replays and reads them
// (package doc, "Dead letters"). A feed keeps a record it handed back for an
// hour in the queue's dead-letter stream and replays it at start
// (internal/changefeed). Feeds and an operator's listing and discard
// ([FeedDeadLetters]) reach the entries through [deadLetterLedger]; the alarm
// reading through [deadLetterWatch]. ONLY A FEED'S REPLAY READS A FRAME: the
// rest read headers, the alarm only once the broker's marks moved.
//
// The inbound group's unhandled deliveries (internal/notify's UnhandledSubject)
// are read the same way for `inbound_dead_lettered`, and its replay drops
// through the same ledger ([Engine.startNotifications]); the operator's listing
// and discard never show them.

// deadLetterStore is the queue's dead-letter verbs as this node calls them: a
// feed's ([changefeed.DeadLetters]) and the marks the alarm's reading counts the
// feeds' entries by ([queue.EventQueue.DeadLetterMarks]).
type deadLetterStore interface {
	changefeed.DeadLetters
	DeadLetterMarks(ctx context.Context, under string) (queue.Marks, error)
}

// deadLetterLedger is the queue's dead-letter verbs as this node's wake feeds
// and operator surface call them, ACCOUNTING FOR every entry either drops (a
// replay, or a discard).
//
// The reading counts an entry gone at its retention as EXPIRED, a wake lost; a
// replayed or discarded entry vanishes too, and within a beat of its expiry
// would read by age alone as the retention's. So a drop takes the expiry
// count's claim ([goneKey]), the first taker being the one account fleet-wide,
// and remembers the drop here for when the claim store cannot be reached.
type deadLetterLedger struct {
	store deadLetterStore

	// claims accounts for a drop fleet-wide, on the clock now; nil claims
	// nothing.
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
func newDeadLetterLedger(store deadLetterStore, claims expiryClaims) *deadLetterLedger {
	if store == nil {
		return nil
	}
	return &deadLetterLedger{store: store, claims: claims,
		now:     func() time.Time { return time.Now().UTC() },
		dropped: map[letterAt]struct{}{}}
}

// KeepDeadLetter implements [changefeed.DeadLetters]: the queue's keep, passed
// through; it accounts for nothing, since no entry goes.
func (l *deadLetterLedger) KeepDeadLetter(ctx context.Context, subject, id string, header,
	frame []byte) (bool, error) {

	return l.store.KeepDeadLetter(ctx, subject, id, header, frame)
}

// DeadLettersOn implements [changefeed.DeadLetters].
func (l *deadLetterLedger) DeadLettersOn(ctx context.Context, subject string) ([]queue.Kept, error) {
	return l.store.DeadLettersOn(ctx, subject)
}

// DeadLetterFrame implements [changefeed.DeadLetters].
func (l *deadLetterLedger) DeadLetterFrame(ctx context.Context, subject string, seq uint64) ([]byte, bool, error) {
	return l.store.DeadLetterFrame(ctx, subject, seq)
}

// DropDeadLetter implements [changefeed.DeadLetters], accounting for the entry
// once it is gone ([deadLetterLedger.drop]).
func (l *deadLetterLedger) DropDeadLetter(ctx context.Context, subject string, seq uint64) error {
	_, err := l.drop(ctx, subject, seq)
	return err
}

// DeadLetterMarks implements [notify.DeadLetters]: the broker's marks of every
// dead-letter subject under under, the feeds' ([changefeed.DeadLettersUnder])
// or the inbound group's ([inboundLettersUnder]).
func (l *deadLetterLedger) DeadLetterMarks(ctx context.Context, under string) (queue.Marks, error) {
	return l.store.DeadLetterMarks(ctx, under)
}

// inboundLettersUnder is the subtree holding the inbound group's dead-letter
// subjects, its unhandled subject among them.
var inboundLettersUnder = topics.DeadLettersUnder(topics.NotificationsInbound)

// drop removes the entry seq from subject and accounts for its going,
// reporting whether this drop is THE account of it: the claim was taken here,
// or there is no claim store or it could not answer (failing open towards this
// drop), rather than by whoever took it first.
//
// A replay ignores the answer: its wake is published either way. A discard
// ([FeedDeadLetters.Discard]) needs it: the queue answers a drop of an entry
// already gone as done, so a discard that lost the claim removed nothing and
// must neither count a wake lost nor report a discard.
func (l *deadLetterLedger) drop(ctx context.Context, subject string, seq uint64) (bool, error) {
	if err := l.store.DropDeadLetter(ctx, subject, seq); err != nil {
		return false, err
	}
	at := letterAt{subject, seq}
	l.mu.Lock()
	l.dropped[at] = struct{}{}
	l.mu.Unlock()
	if l.claims == nil {
		return true, nil
	}
	won, err := l.claims.Claim(ctx, goneKey(at), expiredClaimTTL, l.now())
	if err != nil {
		// A FAILED CLAIM ACCOUNTS FOR NOTHING and the drop stands: a peer
		// may then count it expired, the side a count of losses may err on.
		log.DebugContext(ctx, "changefeed_dead_letter_drop_unaccounted",
			"subject", subject, "seq", seq, "error", err.Error())
		return true, nil
	}
	return won, nil
}

// droppedHere reports whether this node dropped the entry.
func (l *deadLetterLedger) droppedHere(at letterAt) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, dropped := l.dropped[at]
	return dropped
}

// settle forgets every drop on subject that listed no longer holds: the
// reading has seen it gone, so the ledger keeps only what is in flight.
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
// whose dead-letter subject the alarm reading lists. Through [Engine.feedFor],
// the one door a feed is built through, so it is the source the feed keeps
// under; a log with no feed, or one disagreeing with its declaration, has no
// entry. Read once at the trim's start: a declaration never changes while the
// node runs.
func (e *Engine) feedSources(running []*runningLog) map[string]changefeed.Source {
	out := map[string]changefeed.Source{}
	for _, l := range running {
		if translator, _, err := e.feedFor(l); err == nil && translator != nil {
			out[l.domain.Name()] = translator.Source()
		}
	}
	return out
}

// expiredClaimTTL is how long the claim accounting for one entry's going is
// held ([goneKey]): the longest the store allows, [coord.MaxClaimTTL] (half an
// hour; it refuses more rather than clamping).
//
// A node that saw the entry judges its going on its next answering listing, a
// beat later, so the window only has to cover a node whose reads failed across
// that moment. One failing for over half an hour can count an expiry twice, or
// count a replayed or discarded entry as expired: erring towards a loss, on an
// outage that raised its own alarms. The key names an entry no later one
// shares, so the long window costs only the row.
const expiredClaimTTL = coord.MaxClaimTTL

// expiryClaims is the fleet's claims, narrowed to the one verb an entry's
// account takes ([changefeed.Claims] fails open the same way).
type expiryClaims interface {
	Claim(ctx context.Context, key string, ttl time.Duration, now time.Time) (bool, error)
}

// deadLetterWatch is what the alarm reading knows of the wake feeds' and the
// inbound group's dead letters: each subject's last listing that answered, and
// the broker's marks those listings agree with (package doc, "Dead letters").
//
// Each beat asks the broker one question per subtree, its MARKS (each subject's
// count and the stream's last sequence); while they stand, the cached listings
// are aged to now and nothing else is read. A moved last sequence relists every
// subject holding or having held an entry; a moved count alone relists that
// subject. Marks are recorded only when every listing agrees with them, and a
// failed read keeps the last listing. An entry gone past, or within a beat of,
// its expiry is counted as a lost wake once fleet-wide, by whoever first takes
// the claim on its going ([goneKey]).
type deadLetterWatch struct {
	// ledger reads the marks and listings and holds this node's drops; nil
	// reads nothing.
	ledger *deadLetterLedger

	// claims accounts for each entry's going once fleet-wide; nil counts an
	// expiry on every node that saw it, and an unreachable store counts it
	// anyway (the feed's fail-open rule).
	claims expiryClaims

	metrics *metrics.Recorder

	// mu is held for a whole reading, so the beat's report and `crewlet
	// retention status` never list twice for one set of marks, nor record
	// marks the other listed for.
	mu sync.Mutex
	// feeds and inbound are the subtrees counted by their marks: the feeds'
	// head, and the inbound topic's.
	feeds, inbound letterTree
	// seen is each subject's last listing that answered.
	seen map[string]letterSight
}

// letterTree is the marks every cached listing of one subtree agrees with, and
// whether any were recorded yet: until then every subject the marks count an
// entry on is listed.
type letterTree struct {
	agreed   queue.Marks
	recorded bool
}

// watched is one subject the reading lists, and how an entry the retention let
// go from it is counted once this node has taken the claim on its going.
type watched struct {
	subject string
	expired func(ctx context.Context, k queue.Kept)
}

// letterSight is one listing that answered: when, and each entry's sequence and
// instants, never its header, which the reading does not need to hold.
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

// read is the dead letters of each domain's feed in feeds as the alarm reads
// them at now, each aged and every expiry counted ([deadLetterWatch]). A domain
// whose feed holds none has no entry.
func (w *deadLetterWatch) read(ctx context.Context, feeds map[string]changefeed.Source,
	now time.Time) map[string][]statelog.FeedDeadLetter {

	if w == nil || w.ledger == nil || len(feeds) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	subjects := make([]watched, 0, len(feeds))
	for _, src := range feeds {
		subject := changefeed.DeadLetterSubject(src)
		subjects = append(subjects, watched{subject: subject, expired: w.feedExpired(src, subject)})
	}
	w.readTree(ctx, &w.feeds, changefeed.DeadLettersUnder(), subjects, now)
	out := make(map[string][]statelog.FeedDeadLetter, len(feeds))
	for domain, src := range feeds {
		if letters := lettersAt(w.seen[changefeed.DeadLetterSubject(src)].kept, now); len(letters) > 0 {
			out[domain] = letters
		}
	}
	return out
}

// readInbound is the inbound group's unhandled deliveries as
// `inbound_dead_lettered` reads them at now, read as [deadLetterWatch.read]
// reads a feed's. Empty while it holds none.
func (w *deadLetterWatch) readInbound(ctx context.Context, now time.Time) []statelog.FeedDeadLetter {
	if w == nil || w.ledger == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	subject := notify.UnhandledSubject()
	w.readTree(ctx, &w.inbound, inboundLettersUnder,
		[]watched{{subject: subject, expired: w.inboundExpired(subject)}}, now)
	return lettersAt(w.seen[subject].kept, now)
}

// readTree reads the marks of the subtree under once and lists again each of
// subjects they say can have changed since tree's agreed marks
// ([deadLetterWatch.listMoved]). A marks read that fails keeps the last
// listings that answered, aged to now by the caller.
func (w *deadLetterWatch) readTree(ctx context.Context, tree *letterTree, under string,
	subjects []watched, now time.Time) {

	marks, err := w.ledger.DeadLetterMarks(ctx, under)
	if err != nil {
		// QUIET, because the broker that did not answer raises its own
		// alarms, and this runs on every beat of every data node.
		log.DebugContext(ctx, "dead_letter_marks_unread", "under", under, "error", err.Error(),
			"detail", "the alarm reads the last listings that answered, aged to now")
		return
	}
	w.listMoved(ctx, tree, subjects, marks, now)
}

// listMoved relists each of subjects marks says can have changed since tree's
// agreed marks, counts what expired from each, and records marks as agreed only
// if every listing agrees with them ([deadLetterWatch]).
func (w *deadLetterWatch) listMoved(ctx context.Context, tree *letterTree, subjects []watched,
	marks queue.Marks, now time.Time) {

	// SOMETHING WAS KEPT since the agreed marks — or none were ever
	// agreed — so every subject that holds an entry or held one is listed.
	stored := !tree.recorded || marks.Last != tree.agreed.Last
	agrees := true
	for _, sub := range subjects {
		subject := sub.subject
		prior := w.seen[subject]
		count := marks.Counts[subject]
		switch {
		case stored && (count > 0 || len(prior.kept) > 0):
		case !stored && count != tree.agreed.Counts[subject]:
		default:
			continue
		}
		listed, err := w.ledger.DeadLettersOn(ctx, subject)
		if err != nil {
			log.DebugContext(ctx, "dead_letters_unread", "subject", subject,
				"error", err.Error(),
				"detail", "the alarm reads the last listing that answered, aged to now")
			agrees = false
			continue
		}
		w.countExpired(ctx, sub, prior, listed, now)
		w.ledger.settle(subject, listed)
		sight := letterSight{at: now, kept: make([]queue.Kept, len(listed))}
		for i, k := range listed {
			sight.kept[i] = queue.Kept{Seq: k.Seq, StoredAt: k.StoredAt, Expires: k.Expires}
		}
		w.seen[subject] = sight
		if uint64(len(listed)) != count {
			// A REPLICA BEHIND THE LEADER, or an entry kept or let go
			// between the marks and the listing: the next beat lists it
			// again.
			agrees = false
		}
	}
	if agrees {
		tree.agreed, tree.recorded = marks, true
	}
}

// countExpired counts, as sub counts one, every entry prior held that kept
// does not, that this node did not drop, and that is due or was within a beat
// of its expiry when prior saw it: once fleet-wide, never for one a replay or a
// discard accounted for.
func (w *deadLetterWatch) countExpired(ctx context.Context, sub watched, prior letterSight,
	kept []queue.Kept, now time.Time) {

	subject := sub.subject
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
			// KEPT BY NO RETENTION, so none let it go: a replay or a
			// discard.
			continue
		case k.Expires.After(now) && prior.at.Add(statelog.AlarmInterval).Before(k.Expires):
			// GONE BEFORE ITS EXPIRY: not due and more than a beat from it
			// when last seen, so a replay or a discard. One due by now goes to
			// the claim however long since seen, so an expiry during failed
			// reads still counts; the beat covers a broker clock running ahead.
			continue
		}
		if !w.firstToCount(ctx, at, now) {
			continue
		}
		sub.expired(ctx, k)
	}
}

// feedExpired counts one entry the retention let go from src's subject as the
// wake it lost.
func (w *deadLetterWatch) feedExpired(src changefeed.Source, subject string) func(context.Context, queue.Kept) {
	return func(ctx context.Context, k queue.Kept) {
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

// inboundExpired counts one delivery the retention let go from the inbound
// group's unhandled subject as the wake it lost.
func (w *deadLetterWatch) inboundExpired(subject string) func(context.Context, queue.Kept) {
	return func(ctx context.Context, k queue.Kept) {
		if w.metrics != nil {
			w.metrics.Add(metrics.NotifyInboundDeadLetterExpired, 1, nil)
		}
		log.WarnContext(ctx, "inbound_dead_letter_expired", "subject", subject, "seq", k.Seq,
			"kept_at", k.StoredAt, "expired_at", k.Expires,
			"detail", "the dead-letter stream's retention let it go before any node's replay "+
				"delivered it, and the wake it held is lost: no node of a build that reads it "+
				"ran within stream.event_retention_hours")
	}
}

// firstToCount reports whether this node counts the expiry of at: the claim's
// winner does (none, where a drop took it first), and every node does when the
// claim store cannot answer.
func (w *deadLetterWatch) firstToCount(ctx context.Context, at letterAt, now time.Time) bool {
	if w.claims == nil {
		return true
	}
	won, err := w.claims.Claim(ctx, goneKey(at), expiredClaimTTL, now)
	if err != nil {
		log.WarnContext(ctx, "dead_letter_expiry_claim_unavailable",
			"subject", at.subject, "seq", at.seq, "error", err.Error(),
			"detail", "counted here anyway: a lost wake counted twice is read; one "+
				"counted by nobody is not")
		return true
	}
	return won
}

// goneKey is the claim an entry's going is accounted under, taken by the drop
// that removed it and by the reading counting its expiry: the first taker is
// the one account. Keyed by subject and sequence, which no later entry reuses.
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
