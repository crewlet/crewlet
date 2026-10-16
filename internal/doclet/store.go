package doclet

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
// expectation. A create and a rename that MOVES the address contend for THE
// ADDRESS — two people making "Deploy Runbook" must fight, and two fresh uuids
// never would — so they arbitrate on the title, create-only.
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
//
// # The third case: a rename that does NOT move the address
//
// "Runbook" -> "RUNBOOK" changes the DISPLAYED title and nothing else. The
// claim is keyed on the normalised title, so it does not move; the only row
// the write touches is the page's own head. It therefore arbitrates on THE
// PAGE, like every other write to a field of the head, and carries its own op
// — see [OpRetitle] and [Store.retitle]. Sending it to the title's subject
// instead would take a create-only append at an address this page already
// holds, lose to its own claim, and report a name somebody else took; and the
// version of this that DISCARDED it as a no-op answered `applied` while the
// displayed title every reader renders stayed where it was.

// Store is the knowledge base's write path.
type Store struct {
	publisher *statelog.Publisher

	// db is the replicated estate, read by the few decisions that need to
	// see rows before they form a record. It is never the write path's own
	// snapshot — that is the framework's, taken per append — and nothing
	// read here is paired with an expectation.
	db store.ReplicatedReader

	// reserved names the containers a SEAT may not write to, or not add a
	// page to, and every container a page may be created in at all. See
	// [Options.Reserved].
	reserved func() Reserved

	now      func() time.Time
	newSeqID func() string
}

// Options configure a store.
type Options struct {
	// Publisher is the domain's write authority, and DB the replicated
	// estate its decisions read.
	Publisher *statelog.Publisher
	DB        store.ReplicatedReader

	// Now is the clock the AUTHORED instants are stamped from. Nil takes
	// the wall clock in UTC. An argument rather than a package call, so a
	// test can pin it and so nothing on the write path reads a clock the
	// applier is forbidden.
	Now func() time.Time

	// Reserved names the containers this company holds back from an
	// AGENT's writes — see [Reserved] for the two and what each refuses;
	// a refused write answers [ErrReserved] — and the spaces it DECLARES,
	// outside which nobody creates a page ([Reserved.Declared]).
	//
	// NIL DECLARES NOTHING, so a store built without it creates no page:
	// the declared spaces are the one rule here that binds a person as well
	// as an agent, and a seam whose absence meant "anywhere" would be the
	// way a surface wired later created pages in containers nobody named.
	//
	// # Why it is here and not at a tool
	//
	// It was one check at `write_page`, so a seat that could not CREATE a
	// page in the skills container could still save over one, rename one
	// and comment on one — and every surface that grows later inherits
	// the same hole, silently, because nothing says the rule exists. The
	// store is the one place every write path goes through.
	//
	// # Why the ACTOR and not the authority table
	//
	// Because this is not a question about capability. internal/authz
	// decides who may write a page at all, and — for a person — who may
	// write a tool skill; what is left here is a rule about what an AGENT
	// cannot see from anything it holds: that a page in the skills
	// container is machinery rather than knowledge, and that the org root
	// is the company's own canon rather than a team's notebook. A person
	// publishing into either on purpose is what those containers are for.
	//
	// A FUNCTION because the keys are Tier B: `knowledge.skills_container`
	// and `knowledge.root_space` move on an apply, and this store is built
	// once per node.
	Reserved func() Reserved
}

// Reserved is what a company holds back from an AGENT's writes.
//
// TWO CONTAINERS, AND THEY ARE NOT RESERVED FOR THE SAME REASON, so they do not
// refuse the same gestures. Folding them into one list is what made every
// write into the org root a refusal whose sentence was false: the root is
// searched and routed like any other container, and only the skills container
// is not.
type Reserved struct {
	// Skills is the tool-skills container, and EVERY write an agent makes
	// there is refused — a create, a save, a rename and every comment
	// gesture. Its pages are machinery the engine injects into a phase of
	// every seat's turn as an instruction, and it is excluded from
	// knowledge search and from routing: a page a seat writes there is
	// either never read or read by every seat as an order, and a remark on
	// one reaches nobody. Empty is a company with tool skills turned off.
	Skills string

	// Root is the organisation's own container — what is true of the
	// whole company, starting with the Onboarding page every seat reads
	// first — and an agent may not ADD a page there. A new page at the top
	// of the company is a person's decision; a seat publishes into its own
	// team's space. The pages already there are ordinary pages, searched
	// and routed like any other, so a seat keeping one current, renaming
	// it or remarking on it is ordinary upkeep and is not refused.
	Root string

	// Declared is every container key the company DECLARES — each unit's
	// and each seat's `space:`, `knowledge.root_space` and
	// `knowledge.skills_container` — and a page is CREATED only in one of
	// them, by anybody: [ErrInvalid] naming the key and how a space is
	// declared otherwise.
	//
	// # Why a create, and why a person too
	//
	// A container is a space the org chart names — who leads it, who is
	// woken by what happens in it, which team's knowledge it is — and a
	// page filed under a key nothing declares belongs to nobody: no lead
	// tidies it, no routing reaches it, and a typo of a real key splits a
	// team's knowledge in two with nothing to say so. That is a fact about
	// the KEY rather than about who typed it, which is why this, unlike the
	// two above, refuses a person exactly as it refuses an agent — the same
	// rule a task's create holds an undeclared project to. Only a create
	// asks: a save cannot move a page across containers and a rename keeps
	// its container, so every other write is already inside a space that
	// was declared when the page was made.
	//
	// THE MEANINGFUL ZERO: nil and empty both declare nothing, and every
	// create is refused.
	Declared []string
}

// NewStore builds the knowledge base over its own log.
func NewStore(opts Options) (*Store, error) {
	if opts.Publisher == nil {
		return nil, errors.New("doclet: a write authority is required; every " +
			"write here is a record on the pages log, and a store with no " +
			"publisher could validate a page and then write it nowhere")
	}
	if opts.DB.IsZero() {
		return nil, errors.New("doclet: a store is required: a write decides " +
			"from this node's own applied rows, inside the snapshot the " +
			"expectation is formed in")
	}
	s := &Store{
		publisher: opts.Publisher, db: opts.DB, now: opts.Now,
		reserved: opts.Reserved,
		newSeqID: newTimeOrderedID,
	}
	if s.now == nil {
		s.now = nowUTC
	}
	return s, nil
}

// newTimeOrderedID mints a fresh operation id, which sorts by creation time and
// carries the instant it was minted.
//
// THROUGH [statelog.NewOpID] rather than a UUIDv7 minted here, because the
// instant is what the state log reads to decide whether its ledger can vouch
// for a retry of this operation — and the fallback this had, a v4 when a v7
// could not be minted, is an id carrying no instant at all, which a node whose
// ledger's sweep ever lost a row answers `unknown` for ever.
//
// THE WALL CLOCK and not the store's own [Store.now], which is the clock the
// AUTHORED instants are stamped from and which a test pins: the mint instant
// is compared with the ledger's watermark, which a sweep sets off the wall.
func newTimeOrderedID() string { return statelog.NewOpID(time.Now(), "") }

// publish forms one record and appends it, translating the framework's
// refusals into this package's own vocabulary.
//
// THE THREE OUTCOMES TRAVEL UNCHANGED. What this adds is the one thing the
// framework cannot know: which of its refusals a caller can act on. A refused
// create is a NAME ALREADY TAKEN and a write that ran out of compare-and-set
// rounds is A PAGE SOMEBODY ELSE KEPT MOVING — two different instructions to
// whoever hit them, and the only two refusals here a person can act on without
// reading a log.
//
// The conflict carries the framework's error along with this package's, so a
// caller reasoning in either vocabulary matches. A taken title does not: "that
// name is held in this container" is the whole of what happened, while the
// framework's wording for it — the object already exists — names an object
// nobody asked about.
//
// ONLY ON A SUBJECT A PERSON EDITS — see [refusal], which reads the subject's
// kind: a node's eviction gate goes through here too, and on its subject the
// translation named a page after a node id.
func (s *Store) publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	result, err := s.publisher.Publish(ctx, req)
	return result, refusal(err, req.Subject)
}

// onPage publishes one write about one page, stating the page's CONTAINER in
// the request's scope.
//
// # Why the container has to be read before the decide
//
// The request's scope is what the framework probes for a deferred record
// before the decision and waits on after the append ([statelog.Request]), and
// it is stated before the decide runs. A page's subject is its uuid, and the
// container is a fact about its ROW — one a rename record states and its apply
// writes — so the subject alone resolves to the path of a page in no container
// at all. Every page
// write said exactly that, while the record it decided carried the real
// container: a record deferred on the page's space, or on the page itself
// under its space, was never matched, so a write decided over rows that were
// behind for that space and reported them applied — the containment probe the
// scope grammar exists for, silently answering "nothing is deferred". So the
// container is read first and the decide checks it still holds, the shape the
// tracker takes with a task's project and [Store.Rename] with a page's title.
//
// A PAGE THIS NODE DOES NOT HOLD is published with the subject alone: the
// decide refuses it as not found, which no scope makes wrong, and the one
// thing that must still answer — a retried operation the ledger already holds
// — is answered before any scope is probed. A page that arrives between the
// read and the decide is a move like any other.
//
// request builds the request for a container; its decide must refuse a head
// in any other container with [errPageMoved], via [inContainer].
func (s *Store) onPage(ctx context.Context, pageID string,
	request func(container string) statelog.Request) (statelog.Result, error) {

	return followMoves(
		func() (string, error) { return s.containerOf(ctx, pageID) },
		func(container string) (statelog.Result, error) {
			return s.publish(ctx, request(container))
		})
}

// followMoves is [Store.onPage]'s loop over values: read the container,
// publish scoped to it, and read again when the decide found the page moved —
// at most [pageMoveAttempts] times, answering the last move after that.
func followMoves(read func() (string, error),
	publish func(container string) (statelog.Result, error)) (statelog.Result, error) {

	var (
		result statelog.Result
		err    error
	)
	for range pageMoveAttempts {
		var container string
		if container, err = read(); err != nil {
			return statelog.Result{}, err
		}
		result, err = publish(container)
		if !errors.Is(err, errPageMoved) {
			return result, err
		}
	}
	return result, err
}

// pageMoveAttempts is how many times [Store.onPage] reads a page's container
// again after it moved between the read and the decide.
//
// THREE: a move is a rename somebody chose to make, so two inside the window
// between one read and one snapshot is already a coincidence, and a third is
// a page being moved faster than anybody can write to it.
const pageMoveAttempts = 3

// errPageMoved is a page that left the container its request's scope named
// between the read that named it and the decide.
var errPageMoved = errors.New("doclet: the page moved to another space under this write")

// containerOf is the container pageID is in, read outside any decision, or
// empty when this node holds no such page.
func (s *Store) containerOf(ctx context.Context, pageID string) (string, error) {
	var container string
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`SELECT container FROM pages_heads WHERE id = ?`, pageID).Scan(&container)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return "", fmt.Errorf("doclet: read the space page %s is in: %w", pageID, err)
	}
	return container, nil
}

// inContainer refuses a head that is not in the container its request's scope
// named — see [Store.onPage].
func inContainer(head Page, container string) error {
	if head.Container == container {
		return nil
	}
	return fmt.Errorf("%w: page %s is in %q, and this write was scoped to %q",
		errPageMoved, head.ID, head.Container, container)
}

// refusal is that translation, as a function over values.
//
// IT TRANSLATES ONLY ON A SUBJECT A PERSON EDITS — a page, a title, a
// container. The log's own subjects (a node's eviction gate, a reanchor's
// generation, a barrier) are arbitrated by the same framework, and their
// refusals stay in its words: translated, two operators contending over one
// node's gate were told "the page kept changing under this write: <node id>",
// about an edit nobody was making.
//
// Separate from the append because the append needs a broker, a store and a
// log, and the two refusals that reach a person are reachable through none of
// them on demand: an exhausted compare-and-set is sixteen lost races, which no
// test can stage. A rule that can only be exercised by a race is a rule nobody
// re-reads — and the branch a seat is told to act on had no producer at all
// until this function existed, so [ErrConflict] was a sentinel the tool
// surface matched on and nothing ever returned.
func refusal(err error, subject statelog.Subject) error {
	if err == nil {
		return nil
	}
	switch ObjectKind(subject.Kind) {
	case KindPage, KindTitle, KindContainer:
	default:
		return err
	}
	switch {
	case errors.Is(err, statelog.ErrExists):
		return fmt.Errorf("%w: %s", ErrTitleTaken, subject.ID)
	case errors.Is(err, statelog.ErrConflict):
		// The retry bound lives in the framework, which is where the
		// rounds are counted; what does not live there is that a page is
		// a thing a PERSON is editing, so the answer a seat needs is
		// "read it again", not "the append failed".
		return fmt.Errorf("%w: %s: %w", ErrConflict, subject.ID, err)
	}
	return err
}

// decide builds one record inside the snapshot's own transaction.
//
// THE STAMP IS THE FRAMEWORK'S, and it goes on the envelope here because this
// is the one builder every write path shares. It was never set: the writer and
// the generation were fields every record carried empty, so the eviction gate
// — which reads the writer on every applier before any kind rule — had nobody
// to drop, and a node the fleet had evicted went on writing pages everybody
// applied. The publisher now refuses a record that does not carry it.
func (s *Store) decide(stamp statelog.Stamp, actor Actor, subject Subject, op OpKind,
	scope ScopeSet, opID string, payload any, notify *Notify, at time.Time) (statelog.Decision, error) {

	if err := scope.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Decision{}, fmt.Errorf("doclet: encode the %s payload "+
			"for %s: %w", op, subject, err)
	}
	record := MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// NO VERSION: the encoder stamps the lowest one that reads
			// what this record carries — see [RecordVersion].
			OpID: opID, Subject: subject, Op: op,
			CreatedAt: at, Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
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
	return statelog.Decision{Payload: encoded}, nil
}

// Validate refuses a scope a writer cannot mean.
func (s ScopeSet) Validate() error {
	if s.Subject {
		return nil
	}
	if len(s.Terms) == 0 {
		return errors.New("doclet: a record's scope names nothing, which would " +
			"say it makes no read stale — the one claim a record this build " +
			"may not be able to decode must never make")
	}
	if len(s.Terms) > MaxScopeTerms {
		return fmt.Errorf("doclet: a record's scope names %d objects and the cap "+
			"is %d — a writer whose affected set is wider emits the covering "+
			"container term instead", len(s.Terms), MaxScopeTerms)
	}
	for _, t := range s.Terms {
		switch t.Kind {
		case TermObject, TermTitle, TermContainer:
			if t.ID == "" {
				return fmt.Errorf("doclet: a %s term names nothing", t.Kind)
			}
		case TermDomain:
		default:
			return fmt.Errorf("doclet: scope term kind %q is not one this build "+
				"writes; it is READ as the whole domain, which is safe, and "+
				"WRITING one is a writer that cannot say what it touched", t.Kind)
		}
	}
	return nil
}

// The sentinels. Each is a fact a caller acts on differently, which is why
// they are separate: a title somebody else holds is a rename to negotiate, a
// stale version is an edit to re-base, and an unreachable store is neither.
var (
	// ErrNotFound reports a page, container or revision that does not exist.
	ErrNotFound = errors.New("doclet: no such record")

	// ErrTitleTaken reports a title another page in the container holds.
	ErrTitleTaken = errors.New("doclet: that title is taken in this container")

	// ErrStaleVersion reports a save against a version that has moved.
	ErrStaleVersion = errors.New("doclet: the page has changed since it was read")

	// ErrConflict reports a write that lost its race too many times.
	ErrConflict = errors.New("doclet: the page kept changing under this write")

	// ErrReserved reports a container the engine holds for itself.
	ErrReserved = errors.New("doclet: that container is reserved")

	// ErrParent reports a parent a page cannot be filed under: a page that
	// does not exist or was purged, one in the trash, one in another
	// container, or the page itself or one beneath it. Every such refusal
	// also answers [ErrInvalid], because it IS a refusal of one field — a
	// caller handling field refusals handles it — and this sentinel is what
	// tells "choose another parent" from every other field's remedy.
	ErrParent = errors.New("doclet: not a parent this page can have")
)

// Actor is who is making a write, on [worklet.Writer]'s terms, except that it
// travels per CALL here rather than on the store, because one node's knowledge
// base serves every seat and every operator through one write path.
type Actor struct {
	Handle     string
	Kind       AuthorKind
	OperatorID string
	TurnID     string
	Chain      []string

	// OpKey is the caller's own operation key: the value a RETRY of this
	// same gesture reproduces, which every write here derives its operation
	// id from when it is set.
	//
	// ON THE ACTOR because it travels with the call and not with the store,
	// for the same reason the actor does — and because it is what the
	// tracker's actor already carries (`builtin.Actor.WorkKey`), so the two
	// domains take a retry key the same way. EMPTY MINTS A FRESH ID PER
	// CALL, which is right for every caller that will never retry the same
	// gesture — and wrong for the one that must: an HTTP write whose answer
	// was `unknown` can only be retried safely under the SAME id, because
	// the operation ledger is what recognises a second arrival, and a fresh
	// one would append the trash, the rename or the comment twice.
	//
	// AN OPERATION ID THE ENGINE MINTED, held to [statelog.CheckCallerOpID]
	// by every surface that takes one and by every write here, which refuses
	// an actor carrying any other: the ids derived from it carry its instant
	// — see [Store.operation].
	OpKey string
}

// operation is the id one write is published under: derived from the actor's
// [Actor.OpKey] when there is one, and freshly minted when there is not.
//
// THE VERB AND THE OBJECT ARE PART OF THE DERIVATION, so one key covers every
// record one gesture writes — a save and the rename that follows it are two
// records, and one id for both would have the ledger collapse the second as a
// redelivery of the first. The verb is also the id's NAME, which is what a
// reader of the ledger finds it by, so it never holds a dot: in the grammar a
// dot begins a gesture's STEP ([statelog.StepOpID]), and `comment.edit` would
// read as the edit step of an operation called `comment`.
//
// AND WHAT THE WRITE SAYS IS PART OF IT: content is the write's own input —
// the new page, the save, the title, the reason — digested whole. The ledger
// answers an operation it already holds BEFORE the write is decided
// ([statelog.Result.Collapsed]), so derived from the key, the verb and the
// object alone, the same key sent with another body, another title or another
// reason was the first write's operation: answered from the ledger, with
// nothing of the second written — a change reported as made and silently
// dropped. The human write surface bound its key to the request for exactly
// that reason before handing it here; bound here, every caller that brings a
// key is held to it, and a retry — which resends the same input — is still the
// same operation.
//
// AND IT CARRIES THE KEY'S OWN INSTANT. The key is an operation id the engine
// minted — every surface that takes one holds it to
// [statelog.CheckCallerOpID] — and the ledger vouches for a retry by the
// instant its id carries ([statelog.OpMintedAt]), so a derivation that
// dropped it (a name-based uuid over the key) would be read as minted before
// every loss the ledger ever had, and once it had swept anything such a write
// was answered `unknown` without being published, on every attempt.
//
// An error is an input that will not encode, which is a type this package
// declared wrongly rather than anything a caller sent.
func (s *Store) operation(actor Actor, verb, object string, content any) (string, error) {
	key := strings.TrimSpace(actor.OpKey)
	if key == "" {
		return s.newSeqID(), nil
	}
	said, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("doclet: digest what a %s says: %w", verb, err)
	}
	sum := sha256.Sum256(said)
	at, _ := statelog.OpMintedAt(key)
	return statelog.DeriveOpID(at, verb, key, object, hex.EncodeToString(sum[:])), nil
}

// pageIDOf is the id a create gives its page: a FUNCTION OF THE OPERATION, so
// the retry an `unknown` asks for names the page its first attempt created.
//
// Minted per call instead, a keyed retry of a create that had landed was
// answered from the ledger ([statelog.Result.Collapsed]) with a page id this
// call had just made up — one no row, no link and no later read would ever
// find — while the page the operation did create went unreported. A fresh
// operation is a fresh id either way, since [statelog.NewOpID] carries random
// bits, so nothing changes for a caller that brings no key.
func pageIDOf(opID string) string {
	return uuid.NewSHA1(pageIDNamespace, []byte(opID)).String()
}

// pageIDNamespace scopes [pageIDOf]. FIXED for the life of the format: a new
// one would give a retry that straddles an upgrade a second page.
var pageIDNamespace = uuid.MustParse("b4c1e7a2-5f39-5d08-8e6b-1a2f9c3d7e45")

// landed is the answer to a write this call did not decide: a retry of an
// operation that had ALREADY LANDED under an earlier copy
// ([statelog.Result.Collapsed]).
//
// # Why it is read back rather than taken from the decision
//
// The framework answers such a retry from its ledger before any decision runs,
// or from the broker's duplicate acknowledgement after one ran and was never
// stored — so whatever a write computes inside its decision is either empty or
// describes a record nothing published. Every write here that answered with
// the page its decision read reported, on exactly the retry an `unknown` asks
// for, a page with no id, no title and version zero, as success. What the
// operation DID is on the rows, so that is what is answered: the page as this
// node holds it now, at the earlier copy's revision.
//
// # When the rows cannot say
//
// The earlier copy may not have applied here yet — the outcome then says
// `pending` and where to wait — or the page may be gone since (a purge, or the
// retry of one). Neither is a failure of this write, which did land, so the
// answer carries the page's identity and nothing a row would have to vouch for.
func (s *Store) landed(ctx context.Context, identity Page, opID string,
	result statelog.Result) (Written, error) {

	out := Written{
		Page: identity, Revision: writtenRevision(result, 0), ChangeID: opID,
		Outcome: result,
	}
	page, _, err := s.headAt(ctx, identity.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return out, nil
	case err != nil:
		return out, fmt.Errorf("doclet: page %s was written by an earlier copy "+
			"of operation %s; read what it wrote: %w", identity.ID, opID, err)
	}
	out.Page = page
	return out, nil
}

// landedComment is [Store.landed] for a remark: the comment as this node's
// rows hold it, or the identity the operation pins where they hold none.
func (s *Store) landedComment(ctx context.Context, identity Comment,
	opID string) (Comment, error) {

	var held Comment
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		held, err = readCommentTx(ctx, tx, identity.PageID, identity.ID)
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return identity, nil
	case err != nil:
		return identity, fmt.Errorf("doclet: comment %s was written by an earlier "+
			"copy of operation %s; read what it wrote: %w", identity.ID, opID, err)
	}
	return held, nil
}

// Name is how this actor is recorded and rendered: its handle, BARE.
//
// AN OPERATOR HAS NO SEAT HANDLE, and that is the whole shape of the operator
// surface: a write there carries the CREDENTIAL's own name — the login
// [iam.ActorFor] records it under on every other row — and author kind
// `operator`, and there is deliberately no way for a caller to name a seat to
// act as: a knowledge base whose author field is chosen by the writer is not
// an audit trail.
//
// NO PREFIX AND NO FALLBACK. This rendered `"operator:" + OperatorID` for an
// actor with no handle, which was a second spelling of one party: the kind is
// already a column on the row, and the audit feed — which reads this history
// beside the tracker's, where the same operator is the bare name — showed one
// person as two. A write with no handle is refused instead ([Actor.validate]).
func (a Actor) Name() string { return a.Handle }

// subscriber is the handle this actor's own subscription to a page is kept
// under — a create's author watching what they wrote, a save's `watch` — and
// EMPTY FOR AN OPERATOR.
//
// A seat or a person bound to one writes under a handle a wake can reach; an
// operator writes under the credential's own login, which is no seat, so a
// subscription kept under it is a watcher nothing can wake and a `watch: false`
// that published a record and woke every real watcher for a change to nobody's
// subscription. It was keyed on the handle being EMPTY, which held only while
// an operator carried none — every surface names one now ([Actor.Name]) — so
// the KIND is what says there is no seat.
func (a Actor) subscriber() string {
	if a.Kind == AuthorOperator {
		return ""
	}
	return a.Handle
}

// refuseReserved refuses an AGENT's write to a container the company holds
// back from it — see [Reserved] for which gesture each one refuses.
//
// creating is whether the write ADDS a page, which is the one gesture the org
// root refuses; every write path passes it, so the difference between the two
// reservations is stated here once rather than at each caller.
//
// THE CONTAINER KEY IS CANONICALISED on both sides, because an operator types
// `eng` in one file and `ENG` in another and a comparison that read them as
// two containers would let a seat write to the one it was refused.
func (s *Store) refuseReserved(actor Actor, container string, creating bool) error {
	if s.reserved == nil || actor.IsHuman() {
		return nil
	}
	key := ContainerKey(container)
	if key == "" {
		return nil
	}
	held := s.reserved()
	// THE SKILLS CONTAINER FIRST, because its rule is the stronger: a
	// company that named one key for both is refused every write there.
	if skills := ContainerKey(held.Skills); skills != "" && skills == key {
		return fmt.Errorf("%w: %s holds this company's tool skills — pages the "+
			"engine injects into seats' turns as instructions, excluded from "+
			"knowledge search and from routing — so a page a seat writes "+
			"there is either never read or read by every seat as an order. "+
			"Write it somewhere a reader will find it, or ask a person to "+
			"publish or change the skill", ErrReserved, key)
	}
	if root := ContainerKey(held.Root); creating && root != "" && root == key {
		return fmt.Errorf("%w: %s is the organisation's own container — what "+
			"is true of the whole company, starting with the Onboarding page "+
			"every seat reads first — and a new page there is a person's "+
			"decision. Write this in your team's space, or ask a person to "+
			"publish it at the top of the company; the pages already there "+
			"can be edited and commented on as usual", ErrReserved, key)
	}
	return nil
}

// refuseUndeclared refuses a page created in a container this company does
// not declare — see [Reserved.Declared].
//
// NEITHER OF [Store.refuseReserved]'s EARLY RETURNS: a store with no reserved
// seam declares nothing, and a person is held to the declared spaces exactly
// as an agent is.
func (s *Store) refuseUndeclared(container string) error {
	var held Reserved
	if s.reserved != nil {
		held = s.reserved()
	}
	key := ContainerKey(container)
	declared := make([]string, 0, len(held.Declared))
	for _, raw := range held.Declared {
		space := ContainerKey(raw)
		if space == key {
			return nil
		}
		if space != "" && !slices.Contains(declared, space) {
			declared = append(declared, space)
		}
	}
	slices.Sort(declared)
	where := "this company declares none yet"
	if len(declared) > 0 {
		where = "write in one of: " + strings.Join(declared, ", ")
	}
	return invalid("container", "%s is not a space this company declares — a "+
		"page lives in a declared space: a unit's or a seat's `space:`, "+
		"`knowledge.root_space`, or the tool-skills container "+
		"(`knowledge.skills_container`). Declare it in the company document, "+
		"or %s", key, where)
}

// SystemActor is how this domain records a write the engine makes on its own
// behalf, under the name given: a container a chart declares, a page the trash
// reaper purges.
//
// AN OPERATOR, because the knowledge base has no author kind for the engine
// ([AuthorKinds]) and an operator is the party that acts on the company's
// behalf while being neither a seat nor a person, which is the rule
// [generationActor] applies to a generation the engine wrote. ONE CONSTRUCTOR
// so every such write is recorded the same way: an operator holds no seat, so
// nothing it writes subscribes anybody ([Actor.subscriber]), and no
// reservation refuses it ([Store.refuseReserved]).
func SystemActor(name string) Actor { return Actor{Handle: name, Kind: AuthorOperator} }

// IsHuman reports an actor a notification treats as a person.
func (a Actor) IsHuman() bool { return a.Kind == AuthorHuman || a.Kind == AuthorOperator }

func (a Actor) validate() error {
	if !a.Kind.Valid() {
		return invalid("actor.kind", "%q is not one of %v", a.Kind, AuthorKinds())
	}
	// A KEY IS AN OPERATION ID THE ENGINE MINTED, or no key. Every id a
	// write derives from it carries the key's own instant ([Store.operation]),
	// and a key outside the grammar carries none — read as minted at the
	// epoch, which is before every loss the ledger will ever have, so once it
	// had swept anything each such write would be answered `unknown` without
	// being published, on every attempt, for ever. Refused here, at the one
	// boundary every write passes, rather than trusted to every surface.
	if key := strings.TrimSpace(a.OpKey); key != "" {
		if err := statelog.CheckCallerOpID(key); err != nil {
			return invalid("actor.op_key", "%v", err)
		}
	}
	// EVERY KIND MUST NAME ITS AUTHOR: a seat or a person the handle they
	// act as, an operator the credential's own login. A write with no
	// handle is a change nobody made, and there is no name to fall back on
	// that is not a second spelling of somebody — see [Actor.Name].
	if strings.TrimSpace(a.Handle) == "" {
		return invalid("actor.handle", "an %s write must name who made it — "+
			"the seat or the person it acts as, or for an operator the "+
			"credential's own login", a.Kind)
	}
	return nil
}

// excerpt is what a card shows: at most [MaxExcerpt] bytes INCLUDING its
// marker, cut on a rune boundary.
//
// A PREVIEW FOR A LIST, never what a seat acts on: the wake prompt is given a
// comment whole from the record ([wakeBody]). [textcut.Within] rather than
// Ellipsis, because the record and the history row both promise "at most
// MaxExcerpt bytes", and Ellipsis puts its marker outside the budget — so a
// long comment's excerpt was three bytes past the cap it was documented at.
func excerpt(text string) string {
	return textcut.Within(strings.Join(strings.Fields(text), " "), MaxExcerpt)
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
		return invalid("title", "a page needs a title, because it is its address")
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

// HeadRevision is the SQL expression a page's own log revision is read with,
// written ONCE because three readers compare against it and none may disagree:
// a decision's own read ([readHeadTx]), the head read a caller measures its
// write against ([Store.Page]), and [Reader.locate], which is what a read
// barrier reports.
//
// MAX(version, scoped_through) RATHER THAN version. A rename stamps
// `scoped_through` and deliberately never `version` — so the row's broker
// expectation still matches its subject's last message and cannot be poisoned
// into permanent unwritability — and a number taken from the version column
// alone therefore does not move when a page changes address. A caller
// comparing it to decide whether its own rename is visible here would wait for
// ever.
const HeadRevision = `MAX(version, scoped_through)`

// readHeadTx reads one page's head from inside a decision's own transaction,
// and the log revision the row was written through.
//
// THE REVISION TRAVELS WITH THE PAGE rather than being read again afterwards,
// because the two are one fact about one row: read separately they would come
// from two statements, and a decision that paired a page with a revision taken
// after a concurrent apply would report a number its own answer is not at.
func readHeadTx(ctx context.Context, tx *sql.Tx, id string) (Page, uint64, error) {
	var document []byte
	var revision int64
	err := tx.QueryRowContext(ctx,
		`SELECT document, `+HeadRevision+` FROM pages_heads WHERE id = ?`, id).
		Scan(&document, &revision)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Page{}, 0, fmt.Errorf("%w: page %s", ErrNotFound, id)
	case err != nil:
		return Page{}, 0, fmt.Errorf("doclet: read the head of %s: %w", id, err)
	}
	page, err := DecodePage(document)
	if err != nil {
		return Page{}, 0, err
	}
	return page, uint64(revision), nil
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
