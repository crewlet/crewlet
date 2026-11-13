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

// THE WAKE FEEDS' DEAD LETTERS, as one node keeps, replays and reads them.
//
// A wake feed keeps a record it handed back for an hour as a dead letter in the
// queue's dead-letter stream, and its start replays what it kept
// (internal/changefeed). Three things on a node touch those entries: its feeds,
// through [deadLetterLedger]; an operator's listing and discard, through the
// same ledger ([FeedDeadLetters], feeddeadletterops.go); and the alarm reading
// every beat takes, through [deadLetterWatch] — which raises
// `feed_dead_lettered` from the first entry, ages each one on every beat, and
// counts an entry the retention let go before anybody replayed or discarded it.
//
// ONLY A FEED'S REPLAY READS A FRAME. The listing and the discard read the
// header each entry is kept under, and the alarm's reading reads even those
// only when the broker's count of the feeds' entries has moved.
//
// AND THE INBOUND GROUP'S UNHANDLED DELIVERIES, BESIDE THEM. The notification
// service's inbound group keeps a delivery no member handled on its unhandled
// subject (internal/notify's UnhandledSubject), and every node that reads it
// replays it, a running one within a poll of its keep (internal/notify's
// replay.go): the same kind of entry, a wake nobody has had, waiting for a node
// that reads it. So the same reading ages
// those entries on every beat for `inbound_dead_lettered` and counts one the
// retention let go as the wake it lost; the replay drops through the same
// ledger ([Engine.startNotifications] hands it to the service), so its drops
// are accounted as a feed's are. They are not a feed's: the operator's
// listing and discard never show them.

// deadLetterStore is the queue's dead-letter verbs as this node calls them: a
// feed's ([changefeed.DeadLetters]) and the marks the alarm's reading counts the
// feeds' entries by ([queue.EventQueue.DeadLetterMarks]).
type deadLetterStore interface {
	changefeed.DeadLetters
	DeadLetterMarks(ctx context.Context, under string) (queue.Marks, error)
}

// deadLetterLedger is the queue's dead-letter verbs as this node's wake feeds
// and its operator surface call them, ACCOUNTING FOR every entry either drops:
// a feed's replay, or an operator's discard.
//
// WHY IT ACCOUNTS: the reading counts an entry that vanished at its retention
// as EXPIRED — a wake lost for good — and an entry a replay dropped vanishes
// too, with its wake published, as does one an operator discarded, whose loss
// is counted as theirs. Told apart by age alone, either within a beat of the
// entry's expiry would read as the retention's loss. So a drop takes the claim
// the expiry count takes ([goneKey]) — whoever takes it first is the one
// account of where the entry went, on every node — and remembers the drop on
// this node besides, for a claim store that could not be reached.
type deadLetterLedger struct {
	store deadLetterStore

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
func newDeadLetterLedger(store deadLetterStore, claims expiryClaims) *deadLetterLedger {
	if store == nil {
		return nil
	}
	return &deadLetterLedger{store: store, claims: claims,
		now:     func() time.Time { return time.Now().UTC() },
		dropped: map[letterAt]struct{}{}}
}

// KeepDeadLetter implements [changefeed.DeadLetters]: the feed's keep, under the
// identity it names and answered as the queue answers it — already kept
// included, which accounts for nothing here, since no entry came or went.
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

// DeadLetterMarks implements [notify.DeadLetters]: what every dead-letter
// subject under under holds, as the broker counts it — every feed's
// ([changefeed.DeadLettersUnder]), or the inbound group's
// ([inboundLettersUnder]), which the alarm's reading and the notification
// service's running replay both ask.
func (l *deadLetterLedger) DeadLetterMarks(ctx context.Context, under string) (queue.Marks, error) {
	return l.store.DeadLetterMarks(ctx, under)
}

// inboundLettersUnder is the subtree the inbound group's dead-letter subjects
// lie under, its unhandled subject among them: the inbound topic's head
// ([topics.DeadLettersUnder]).
var inboundLettersUnder = topics.DeadLettersUnder(topics.NotificationsInbound)

// drop removes the entry seq from subject and accounts for its going,
// reporting whether this drop is THE account of where it went: the claim was
// taken here — or there is no claim store to take it in, or one that could not
// answer, which fails open towards this drop — rather than by a replay, an
// expiry count or a discard that took it first.
//
// A REPLAY DOES NOT ASK, since its wake is published whoever accounts for the
// entry. A DISCARD DOES ([FeedDeadLetters.Discard]): the queue answers a drop
// of an entry already gone as done, so a discard whose entry somebody else
// accounted for first removed nothing, and must neither count a wake lost nor
// say it discarded one — a wake a replay published is no loss at all.
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
		// A CLAIM THAT FAILS ACCOUNTS FOR NOTHING, and the drop stands: a
		// node that then counts the entry as expired reports a wake lost
		// that was published or discarded, the side a count of losses may
		// err on.
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
// expired one a replay or a discard dropped — a loss counted that was not the
// retention's, on an outage that raised its own alarms long before, rather
// than a loss counted by nobody. The key names one entry on one subject, which
// is never kept again, so the longest window costs nothing but the row.
const expiredClaimTTL = coord.MaxClaimTTL

// expiryClaims is the fleet's claims, narrowed to the one verb an entry's
// account takes ([changefeed.Claims] fails open the same way).
type expiryClaims interface {
	Claim(ctx context.Context, key string, ttl time.Duration, now time.Time) (bool, error)
}

// deadLetterWatch is what the alarm reading knows of every wake feed's dead
// letters: each feed's last listing that answered, and the broker's marks those
// listings agree with.
//
// # One count a beat, and a listing only when it moved
//
// Every report — every beat of every data node — asks the queue ONE question
// for each subtree it reads: the MARKS of the subtree every feed's subject lies
// under ([changefeed.DeadLettersUnder]), and of the inbound topic's, where the
// inbound group's unhandled subject lies ([inboundLettersUnder]) — each
// subject's count of entries and the dead-letter stream's last sequence, as the
// broker's leader states them in one request however many entries there are.
// While they are the marks the cached listings agree with, nothing else is
// read: each cached listing is aged to now — every entry's age and the time it
// has left, so the five soonest to expire and the count expiring within a day
// move with the beat — and that is what `feed_dead_lettered` and
// `inbound_dead_lettered` read. Each subtree keeps the marks of its own that
// its listings agree with ([letterTree]), since a keep on one moves the last
// sequence the other reads too.
//
// When the marks MOVE, the subjects whose entries can have changed are listed
// again — headers only, never a frame ([queue.EventQueue.DeadLettersOn]):
//
//   - THE LAST SEQUENCE MOVED, so something was kept, and EVERY feed subject
//     that holds an entry now or held one at its last listing is listed
//     again, not only one whose count changed. `CREWLET_DLQ` keeps
//     subscriptions' poison too (`dlq.<topic>.<group>`), whose keeps move
//     the last sequence and no feed's count; and a keep and an expiry on ONE
//     feed's subject inside one beat leave its count where it was, where a
//     count alone would hide the new entry from the alarm and the expired
//     one from the count of wakes lost.
//   - ONLY A COUNT MOVED, so nothing was kept and entries can only have gone
//     — an expiry leaves from the front of a subject, since the stream keeps
//     by age and gives its sequences in the order it stores, and a drop from
//     anywhere — and every going lowers the count of the subject it left. The
//     subjects whose count moved are listed again, and no other.
//
// # A listing satisfies the marks only when it agrees with them
//
// The marks are the broker's leader's, and a listing is a REPLICA's, which can
// be behind it. A listing whose count is not the marks' count for its subject
// is still taken for what it says — the newest sight there is — but the marks
// it was taken for are NOT recorded as agreed: the last marks that were stay,
// so the next beat sees the move again and lists again, until a listing agrees.
// Recorded on a lagging listing, the marks would never move again for it, and
// the alarm would stay short an entry kept a moment before — or keep one
// dropped, and count its expiry a day late. A marks read or a listing that
// FAILS keeps the last listing that answered, aged to now, the same way: an
// entry is not gone because one read did not reach the broker, and an alarm
// that cleared on a broker blip would raise again on the next beat for nothing.
//
// # An entry that vanished at its expiry is a wake lost
//
// The broker lets an entry go at its retention without telling anybody, and an
// entry that goes is replayed — its wake published — discarded by an operator,
// whose loss `crewlet.tracker.feed.dead_letter_discarded` counts, or expired,
// its wake lost for good. So an entry the last listing held and this one does
// not is counted as expired (`crewlet.tracker.feed.dead_letter_expired`) when
// it is past its expiry by this listing, however many reads failed in
// between, or was within a beat ([statelog.AlarmInterval]) of it when last
// seen, since the broker's clock and this node's are not one. It is counted by
// whichever node first takes the claim on its going ([goneKey]), which a
// replay or a discard that dropped it took first ([deadLetterLedger]), so it
// is counted ONCE FLEET-WIDE and never for an entry somebody replayed or
// discarded, though every data node reads the marks on every beat. A listing
// names exactly which entries went, so a subject listed again because its
// count fell, or because a keep beside an expiry moved the last sequence, is
// judged entry by entry. What is NOT counted is an expiry across a restart of
// every node that saw it: a node starts with no sight of what it had seen. And
// a replay or a discard whose claim could not be taken, as near its expiry,
// would be counted by a peer — a count of losses erring towards a loss.
type deadLetterWatch struct {
	// ledger is the store the marks and the listings are read through and
	// the drops this node made; nil reads nothing.
	ledger *deadLetterLedger

	// claims accounts for each entry's going once across the fleet; nil
	// counts an expiry on every node that saw it, and an unreachable store
	// counts it anyway, the feed's own fail-open rule.
	claims expiryClaims

	metrics *metrics.Recorder

	// mu is held for a whole reading, so two reports — the beat's and an
	// operator's `crewlet retention status` — never list from one set of
	// marks twice, nor record the marks one of them listed for as the
	// other's.
	mu sync.Mutex
	// feeds and inbound are the two subtrees the reading counts by their
	// marks: every wake feed's subject, under the feeds' head, and the
	// inbound group's unhandled subject, under the inbound topic's.
	feeds, inbound letterTree
	// seen is each subject's last listing that answered.
	seen map[string]letterSight
}

// letterTree is what the reading knows of one subtree's marks: the marks every
// cached listing of its subjects agrees with, and whether any reading has
// recorded marks yet — before one has, every subject of it the marks count an
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

// letterSight is one listing that answered: when, and what it held — each
// entry's sequence and instants, never its header, which the reading does not
// need and would otherwise hold in memory for nothing.
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

// read is every feed's dead letters as the alarm reads them at now, by the
// domain whose feed keeps them — feeds is each domain's feed: the marks read
// once, each subject listed again where they say it can have changed, every
// entry that vanished at its expiry counted, and each entry's age and the time
// it has left. A domain whose feed holds no entry has none.
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

// readInbound is the inbound group's unhandled deliveries as the alarm reads
// them at now — the marks of its subtree read once, its subject listed again
// where they say it can have changed, every entry that vanished at its expiry
// counted, and each entry's age and the time it has left — for
// `inbound_dead_lettered`. Empty while it holds none.
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

// listMoved lists again every one of subjects that marks says can have changed
// since tree's agreed marks, counts what vanished from each at its expiry, and
// records marks as tree's agreed once every listing it took agrees with them —
// see [deadLetterWatch].
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

// countExpired counts every entry prior held that kept does not, that this
// node did not drop, and that was within a beat of its expiry when prior saw
// it — once across the fleet, and never for one a replay or a discard
// accounted for — as sub counts one.
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
			// GONE BEFORE ITS EXPIRY: not yet due by this listing, and more
			// than a beat from it when last seen — a replay or a discard,
			// here or on a peer. ONE DUE BY NOW IS JUDGED BY THE CLAIM
			// however long ago it was last seen, so an expiry while this
			// node's reads failed is still counted; the beat's slack covers
			// a broker whose clock runs a little ahead of this node's.
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
// winner does — none, where a replay took it first — and so does every node
// when the claim store cannot answer.
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

// goneKey is the claim an entry's going is accounted for under: the replay or
// the discard that dropped it takes it, and so does the reading that counts
// its expiry, so whoever takes it first is the one account of where the entry
// went. The entry's subject and sequence, which no later entry ever carries
// again.
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
