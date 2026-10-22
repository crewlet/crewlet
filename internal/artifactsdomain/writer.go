package artifactsdomain

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

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ARTIFACTS WRITE PATH: one record per gesture step, published to the
// subject of the thing it contends for, decided inside one snapshot of this
// node's own rows.
//
// # What the writer decides, and what it leaves to the applier
//
// The writer holds every rule a later build may change (the path grammar, the
// media table, every bound in internal/artifacts/limits.go) and every rule that
// needs more than this log's rows: the two domain rules a share is held to
// (never above the sharer's role, never company-wide from an agent), the
// freshness of an uploaded object's key, the retention list, the labels kept.
// It refuses early from its snapshot on everything the applier will check again
// (a moved head, a carry, a quota), because a refusal before the append costs
// the log nothing.
//
// The applier then decides, from the record and its own rows alone, whatever
// can still go the other way between the decide and the apply, and the writer
// READS ITS OWN VERDICT BACK from the history row its operation wrote: a record
// the broker accepted is not a record that applied.
//
// # The authority is stamped, never re-decided here
//
// Who may publish, share or purge is the authority table's, decided at the API
// and carried in [Call.Authority]. This package does not evaluate the role
// ladder against an action; a second evaluation would be two implementations
// of one rule (ADR-0008).

// Call is one write gesture's caller context.
type Call struct {
	// Authority is what the API decided about the caller, stamped into
	// every quota-consuming record.
	Authority artifacts.Authority

	// OpKey is the caller's operation key: an Idempotency-Key or a turn's
	// unit of work, already held to the state log's grammar by the surface
	// that took it (statelog.CheckCallerOpID). Every record id is derived
	// from it, so a retry is the same operation. EMPTY MINTS A FRESH ID,
	// which is right for a caller that never retries.
	OpKey string

	// TurnID is the agent turn the write came from. Provenance only.
	TurnID string
}

// CreateInput is a new artifact and its first version.
type CreateInput struct {
	Kind        artifacts.Kind
	Title       string
	Description string
	Icon        string

	// Owner is the seat handle (or machine login) that owns it, and
	// OwnerKind its actor kind.
	Owner     string
	OwnerKind iam.ActorKind

	// SourceKey is the optional republish-in-place address.
	SourceKey string

	// First is the first version; its Artifact and Base are ignored.
	First PublishInput

	// Shares are granted with the create, such as the requester as editor
	// when an agent creates.
	Shares []artifacts.Share

	// Folder is the folder the artifact is created in, empty for the top
	// level: a live folder the caller's decision already admitted filing
	// into (contributor on it).
	Folder string
}

// PublishInput is one new version of an artifact.
type PublishInput struct {
	Artifact artifacts.ID

	// Base is the head the caller decided against (If-Match), ignored when
	// Force.
	Base int

	// Force publishes over a moved head; only a person with presence is
	// admitted to it (D08), and the version is marked forced.
	Force bool

	// RestoredFrom rolls back to an earlier version: the new version is that
	// version's files, every entry carried. Manifest is then left empty.
	RestoredFrom int

	Label        string
	Contract     string
	Capabilities []artifacts.Capability
	Manifest     artifacts.Manifest

	// Uploaded is every key this publish uploaded, minted at most
	// objstore.RecordWithin before the decide.
	Uploaded []objstore.Key

	// SearchText is at most artifacts.MaxSearchTextBytes.
	SearchText string

	// BrandOff is a version whose page opts out of the company's adopted
	// brand (`<html data-crewlet-brand="off">`, found by the linter): a
	// framed version so marked pins no stylesheet. Every other framed
	// version pins the one adopted when it is published.
	BrandOff bool
}

// Published is a version a write landed.
type Published struct {
	Artifact  artifacts.ID
	Number    int
	VersionID uuid.UUID
	Result    statelog.Result
}

// Written is a write's outcome.
type Written struct{ Result statelog.Result }

// Patch changes the head's displayed fields or one version's label. A nil field
// is unchanged.
type Patch struct {
	Title, Description, Icon *string
	Label                    *VersionLabel
}

// AssetInput is one asset to add.
type AssetInput struct {
	Artifact    artifacts.ID
	Name        string
	ContentType string
	Object      objstore.Object
}

// Dependent is state another domain keeps per artifact that a purge removes
// FIRST, data before the artifact, so no step order leaves state without its
// artifact: a crash between the steps leaves a trashed artifact with no data,
// and a retry of the gesture collapses the steps that landed.
type Dependent interface {
	PurgeArtifactState(ctx context.Context, c Call, opID string, id artifacts.ID) error
}

// WriterDeps is what a writer is built from.
type WriterDeps struct {
	// Publisher is the domain's write authority, and DB the replicated
	// estate its decisions read.
	Publisher *statelog.Publisher
	DB        store.ReplicatedReader

	// Now is the clock authored instants and upload freshness are judged
	// by; nil is the wall clock in UTC.
	Now func() time.Time

	// Dependents are the purge's dependents, read at each purge; nil is
	// none.
	Dependents func() []Dependent

	// SeatKind resolves a seat handle to the actor kind of the seat (a
	// person's or an agent's) against the running chart: what a transfer
	// records as the new owner's kind, which opens the lead arm (D12).
	// Required.
	SeatKind func(ctx context.Context, handle string) (iam.ActorKind, error)
}

// Writer is the artifacts write path.
type Writer struct {
	publisher  *statelog.Publisher
	db         store.ReplicatedReader
	now        func() time.Time
	dependents func() []Dependent
	seatKind   func(ctx context.Context, handle string) (iam.ActorKind, error)
}

// NewWriter builds the write path.
func NewWriter(deps WriterDeps) (*Writer, error) {
	switch {
	case deps.Publisher == nil:
		return nil, errors.New("artifactsdomain: a write authority is required: every " +
			"write is a record on the artifacts log")
	case deps.DB.IsZero():
		return nil, errors.New("artifactsdomain: the replicated estate is required: " +
			"a write decides from this node's own applied rows")
	case deps.SeatKind == nil:
		return nil, errors.New("artifactsdomain: a seat resolver is required: a " +
			"transfer records the new owner's kind, which decides whether its lead " +
			"may act on the artifact")
	}
	w := &Writer{
		publisher: deps.Publisher, db: deps.DB, now: deps.Now,
		dependents: deps.Dependents, seatKind: deps.SeatKind,
	}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	return w, nil
}

// The namespaces a version's and an asset's ids are derived in from their
// creating operation. FIXED for the life of the format: a new one would give a
// retry that straddles an upgrade a second version or asset.
var (
	VersionNamespace = uuid.MustParse("6a3c1f0e-8b2d-5e47-9c1a-4d7e2b9f0c35")
	AssetNamespace   = uuid.MustParse("f1e2d3c4-b5a6-5978-8a9b-0c1d2e3f4a5b")
)

// versionIDOf is the version a publish under opID makes.
func versionIDOf(opID string) uuid.UUID { return uuid.NewSHA1(VersionNamespace, []byte(opID)) }

// assetIDOf is the asset an upload under opID makes.
func assetIDOf(opID string) uuid.UUID { return uuid.NewSHA1(AssetNamespace, []byte(opID)) }

// operation is the id one write is published under: derived from the call's
// key when there is one, freshly minted when there is not.
//
// THE VERB, THE OBJECT AND WHAT THE WRITE SAYS ARE PART OF THE DERIVATION, the
// pages store's rule: one key covers every record one gesture writes, and the
// same key sent with another body is another operation rather than the first
// answered back from the ledger with nothing of the second written. AND IT
// CARRIES THE KEY'S OWN INSTANT, which the ledger vouches for a retry by.
func (w *Writer) operation(c Call, verb, object string, content any) (string, error) {
	key := strings.TrimSpace(c.OpKey)
	if key == "" {
		return statelog.NewOpID(time.Now(), verb), nil
	}
	if err := statelog.CheckCallerOpID(key); err != nil {
		return "", fmt.Errorf("%w: the operation key: %w", ErrInvalid, err)
	}
	said, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("artifactsdomain: digest what a %s says: %w", verb, err)
	}
	sum := sha256.Sum256(said)
	at, _ := statelog.OpMintedAt(key)
	return statelog.DeriveOpID(at, verb, key, object, hex.EncodeToString(sum[:])), nil
}

// validCall refuses a caller the records could not name.
func validCall(c Call) error {
	a := c.Authority
	switch {
	case strings.TrimSpace(a.Actor.Name) == "":
		return fmt.Errorf("%w: the call names no actor: every record names who wrote it", ErrInvalid)
	case !a.Actor.Kind.Valid():
		return fmt.Errorf("%w: actor kind %q is not one of %v", ErrInvalid, a.Actor.Kind, iam.ActorKinds)
	case a.Role != "" && !a.Role.Valid():
		return fmt.Errorf("%w: role %q is not one of %v", ErrInvalid, a.Role, artifacts.Roles)
	case a.Origin != "" && !a.Origin.Valid():
		return fmt.Errorf("%w: origin %q is not one of %v", ErrInvalid, a.Origin, artifacts.WriteOrigins)
	}
	return nil
}

// quotaCall refuses a quota-consuming write whose authority carries no limits:
// the applier refuses such a record on every data node, so it is refused here first.
func quotaCall(c Call) error {
	if !stampedLimits(c.Authority) {
		return fmt.Errorf("%w: the call's authority carries no quota limits: the API "+
			"stamps the limits in force (config.Artifacts.Stamp) into every write "+
			"that consumes quota", ErrInvalid)
	}
	return nil
}

// decision encodes one record inside the snapshot's own transaction, stamped
// with the framework's writer and generation.
func (w *Writer) decision(stamp statelog.Stamp, c Call, subject Subject, op OpKind,
	scope ScopeSet, opID string, payload any, at time.Time) (statelog.Decision, error) {

	if err := subject.writable(); err != nil {
		return statelog.Decision{}, err
	}
	if err := scope.Validate(); err != nil {
		return statelog.Decision{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return statelog.Decision{}, fmt.Errorf("artifactsdomain: encode the %s payload "+
			"for %s: %w", op, subject, err)
	}
	actor := c.Authority.Actor
	encoded, err := Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			OpID: opID, Subject: subject, Op: op, CreatedAt: at,
			Gen: stamp.Gen, Writer: stamp.Writer, Scope: scope,
		},
		Mutation: body, Actor: actor.Name, ActorKind: actor.Kind,
		OperatorID: actor.OperatorID, TurnID: c.TurnID,
	})
	if err != nil {
		return statelog.Decision{}, err
	}
	return statelog.Decision{Payload: encoded}, nil
}

// publish appends one write, translating the framework's exhausted rounds into
// this package's conflict: somebody else keeps changing the artifact.
func (w *Writer) publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	res, err := w.publisher.Publish(ctx, req)
	return res, publishError(req.Subject, err)
}

// publishError is the framework's refusal of a publish on subject, in this
// domain's terms: a lost race is an artifact somebody kept changing, and a
// create the framework found already made ([statelog.ErrExists]) is a
// COLLISION — on a source address, another artifact holds it
// ([ErrSourceTaken]); on anything else, what the create makes is there
// ([ErrExists]). The framework's bare sentinel reached the artifacts API's
// fallback and was answered as this node's fault.
//
// AND A WRITE THE DELETION GATE DROPPED IS A PURGE ([ErrPurged]): the decide
// passed, and the artifact's purge landed ahead of the record, so its deletion
// marker held the record on every node ([Gates.GatedAt]). That is the same
// fact the decide reports when it is the one that finds the marker, and it is
// told the same way — the framework states it as one of its conditions, which
// the artifacts API answered `503` "try again" and a tool "try again
// shortly", about an artifact nothing will ever write again.
func publishError(subject statelog.Subject, err error) error {
	var refused *statelog.Unavailable
	switch {
	case errors.As(err, &refused) && refused.Reason == statelog.ReasonDeleted:
		artifact, ok := gatedArtifact(subject.Kind, subject.ID)
		if !ok {
			artifact = subject.ID
		}
		return fmt.Errorf("%w: %s", ErrPurged, artifact)
	case errors.Is(err, statelog.ErrConflict):
		return fmt.Errorf("%w: %s: %w", ErrConflict, subject.ID, err)
	// THE COLLISION IS THE WHOLE OF WHAT HAPPENED, so the framework's own
	// words, which name the subject's broker path, are not carried after
	// it: a person reads this error on the artifacts API.
	case errors.Is(err, statelog.ErrExists) && ObjectKind(subject.Kind) == KindSource:
		return ErrSourceTaken
	case errors.Is(err, statelog.ErrExists):
		return &AlreadyExists{What: subject.ID}
	}
	return err
}

// request is one write's framework request on subject, its scope resolved.
func request(subject Subject, scope ScopeSet, opID string, pattern statelog.Pattern,
	decide func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error)) statelog.Request {

	return statelog.Request{
		Subject: subject.framework(), Scope: scope.Resolve(subject), OpID: opID,
		Pattern: pattern, Decide: decide,
	}
}

// verdict is what the apply of operation opID did on artifact id, read from
// this node's own rows: the history row it wrote, and false where there is
// none to read (the record has not applied here yet, it writes no history, or
// the artifact was purged since).
func (w *Writer) verdict(ctx context.Context, id, opID string, res statelog.Result) (HistoryEntry, bool, error) {
	if res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() {
		return HistoryEntry{}, false, nil
	}
	var (
		e                       HistoryEntry
		outcome, reason, sum, k string
		at                      int64
		elevated                int
		found                   bool
	)
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT position, op, outcome, reason, number, actor, actor_kind,
			       operator_id, elevated, origin, work_item, at, summary
			FROM artifact_history WHERE op_id = ? AND artifact_id = ?
			ORDER BY position LIMIT 1`, opID, id).Scan(&e.Position, &e.Op, &outcome,
			&reason, &e.Number, &e.Actor.Name, &k, &e.Actor.OperatorID, &elevated,
			&e.Origin, &e.WorkItem, &at, &sum)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return json.Unmarshal([]byte(sum), &e.Summary)
	})
	if err != nil {
		return HistoryEntry{}, false, fmt.Errorf("artifactsdomain: read what operation "+
			"%s did on %s: %w", opID, id, err)
	}
	e.OpID, e.Outcome, e.Reason = opID, Outcome(outcome), Refusal(reason)
	e.Actor.Kind, e.Elevated, e.At = iam.ActorKind(k), elevated != 0, store.DecodeTime(at)
	return e, found, nil
}

// refused is the error a refused verdict answers, or nil for one that applied.
func (w *Writer) refused(ctx context.Context, id string, e HistoryEntry) error {
	if e.Outcome != OutcomeRefused {
		return nil
	}
	return w.refusal(ctx, id, e.Reason, e.Summary)
}

// refusal translates an apply's refusal into the error a caller acts on.
func (w *Writer) refusal(ctx context.Context, id string, reason Refusal, s HistorySummary) error {
	switch reason {
	case RefusedNotFound, RefusedVersionNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	case RefusedTrashed:
		return fmt.Errorf("%w: %s", ErrTrashed, id)
	case RefusedPurged:
		return fmt.Errorf("%w: %s", ErrPurged, id)
	case RefusedHeadMoved:
		var moved error
		if err := w.db.Read(ctx, func(tx *sql.Tx) error {
			moved = versionMovedTx(ctx, tx, id)
			return nil
		}); err != nil {
			return err
		}
		return moved
	case RefusedCarriedObjectGone:
		return &CarriedObjectGone{Paths: s.Paths}
	case RefusedQuotaArtifacts, RefusedQuotaBytes:
		return &QuotaExceeded{Limit: s.Limit, Usage: s.Usage, Key: s.Key}
	case RefusedLabelTaken:
		return fmt.Errorf("%w: %q", ErrLabelTaken, s.Label)
	case RefusedSourceTaken:
		return ErrSourceTaken
	case RefusedObjectNamedElsewhere:
		return fmt.Errorf("%w: %s name objects another artifact or an asset already "+
			"names; upload them again", ErrInvalid, strings.Join(s.Paths, ", "))
	case RefusedExists:
		return &AlreadyExists{What: id}
	case RefusedLive:
		return fmt.Errorf("%w: artifact %s is not in the trash", ErrInvalid, id)
	case RefusedPurging:
		return fmt.Errorf("%w: %s", ErrPurging, id)
	case RefusedFolderGone:
		return fmt.Errorf("%w: it was removed while this write was decided", ErrFolderNotFound)
	case RefusedFolderCycle:
		return ErrFolderCycle
	case RefusedFolderDepth:
		return ErrFolderDepth
	case RefusedFolderNameTaken:
		return ErrFolderNameTaken
	case RefusedFolderLimit:
		return ErrFolderLimit
	}
	return fmt.Errorf("%w: the log refused this write as %s", ErrInvalid, reason)
}

// versionMovedTx is the 412 a publish over a moved head answers: the live head
// and its version, read in tx.
func versionMovedTx(ctx context.Context, tx *sql.Tx, id string) error {
	head, err := mustHead(ctx, tx, id)
	if err != nil {
		return err
	}
	live, found, err := readVersionTx(ctx, tx, id, head.HeadNumber)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("artifactsdomain: artifact %s's head version %d has no row",
			id, head.HeadNumber)
	}
	return &VersionMoved{Head: head, Live: live}
}

// planRefusal is a decide-time refusal of an object plan, in the caller's
// terms.
func planRefusal(r *refusalFacts) error {
	switch r.reason {
	case RefusedCarriedObjectGone:
		return &CarriedObjectGone{Paths: r.paths}
	case RefusedObjectNamedElsewhere:
		return fmt.Errorf("%w: %s name objects another artifact or an asset already "+
			"names; a carry never crosses an artifact, so upload them", ErrInvalid,
			strings.Join(r.paths, ", "))
	}
	return fmt.Errorf("%w: %s name a key this artifact holds with other bytes",
		ErrInvalid, strings.Join(r.paths, ", "))
}

// checkQuota refuses, from the snapshot, a write the stamped limits would
// refuse at the apply.
func checkQuota(ctx context.Context, tx *sql.Tx, limits artifacts.Limits, artifactsAdded, bytesAdded int64) error {
	usage, err := readUsage(ctx, tx)
	if err != nil {
		return err
	}
	if artifactsAdded > 0 && usage.artifacts+artifactsAdded > limits.Artifacts {
		return &QuotaExceeded{Limit: limits.Artifacts, Usage: usage.artifacts, Key: QuotaKeyArtifacts}
	}
	if bytesAdded > 0 && usage.bytes+bytesAdded > limits.CompanyBytes {
		return &QuotaExceeded{Limit: limits.CompanyBytes, Usage: usage.bytes, Key: QuotaKeyCompanyBytes}
	}
	return nil
}

// checkUploads refuses a version whose uploaded entries are not ones this
// publish uploaded, or whose keys are older than objstore.RecordWithin by the
// deciding clock: such a key may already be the collector's.
func checkUploads(m artifacts.Manifest, uploaded []objstore.Key, now time.Time) error {
	for _, k := range uploaded {
		if age := now.Sub(k.Minted()); age > objstore.RecordWithin {
			return fmt.Errorf("%w: object %s was minted %s ago and a publish names an "+
				"upload at most %s after it", ErrUploadStale, k, age.Truncate(time.Second),
				objstore.RecordWithin)
		}
	}
	var newFiles int
	var newBytes int64
	for _, e := range m.Files {
		if e.Carried {
			continue
		}
		if !slices.Contains(uploaded, e.Object.Key) {
			return fmt.Errorf("%w: %q names object %s, which this publish did not "+
				"upload: carry it from this artifact or upload it", ErrInvalid, e.Path, e.Object.Key)
		}
		newFiles++
		newBytes += e.Object.Size
	}
	if newFiles > artifacts.MaxFilesPerPublish {
		return fmt.Errorf("%w: the publish uploads %d files and the maximum is %d",
			ErrInvalid, newFiles, artifacts.MaxFilesPerPublish)
	}
	if newBytes > artifacts.MaxPublishBytes {
		return fmt.Errorf("%w: the publish uploads %d bytes and the maximum is %d",
			ErrInvalid, newBytes, artifacts.MaxPublishBytes)
	}
	return nil
}

// checkText refuses a text field outside its bounds.
func checkText(field, value string, max int, required bool) error {
	switch {
	case required && strings.TrimSpace(value) == "":
		return fmt.Errorf("%w: %s is required", ErrInvalid, field)
	case len(value) > max:
		return fmt.Errorf("%w: %s is %d bytes and the maximum is %d", ErrInvalid, field,
			len(value), max)
	}
	return nil
}

// checkCapabilities refuses a capability this build does not serve
// (`capability_unknown`).
func checkCapabilities(caps []artifacts.Capability) error {
	for _, c := range caps {
		if !c.Valid() {
			return fmt.Errorf("%w: capability %q is not one of %v", ErrInvalid, c,
				artifacts.Capabilities)
		}
	}
	return nil
}

// canonicalCapabilities is a declared set, sorted and unique.
func canonicalCapabilities(caps []artifacts.Capability) []artifacts.Capability {
	if len(caps) == 0 {
		return nil
	}
	return slices.Compact(slices.Sorted(slices.Values(caps)))
}

// checkName refuses a seat handle or login a subject or a scope path could not
// carry.
func checkName(field, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalid, field)
	}
	if strings.ContainsAny(name, " \t\r\n*>/") || len(name) > artifacts.MaxAudienceIDBytes {
		return fmt.Errorf("%w: %s %q carries whitespace, a wildcard or a slash, or is "+
			"longer than %d bytes", ErrInvalid, field, name, artifacts.MaxAudienceIDBytes)
	}
	return nil
}

// retention is the unlabelled versions a write retires so the artifact keeps at
// most [artifacts.VersionsKeptUnlabelled] of them, oldest first: the versions
// unlabelled now (with unlabelling counted among them, a label this write
// removes), plus the new version when adding is true and it carries no label.
// The head is never retired.
func retention(ctx context.Context, tx *sql.Tx, id string, adding bool, unlabelling, head int) ([]int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT number FROM artifact_versions
		WHERE artifact_id = ? AND (label = '' OR number = ?)
		ORDER BY number`, id, unlabelling)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: read %s's unlabelled versions: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var numbers []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("artifactsdomain: read %s's unlabelled versions: %w", id, err)
		}
		numbers = append(numbers, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	count := len(numbers)
	if adding {
		count++
	}
	excess := count - artifacts.VersionsKeptUnlabelled
	var out []int
	for _, n := range numbers {
		if excess <= 0 {
			break
		}
		if n == head && !adding {
			continue
		}
		out = append(out, n)
		excess--
	}
	return out, nil
}

// labelled counts artifact id's labelled versions.
func labelled(ctx context.Context, tx *sql.Tx, id string) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artifact_versions WHERE artifact_id = ? AND label != ''`,
		id).Scan(&n); err != nil {
		return 0, fmt.Errorf("artifactsdomain: count %s's labels: %w", id, err)
	}
	return n, nil
}

// pinsBrand reports whether a version of kind pins the adopted brand's
// stylesheet: a framed kind (a page the artifact origin serves, where the
// stylesheet's link is placed) whose page does not opt out. A native kind is
// drawn by the dashboard, which follows the adopted brand live, and a deck is
// branded by the copy its starter made.
func pinsBrand(kind artifacts.Kind, in PublishInput) bool {
	return kind.Framed() && !in.BrandOff
}

// namesBrand reports whether a write that publishes, retires or purges
// versions of an artifact of kind names the adopted brand in its scope: a
// framed kind's version may pin an adoption's stylesheet, so such a record's
// apply reads the brand's rows ([pinnedBrand]) or prunes them once the last
// version naming a superseded stylesheet is gone ([deleteVersions]). A
// version that opts out of the brand still retires older ones that may pin.
func namesBrand(kind artifacts.Kind) bool { return kind.Framed() }

// kindOf is an artifact's kind as this node's rows hold it, read before a
// publish so its scope names the brand exactly when its apply reads the
// brand's rows. A kind is fixed at create, so a stale row answers it; an
// artifact this node does not hold answers none, and the decide refuses it.
func (w *Writer) kindOf(ctx context.Context, id artifacts.ID) (artifacts.Kind, error) {
	var kind artifacts.Kind
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		head, found, err := readHeadTx(ctx, tx, id.String())
		if found {
			kind = head.Kind
		}
		return err
	})
	if err != nil {
		return "", fmt.Errorf("artifactsdomain: read artifact %s's kind: %w", id, err)
	}
	return kind, nil
}

// sourceIDOf is the source subject id an artifact's head holds, empty for none.
func sourceIDOf(head Head) string {
	if head.SourceKey == "" {
		return ""
	}
	return SourceSubject(head.Owner, head.SourceKey).ID
}

// errSourceMoved is an artifact whose source address moved between the read
// that named it in a request's scope and the decide.
var errSourceMoved = errors.New("artifactsdomain: the artifact's source address moved under this write")

// sourcedMoveAttempts is how many times a write re-reads an artifact's source
// address after it moved between the read and the decide. THREE: an address
// moves only on a transfer or a purge, so two inside one write's window is
// already a coincidence.
const sourcedMoveAttempts = 3

// sourcedScope is a write's scope on an artifact that holds a source address:
// the artifact, and the address the write releases.
func sourcedScope(id artifacts.ID, sourceID string) ScopeSet {
	if sourceID == "" {
		return ScopeSet{Subject: true}
	}
	return ScopeSet{Terms: []ScopeTerm{
		artifactTerm(id.String()), {Kind: TermSource, ID: sourceID},
	}}
}

// onSourced publishes a write that releases an artifact's source address, with
// the address in its scope: read first, then the decide refuses an artifact
// whose address is no longer the one the scope names, and the write reads
// again.
func (w *Writer) onSourced(ctx context.Context, id artifacts.ID,
	build func(sourceID string) statelog.Request) (statelog.Result, error) {

	var (
		res statelog.Result
		err error
	)
	for range sourcedMoveAttempts {
		var sourceID string
		if err = w.db.Read(ctx, func(tx *sql.Tx) error {
			head, found, err := readHeadTx(ctx, tx, id.String()) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if found {
				sourceID = sourceIDOf(head)
			}
			return err
		}); err != nil {
			return statelog.Result{}, fmt.Errorf("artifactsdomain: read %s's source "+
				"address: %w", id, err)
		}
		res, err = w.publish(ctx, build(sourceID))
		if !errors.Is(err, errSourceMoved) {
			return res, err
		}
	}
	return res, err
}

// sameSource refuses a head whose source address is not the one a request's
// scope named.
func sameSource(head Head, sourceID string) error {
	if sourceIDOf(head) == sourceID {
		return nil
	}
	return errSourceMoved
}
