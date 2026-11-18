package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE WAKE FEEDS' DEAD LETTERS, as an operator lists and discards them
// (`GET /feeds/dead-letters`, `POST /feeds/dead-letters/{id}/discard`, and
// `crewlet feeds`). A discard exists because an entry no build will ever read
// keeps `feed_dead_lettered` raised for the whole retention, burying the next
// one that matters; the operator's decision is recorded as theirs (a WARN and
// `crewlet.tracker.feed.dead_letter_discarded`).
//
// It drops through this node's [deadLetterLedger], taking the expiry count's
// claim ([goneKey]), so a peer's alarm ([deadLetterWatch]) never counts it as
// a wake the retention lost. Every node lists alike: feeds come from the
// register ([declaredFeedSources]), not this node's running logs, and only
// each entry's header is read, never its frame, which can be huge.

// FeedDeadLetters is the wake feeds' dead letters, for the operator surfaces.
type FeedDeadLetters struct {
	// ledger is this node's one dead-letter ledger, shared with every feed's
	// replay and the alarm reading ([Engine.deadLetterLedger]).
	ledger *deadLetterLedger

	// sources is every wake feed this build declares, in register order.
	sources []changefeed.Source

	// metrics counts each discard; nil counts nothing.
	metrics *metrics.Recorder

	// now is the clock a listing is dated on.
	now func() time.Time
}

// FeedDeadLetters is this engine's operator surface over the wake feeds' dead
// letters, or nil on an engine with no queue, which the API refuses by name.
func (e *Engine) FeedDeadLetters() *FeedDeadLetters {
	if e.deadLetterLedger == nil {
		return nil
	}
	return &FeedDeadLetters{ledger: e.deadLetterLedger, sources: declaredFeedSources(),
		metrics: e.metrics, now: func() time.Time { return time.Now().UTC() }}
}

// declaredFeedSources is the wake feed every registered domain declares, in
// register order. Built through [Engine.feedFor] on a ZERO ENGINE AND A BARE
// LOG, as [checkFeedEntry] does, since a source is its translator's constant:
// a node whose logs are not running names the same feeds as any other. A
// domain whose feed fails is left out; [checkRegister] refuses that build on
// every data node, which is every node serving the API.
func declaredFeedSources() []changefeed.Source {
	var out []changefeed.Source
	for _, domain := range registeredDomains() {
		translator, _, err := (&Engine{}).feedFor(&runningLog{domain: domain})
		if err == nil && translator != nil {
			out = append(out, translator.Source())
		}
	}
	return out
}

// FeedDeadLetterListing is every wake feed's dead letters, as an operator
// reads them.
type FeedDeadLetterListing struct {
	// At is when the answering node listed them, on its clock, which each
	// entry's age and remaining time are read against.
	At time.Time `json:"at"`

	// Feeds is every wake feed this build declares, in register order, empty
	// ones included and a failed one named with why: an unread feed is not
	// an empty one.
	Feeds []FeedDeadLetterFeed `json:"feeds"`
}

// FeedDeadLetterFeed is one wake feed's dead letters.
type FeedDeadLetterFeed struct {
	// Source and Group are the feed ([changefeed.Source]), and Subject the
	// dead-letter subject it keeps its entries on
	// ([changefeed.DeadLetterSubject]).
	Source  string `json:"source"`
	Group   string `json:"group"`
	Subject string `json:"subject"`

	// Entries is what the subject holds, oldest first: empty where it holds
	// none, and null beside a ListError.
	Entries []FeedDeadLetter `json:"entries"`

	// ListError is why the subject could not be listed, absent if it was.
	ListError string `json:"list_error,omitempty"`
}

// FeedDeadLetter is one dead letter as an operator reads it: what the feed
// recorded of the record, and when the broker kept it and lets it go. NEVER
// THE RECORD'S FRAME: sealed bytes only a replay opens, possibly megabytes.
type FeedDeadLetter struct {
	// ID is the broker's sequence for the entry: what a discard names,
	// unique across every dead-letter subject and never reused.
	ID uint64 `json:"id"`

	// Source is the feed whose subject holds the entry.
	Source string `json:"source"`

	// Stream, Position and Key are the record's log, sequence there and
	// arrival subject; Generation the log's generation and OpID the record's
	// operation id, both absent where the node that gave up never read the
	// envelope.
	Stream     string `json:"stream,omitempty"`
	Position   uint64 `json:"position,omitempty"`
	Generation uint64 `json:"generation,omitempty"`
	Key        string `json:"key,omitempty"`
	OpID       string `json:"op_id,omitempty"`

	// Reason is why no node read it ([changefeed.DeferReason]), and Detail
	// what the node that gave up met.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`

	// StoredAt is the broker's instant for the record, Delivered how many
	// times it had been delivered when the feed gave up, and
	// DeadLetteredAt when it did, on that node's clock.
	StoredAt       time.Time `json:"stored_at,omitzero"`
	Delivered      uint64    `json:"delivered,omitempty"`
	DeadLetteredAt time.Time `json:"dead_lettered_at,omitzero"`

	// KeptAt is when the broker kept the entry, and ExpiresAt when its
	// retention lets it go.
	KeptAt    time.Time `json:"kept_at"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`

	// DecodeError is why this build could not read the entry's header (a
	// newer build's, or no feed's), absent where it could. Such an entry is
	// still listed by what the broker says and can still be discarded.
	DecodeError string `json:"decode_error,omitempty"`
}

// List is every wake feed's dead letters, read from the broker now. A feed
// whose listing failed is named with its error, never left out or empty.
func (f *FeedDeadLetters) List(ctx context.Context) FeedDeadLetterListing {
	out := FeedDeadLetterListing{At: f.now(), Feeds: make([]FeedDeadLetterFeed, 0, len(f.sources))}
	for _, src := range f.sources {
		feed := FeedDeadLetterFeed{Source: src.Name, Group: src.Group,
			Subject: changefeed.DeadLetterSubject(src)}
		kept, err := f.ledger.DeadLettersOn(ctx, feed.Subject)
		if err != nil {
			feed.ListError = err.Error()
		} else {
			feed.Entries = make([]FeedDeadLetter, 0, len(kept))
			for _, k := range kept {
				feed.Entries = append(feed.Entries, letterOf(src, k))
			}
		}
		out.Feeds = append(out.Feeds, feed)
	}
	return out
}

// letterOf is the entry k of src's subject as an operator reads it: the
// broker's facts, plus its header where this build can read it.
func letterOf(src changefeed.Source, k queue.Kept) FeedDeadLetter {
	out := FeedDeadLetter{ID: k.Seq, Source: src.Name, KeptAt: k.StoredAt, ExpiresAt: k.Expires}
	letter, err := changefeed.DecodeDeadLetter(k.Header)
	if err != nil {
		out.DecodeError = err.Error()
		return out
	}
	out.Stream, out.Position, out.Generation = letter.Stream, letter.Seq, letter.Gen
	out.Key, out.OpID = letter.Key, letter.OpID
	out.Reason, out.Detail = string(letter.Reason), letter.Detail
	out.StoredAt, out.Delivered, out.DeadLetteredAt = letter.StoredAt, letter.Delivered,
		letter.DeadLetteredAt
	return out
}

// FeedDeadLetterDiscard is one discard: the entry's id, and who pressed.
type FeedDeadLetterDiscard struct {
	// ID is the entry's broker sequence, as a listing names it.
	ID uint64

	// By is who pressed (author, kind, credential), named on the discard's
	// log line.
	By iam.Actor
}

// ErrNoDeadLetter is a discard naming an id no wake feed's dead letters hold:
// never one, already taken by a replay, expiry or another discard, or on
// another `dlq.` subject (a subscription's poison). Nothing was removed. A
// just-kept entry can read as none until the listed copy catches up; a
// retried discard finds it.
var ErrNoDeadLetter = errors.New("engine: no wake feed holds a dead letter by that id")

// DeadLettersUnread is a discard whose id no wake feed that could be listed
// holds, beside a feed whose listing failed: the entry may be that feed's, so
// nothing was removed and whether it is there is unknown.
type DeadLettersUnread struct {
	// ID is the entry the discard named.
	ID uint64

	// Feeds is each feed that could not be listed, with why, in the
	// register's order.
	Feeds []UnreadFeed
}

// UnreadFeed is one wake feed whose dead letters could not be listed, and
// why.
type UnreadFeed struct {
	Source string
	Err    error
}

func (e *DeadLettersUnread) Error() string {
	parts := make([]string, len(e.Feeds))
	for i, feed := range e.Feeds {
		parts[i] = fmt.Sprintf("the %s feed's (%v)", feed.Source, feed.Err)
	}
	return fmt.Sprintf("engine: no wake feed that could be listed holds dead letter %d, and "+
		"these could not be listed, so whether it is theirs is unknown and nothing was "+
		"discarded: %s", e.ID, strings.Join(parts, "; "))
}

// Unwrap is each unread feed's error, so a caller asks of them whether
// waiting may clear the refusal ([queue.ErrUnavailable]).
func (e *DeadLettersUnread) Unwrap() []error {
	errs := make([]error, len(e.Feeds))
	for i, feed := range e.Feeds {
		errs[i] = feed.Err
	}
	return errs
}

// Discard drops the dead letter req.ID from whichever wake feed's subject
// holds it, through this node's ledger, and answers the entry it dropped.
//
// The id is the broker sequence alone, but a drop names a subject, so the
// feeds' subjects are listed until one holds it. Only wake feeds' subjects are
// asked, so a discard never reaches another `dlq.` subject's entry
// ([ErrNoDeadLetter]); with a feed unlisted and no hit the answer is
// [DeadLettersUnread], never "no such entry".
//
// The queue answers a drop of an already-gone entry as done, so only a first
// claim on its going ([deadLetterLedger.drop]) counts; otherwise it answers
// [ErrNoDeadLetter] rather than count a replay's published wake as lost.
func (f *FeedDeadLetters) Discard(ctx context.Context, req FeedDeadLetterDiscard) (FeedDeadLetter, error) {
	if req.ID == 0 {
		return FeedDeadLetter{}, fmt.Errorf("%w: a dead letter's id is never 0", ErrNoDeadLetter)
	}
	var unread DeadLettersUnread
	for _, src := range f.sources {
		subject := changefeed.DeadLetterSubject(src)
		kept, err := f.ledger.DeadLettersOn(ctx, subject)
		if err != nil {
			unread.Feeds = append(unread.Feeds, UnreadFeed{Source: src.Name, Err: err})
			continue
		}
		if i := slices.IndexFunc(kept, func(k queue.Kept) bool { return k.Seq == req.ID }); i >= 0 {
			return f.discard(ctx, src, subject, kept[i], req.By)
		}
	}
	if len(unread.Feeds) > 0 {
		unread.ID = req.ID
		return FeedDeadLetter{}, &unread
	}
	return FeedDeadLetter{}, fmt.Errorf("%w: %d is on no wake feed's dead-letter subject",
		ErrNoDeadLetter, req.ID)
}

// discard drops the entry k from src's subject, counting and recording the
// loss only where this drop is the first claim on its going.
func (f *FeedDeadLetters) discard(ctx context.Context, src changefeed.Source, subject string,
	k queue.Kept, by iam.Actor) (FeedDeadLetter, error) {

	entry := letterOf(src, k)
	first, err := f.ledger.drop(ctx, subject, k.Seq)
	if err != nil {
		return FeedDeadLetter{}, fmt.Errorf("engine: discard dead letter %d of the %s feed: %w",
			k.Seq, src.Name, err)
	}
	if !first {
		return FeedDeadLetter{}, fmt.Errorf("%w: %d went before this discard reached it, taken "+
			"first by a replay that published its wake, by its expiry, or by another discard",
			ErrNoDeadLetter, k.Seq)
	}
	if f.metrics != nil {
		f.metrics.Add(metrics.TrackerFeedDeadLetterDiscarded, 1, metrics.Attrs{"source": src.Name})
	}
	log.WarnContext(ctx, "changefeed_dead_letter_discarded", "by", by.Name,
		"by_kind", string(by.Kind), "operator", by.OperatorID, "source", src.Name,
		"subject", subject, "id", k.Seq, "stream", entry.Stream, "position", entry.Position,
		"key", entry.Key, "op_id", entry.OpID, "reason", entry.Reason, "kept_at", entry.KeptAt,
		"detail", "an operator discarded it: the wake it held is lost for good, and no node "+
			"will derive it")
	return entry, nil
}
