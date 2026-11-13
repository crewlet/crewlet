package atlasdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// THE ATLAS WRITE PATH: one record per claim a person or an agent makes, per
// integration's read of an entry, per purge and per expired hour, decided
// inside one snapshot of this node's rows. EVERY VALUE IS
// JUDGED HERE, by the vocabulary's own grammar, before it is published: the
// applier judges none, so a rule a newer build learns cannot make two nodes
// disagree about a row.

// Call is one write's caller: who is writing and under which operation.
type Call struct {
	// By is the author as iam.ActorFor records a principal: a person's claim
	// names the person, by the name their record is kept under.
	By iam.Actor

	// OpID is the operation the record is published under.
	OpID string
}

// validate refuses a call the records could not name.
func (c Call) validate() error {
	switch {
	case strings.TrimSpace(c.By.Name) == "":
		return invalid("the call names no actor: every record names who wrote it")
	case !c.By.Kind.Valid():
		return invalid("actor kind %q is not one of %s", c.By.Kind, words.Or(iam.ActorKinds))
	}
	if err := statelog.CheckCallerOpID(c.OpID); err != nil {
		return fmt.Errorf("%w: the operation id %w", ErrInvalid, err)
	}
	return nil
}

// person refuses a call that is not a person's. An agent's claim is a record of
// its own, backed by the evidence of its turn; what the engine itself knows is
// computed when the atlas is read and never recorded.
func (c Call) person() error {
	switch c.By.Kind {
	case iam.ActorHuman, iam.ActorOperator:
		return nil
	case iam.ActorAgent:
		return invalid("an agent's claim is not a person's: it is recorded with the evidence of its turn")
	}
	return invalid("the engine records nothing as a person: what it knows is computed when the atlas is read")
}

// NoIfMatch is a person's write conditioned on no version of its entry: an
// edit whose caller read nothing first.
const NoIfMatch uint64 = 0

// IfNoRecord is a person's write conditioned on its entry holding no record
// yet: decided from a read that found none, so a write somebody made since
// refuses it as [ErrStaleVersion] rather than landing beside it unseen. No
// version is this value: a version is a log position, which stays far below.
const IfNoRecord uint64 = 1<<64 - 1

// claimNamespace is the name space a claim's id is derived in. Fixed for good:
// a different namespace is a different id for every claim a retry recorded.
var claimNamespace = uuid.MustParse("5d0e3b8a-7c41-4f2e-9a63-1b8f2c7d4e90")

// ClaimID is the id of the claim operation opID records: one claim an
// operation, so every round and every retry of it names the same claim.
func ClaimID(opID string) string { return uuid.NewSHA1(claimNamespace, []byte(opID)).String() }

// WriterDeps is what a writer is built from.
type WriterDeps struct {
	// Publisher is the domain's write authority, and DB the replicated estate
	// its decisions read.
	Publisher *statelog.Publisher
	DB        store.ReplicatedReader

	// Now is the clock a record's authored instant is read off; nil is the
	// wall clock in UTC. No row stores it.
	Now func() time.Time
}

// Writer is the atlas write path.
type Writer struct {
	publisher *statelog.Publisher
	db        store.ReplicatedReader
	now       func() time.Time
}

// NewWriter builds the write path.
func NewWriter(deps WriterDeps) (*Writer, error) {
	switch {
	case deps.Publisher == nil:
		return nil, errors.New("atlasdomain: a write authority is required: every write is " +
			"a record on the atlas log")
	case deps.DB.IsZero():
		return nil, errors.New("atlasdomain: the replicated estate is required: a write " +
			"decides from this node's own applied rows")
	}
	w := &Writer{publisher: deps.Publisher, db: deps.DB, now: deps.Now}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	return w, nil
}

// decision encodes one record inside the snapshot's own transaction.
func decision(stamp statelog.Stamp, by iam.Actor, subject Subject, op OpKind,
	scope ScopeSet, opID string, payload any, at time.Time, version int64) (statelog.Decision, error) {

	if err := subject.writable(); err != nil {
		return statelog.Decision{}, err
	}
	if err := scope.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Decision{}, fmt.Errorf("atlasdomain: encode the %s payload for %s: %w",
			op, subject, err)
	}
	encoded, err := Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			OpID: opID, Subject: subject, Op: op, CreatedAt: at,
			Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
		},
		Mutation: body, Actor: by.Name, ActorKind: by.Kind, OperatorID: by.OperatorID,
	})
	if err != nil {
		return statelog.Decision{}, err
	}
	return statelog.Decision{Payload: encoded, Version: version}, nil
}

// publish sends one request, naming a lost compare-and-set and a create that
// found its record already there in this domain's words.
func publish(ctx context.Context, publisher *statelog.Publisher,
	req statelog.Request) (statelog.Result, error) {

	res, err := publisher.Publish(ctx, req)
	switch {
	case errors.Is(err, statelog.ErrConflict):
		return res, fmt.Errorf("%w: %s: %w", ErrConflict, req.Subject, err)
	case errors.Is(err, statelog.ErrExists):
		return res, fmt.Errorf("%w: %s: %w", ErrExists, req.Subject, err)
	}
	return res, err
}

// request is one write's request.
func request(subject Subject, scope ScopeSet, opID string,
	decide func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error)) statelog.Request {

	return statelog.Request{
		Subject: subject.framework(), Scope: scope.Resolve(subject), OpID: opID,
		Pattern: statelog.PatternArbitrated, Decide: decide,
	}
}

// versionOf is an entry's version: the position of the newest record on it,
// zero for an entry nobody recorded anything about.
func versionOf(ctx context.Context, tx *sql.Tx, entry string) (int64, error) {
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT version FROM atlas_entries WHERE entry_id = ?`,
		entry).Scan(&version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("atlasdomain: read entry %s's version: %w", entry, err)
	}
	return version, nil
}

// ---- a person's claim --------------------------------------------------- //

// Record writes one person's claim on its entry: an entry they add, retire or
// restore, a fact they set, correct or deny, a relation they draw or remove, an
// owner they name, or two entries they join or keep apart. The claim's source
// is the caller, whatever the claim says; its value is judged by the
// vocabulary's grammar, and its value and every key it names are refused if
// shaped like a credential.
//
// ifMatch is the entry's version the person decided from ([EntryRow.Version]),
// [IfNoRecord] for a decision made where the entry held none, or [NoIfMatch]:
// an edit over a record they did not see answers [ErrStaleVersion]. A claim on, naming or carrying anything purged answers
// [ErrPurged].
func (w *Writer) Record(ctx context.Context, c Call, claim atlas.Claim, ifMatch uint64) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if err := c.person(); err != nil {
		return statelog.Result{}, err
	}
	return w.claim(ctx, c, claim, atlas.PersonSource(c.By.Name), ifMatch)
}

// Observation is where an agent's claim comes from, as the node that ran its
// turn established it: the run, and the class of evidence its cited call is.
type Observation struct {
	// Turn is the run the claim was recorded in.
	Turn string

	// Evidence is what the cited call of that turn is evidence of, derived
	// from the call's tool by the engine and never passed by the agent.
	Evidence atlas.Evidence
}

// Observe writes one agent's claim on its entry (ADR-0132): an entry, a fact
// or a relation a call of its own turn showed it, or its dispute of one the
// atlas holds. The claim's source is the calling seat, in the turn and with
// the evidence seen names, whatever the claim says. The vocabulary refuses
// what an agent never records (a fact of meaning, an owner, "the same" and
// "not the same"), and its value and every key it names are refused if shaped
// like a credential. An agent's claim is conditioned on no version: it
// replaces only the seat's own earlier claim on the slot, and outranks nobody.
//
// WHETHER THE CITED CALL SHOWED THE VALUE is the calling node's to check,
// since only the node that ran the turn holds what its calls answered; this
// writer holds the claim to everything a record can be held to.
func (w *Writer) Observe(ctx context.Context, c Call, claim atlas.Claim, seen Observation) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if c.By.Kind != iam.ActorAgent {
		return statelog.Result{}, invalid("an agent's claim is recorded by the agent's own seat, not by a %s",
			c.By.Kind)
	}
	if strings.TrimSpace(seen.Turn) == "" || len(seen.Turn) > MaxTurnBytes {
		return statelog.Result{}, invalid("an agent's claim names the turn it was recorded in, in at most %d bytes",
			MaxTurnBytes)
	}
	return w.claim(ctx, c, claim, atlas.AgentSource(c.By.Name, seen.Turn, seen.Evidence), NoIfMatch)
}

// claim publishes one claim as src, judged and conditioned as [Writer.Record]
// says.
func (w *Writer) claim(ctx context.Context, c Call, claim atlas.Claim, src atlas.Source,
	ifMatch uint64) (statelog.Result, error) {

	claim.Source, claim.Position, claim.ObservedAt = src, 0, w.now()
	// THE KEYS FIRST: a refusal of the claim's grammar names its entry, and a
	// key shaped like a credential is refused before anything repeats it.
	if err := claim.Recordable(); err != nil {
		return statelog.Result{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := claim.Validate(); err != nil {
		return statelog.Result{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	p := claimOf(ClaimID(c.OpID), claim)
	subject, scope := EntrySubject(claim.Subject), ScopeSet{Subject: true}
	markers := claimMarkers(subject.ID, p)
	return publish(ctx, w.publisher, request(subject, scope, c.OpID,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, held, err := heldBy(ctx, tx, markers, c.OpID); err != nil || held {
				return statelog.Decision{}, purgedOr(err, "%s, or something it names, was purged", claim.Subject)
			}
			version, err := versionOf(ctx, tx, subject.ID)
			if err != nil {
				return statelog.Decision{}, err
			}
			switch {
			case ifMatch == IfNoRecord && version != 0:
				return statelog.Decision{}, fmt.Errorf("%w: %s is at version %d and the edit was "+
					"decided where it held no record; read it again and decide", ErrStaleVersion,
					claim.Subject, version)
			case ifMatch != NoIfMatch && ifMatch != IfNoRecord && uint64(version) != ifMatch:
				return statelog.Decision{}, fmt.Errorf("%w: %s is at version %d and the edit was "+
					"decided from %d; read it again and decide", ErrStaleVersion, claim.Subject,
					version, ifMatch)
			}
			return decision(stamp, c.By, subject, OpClaim, scope, c.OpID, p, w.now(), version)
		}))
}

// purgedOr is err where the gate could not be read, and otherwise [ErrPurged]
// saying what.
func purgedOr(err error, format string, args ...any) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", ErrPurged, fmt.Sprintf(format, args...))
}

// ---- purges ------------------------------------------------------------- //

// MaxPurgedValueBytes bounds the text a value purge names: the longest key or
// value the vocabulary admits.
const MaxPurgedValueBytes = atlas.MaxKeyBytes

// PurgeEntry erases an entry everywhere, every claim on it and every claim
// naming it, its history included, and marks it so nothing records it again.
// Whether the caller may is the caller's to decide: a purge is an
// administrator's, after a fresh sign-in, and never an agent's.
func (w *Writer) PurgeEntry(ctx context.Context, c Call, ref atlas.Ref) (statelog.Result, error) {
	canonical, err := atlas.NewRef(ref.Kind, ref.Key)
	if err != nil {
		return statelog.Result{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if canonical != ref {
		return statelog.Result{}, invalid("the entry to purge is named by its canonical key")
	}
	return w.purge(ctx, c, EntrySubject(ref))
}

// PurgeValue erases a value everywhere: every claim carrying it, every entry it
// is the key of with everything recorded about it, and every claim naming such
// an entry, history included, and marks it so nothing records it again. text
// is the value as the atlas holds it, its canonical text; the record names its
// digest and never the text.
func (w *Writer) PurgeValue(ctx context.Context, c Call, text string) (statelog.Result, error) {
	switch {
	case text == "" || strings.TrimSpace(text) != text:
		return statelog.Result{}, invalid("a purged value is the value as the atlas holds it, with no space around it")
	case !utf8.ValidString(text):
		return statelog.Result{}, invalid("a purged value is valid UTF-8")
	case len(text) > MaxPurgedValueBytes:
		return statelog.Result{}, invalid("a purged value is at most %d bytes", MaxPurgedValueBytes)
	}
	return w.PurgeDigest(ctx, c, Digest(text))
}

// PurgeDigest is [Writer.PurgeValue] for a value named by its [Digest], which is
// how a purge crosses between nodes: the value it erases never travels.
func (w *Writer) PurgeDigest(ctx context.Context, c Call, digest string) (statelog.Result, error) {
	if !IsDigest(digest) {
		return statelog.Result{}, invalid("a purged value is named by the SHA-256 of its text, 64 lowercase hex digits")
	}
	return w.purge(ctx, c, Subject{Kind: KindValue, ID: digest})
}

// purge publishes one purge on its subject, decided in the snapshot: a subject
// already purged answers [ErrPurged].
func (w *Writer) purge(ctx context.Context, c Call, subject Subject) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if c.By.Kind == iam.ActorAgent {
		return statelog.Result{}, invalid("an agent never removes anything from the atlas")
	}
	scope := domainScope()
	own := markersOf(subject.framework(), nil)
	return publish(ctx, w.publisher, request(subject, scope, c.OpID,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, held, err := heldBy(ctx, tx, own, c.OpID); err != nil || held {
				return statelog.Decision{}, purgedOr(err, "this %s was purged already", subject.Kind)
			}
			return decision(stamp, c.By, subject, OpPurge, scope, c.OpID,
				Purge{V: GateRecordVersion}, w.now(), 0)
		}))
}

// ---- retention ---------------------------------------------------------- //

// Expire removes the history of the hour starting at hour, and everything
// older, that is past the retention cutoff: the claims a newer claim of the
// same voice replaced, and the entries a person retired that nothing was
// recorded about since. Decided again in the write's own snapshot: an hour
// with nothing left answers [ErrNotDue].
func (w *Writer) Expire(ctx context.Context, c Call, hour, cutoff time.Time) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	hour = hour.UTC()
	if !hour.Equal(hour.Truncate(ExpiryBucket)) {
		return statelog.Result{}, invalid("an expiry names the start of an hour, and %s is not one", hour)
	}
	through := hour.Add(ExpiryBucket)
	if cutoff.Before(through) {
		through = cutoff.UTC()
	}
	if !through.After(hour) {
		return statelog.Result{}, fmt.Errorf("%w: the hour %s is not past the cutoff %s", ErrNotDue,
			Bucket(hour), cutoff.UTC())
	}
	subject, scope := ExpirySubject(hour), domainScope()
	return publish(ctx, w.publisher, request(subject, scope, c.OpID,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			due, err := expirableBefore(ctx, tx, store.EncodeTime(through))
			if err != nil {
				return statelog.Decision{}, err
			}
			if !due {
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrNotDue, Bucket(hour))
			}
			return decision(stamp, c.By, subject, OpExpire, scope, c.OpID,
				Expire{V: baseRecordVersion, Through: through}, w.now(), 0)
		}))
}

// expirableStatements find whether any row an expiry would take is older than
// an instant, one indexed probe each.
var expirableStatements = []string{
	`SELECT 1 FROM atlas_claims INDEXED BY atlas_claims_expiry_idx
		WHERE replaced = 1 AND superseded_at < ? LIMIT 1`,
	`SELECT 1 FROM atlas_entries INDEXED BY atlas_entries_retired_idx
		WHERE retired = 1 AND last_at < ? LIMIT 1`,
}

// expirableBefore reports whether an expiry through `before` would take a row.
func expirableBefore(ctx context.Context, tx *sql.Tx, before int64) (bool, error) {
	for _, statement := range expirableStatements {
		var one int
		err := tx.QueryRowContext(ctx, statement, before).Scan(&one)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return false, fmt.Errorf("atlasdomain: probe the retention: %w", err)
		}
		return true, nil
	}
	return false, nil
}

// ---- an integration's read ---------------------------------------------- //

// ReadInput is one integration's read of one entry, as the collect duty hands
// it to the log (ADR-0129): the voice it speaks in, the entry, the claims the
// read holds on it that are new or changed, the slots of the voice's earlier
// claims it no longer holds, and when the read saw it.
type ReadInput struct {
	Source     atlas.Source
	Entry      atlas.Ref
	Claims     []atlas.Claim
	Withdrawn  []string
	ObservedAt time.Time
}

// Read writes one integration's read of one entry. The caller is the engine's
// own duty, never a person or an agent; the source is an integration's; every
// claim is on the entry, in the read's voice, judged by the vocabulary's
// grammar with credentials refused in its value and in every key it names;
// and a slot is withdrawn or claimed, never both. A read on, naming or
// carrying anything purged answers [ErrPurged], and is recorded nowhere.
func (w *Writer) Read(ctx context.Context, c Call, in ReadInput) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if c.By.Kind != iam.ActorSystem {
		return statelog.Result{}, invalid("an integration's read is the engine's own record: a %s records a claim",
			c.By.Kind)
	}
	if in.Source.Kind != atlas.SourceIntegration {
		return statelog.Result{}, invalid("a read speaks as an integration, not as a %s", in.Source.Kind)
	}
	if _, err := atlas.Exists(in.Entry, in.Source, w.now()); err != nil {
		return statelog.Result{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(in.Claims) == 0 && len(in.Withdrawn) == 0 {
		return statelog.Result{}, invalid("a read of %s holds nothing and withdraws nothing", in.Entry)
	}
	at := in.ObservedAt.UTC()
	if at.IsZero() {
		at = w.now()
	}
	subject, scope := EntrySubject(in.Entry), ScopeSet{Subject: true}
	payload := Read{V: baseRecordVersion, Source: Source{Kind: in.Source.Kind, Name: in.Source.Name},
		ObservedAt: at}
	markers := []marker{{markerEntry, subject.ID}}
	slots := map[string]bool{}
	for _, claim := range in.Claims {
		claim.Source, claim.Position, claim.ObservedAt = in.Source, 0, at
		if claim.Subject != in.Entry {
			return statelog.Result{}, invalid("a read of %s carries a claim on %s", in.Entry, claim.Subject)
		}
		if err := claim.Recordable(); err != nil {
			return statelog.Result{}, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		if err := claim.Validate(); err != nil {
			return statelog.Result{}, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		slot := claim.Slot()
		if slots[slot] {
			return statelog.Result{}, invalid("a read of %s claims one slot twice", in.Entry)
		}
		slots[slot] = true
		p := claimOf(ReadClaimID(c.OpID, slot), claim)
		p.ObservedAt = at
		payload.Claims = append(payload.Claims, p)
		for _, m := range claimMarkers(subject.ID, p) {
			if !slices.Contains(markers, m) {
				markers = append(markers, m)
			}
		}
	}
	for _, slot := range in.Withdrawn {
		switch {
		case slot == "":
			return statelog.Result{}, invalid("a read of %s withdraws a slot it does not name", in.Entry)
		case slots[slot]:
			return statelog.Result{}, invalid("a read of %s claims a slot it also withdraws", in.Entry)
		}
		slots[slot] = true
		payload.Withdrawn = append(payload.Withdrawn, slot)
	}
	return publish(ctx, w.publisher, request(subject, scope, c.OpID,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, held, err := heldBy(ctx, tx, markers, c.OpID); err != nil || held {
				return statelog.Decision{}, purgedOr(err, "%s, or something the read names, was purged", in.Entry)
			}
			version, err := versionOf(ctx, tx, subject.ID)
			if err != nil {
				return statelog.Decision{}, err
			}
			return decision(stamp, c.By, subject, OpRead, scope, c.OpID, payload, w.now(), version)
		}))
}
