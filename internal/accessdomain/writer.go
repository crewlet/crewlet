package accessdomain

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// THE ACCESS WRITE PATH: one record per told turn, signed certificate, command,
// giver's verdict, removed seat or expired hour, decided inside one snapshot of
// this node's rows.
// Every write is the engine's own record of what it did, so a [Call] names the
// engine and an operation id derived per execution, and a retry lands once.

// Call is one write's caller: who is writing and under which operation.
type Call struct {
	// By is the author as iam.ActorFor records a principal: the engine, as
	// `system`, for every record here.
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

// ToldOpID is the operation a turn's told record is published under: the seat
// and the turn, at the instant the turn's derived operations are minted at.
func ToldOpID(minted time.Time, handle, turn string) string {
	return statelog.DeriveOpID(minted, "access-told", "crewlet.access", handle, turn)
}

// IssuedOpID is the operation a certificate's record is published under.
func IssuedOpID(minted time.Time, handle string, serial uint64) string {
	return statelog.DeriveOpID(minted, "access-issued", "crewlet.access", handle, SerialString(serial))
}

// RanOpID is the operation a command's record is published under.
func RanOpID(minted time.Time, handle, commandID string) string {
	return statelog.DeriveOpID(minted, "access-ran", "crewlet.access", handle, commandID)
}

// VerdictOpID is the operation one giver's verdict on a request of seat handle
// is published under, by empty for the engine.
func VerdictOpID(minted time.Time, handle, request, by string) string {
	return statelog.DeriveOpID(minted, "access-verdict", "crewlet.access", handle, request, by)
}

// WriterDeps is what a writer is built from.
type WriterDeps struct {
	// Publisher is the domain's write authority, and DB the replicated estate
	// its decisions read.
	Publisher *statelog.Publisher
	DB        store.ReplicatedReader

	// Sealer seals a command's text before it is published.
	Sealer *Sealer

	// Now is the clock a record's authored instant is read off; nil is the
	// wall clock in UTC. No row stores it.
	Now func() time.Time
}

// Writer is the access write path.
type Writer struct {
	publisher *statelog.Publisher
	db        store.ReplicatedReader
	sealer    *Sealer
	now       func() time.Time
}

// NewWriter builds the write path.
func NewWriter(deps WriterDeps) (*Writer, error) {
	switch {
	case deps.Publisher == nil:
		return nil, errors.New("accessdomain: a write authority is required: every write is " +
			"a record on the access log")
	case deps.DB.IsZero():
		return nil, errors.New("accessdomain: the replicated estate is required: a write " +
			"decides from this node's own applied rows")
	case deps.Sealer == nil:
		return nil, errors.New("accessdomain: a sealer is required: a command's text is " +
			"never published in the clear")
	}
	w := &Writer{publisher: deps.Publisher, db: deps.DB, sealer: deps.Sealer, now: deps.Now}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	return w, nil
}

// decision encodes one record inside the snapshot's own transaction.
func decision(stamp statelog.Stamp, by iam.Actor, subject Subject, op OpKind,
	scope ScopeSet, opID string, payload any, at time.Time) (statelog.Decision, error) {

	if err := subject.writable(); err != nil {
		return statelog.Decision{}, err
	}
	if err := scope.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Decision{}, fmt.Errorf("accessdomain: encode the %s payload for %s: %w",
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
	return statelog.Decision{Payload: encoded}, nil
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
		return res, fmt.Errorf("%w: %s", ErrRecorded, req.Subject)
	}
	return res, err
}

// request is one write's request.
func request(subject Subject, scope ScopeSet, opID string, pattern statelog.Pattern,
	decide func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error)) statelog.Request {

	return statelog.Request{
		Subject: subject.framework(), Scope: scope.Resolve(subject), OpID: opID,
		Pattern: pattern, Decide: decide,
	}
}

// checkSeat refuses a handle no seat holds.
func checkSeat(handle string) error {
	if !iam.ValidSeatHandle(handle) {
		return invalid("%q is not a seat handle", handle)
	}
	return nil
}

// checkHolder refuses a handle that is neither a seat's nor the engine's own.
func checkHolder(handle string) error {
	if handle == EngineHolder {
		return nil
	}
	return checkSeat(handle)
}

// checkRef refuses an id a record carries beside its own outside its bounds or
// holding what a subject or a column must not.
func checkRef(field, value string, required bool) error {
	switch {
	case value == "" && required:
		return invalid("%s is required", field)
	case strings.ContainsAny(value, " \t\r\n*>/."):
		return invalid("%s %q carries whitespace, a wildcard, a slash or a dot", field, value)
	}
	return checkText(field, value, RefMaxBytes)
}

// The patterns below are compiled at first use, not at init: a bounded repeat
// compiles to a program holding every count, and every process that links this
// package (the git hook among them) would pay for it on start.

// digestPattern is a snapshot digest's one spelling.
var digestPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
})

// fingerprintPattern is a key fingerprint as ssh-keygen prints it.
var fingerprintPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
})

// hostPattern is a registered host's id, and userPattern a login user's name.
var (
	hostPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	})
	userPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	})
)

// shaPattern is a SHA-256 digest in lowercase hex.
var shaPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[0-9a-f]{64}$`)
})

// ---- told --------------------------------------------------------------- //

// NewTold is what one turn of a seat was told.
type NewTold struct {
	Handle string
	Turn   string

	// Digest, Snapshot and Changes are the capability package's: the
	// snapshot's digest and canonical JSON, and the changes since the seat's
	// baseline as a JSON array, empty on a baseline.
	Digest   string
	Snapshot json.RawMessage
	Changes  json.RawMessage
	Baseline bool

	RevisionFrom string
	RevisionTo   string
	Authors      []string

	// ReadThrough is where the dispatch read the seat's baseline
	// ([Recent.Position]), which a told record newer than it outranks.
	ReadThrough statelog.Position
}

// validate refuses a told record the log could not hold.
func (n NewTold) validate() error {
	if err := checkSeat(n.Handle); err != nil {
		return err
	}
	if _, err := uuid.Parse(n.Turn); err != nil || uuid.MustParse(n.Turn).String() != n.Turn {
		return invalid("the turn %q is not a run id in its canonical spelling", n.Turn)
	}
	switch {
	case !digestPattern().MatchString(n.Digest):
		return invalid("the snapshot digest %q is not sha256: and 64 lowercase hex digits", n.Digest)
	case len(n.Snapshot) == 0 || !json.Valid(n.Snapshot):
		return invalid("the snapshot is not JSON")
	case len(n.Snapshot) > SnapshotMaxBytes:
		return invalid("the snapshot is %d bytes and the maximum is %d", len(n.Snapshot), SnapshotMaxBytes)
	case n.Baseline && len(n.Changes) > 0:
		return invalid("a baseline tells no change")
	case !n.Baseline && (len(n.Changes) == 0 || !json.Valid(n.Changes)):
		return invalid("a told record that is not a baseline carries its changes as JSON")
	case len(n.Changes) > ChangesMaxBytes:
		return invalid("the changes are %d bytes and the maximum is %d", len(n.Changes), ChangesMaxBytes)
	case n.ReadThrough.On(Domain{}.Stream().Name) != nil:
		// [statelog.Position.On], the one rule a named position is held
		// to: a position that names nothing is judged on no stream, so a
		// told record reads the same here wherever it was asked from — the
		// estate router's wire leaves a position its type calls zero off,
		// and the data node it reaches meets no stream at all.
		return invalid("the baseline was read at a position on %s, not on the access log", n.ReadThrough.Stream)
	case len(n.Authors) > MaxAuthors:
		return invalid("a told record names %d authors and the maximum is %d", len(n.Authors), MaxAuthors)
	}
	for _, rev := range []struct{ field, value string }{
		{"revision_from", n.RevisionFrom}, {"revision_to", n.RevisionTo}} {
		if err := checkText(rev.field, rev.value, RevisionIDMaxBytes); err != nil {
			return err
		}
	}
	for _, a := range n.Authors {
		if err := checkText("an author", a, AuthorMaxBytes); err != nil {
			return err
		}
	}
	return nil
}

// Tell records what one turn of a seat was told, once: a second write for the
// same turn answers [ErrRecorded] and changes nothing.
func (w *Writer) Tell(ctx context.Context, c Call, n NewTold) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if err := n.validate(); err != nil {
		return statelog.Result{}, err
	}
	snapshot, err := compact("the snapshot", n.Snapshot)
	if err != nil {
		return statelog.Result{}, err
	}
	changes, err := compact("the changes", n.Changes)
	if err != nil {
		return statelog.Result{}, err
	}
	p := Told{
		V: baseRecordVersion, Handle: n.Handle, Turn: n.Turn, Digest: n.Digest,
		Snapshot: snapshot, Changes: changes, Baseline: n.Baseline,
		RevisionFrom: n.RevisionFrom, RevisionTo: n.RevisionTo, Authors: n.Authors,
		ReadThrough: n.ReadThrough.Packed(),
	}
	subject, scope := ToldSubject(n.Handle, n.Turn), ScopeSet{Subject: true}
	return publish(ctx, w.publisher, request(subject, scope, c.OpID, statelog.PatternCreate,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			removal, err := removalOf(ctx, tx, n.Handle)
			if err != nil {
				return statelog.Decision{}, err
			}
			decided := p
			decided.SeenRemoval = removal
			return decision(stamp, c.By, subject, OpTold, scope, c.OpID, decided, w.now())
		}))
}

// compact is JSON without insignificant whitespace, nil for none.
func compact(field string, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return nil, invalid("%s is not JSON: %v", field, err)
	}
	return out.Bytes(), nil
}

// ---- certificates ------------------------------------------------------- //

// NewCertificate is one SSH certificate the engine signed.
type NewCertificate struct {
	Serial   uint64
	KeyID    string
	Handle   string
	Turn     string
	HeldCall string
	Purpose  CertificatePurpose

	HostIDs    []string
	Principals []string

	ValidAfter  time.Time
	ValidBefore time.Time

	PublicKeySHA256 string
	CASHA256        string
	Node            string
}

// validate refuses a certificate record the log could not hold.
func (n NewCertificate) validate() error {
	if err := checkHolder(n.Handle); err != nil {
		return err
	}
	switch {
	case (n.Handle == EngineHolder) != (n.Purpose == PurposeInventory):
		return invalid("the engine holds the inventory's certificates, and no other")
	case n.Purpose == PurposeInventory && (n.Turn != "" || n.HeldCall != "" || len(n.HostIDs) != 1):
		return invalid("an inventory certificate reaches one host and serves no turn or held call")
	case n.Serial == 0:
		return invalid("a certificate's serial is a random non-zero number")
	case !n.Purpose.Valid():
		return invalid("purpose %q is not one of %s", n.Purpose, words.Or(CertificatePurposes))
	case strings.TrimSpace(n.KeyID) == "":
		return invalid("a certificate names its key id")
	case len(n.HostIDs) == 0 || len(n.HostIDs) > MaxHosts:
		return invalid("a certificate names 1 to %d hosts, and this one names %d", MaxHosts, len(n.HostIDs))
	case len(n.Principals) == 0 || len(n.Principals) > 2*MaxHosts:
		return invalid("a certificate names 1 to %d principals, and this one names %d", 2*MaxHosts,
			len(n.Principals))
	case n.ValidAfter.IsZero() || !n.ValidBefore.After(n.ValidAfter):
		return invalid("a certificate is valid from an instant until a later one")
	case !fingerprintPattern().MatchString(n.PublicKeySHA256):
		return invalid("the certified key's fingerprint %q is not SHA256: and 43 base64 characters",
			n.PublicKeySHA256)
	case !fingerprintPattern().MatchString(n.CASHA256):
		return invalid("the authority's fingerprint %q is not SHA256: and 43 base64 characters", n.CASHA256)
	}
	if err := checkText("the key id", n.KeyID, KeyIDMaxBytes); err != nil {
		return err
	}
	for _, ref := range []struct {
		field, value string
		required     bool
	}{{"the turn", n.Turn, false}, {"the held call", n.HeldCall, false}, {"the node", n.Node, true}} {
		if err := checkRef(ref.field, ref.value, ref.required); err != nil {
			return err
		}
	}
	for _, h := range n.HostIDs {
		if !hostPattern().MatchString(h) {
			return invalid("%q is not a host id", h)
		}
	}
	for _, p := range n.Principals {
		if err := checkText("a principal", p, HostIDMaxBytes+32); err != nil {
			return err
		}
	}
	return nil
}

// Issued records one certificate the engine signed, once.
func (w *Writer) Issued(ctx context.Context, c Call, n NewCertificate) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if err := n.validate(); err != nil {
		return statelog.Result{}, err
	}
	p := Certificate{
		V: baseRecordVersion, Serial: n.Serial, KeyID: n.KeyID, Handle: n.Handle, Turn: n.Turn,
		HeldCall: n.HeldCall, Purpose: n.Purpose, HostIDs: n.HostIDs, Principals: n.Principals,
		ValidAfter: n.ValidAfter.UTC(), ValidBefore: n.ValidBefore.UTC(),
		PublicKeySHA256: n.PublicKeySHA256, CASHA256: n.CASHA256, Node: n.Node,
	}
	subject, scope := CertificateSubject(n.Handle, n.Serial), ScopeSet{Subject: true}
	return publish(ctx, w.publisher, request(subject, scope, c.OpID, statelog.PatternCreate,
		func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return decision(stamp, c.By, subject, OpIssued, scope, c.OpID, p, w.now())
		}))
}

// ---- commands ----------------------------------------------------------- //

// NewCommand is one SSH command the engine ran, or a person denied. Command is
// the plain text, which the writer seals; Stdin and output are their sizes and
// SHA-256 digests in lowercase hex.
type NewCommand struct {
	ID       string
	Handle   string
	Turn     string
	HeldCall string

	Host    string
	User    string
	Level   CommandLevel
	Command string

	StdinBytes  int64
	StdinSHA256 string

	Ran          bool
	ExitCode     int
	Duration     time.Duration
	StdoutBytes  int64
	StderrBytes  int64
	OutputSHA256 string
	Truncated    bool

	Approver string
	Decision CommandDecision

	CertificateSerial uint64

	// Failure is why the command did not run or did not finish, one of
	// [CommandFailures]: never the command, its output or an error's text.
	Failure CommandFailure
}

// validate refuses a command record the log could not hold.
func (n NewCommand) validate() error {
	if err := checkHolder(n.Handle); err != nil {
		return err
	}
	if n.Handle == EngineHolder && (n.Level != LevelRead || n.Turn != "" || n.HeldCall != "") {
		return invalid("the engine's own command is a read that serves no turn or held call")
	}
	if id, err := uuid.Parse(n.ID); err != nil || id.String() != n.ID {
		return invalid("the command id %q is not a uuid in its canonical spelling", n.ID)
	}
	switch {
	case !hostPattern().MatchString(n.Host):
		return invalid("%q is not a host id", n.Host)
	case !userPattern().MatchString(n.User):
		return invalid("%q is not a login user", n.User)
	case !n.Level.Valid():
		return invalid("level %q is not one of %s", n.Level, words.Or(CommandLevels))
	case !n.Decision.Valid():
		return invalid("decision %q is not approved or denied", n.Decision)
	case n.Level == LevelRead && (n.Decision != DecisionNone || n.Approver != ""):
		return invalid("a read is not approved by anyone")
	case n.Level == LevelOperate && (n.Decision == DecisionNone || strings.TrimSpace(n.Approver) == ""):
		return invalid("an operate command names who answered it and what they answered")
	case n.Decision == DecisionDenied && n.Ran:
		return invalid("a denied command did not run")
	case !n.Failure.Valid():
		return invalid("failure %q is not one of %s: a command's failure is a closed word, never "+
			"its text or its output", n.Failure, words.Or(CommandFailures))
	case !n.Ran && n.Decision != DecisionDenied && n.Failure == FailureNone:
		return invalid("a command that did not run says why")
	case n.Decision == DecisionDenied && n.Failure != FailureNone:
		return invalid("a denied command did not fail; it was denied")
	case strings.TrimSpace(n.Command) == "":
		return invalid("a command names what it ran")
	case n.StdinBytes < 0 || n.StdoutBytes < 0 || n.StderrBytes < 0 || n.Duration < 0:
		return invalid("a size or a duration is never negative")
	case n.StdinBytes > 0 && !shaPattern().MatchString(n.StdinSHA256):
		return invalid("stdin's digest is SHA-256 in lowercase hex")
	case n.OutputSHA256 != "" && !shaPattern().MatchString(n.OutputSHA256):
		return invalid("the output's digest is SHA-256 in lowercase hex")
	}
	if err := checkText("the command", n.Command, CommandMaxBytes); err != nil {
		return err
	}
	if err := checkText("the approver", n.Approver, AuthorMaxBytes); err != nil {
		return err
	}
	for _, ref := range []struct {
		field, value string
	}{{"the turn", n.Turn}, {"the held call", n.HeldCall}} {
		if err := checkRef(ref.field, ref.value, false); err != nil {
			return err
		}
	}
	return nil
}

// Ran records one command, once, its text sealed to this record.
func (w *Writer) Ran(ctx context.Context, c Call, n NewCommand) (statelog.Result, error) {
	if err := n.validate(); err != nil {
		return statelog.Result{}, err
	}
	sealed, err := w.sealer.SealCommand(n.Handle, n.ID, n.Command)
	if err != nil {
		return statelog.Result{}, err
	}
	n.Command = ""
	return w.RanSealed(ctx, c, n, sealed)
}

// RanSealed records one command whose text another node sealed to this record:
// n carries no text, and sealed must open under this node's keyring to a text
// within the command's bounds, so a value sealed for another record is refused.
func (w *Writer) RanSealed(ctx context.Context, c Call, n NewCommand,
	sealed SealedCommand) (statelog.Result, error) {

	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if n.Command != "" {
		return statelog.Result{}, invalid("a sealed command carries no text beside its seal")
	}
	text, err := w.sealer.Open(sealed.Sealed, AAD(n.Handle, n.ID))
	if err != nil {
		return statelog.Result{}, fmt.Errorf("%w: the sealed command does not open as this "+
			"record's: %w", ErrInvalid, err)
	}
	if key, ok := secrets.EnvelopeKeyID(sealed.Sealed); !ok || key != sealed.Key {
		return statelog.Result{}, invalid("the sealed command names key %q and is sealed under another",
			sealed.Key)
	}
	n.Command = text
	if err := n.validate(); err != nil {
		return statelog.Result{}, err
	}
	p := Command{
		V: baseRecordVersion, ID: n.ID, Handle: n.Handle, Turn: n.Turn, HeldCall: n.HeldCall,
		Host: n.Host, User: n.User, Level: n.Level, CommandSealed: sealed.Sealed,
		ContentKey: sealed.Key, StdinBytes: n.StdinBytes, StdinSHA256: n.StdinSHA256, Ran: n.Ran,
		ExitCode: n.ExitCode, DurationMS: n.Duration.Milliseconds(), StdoutBytes: n.StdoutBytes,
		StderrBytes: n.StderrBytes, OutputSHA256: n.OutputSHA256, Truncated: n.Truncated,
		Approver: n.Approver, Decision: n.Decision, CertificateSerial: n.CertificateSerial,
		Failure: n.Failure,
	}
	subject, scope := CommandSubject(n.Handle, n.ID), ScopeSet{Subject: true}
	return publish(ctx, w.publisher, request(subject, scope, c.OpID, statelog.PatternCreate,
		func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return decision(stamp, c.By, subject, OpRan, scope, c.OpID, p, w.now())
		}))
}

// ---- verdicts ----------------------------------------------------------- //

// NewVerdict is one decision on a delegated request, by one giver. Subject,
// Reason and Findings are the plain text, which the writer seals; the rest is
// recorded as it is.
type NewVerdict struct {
	// Request is the request's id: a held call's, or a work item ask's.
	// The record's own id is [VerdictID] of Request and By.
	Request string
	Kind    delegated.Kind

	// Rule is the 1-based delegation rule that routed the request, zero for
	// a person's approval through a decision.
	Rule int

	// Handle is the seat whose request it is, Turn that seat's run.
	Handle string
	Turn   string

	// By is the delegate's or the approving person's seat, empty for the
	// engine; Through the path the verdict was given on.
	By      string
	ByKind  VerdictBy
	Through VerdictThrough

	// Asked and Confidence are what a delegate answered, set for an agent's
	// verdict alone; Verdict is what took effect for this record.
	Asked      delegated.Verdict
	Confidence delegated.Confidence
	Verdict    delegated.Verdict

	// Final is the record that decided the request, false for a panel
	// member's verdict that waited on the other.
	Final bool

	// Overruled is the engine's closed words for why it changed or made the
	// verdict, each `^[a-z_]{1,40}$`: never text.
	Overruled []string

	// Sampled is a verdict chosen for a person's spot-check after the fact.
	Sampled bool

	// Digest is the SHA-256 in lowercase hex of the exact request the
	// verdict is bound to.
	Digest string

	// Host, User and the stdin's size and digest are an ssh_operate's alone;
	// User may be empty.
	Host        string
	User        string
	StdinBytes  int64
	StdinSHA256 string

	// Subject is what was decided, in clear for a person: an ssh_operate's
	// command, a later kind's summary. Sealed to the record.
	Subject string

	// Decision is the ask a verdict through a decision answered, and Item
	// the work item the request is on, empty where none.
	Decision string
	Item     string

	// Reason and Findings are the giver's words, sealed with each other.
	Reason   string
	Findings []string
}

// overruledPattern is one of the engine's closed words for an overruling.
var overruledPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[a-z_]{1,40}$`)
})

// validate refuses a verdict record the log could not hold.
func (n NewVerdict) validate() error {
	if err := checkSeat(n.Handle); err != nil {
		return err
	}
	if err := checkRef("the request", n.Request, true); err != nil {
		return err
	}
	switch {
	case !n.Kind.Delegable():
		return invalid("kind %q is not one of %s", n.Kind, words.Or(delegated.DelegableKinds()))
	case !n.ByKind.Valid():
		return invalid("by_kind %q is not one of %s", n.ByKind, words.Or(VerdictGivers))
	case !n.Through.Valid():
		return invalid("through %q is not one of %s", n.Through, words.Or(VerdictPaths))
	case !n.Verdict.Valid():
		return invalid("verdict %q is not one of %s", n.Verdict, words.Or(delegated.Verdicts()))
	case n.Rule < 0 || n.Rule > MaxVerdictRule:
		return invalid("rule %d is not a delegation rule's number, 1 to %d, or zero", n.Rule, MaxVerdictRule)
	case n.ByKind == VerdictByEngine && n.By != "":
		return invalid("the engine's verdict names no seat, and this one names %q", n.By)
	case n.ByKind != VerdictByEngine && !iam.ValidSeatHandle(n.By):
		return invalid("the %s verdict's giver %q is not a seat handle", n.ByKind, n.By)
	case n.ByKind == VerdictByAgent && n.Rule == 0:
		return invalid("an agent's verdict names the rule that routed the request to it")
	case n.ByKind == VerdictByPerson && (n.Through != VerdictThroughDecision || n.Rule != 0):
		return invalid("a person's verdict is an approval through a decision, which no rule routed")
	case n.ByKind == VerdictByAgent && (!n.Asked.Valid() || !n.Confidence.Valid()):
		return invalid("an agent's verdict records what it answered (%s) and how sure it was (%s)",
			words.Or(delegated.Verdicts()), words.Or(delegated.Confidences()))
	case n.ByKind != VerdictByAgent && (n.Asked != "" || n.Confidence != ""):
		return invalid("only an agent's verdict records an answer and a confidence")
	case n.ByKind == VerdictByEngine && len(n.Overruled) == 0:
		return invalid("the engine's verdict says why it made it, in its closed words")
	case len(n.Overruled) > MaxOverruled:
		return invalid("a verdict names %d overrulings and the maximum is %d", len(n.Overruled), MaxOverruled)
	case !shaPattern().MatchString(n.Digest):
		return invalid("the request's digest is SHA-256 in lowercase hex")
	case n.Through == VerdictThroughDecision && n.Decision == "":
		return invalid("a verdict through a decision names the ask it answered")
	case n.Through != VerdictThroughDecision && n.Decision != "":
		return invalid("only a verdict through a decision names an ask")
	case strings.TrimSpace(n.Subject) == "":
		return invalid("a verdict names what it decided")
	case len(n.Findings) > MaxVerdictFindings:
		return invalid("a verdict carries %d findings and the maximum is %d", len(n.Findings), MaxVerdictFindings)
	}
	for _, word := range n.Overruled {
		if !overruledPattern().MatchString(word) {
			return invalid("overruled %q is not a closed word of lowercase letters and underscores, "+
				"at most %d: never text", word, OverruledMaxBytes)
		}
	}
	if err := n.checkSSH(); err != nil {
		return err
	}
	if err := checkText("the subject", n.Subject, VerdictSubjectMaxBytes); err != nil {
		return err
	}
	if err := checkRunes("the reason", n.Reason, VerdictReasonMaxRunes); err != nil {
		return err
	}
	for _, finding := range n.Findings {
		if strings.TrimSpace(finding) == "" {
			return invalid("a finding says something")
		}
		if err := checkRunes("a finding", finding, VerdictFindingMaxRunes); err != nil {
			return err
		}
	}
	for _, ref := range []struct {
		field, value string
	}{{"the turn", n.Turn}, {"the decision", n.Decision}, {"the item", n.Item}} {
		if err := checkRef(ref.field, ref.value, false); err != nil {
			return err
		}
	}
	return nil
}

// checkSSH holds the fields only an ssh_operate verdict carries to that kind.
func (n NewVerdict) checkSSH() error {
	if n.Kind != delegated.KindSSHOperate {
		if n.Host != "" || n.User != "" || n.StdinBytes != 0 || n.StdinSHA256 != "" {
			return invalid("only an %s verdict names a host, a user or stdin", delegated.KindSSHOperate)
		}
		return nil
	}
	switch {
	case !hostPattern().MatchString(n.Host):
		return invalid("%q is not a host id", n.Host)
	case n.User != "" && !userPattern().MatchString(n.User):
		return invalid("%q is not a login user", n.User)
	case n.StdinBytes < 0:
		return invalid("a size is never negative")
	case (n.StdinBytes > 0 || n.StdinSHA256 != "") && !shaPattern().MatchString(n.StdinSHA256):
		return invalid("stdin's digest is SHA-256 in lowercase hex")
	}
	return nil
}

// checkRunes refuses a text field that is not UTF-8 or is longer than maxRunes.
func checkRunes(field, value string, maxRunes int) error {
	switch {
	case !utf8.ValidString(value):
		return invalid("%s is not valid UTF-8", field)
	case utf8.RuneCountInString(value) > maxRunes:
		return invalid("%s is %d characters and the maximum is %d", field,
			utf8.RuneCountInString(value), maxRunes)
	}
	return nil
}

// Verdict records one giver's verdict on a delegated request, once, what it
// decided and the giver's words sealed to the record.
//
// ONE RECORD PER REQUEST AND GIVER, the first written: a second verdict by
// the same giver on the same request, under another operation, answers
// [ErrRecorded] and changes nothing, while a panel's other delegate records
// its own. A retry of the same operation is answered as the first copy's.
func (w *Writer) Verdict(ctx context.Context, c Call, n NewVerdict) (statelog.Result, error) {
	if err := n.validate(); err != nil {
		return statelog.Result{}, err
	}
	sealed, err := w.sealer.SealVerdict(n.Handle, VerdictID(n.Request, n.By), n.Subject, n.Reason, n.Findings)
	if err != nil {
		return statelog.Result{}, err
	}
	n.Subject, n.Reason, n.Findings = "", "", nil
	return w.VerdictSealed(ctx, c, n, sealed)
}

// VerdictSealed records one verdict whose texts another node sealed to this
// record: n carries none of them, and sealed must open under this node's
// keyring to texts within the verdict's bounds, so a value sealed for another
// record is refused. [Writer.Verdict] says what a second verdict answers.
func (w *Writer) VerdictSealed(ctx context.Context, c Call, n NewVerdict,
	sealed SealedVerdict) (statelog.Result, error) {

	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if n.Subject != "" || n.Reason != "" || len(n.Findings) > 0 {
		return statelog.Result{}, invalid("a sealed verdict carries no text beside its seal")
	}
	if err := checkSeat(n.Handle); err != nil {
		return statelog.Result{}, err
	}
	id := VerdictID(n.Request, n.By)
	text, reason, findings, err := w.sealer.openVerdict(n.Handle, id, sealed.Subject, sealed.Words)
	if err != nil {
		return statelog.Result{}, fmt.Errorf("%w: the sealed verdict does not open as this "+
			"record's: %w", ErrInvalid, err)
	}
	for _, value := range []string{sealed.Subject, sealed.Words} {
		if key, ok := secrets.EnvelopeKeyID(value); !ok || key != sealed.Key {
			return statelog.Result{}, invalid("the sealed verdict names key %q and is sealed under another",
				sealed.Key)
		}
	}
	n.Subject, n.Reason, n.Findings = text, reason, findings
	if err := n.validate(); err != nil {
		return statelog.Result{}, err
	}
	p := Verdict{
		V: baseRecordVersion, ID: id, Handle: n.Handle, Request: n.Request, Turn: n.Turn,
		Kind: n.Kind, Rule: n.Rule, By: n.By, ByKind: n.ByKind, Through: n.Through,
		Asked: n.Asked, Confidence: n.Confidence, Verdict: n.Verdict, Overruled: n.Overruled,
		Final: n.Final, Sampled: n.Sampled, Digest: n.Digest, Host: n.Host, User: n.User,
		StdinBytes: n.StdinBytes, StdinSHA256: n.StdinSHA256, Decision: n.Decision, Item: n.Item,
		SubjectSealed: sealed.Subject, WordsSealed: sealed.Words, ContentKey: sealed.Key,
	}
	subject, scope := VerdictSubject(n.Handle, n.Request, n.By), ScopeSet{Subject: true}
	return publish(ctx, w.publisher, request(subject, scope, c.OpID, statelog.PatternCreate,
		func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return decision(stamp, c.By, subject, OpVerdict, scope, c.OpID, p, w.now())
		}))
}

// ---- a removed seat ----------------------------------------------------- //

// Forget removes what a seat the company removed was told. Its certificates,
// commands and verdicts stay: they are the audit trail, kept to their retention.
func (w *Writer) Forget(ctx context.Context, c Call, handle string) (statelog.Result, error) {
	if err := c.validate(); err != nil {
		return statelog.Result{}, err
	}
	if err := checkSeat(handle); err != nil {
		return statelog.Result{}, err
	}
	subject, scope := SeatSubject(handle), seatScope(handle)
	return publish(ctx, w.publisher, request(subject, scope, c.OpID, statelog.PatternArbitrated,
		func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return decision(stamp, c.By, subject, OpForget, scope, c.OpID,
				Forget{V: baseRecordVersion, Handle: handle}, w.now())
		}))
}

// ---- retention ---------------------------------------------------------- //

// Expire removes every record of the hour starting at hour, and everything
// older, that is past the retention cutoff; each seat's baseline stays. Decided
// again in the write's own snapshot: an hour with nothing left answers
// [ErrNotDue].
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
	return publish(ctx, w.publisher, request(subject, scope, c.OpID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			due, err := expirableBefore(ctx, tx, store.EncodeTime(through))
			if err != nil {
				return statelog.Decision{}, err
			}
			if !due {
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrNotDue, Bucket(hour))
			}
			return decision(stamp, c.By, subject, OpExpire, scope, c.OpID,
				Expire{V: baseRecordVersion, Through: through}, w.now())
		}))
}

// expirableStatements find whether any row an expiry would take is older than
// an instant, one indexed probe per table.
var expirableStatements = []string{
	`SELECT 1 FROM access_told INDEXED BY access_told_expiry_idx WHERE latest = 0 AND at < ? LIMIT 1`,
	`SELECT 1 FROM access_seat_removals WHERE at < ? LIMIT 1`,
	`SELECT 1 FROM access_certificates WHERE at < ? LIMIT 1`,
	`SELECT 1 FROM access_commands WHERE at < ? LIMIT 1`,
	`SELECT 1 FROM access_verdicts WHERE at < ? LIMIT 1`,
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
			return false, fmt.Errorf("accessdomain: probe the retention: %w", err)
		}
		return true, nil
	}
	return false, nil
}
