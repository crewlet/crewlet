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

// THE WAKE FEEDS' DEAD LETTERS, as an operator lists and discards them:
// `GET /feeds/dead-letters` and `POST /feeds/dead-letters/{id}/discard`
// (internal/api), which `crewlet feeds` is a client of.
//
// # Why an operator may discard one at all
//
// A replay clears an entry once a node starts able to read it, and the
// retention lets one go after `stream.event_retention_hours`. Neither is a
// decision anybody makes: an entry no build will ever read — a record sealed
// under a key retired fleet-wide, one no build translates and none will — keeps
// `feed_dead_lettered` raised for the whole retention, and an alarm that stands
// for a month for a loss already accepted is one nobody reads when the next
// entry, the one that matters, joins it. So an operator who has decided an
// entry's wake can be lost discards it, and the decision is recorded as
// theirs: a WARN naming who pressed, and
// `crewlet.tracker.feed.dead_letter_discarded`.
//
// # Through the ledger, never the raw queue
//
// A discarded entry vanishes as an expired one does, so a peer's alarm reading
// that saw it within a beat of its expiry would count it as a wake the
// retention lost ([deadLetterWatch]). The discard drops it through this node's
// [deadLetterLedger], which takes the claim the expiry count takes ([goneKey]),
// so the entry is accounted for once fleet-wide, as discarded.
//
// # Every node answers alike, and reads no frame
//
// The entries are the broker's, so every node serving the API lists the same
// ones — each from a copy of the dead-letter stream that can be a moment
// behind the broker's own ([queue.EventQueue.DeadLettersOn]) — and which feeds
// exist is the register's declaration ([declaredFeedSources]), never the logs
// this node happens to have running. A listing and a discard read the header
// each entry is kept under and never its frame: what an operator decides by is
// what the feed recorded of the record, and a record may be as large as its
// log admits.

// FeedDeadLetters is the wake feeds' dead letters, for the operator surfaces.
type FeedDeadLetters struct {
	// ledger is this node's one dead-letter ledger, which every feed's
	// replay and the alarm reading share ([Engine.deadLetterLedger]).
	ledger *deadLetterLedger

	// sources is every wake feed this build declares, in the register's
	// order.
	sources []changefeed.Source

	// metrics counts each discard; nil counts nothing.
	metrics *metrics.Recorder

	// now is the clock a listing is dated on.
	now func() time.Time
}

// FeedDeadLetters is this engine's operator surface over the wake feeds' dead
// letters, or nil on an engine that keeps none — a literal a case builds
// without a queue — which the API refuses by name rather than serving over
// nothing.
func (e *Engine) FeedDeadLetters() *FeedDeadLetters {
	if e.deadLetterLedger == nil {
		return nil
	}
	return &FeedDeadLetters{ledger: e.deadLetterLedger, sources: declaredFeedSources(),
		metrics: e.metrics, now: func() time.Time { return time.Now().UTC() }}
}

// declaredFeedSources is the wake feed every domain this build registers
// declares, in the register's order: the feeds whose dead letters an operator
// lists and discards.
//
// THROUGH [Engine.feedFor], the one door a feed is built through, over each
// domain's declaration alone — a ZERO ENGINE AND A BARE LOG, as the register's
// boot check builds each feed ([checkFeedEntry]) — because a feed's source is
// its translator's own constant: nothing of this node is read, so a node whose
// logs are not running names the same feeds as one that runs them all. A
// domain whose feed disagrees with its declaration is left out, and never
// boots: [checkRegister] refuses that build on every data node, which is every
// node that serves the API.
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
	// At is when the answering node listed them, on its clock: what each
	// entry's age and the time it has left are read against.
	At time.Time `json:"at"`

	// Feeds is every wake feed this build declares, in the register's order:
	// one holding no entry included, and one whose listing failed named
	// with why rather than left out, since an unread feed is not an empty
	// one.
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

	// ListError is why the subject could not be listed, absent where it
	// was.
	ListError string `json:"list_error,omitempty"`
}

// FeedDeadLetter is one dead letter as an operator reads it: what the feed that
// kept it recorded of the record, and when the broker kept it and lets it go.
//
// NEVER THE RECORD'S FRAME: sealed bytes no person reads, which only a replay
// opens, and a record may be as large as its log admits — a listing of them
// would carry megabytes to say nothing.
type FeedDeadLetter struct {
	// ID is the broker's sequence for the entry: what a discard names,
	// unique across every dead-letter subject and never reused.
	ID uint64 `json:"id"`

	// Source is the feed whose subject holds the entry.
	Source string `json:"source"`

	// Stream, Position and Key are where the record was delivered from — its
	// log, its sequence there and the subject it arrived on — and
	// Generation the log's generation and OpID the record's operation id,
	// both absent where the node that gave up never read the envelope.
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

	// DecodeError is why this build could not read the entry's header — a
	// newer build kept it, or it is no feed's header at all — absent where
	// it could. Such an entry is still listed, by everything the broker
	// says of it, and can still be discarded: a build that cannot read an
	// entry can still let it go.
	DecodeError string `json:"decode_error,omitempty"`
}

// List is every wake feed's dead letters, read from the broker now.
//
// A FEED WHOSE LISTING FAILED IS NAMED WITH ITS ERROR, never left out and never
// answered as empty: an operator deciding what to discard, or whether anything
// is left, reads an unread feed as a question still open.
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

// letterOf is the entry k of src's subject as an operator reads it: what the
// broker says of it, and what its header says where this build can read it.
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

	// By is who pressed — the author, their kind and the credential they
	// pressed it through — named on the line the discard leaves.
	By iam.Actor
}

// ErrNoDeadLetter is a discard naming an id no wake feed's dead letters hold:
// never one, an entry a replay, its expiry or another discard took first, or
// an entry on another `dlq.` subject — a subscription's poison, which is no
// feed's to discard. Nothing was removed. An entry kept a moment ago can read
// as none while the copy the listing read catches up, and a discard asked
// again finds it.
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
// # Found by listing every feed's subject
//
// A discard names the entry by the broker's sequence alone, which is unique
// across every dead-letter subject, and the drop has to name the subject too:
// so the feeds' subjects are listed until one holds it, and only that one is
// dropped from. An id on no feed's subject — a subscription's poison under
// another `dlq.` subject among them — is [ErrNoDeadLetter] and nothing is
// dropped: no subject outside the wake feeds' is ever asked of, so a discard
// can never reach a dead letter some other part of the engine keeps. Where a
// feed could not be listed and no other holds the id, the answer is
// [DeadLettersUnread], never a "no such entry" a feed that did not answer
// could contradict.
//
// # Counted only where this discard is the account of the entry's going
//
// The queue answers a drop of an entry already gone as done, so an entry a
// replay or another discard dropped, or the retention let go, between the
// listing and the drop would read as discarded here. The ledger says whether
// this drop's claim on the entry's going was the first ([deadLetterLedger.drop]);
// where it was not, the discard removed nothing, and answers [ErrNoDeadLetter]
// rather than count a wake lost — a replay's wake was published.
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

// discard drops the entry k from src's subject, and counts and records the
// loss where this drop is the account of the entry's going.
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
