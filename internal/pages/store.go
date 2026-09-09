package pages

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textcut"
)

// THE KNOWLEDGE BASE'S WRITE PATH: one record per operation, published to the
// subject of the thing it contends for.
//
// # Which subject, and why it is not always the page
//
// A body save, a label, a watch, a comment, a trash and a restore all contend
// for THE PAGE, so they arbitrate on its own subject and its version is the
// expectation. A create and a rename contend for THE ADDRESS — two people
// making "Deploy Runbook" must fight, and two fresh uuids never would — so
// they arbitrate on the title, create-only.
//
// That is why a rename is its own operation rather than a field of a save. One
// record has one subject, and a record that changed both a body and an address
// could only arbitrate one of them; the other would be a lost update with no
// symptom. A caller changing both makes two writes, each complete and each
// individually durable — which is an ordinary sequence of operations rather
// than the torn write the bucket's three-key create was.
//
// A rename stamps the head's `scoped_through` and never its `version`, on the
// tracker's own rule for a record that writes a row from another subject: the
// row's broker expectation still matches its subject's last message and cannot
// be poisoned into permanent unwritability, and a read barrier compares the
// MAX of the two.

// Store is the knowledge base's write path.
type Store struct {
	publisher *statelog.Publisher

	// db is the replicated estate, read by the few decisions that need to
	// see rows before they form a record. It is never the write path's own
	// snapshot — that is the framework's, taken per append — and nothing
	// read here is paired with an expectation.
	db *store.DB

	now      func() time.Time
	newID    func() string
	newSeqID func() string
}

// Options configure a store.
type Options struct {
	// Publisher is the domain's write authority, and DB the replicated
	// estate its decisions read.
	Publisher *statelog.Publisher
	DB        *store.DB

	// Now is the clock the AUTHORED instants are stamped from. Nil takes
	// the wall clock in UTC. An argument rather than a package call, so a
	// test can pin it and so nothing on the write path reads a clock the
	// applier is forbidden.
	Now func() time.Time
}

// NewStore builds the knowledge base over its own log.
func NewStore(opts Options) (*Store, error) {
	if opts.Publisher == nil {
		return nil, errors.New("pages: a write authority is required — every " +
			"write here is a record on the pages log, and a store with no " +
			"publisher could validate a page and then write it nowhere")
	}
	if opts.DB == nil {
		return nil, errors.New("pages: a store is required: a write decides " +
			"from this node's own applied rows, inside the snapshot the " +
			"expectation is formed in")
	}
	s := &Store{
		publisher: opts.Publisher, db: opts.DB, now: opts.Now,
		newID: uuid.NewString, newSeqID: newTimeOrderedID,
	}
	if s.now == nil {
		s.now = nowUTC
	}
	return s, nil
}

// newTimeOrderedID mints an id that sorts by creation time. UUIDv7, for the
// reason [tracker] gives, falling back to v4 rather than failing a write.
func newTimeOrderedID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// publish forms one record and appends it, translating the framework's
// refusals into this package's own vocabulary.
//
// THE THREE OUTCOMES TRAVEL UNCHANGED. What this adds is the one thing the
// framework cannot know: which of its refusals a caller can act on. A refused
// create is a NAME ALREADY TAKEN, which is the only refusal here a person can
// do something about without reading a log.
func (s *Store) publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	result, err := s.publisher.Publish(ctx, req)
	switch {
	case err == nil:
		return result, nil
	case errors.Is(err, statelog.ErrExists):
		return result, fmt.Errorf("%w: %s", ErrTitleTaken, req.Subject.ID)
	}
	return result, err
}

// decide builds one record inside the snapshot's own transaction.
func (s *Store) decide(actor Actor, subject Subject, op OpKind, scope ScopeSet,
	opID string, payload any, notify *Notify, at time.Time) (statelog.Decision, error) {

	if err := scope.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Decision{}, fmt.Errorf("pages: encode the %s payload "+
			"for %s: %w", op, subject, err)
	}
	record := MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: at, Scope: scope,
		},
		Mutation:   body,
		Actor:      actor.Name(),
		ActorKind:  actor.Kind,
		OperatorID: actor.OperatorID,
		TurnID:     actor.TurnID,
		Chain:      actor.Chain,
		Notify:     notify,
	}
	encoded, err := Encode(record)
	if err != nil {
		return statelog.Decision{}, err
	}
	return statelog.Decision{
		Payload: encoded,
		Envelope: statelog.Envelope{
			V: record.V, Kind: string(subject.Kind),
			Subject: statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
			Op:      string(op), OpID: opID, Scope: scope.Resolve(subject),
		},
	}, nil
}

// Validate refuses a scope a writer cannot mean.
func (s ScopeSet) Validate() error {
	if s.Subject {
		return nil
	}
	if len(s.Terms) == 0 {
		return errors.New("pages: a record's scope names nothing, which would " +
			"say it makes no read stale — the one claim a record this build " +
			"may not be able to decode must never make")
	}
	if len(s.Terms) > MaxScopeTerms {
		return fmt.Errorf("pages: a record's scope names %d objects and the cap "+
			"is %d — a writer whose affected set is wider emits the covering "+
			"container term instead", len(s.Terms), MaxScopeTerms)
	}
	for _, t := range s.Terms {
		switch t.Kind {
		case TermObject, TermTitle, TermContainer:
			if t.ID == "" {
				return fmt.Errorf("pages: a %s term names nothing", t.Kind)
			}
		case TermDomain:
		default:
			return fmt.Errorf("pages: scope term kind %q is not one this build "+
				"writes; it is READ as the whole domain, which is safe, and "+
				"WRITING one is a writer that cannot say what it touched", t.Kind)
		}
	}
	return nil
}

// casRounds bounds a read-decide-write retry, on [tracker]'s reasoning.
const casRounds = 16

// The sentinels. Each is a fact a caller acts on differently, which is why
// they are separate: a title somebody else holds is a rename to negotiate, a
// stale version is an edit to re-base, and an unreachable store is neither.
var (
	// ErrNotFound reports a page, container or revision that does not exist.
	ErrNotFound = errors.New("pages: no such record")

	// ErrTitleTaken reports a title another page in the container holds.
	ErrTitleTaken = errors.New("pages: that title is taken in this container")

	// ErrStaleVersion reports a save against a version that has moved.
	ErrStaleVersion = errors.New("pages: the page has changed since it was read")

	// ErrConflict reports a write that lost its race too many times.
	ErrConflict = errors.New("pages: the page kept changing under this write")

	// ErrReserved reports a container the engine holds for itself.
	ErrReserved = errors.New("pages: that container is reserved")
)

// Actor is who is making a write, on [tracker.Writer]'s terms — except that it
// travels per CALL here rather than on the store, because one node's knowledge
// base serves every seat and every operator through one write path.
type Actor struct {
	Handle     string
	Kind       AuthorKind
	OperatorID string
	TurnID     string
	Chain      []string
}

// Name is how this actor is recorded and rendered.
//
// AN OPERATOR HAS NO SEAT HANDLE, and that is the whole shape of the operator
// surface: a write there carries the TOKEN's own name and author kind
// `operator`, and there is deliberately no way for a caller to name a seat to
// act as — a knowledge base whose author field is chosen by the writer is not
// an audit trail.
func (a Actor) Name() string {
	if a.Handle != "" {
		return a.Handle
	}
	if a.OperatorID != "" {
		return "operator:" + a.OperatorID
	}
	return "operator:anonymous"
}

// IsHuman reports an actor a notification treats as a person.
func (a Actor) IsHuman() bool { return a.Kind == AuthorHuman || a.Kind == AuthorOperator }

func (a Actor) validate() error {
	if !a.Kind.Valid() {
		return invalid("actor.kind", "%q is not one of %v", a.Kind, AuthorKinds())
	}
	// EVERY KIND BUT AN OPERATOR MUST NAME ITS SEAT. An operator is named
	// by the token it presented, which [Actor.Name] renders; a seat or an
	// agent with no handle is a change nobody made.
	if a.Kind != AuthorOperator && strings.TrimSpace(a.Handle) == "" {
		return invalid("actor.handle",
			"an %s write must name the seat it acts as", a.Kind)
	}
	return nil
}

// excerpt is what a card shows, cut at [MaxExcerpt] bytes without breaking a
// rune.
func excerpt(text string) string {
	return textcut.Ellipsis(strings.Join(strings.Fields(text), " "), MaxExcerpt)
}

// cleanList trims, drops empties and de-duplicates, keeping the caller's
// order.
func cleanList(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// recipientsOf is the watcher set MINUS the muted, computed once at write time
// so the feed never has to subtract and can never forget to.
func recipientsOf(watchers, muted []string) []string {
	skip := map[string]bool{}
	for _, m := range muted {
		skip[m] = true
	}
	out := make([]string, 0, len(watchers))
	for _, w := range watchers {
		if !skip[w] {
			out = append(out, w)
		}
	}
	slices.Sort(out)
	return out
}

// checkTitle, checkBody and checkLabels refuse a value at WRITE naming the
// field, never cut: a value silently truncated is a value a person will look
// for later.
func (s *Store) checkTitle(title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return invalid("title", "a page needs a title — it is its address")
	}
	if len(title) > MaxTitle {
		return invalid("title", "%d bytes, past the %d-byte cap",
			len(title), MaxTitle)
	}
	return nil
}

func (s *Store) checkBody(body string) error {
	if len(body) > MaxBody {
		return invalid("body", "%d bytes, past the %d-byte cap", len(body), MaxBody)
	}
	return nil
}

func (s *Store) checkLabels(labels []string) error {
	if len(labels) > MaxLabels {
		return invalid("labels", "%d labels, past the cap of %d",
			len(labels), MaxLabels)
	}
	for _, l := range labels {
		if len(l) > MaxLabelLength {
			return invalid("labels", "%q is %d bytes, past the %d-byte cap",
				l, len(l), MaxLabelLength)
		}
	}
	return nil
}

// readHeadTx reads one page's head from inside a decision's own transaction.
func readHeadTx(ctx context.Context, tx *sql.Tx, id string) (Page, error) {
	var document []byte
	err := tx.QueryRowContext(ctx,
		`SELECT document FROM pages_heads WHERE id = ?`, id).Scan(&document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Page{}, fmt.Errorf("%w: page %s", ErrNotFound, id)
	case err != nil:
		return Page{}, fmt.Errorf("pages: read the head of %s: %w", id, err)
	}
	return DecodePage(document)
}

// slicesSort is an ascending sort in place, named so every call site reads as
// the deterministic-order rule it is: a collection written in a different
// order on two nodes makes the replicated file's checksum diverge.
func slicesSort(in []string) { slices.Sort(in) }

// nullableTime is a time column that may be NULL.
func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return store.EncodeTime(*t)
}

// SkillDetector reports whether a page's body is a tool skill.
//
// A SEAM rather than a direct call into the skills package, because it is this
// build's parser answering about this build's rules: a page written by a newer
// node must not carry a claim an older node's parser disagrees with, so the
// flag is DERIVED at apply time and recomputed on every rebuild. Nil answers
// no, which is what a build with no skill parser wired has — and a nil
// detector is why the row is Divergent rather than Replicated.
type SkillDetector interface {
	IsSkill(body string) bool
}
