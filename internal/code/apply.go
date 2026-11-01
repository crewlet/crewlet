package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier, and the rules that make it a PURE FUNCTION of the log.
//
// N nodes derive one SQL state from one ordered stream, and the claim that
// their tables are byte-identical survives exactly as long as these hold —
// the tracker's rules, because they are the framework's:
//
//  1. IT READS NO CONFIG AND NO CLOCK. Every instant it writes is the broker's
//     ([statelog.ApplyOptions.StoredAt]); every limit it keeps is a constant.
//  2. IT WRITES ONLY WHAT THE RECORD SAYS. A push's commands carry their
//     results, so the applier writes them rather than deciding them again:
//     the broker's arbitration on the repository's subject guarantees the
//     decide saw these rows, and a second decide here would be a second
//     opinion the fleet could disagree about.
//  3. EVERY GUARD IS A SKIP, NEVER AN ERROR, for ordinary traffic: a
//     redelivered record applies as nothing, because a constraint violation
//     inside this transaction would abort it identically on every node and
//     stall the fleet's log. What IS an error is a record no correct writer
//     forms — one that arbitrated one object and claims another — because
//     applying it would be the corruption the subject exists to prevent.
//  4. EVERY HORIZON IS A FUNCTION OF THE RECORD: the push history and the ref
//     tombstones a push record removes are chosen by its broker instant and
//     its own repository's rows — the tombstones also by their count against
//     MaxTombstonesPerRepo — never by a sweep that would be a second writer
//     of the replicated estate.

// Applier writes this node's copy of the code host.
//
// It carries NO NODE IDENTITY, and that is the purity rule rather than an
// omission ([statelog.Applier]): an applier that could read which node it runs
// on could write rows its peers do not. The eviction gate needs none — it
// compares the record's own [statelog.Record.Writer] with the eviction rows.
// What it does hold of its node is a LISTENER, told after a commit whose
// notices moved somebody's inbox on this node (internal/inbox's movement.go),
// and a listener writes no row.
type Applier struct {
	// moved is told, after a committed batch, whose inbox moved on this
	// node. Nil is legal and means nobody is listening.
	moved func([]inbox.Movement)

	// inboxed is the notices the batch wrote or removed, filled inside Apply
	// and drained by Committed — see [inbox.Batch] for why it holds no lock.
	inboxed inbox.Batch
}

// NewApplier builds the code host's applier for one node.
//
// moved is called after a COMMITTED batch with every inbox that batch's
// notices moved (apply_notices.go), each movement naming this domain as its
// source. After the commit and never inside the transaction: the store re-runs
// the body of an attempt that failed transiently, and a movement announced
// from inside it would announce a notice no row holds.
func NewApplier(moved func([]inbox.Movement)) *Applier { return &Applier{moved: moved} }

// Committed is the post-commit half: telling THIS node's open dashboards whose
// inbox the batch's notices moved — the one consequence of a code record no
// other node needs. Every data node applies every record, and only a data node
// serves dashboards, so each tells its own. A seat's wake is derived from the
// code log's feed, never from here, and no cache is told to sync — a git
// node's cache converges by reading the rows when it is asked for them.
func (a *Applier) Committed(context.Context) {
	// RESET BEFORE THE CALL, not after: the listener may take long enough
	// for the next batch to be waiting behind it.
	moved := a.inboxed.Drain(Domain{}.Name())
	if len(moved) > 0 && a.moved != nil {
		a.moved(moved)
	}
}

// Gated reports a record that must produce no rows at all — read from this
// same transaction, because the answer has to come from the state the record
// would have applied against:
//
//  1. THE EVICTION GATE. A record written by a node the fleet evicted before
//     the record's own position is dropped everywhere.
//  2. THE DELETION GATE. A record about a repository a purge destroyed applies
//     nowhere, for ever — otherwise a redelivery months later would resurrect
//     rows an operator's configuration removed. Its hit is counted on the
//     marker, an operator surface: a repository still catching records
//     months after its purge is a writer nobody stopped. A record on a
//     CHANGE's subject is about the repository its payload names, read off
//     the record's body, and one on a repository's CHECKS subject about the
//     repository its subject names ([gatedRepo]) — so a check that ends
//     after its repository's purge writes nothing.
//
// The eviction window first, the knowledge base's order: a record both gates
// drop is dropped either way, so the order decides only which gate COUNTS it.
func (a *Applier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (statelog.Reason, bool, error) {
	if rec.Writer != "" {
		var from, readmitted sql.NullInt64
		err := tx.QueryRowContext(ctx, evictionSQL, rec.Writer).Scan(&from, &readmitted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return "", false, fmt.Errorf("code: read the eviction gate for node %s: %w", rec.Writer, err)
		default:
			if evictedAt(rec.Position.Packed(), from, readmitted) {
				return statelog.ReasonEvicted, true, nil
			}
		}
	}
	repoID, ok := gatedRepo(rec.Subject, rec.Payload)
	if !ok {
		return "", false, nil
	}
	var author sql.NullString
	err := tx.QueryRowContext(ctx, markerSQL, repoID).Scan(&author)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("code: read the deletion gate for repository %s: %w", repoID, err)
	}
	// THE ONE EXCEPTION IS THE RECORD THAT WROTE THE MARKER, by its own id —
	// not by its op: "any purge" would let a second purge of a repository id
	// through, and a purge destroys rows.
	if author.Valid && author.String == rec.OpID {
		return "", false, nil
	}
	if _, err := tx.ExecContext(ctx, countRejectSQL,
		store.EncodeTime(rec.StoredAt), repoID); err != nil {
		return "", false, fmt.Errorf("code: count a gate hit on the purged repository %s: %w", repoID, err)
	}
	return statelog.ReasonDeleted, true, nil
}

// countRejectSQL counts one record a deletion marker dropped.
const countRejectSQL = `UPDATE code_deletions SET rejects = rejects + 1, last_reject_at = ? WHERE repo_id = ?`

// Apply writes one record's rows and answers how many it wrote or deleted.
//
// The dispatch is on the SUBJECT KIND, and every kind has a case — including
// the two that write nothing — so a kind added later without one is an error
// rather than a silently ignored record.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	record, err := Decode(rec.Payload)
	if err != nil {
		return 0, fmt.Errorf("code: decode the record at %s: %w", rec.Position, err)
	}
	at := applyContext{
		record:       record,
		position:     rec.Position,
		packed:       rec.Position.Packed(),
		brokerAt:     opts.StoredAt,
		now:          opts.Now,
		epoch:        opts.Epoch,
		maxVariables: opts.MaxVariables,
	}
	switch ObjectKind(rec.Subject.Kind) {
	case KindBarrier:
		// THE READ INDEX'S OWN APPEND: no row on any node, by design.
		return 0, nil
	case KindTrimmed:
		// THE TRIM'S OWN RECORD: the framework removes this log's anchors
		// below its floor in this same transaction (ADR-0050); this domain
		// writes nothing for it.
		return 0, nil
	case KindEviction:
		return a.applyEviction(ctx, tx, at)
	case KindGeneration:
		return a.applyGeneration(ctx, tx, at)
	case KindCompany:
		return a.applyActivation(ctx, tx, at)
	case KindName:
		// A NAME'S SUBJECT carries the records that claim, move or free a
		// name: a create, a rename onto it, and the release of an old name
		// whose redirect ended.
		switch record.Op {
		case OpRename:
			return a.applyRename(ctx, tx, at)
		case OpReleaseName:
			return a.applyReleaseName(ctx, tx, at)
		}
		return a.applyCreate(ctx, tx, at)
	case KindRepo:
		return a.applyRepo(ctx, tx, at)
	case KindChange:
		return a.applyChange(ctx, tx, at)
	case KindChecks:
		return a.applyChecks(ctx, tx, at)
	case KindPerson:
		return a.applyMarks(ctx, tx, at)
	}
	// A KIND THIS BUILD DOES NOT KNOW REACHES HERE ONLY BY WAY OF A RECORD AT
	// A VERSION IT CAN READ, which is a writer publishing a kind it never
	// declared; failing is what makes the writer's mistake visible.
	return 0, fmt.Errorf("code: %s is not a kind this build applies, and the "+
		"record at %s claims version %d — a kind is declared before it is published",
		rec.Subject.Kind, rec.Position, record.V)
}

// applyContext is what every case needs, gathered once.
type applyContext struct {
	record   MutationRecord
	position statelog.Position

	// packed is the composed position: the version every row's guard
	// compares against, and the position every row records.
	packed int64

	// brokerAt is the broker's own instant for this record, and the ONLY
	// instant this domain writes from a record's own apply.
	brokerAt time.Time

	// now is the batch's instant and epoch the company's settings the batch
	// is applied under: what a notice's horizon is judged by — the one
	// DIVERGENT table this domain's records write (apply_notices.go), and
	// nothing else this applier writes reads either.
	now   time.Time
	epoch map[string]any

	// maxVariables is the engine's probed bind-parameter limit, which sizes
	// a multi-row INSERT and decides nothing about which rows land.
	maxVariables int
}

// at is the broker's instant as the store encodes it.
func (c applyContext) at() int64 { return store.EncodeTime(c.brokerAt) }

// applyEviction records a node's removal from this log, or its readmission —
// an INVERSE COMMIT rather than a delete, so an eviction's whole history
// survives a replay.
func (a *Applier) applyEviction(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	e, ok := payload.(Eviction)
	if !ok {
		return 0, fmt.Errorf("code: the eviction at %s carries a %T", at.position, payload)
	}
	if e.NodeID == "" {
		return 0, fmt.Errorf("code: the eviction at %s names no node", at.position)
	}
	if e.Readmitted {
		res, execErr := tx.ExecContext(ctx, readmitSQL, at.packed, at.packed, e.NodeID, at.packed)
		if execErr != nil {
			return 0, fmt.Errorf("code: readmit node %s at %s: %w", e.NodeID, at.position, execErr)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	// A SECOND EVICTION CLEARS THE READMISSION, so evicted-readmitted-evicted
	// reads as three facts rather than one window with a hole in it.
	res, err := tx.ExecContext(ctx, evictSQL,
		e.NodeID, at.at(), e.EvictedBy, at.packed, at.packed)
	if err != nil {
		return 0, fmt.Errorf("code: evict node %s at %s: %w", e.NodeID, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// The eviction statements.
const (
	readmitSQL = `UPDATE code_evictions SET readmitted_position = ?, version = ?
		WHERE node_id = ? AND version < ?`
	evictSQL = `INSERT INTO code_evictions
			(node_id, at, by, from_position, readmitted_position, version)
		VALUES (?, ?, ?, ?, NULL, ?)
		ON CONFLICT (node_id) DO UPDATE SET
			at = excluded.at, by = excluded.by,
			from_position = excluded.from_position,
			readmitted_position = NULL,
			version = excluded.version
		WHERE excluded.version > code_evictions.version`
)

// applyGeneration writes a reanchor's audit row, CREATE-ONLY: two operators
// deriving the same number race at the broker and exactly one wins, so a
// conflict here is a redelivery of the winner's.
func (a *Applier) applyGeneration(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	g, ok := payload.(Generation)
	if !ok {
		return 0, fmt.Errorf("code: the generation record at %s carries a %T", at.position, payload)
	}
	res, err := tx.ExecContext(ctx, generationSQL,
		g.Generation, at.at(), g.By, store.EncodeTime(g.StreamCreatedAt),
		int64(g.PrevHighest), at.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("code: record generation %d at %s: %w", g.Generation, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// generationSQL is a reanchor's audit row.
const generationSQL = `INSERT INTO code_log_generations
		(generation, at, by, new_stream_created_at, prev_last_seq_seen, record_id)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT (generation) DO NOTHING`

// applyActivation raises the activation fence to the record's activation,
// never lowers it: an older activation's record applied after a newer one's
// leaves the fence where the newer one put it, on every node alike.
//
// # Why the fence is safe — the argument the reconcile rests on
//
// A reconcile publishes its activation's fence record FIRST and decides every
// repository from a snapshot taken after that record applied (the session
// floor of its own append). So every config-managed record decided at an
// OLDER activation is either EARLIER in log order than this fence — and then
// already in the rows the newer reconcile decides from, so any difference it
// made is written back — or LATER, and then refused by the fence on every node
// ([Applier.fenced]). No node applying an older activation late can walk a
// newer one back, and an activation that changes nothing writes this one
// record rather than one per declared repository.
func (a *Applier) applyActivation(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	if at.record.Subject.ID != activationID {
		return 0, fmt.Errorf("code: the activation record at %s arbitrated %s, and "+
			"the fence is %s — a record on another company subject is not one this "+
			"build writes", at.position, at.record.Subject, CompanySubject())
	}
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	act, ok := payload.(Activated)
	if !ok {
		return 0, fmt.Errorf("code: the activation record at %s carries a %T", at.position, payload)
	}
	res, err := tx.ExecContext(ctx, raiseFenceSQL, activationID, act.ConfigEpoch, at.packed)
	if err != nil {
		return 0, fmt.Errorf("code: raise the activation fence to %d at %s: %w",
			act.ConfigEpoch, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// The fence's statements.
const (
	raiseFenceSQL = `INSERT INTO code_activation (id, fence, version) VALUES (?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			fence = MAX(code_activation.fence, excluded.fence),
			version = excluded.version
		WHERE excluded.version > code_activation.version`
	fenceSQL = `SELECT fence FROM code_activation WHERE id = ?`
)

// fenced reports a config-managed record decided at an activation below the
// fence this transaction holds: it applies as nothing, on every node, because
// the fence is derived from the log. Only a CONFIG-MANAGED record is held to
// it — the fence orders configuration activations, and nothing else carries
// one — so a TOOL'S record ([ManagedTools], its activation 0) applies at any
// fence: the configuration has nothing to say about it. What keeps a tool's
// record off a repository the configuration manages is its manager
// ([managerRefuses]), not the fence.
func fenced(ctx context.Context, tx *sql.Tx, managedBy ManagedBy, epoch int64) (bool, error) {
	if managedBy != ManagedConfig {
		return false, nil
	}
	var fence int64
	err := tx.QueryRowContext(ctx, fenceSQL, activationID).Scan(&fence)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("code: read the activation fence: %w", err)
	}
	return epoch < fence, nil
}

// applyCreate makes a repository: its name claim, its row and its protection
// rules, in one transaction.
func (a *Applier) applyCreate(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	p, ok := payload.(CreateRepo)
	if !ok {
		return 0, fmt.Errorf("code: the create at %s carries a %T", at.position, payload)
	}
	// THE NAME IS THE ADDRESS: a record that arbitrated one name and claims
	// another would take a name nobody contended for.
	if token := NameToken(p.Name); token != at.record.Subject.ID {
		return 0, fmt.Errorf("code: the create at %s arbitrated name %s and its "+
			"payload names %q, whose token is %s", at.position, at.record.Subject.ID, p.Name, token)
	}
	if p.RepoID == "" || !ValidName(p.Name) {
		return 0, fmt.Errorf("code: the create at %s names repository %q as %q, "+
			"which no writer forms", at.position, p.RepoID, p.Name)
	}
	// A PURGED ID IS NEVER REUSED, so a create of one is a redelivery of the
	// create of a repository since purged. The name subject is not under the
	// repository's marker, so the create checks the marker itself.
	var purged int
	if err = tx.QueryRowContext(ctx, purgedSQL, p.RepoID).Scan(&purged); err != nil {
		return 0, fmt.Errorf("code: read the deletion marker of %s: %w", p.RepoID, err)
	}
	if purged > 0 {
		return 0, nil
	}
	// A HELD NAME stays held, whatever repository it names: the broker
	// arbitrated the name, so a second create lands only as a redelivery,
	// and refusing would stall every node.
	var held int
	if err = tx.QueryRowContext(ctx, guardNameSQL, at.record.Subject.ID).Scan(&held); err != nil {
		return 0, fmt.Errorf("code: read the claim on %q: %w", p.Name, err)
	}
	if held > 0 {
		return 0, nil
	}
	if refused, fenceErr := fenced(ctx, tx, p.Settings.ManagedBy, p.Settings.ConfigEpoch); fenceErr != nil || refused {
		return 0, fenceErr
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO code_names (token, name, repo_id) VALUES (?, ?, ?)`,
		at.record.Subject.ID, p.Name, p.RepoID); err != nil {
		return 0, fmt.Errorf("code: claim %q at %s: %w", p.Name, at.position, err)
	}
	s := p.Settings
	// WHO MADE IT, as the record names its decider: a seat's handle or a
	// person's seat (the credential is on the record), and nobody for a
	// declaration, which the configuration made. Its idle clock starts at
	// its creation.
	createdBy, createdKind := at.record.Actor, string(at.record.ActorKind)
	if at.record.ActorKind == ActorConfig {
		createdBy = ""
	}
	// ITS MIRRORS AND WHAT ITS IMPORT FETCHES, each as its column holds it:
	// a create that imports — its own import, or a pull mirror's first fetch
	// from its source — makes the repository importing ([CreateRepo.status]).
	from, to, imports, err := remoteColumns(s, p.importSource())
	if err != nil {
		return 0, fmt.Errorf("code: the create at %s: %w", at.position, err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO code_repos
			(id, name, status, managed_by, kind, owner_kind, owner, visibility, search, default_branch,
			 description, config_epoch, created_at, created_by, created_by_kind, last_activity_at, version,
			 mirror_from, mirror_to, import_source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		p.RepoID, p.Name, string(p.status()), string(s.ManagedBy), string(s.Kind), string(s.Owner.Kind),
		s.Owner.Ref, string(s.Visibility), string(s.Search), s.DefaultBranch, s.Description, s.ConfigEpoch,
		at.at(), createdBy, createdKind, at.at(), at.packed, from, to, imports)
	if err != nil {
		return 0, fmt.Errorf("code: create repository %s at %s: %w", p.RepoID, at.position, err)
	}
	created, _ := res.RowsAffected()
	// AND COUNTED IN WITH ITS KIND, only where this record made the row: the
	// count a tool's create of a project is held to (repo_limit), which the
	// purge that removes the row counts out again.
	if created > 0 {
		if _, err = tx.ExecContext(ctx, countKindSQL, string(s.Kind)); err != nil {
			return 0, fmt.Errorf("code: count repository %s in at %s: %w", p.RepoID, at.position, err)
		}
	}
	protections, err := insertProtections(ctx, tx, at.maxVariables, p.RepoID, s.Protections)
	if err != nil {
		return 0, fmt.Errorf("code: write %s's protections at %s: %w", p.RepoID, at.position, err)
	}
	owners, err := insertPathOwners(ctx, tx, at.maxVariables, p.RepoID, s.PathOwners)
	if err != nil {
		return 0, fmt.Errorf("code: write %s's path owners at %s: %w", p.RepoID, at.position, err)
	}
	return 1 + 2*int(created) + protections + owners, nil
}

// insertPathOwners writes a repository's path owners, one row an owner with its
// globs packed one a line ([packPaths]). An owner the record names twice —
// which no writer forms, the settings holding each owner once — keeps its first
// row rather than aborting the transaction on every node, as a protection's
// pattern does.
func insertPathOwners(ctx context.Context, tx *sql.Tx, maxVariables int, repoID string,
	owners []PathOwner) (int, error) {

	return store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO code_path_owners (repo_id, owner_kind, owner, paths) VALUES `,
		`(?, ?, ?, ?)`, `ON CONFLICT (repo_id, owner_kind, owner) DO NOTHING`, len(owners),
		func(i int) []any {
			po := owners[i]
			return []any{repoID, string(po.Owner.Kind), po.Owner.Ref, packPaths(po.Paths)}
		})
}

// packPaths and unpackPaths are a path owner's globs as its row holds them:
// one a line, in the record's order. A glob holds no control character
// (gitref.ParsePathGlob), so no newline is a glob's and the packing is
// exact both ways.
func packPaths(paths []string) string { return strings.Join(paths, "\n") }

func unpackPaths(packed string) []string { return strings.Split(packed, "\n") }

// insertProtections writes a repository's rules in the record's order, each
// with its merge gate as the record encodes it ([gateColumn]). A pattern the
// record names twice — which no writer forms, the settings being sorted and
// unique — keeps its first rule rather than aborting the transaction on every
// node.
func insertProtections(ctx context.Context, tx *sql.Tx, maxVariables int, repoID string,
	rules []Protection) (int, error) {

	gates := make([]string, len(rules))
	for i, r := range rules {
		var err error
		if gates[i], err = gateColumn(r.Gate); err != nil {
			return 0, err
		}
	}
	return store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO code_protections (repo_id, pattern, allow_force, allow_delete, direct_push, gate) VALUES `,
		`(?, ?, ?, ?, ?, ?)`, `ON CONFLICT (repo_id, pattern) DO NOTHING`, len(rules),
		func(i int) []any {
			r := rules[i]
			return []any{repoID, r.Pattern, r.AllowForce, r.AllowDelete, string(r.DirectPush), gates[i]}
		})
}

// applyRepo is every record on a repository's own subject.
//
// THE SUBJECT GUARD: a record on one repository's subject may change only that
// repository — the payload's id must be the subject's — which is what makes
// the subject the arbitration unit rather than a label.
//
// THE VERSION GUARD (every record but the purge, which is the marker's own):
// under a strict replay the create is below this position, so a missing row
// is an error; a row at or past this position is a redelivery, applied as
// nothing.
func (a *Applier) applyRepo(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	id := at.record.Subject.ID
	var payloadRepo string
	switch p := payload.(type) {
	case PushBatch:
		payloadRepo = p.RepoID
	case SettingsRecord:
		payloadRepo = p.RepoID
	case LifecycleRecord:
		payloadRepo = p.RepoID
	case PurgePart:
		payloadRepo = p.RepoID
	case Purge:
		payloadRepo = p.RepoID
	case Compaction:
		payloadRepo = p.RepoID
	case RetireSweep:
		payloadRepo = p.RepoID
	case ExpungeRequest:
		payloadRepo = p.RepoID
	case ExpungeBegin:
		payloadRepo = p.RepoID
	case ExpungeCommit:
		payloadRepo = p.RepoID
	case ImportChunk:
		payloadRepo = p.RepoID
	case ImportDone:
		payloadRepo = p.RepoID
	case MirrorSync:
		payloadRepo = p.RepoID
	case Hold:
		payloadRepo = p.RepoID
	case ChangeOpen:
		payloadRepo = p.RepoID
	case ChangeEdit:
		payloadRepo = p.RepoID
	case ChangeDraft:
		payloadRepo = p.RepoID
	case ChangeRetarget:
		payloadRepo = p.RepoID
	case ChangeClose:
		payloadRepo = p.RepoID
	case ChangeReopen:
		payloadRepo = p.RepoID
	case ChangeLanded:
		payloadRepo = p.RepoID
	case Merge:
		payloadRepo = p.RepoID
	case Enqueue:
		payloadRepo = p.RepoID
	case Dequeue:
		payloadRepo = p.RepoID
	case Candidate:
		payloadRepo = p.RepoID
	case Eject:
		payloadRepo = p.RepoID
	case Land:
		payloadRepo = p.RepoID
	case Green:
		payloadRepo = p.RepoID
	case Broken:
		payloadRepo = p.RepoID
	case QueueSweep:
		payloadRepo = p.RepoID
	default:
		return 0, fmt.Errorf("code: %s is not an op this build applies to a repository "+
			"(the record at %s)", at.record.Op, at.position)
	}
	if payloadRepo != id {
		return 0, fmt.Errorf("code: the record at %s arbitrated repo.%s and its payload "+
			"names %s — a record on one repository's subject may change only that "+
			"repository", at.position, id, payloadRepo)
	}
	if p, ok := payload.(Purge); ok {
		return a.applyPurge(ctx, tx, at, p)
	}
	row, err := repoVersion(ctx, tx, id)
	if err != nil {
		return 0, fmt.Errorf("code: the %s at %s: %w", at.record.Op, at.position, err)
	}
	if row.version >= at.packed {
		return 0, nil
	}
	switch p := payload.(type) {
	case PushBatch:
		n, err := a.applyPush(ctx, tx, at, p)
		return a.noticed(ctx, tx, at, id, payload, n, err)
	case SettingsRecord:
		return a.applySettings(ctx, tx, at, p, row)
	case LifecycleRecord:
		if at.record.Op == OpDelete {
			return a.applyDelete(ctx, tx, at, p, row)
		}
		return a.applyRestore(ctx, tx, at, p, row)
	case Hold:
		return a.applyHold(ctx, tx, at, p)
	case PurgePart:
		return a.applyPurgePart(ctx, tx, at, p)
	case Compaction:
		return a.applyCompact(ctx, tx, at, p)
	case RetireSweep:
		return a.applyRetireSweep(ctx, tx, at, p)
	case ExpungeRequest:
		return a.applyExpungeRequest(ctx, tx, at, p)
	case ExpungeBegin:
		return a.applyExpungeBegin(ctx, tx, at, p)
	case ExpungeCommit:
		return a.applyExpungeCommit(ctx, tx, at, p)
	case ImportChunk:
		return a.applyImportChunk(ctx, tx, at, p)
	case ImportDone:
		return a.applyImportDone(ctx, tx, at, p)
	case MirrorSync:
		return a.applyMirrorSync(ctx, tx, at, p)
	case ChangeOpen:
		n, err := a.applyChangeOpen(ctx, tx, at, p)
		return a.noticed(ctx, tx, at, id, payload, n, err)
	case ChangeEdit, ChangeDraft, ChangeRetarget, ChangeClose, ChangeReopen, ChangeLanded:
		n, err := a.applyChangeWrite(ctx, tx, at, payload)
		return a.noticed(ctx, tx, at, id, payload, n, err)
	case Merge:
		return a.applyMerge(ctx, tx, at, p)
	case Enqueue:
		return a.applyEnqueue(ctx, tx, at, p)
	case Dequeue:
		return a.applyDequeue(ctx, tx, at, p)
	case Candidate:
		return a.applyCandidate(ctx, tx, at, p)
	case Eject:
		return a.applyEject(ctx, tx, at, p)
	case Land:
		return a.applyLand(ctx, tx, at, p)
	case Green:
		return a.applyGreen(ctx, tx, at, p)
	case Broken:
		return a.applyBroken(ctx, tx, at, p)
	case QueueSweep:
		return a.applyQueueSweep(ctx, tx, at, p)
	}
	return 0, fmt.Errorf("code: the record at %s carries a %T", at.position, payload)
}

// noticed is a record about changes applied, n rows, with its notices written
// beside them ([Applier.writeNotices]) — none where its apply wrote nothing: a
// redelivery, or a guard's skip, whose notices the record's first apply wrote
// or no change holds.
func (a *Applier) noticed(ctx context.Context, tx *sql.Tx, at applyContext, repoID string, payload any, n int,
	err error) (int, error) {

	if err != nil || n == 0 {
		return n, err
	}
	written, err := a.writeNotices(ctx, tx, at, repoID, payload)
	if err != nil {
		return 0, err
	}
	return n + written, nil
}

// versionRow is what the version guard reads of a repository.
type versionRow struct {
	version   int64
	managedBy ManagedBy
}

// repoVersion reads a repository's guard, refusing one that has no row: under
// a strict replay its create is below every record about it, and every record
// about a PURGED repository was already dropped by its marker.
func repoVersion(ctx context.Context, tx *sql.Tx, id string) (versionRow, error) {
	var row versionRow
	var managed string
	err := tx.QueryRowContext(ctx, repoVersionSQL, id).Scan(&row.version, &managed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return versionRow{}, fmt.Errorf("repository %s has no row — under a strict replay "+
			"its create is below this record, so the log is not what this node was "+
			"built from", id)
	case err != nil:
		return versionRow{}, fmt.Errorf("read repository %s: %w", id, err)
	}
	row.managedBy = ManagedBy(managed)
	return row, nil
}

// applySettings replaces a repository's settings whole, and moves its idle
// clock. A repository that is deleted or purging keeps its status: settings
// never resurrect one, a restore does.
//
// A RECORD OF ANOTHER MANAGER THAN THE REPOSITORY'S APPLIES AS NOTHING
// ([managerRefuses]), as a record below the activation fence does: a tool's
// settings never reach a repository code.repos declares, nor a
// configuration's one a tool made.
func (a *Applier) applySettings(ctx context.Context, tx *sql.Tx, at applyContext, p SettingsRecord,
	row versionRow) (int, error) {

	s := p.Settings
	if managerRefuses(row, s.ManagedBy) {
		return 0, nil
	}
	if refused, err := fenced(ctx, tx, s.ManagedBy, s.ConfigEpoch); err != nil || refused {
		return 0, err
	}
	from, to, _, err := remoteColumns(s, nil)
	if err != nil {
		return 0, fmt.Errorf("code: the settings at %s: %w", at.position, err)
	}
	res, err := tx.ExecContext(ctx, settingsSQL,
		string(s.ManagedBy), string(s.Owner.Kind), s.Owner.Ref, string(s.Visibility), string(s.Search),
		s.DefaultBranch, s.Description, from, to, string(s.status()), s.ConfigEpoch, at.at(), at.packed, p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("code: settle %s's settings at %s: %w", p.RepoID, at.position, err)
	}
	n, _ := res.RowsAffected()
	replaced, err := replaceProtections(ctx, tx, at, p.RepoID, s.Protections)
	if err != nil {
		return 0, err
	}
	owners, err := replacePathOwners(ctx, tx, at, p.RepoID, s.PathOwners)
	if err != nil {
		return 0, err
	}
	return int(n) + replaced + owners, nil
}

// replacePathOwners deletes a repository's path owners and writes the new set:
// at most MaxPathOwners rows each way.
func replacePathOwners(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	owners []PathOwner) (int, error) {

	res, err := tx.ExecContext(ctx, clearPathOwnersSQL, repoID)
	if err != nil {
		return 0, fmt.Errorf("code: clear %s's path owners at %s: %w", repoID, at.position, err)
	}
	removed, _ := res.RowsAffected()
	written, err := insertPathOwners(ctx, tx, at.maxVariables, repoID, owners)
	if err != nil {
		return 0, fmt.Errorf("code: write %s's path owners at %s: %w", repoID, at.position, err)
	}
	return int(removed) + written, nil
}

// replaceProtections deletes a repository's rules and writes the new set.
func replaceProtections(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	rules []Protection) (int, error) {

	res, err := tx.ExecContext(ctx, clearProtectionsSQL, repoID)
	if err != nil {
		return 0, fmt.Errorf("code: clear %s's protections at %s: %w", repoID, at.position, err)
	}
	removed, _ := res.RowsAffected()
	written, err := insertProtections(ctx, tx, at.maxVariables, repoID, rules)
	if err != nil {
		return 0, fmt.Errorf("code: write %s's protections at %s: %w", repoID, at.position, err)
	}
	return int(removed) + written, nil
}

// applyDelete marks a repository deleted. A delete of a repository already
// deleted or purging changes nothing and is applied completely as a no-op.
func (a *Applier) applyDelete(ctx context.Context, tx *sql.Tx, at applyContext, p LifecycleRecord,
	row versionRow) (int, error) {

	// A DELETE CARRIES NO SETTINGS, so whether the fence governs it is the
	// repository's own: the configuration's decision to retire a repository
	// it manages.
	if refused, err := fenced(ctx, tx, row.managedBy, p.ConfigEpoch); err != nil || refused {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, deleteRepoSQL,
		at.at(), p.ConfigEpoch, at.packed, p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("code: delete %s at %s: %w", p.RepoID, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyRestore brings a deleted repository back with the settings the restore
// carries, and moves its idle clock. Its refs and packs were never touched, so
// they come back as they were. A restore of another manager than the
// repository's applies as nothing ([managerRefuses]).
func (a *Applier) applyRestore(ctx context.Context, tx *sql.Tx, at applyContext, p LifecycleRecord,
	row versionRow) (int, error) {

	if p.Settings == nil {
		return 0, fmt.Errorf("code: the restore at %s carries no settings — a restore "+
			"states what the repository comes back with", at.position)
	}
	s := *p.Settings
	if managerRefuses(row, s.ManagedBy) {
		return 0, nil
	}
	if refused, err := fenced(ctx, tx, s.ManagedBy, p.ConfigEpoch); err != nil || refused {
		return 0, err
	}
	from, to, _, err := remoteColumns(s, nil)
	if err != nil {
		return 0, fmt.Errorf("code: the restore at %s: %w", at.position, err)
	}
	res, err := tx.ExecContext(ctx, restoreRepoSQL,
		string(s.ManagedBy), string(s.Owner.Kind), s.Owner.Ref, string(s.Visibility), string(s.Search),
		s.DefaultBranch, s.Description, from, to, string(s.status()), p.ConfigEpoch, at.at(), at.packed, p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("code: restore %s at %s: %w", p.RepoID, at.position, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, nil
	}
	replaced, err := replaceProtections(ctx, tx, at, p.RepoID, s.Protections)
	if err != nil {
		return 0, err
	}
	owners, err := replacePathOwners(ctx, tx, at, p.RepoID, s.PathOwners)
	if err != nil {
		return 0, err
	}
	return int(n) + replaced + owners, nil
}

// managerRefuses reports a settings or restore record whose manager is not the
// repository's: it applies as nothing, on every node alike.
//
// A REPOSITORY'S MANAGER NEVER CHANGES — a create writes it, and neither a
// tool's decide nor a reconcile's reaches a repository the other manages (the
// tool's is refused config_managed, the reconcile's [ErrNotConfigManaged]) —
// so a record naming the other is one only a race with an activation could
// land, and applied it would hand the repository from one manager to the
// other. The activation fence ignores a tool's record ([fenced]: it orders
// configuration activations, and nothing else carries one), so this, not the
// fence, is what keeps a tool's settings off a declared repository.
func managerRefuses(row versionRow, record ManagedBy) bool { return row.managedBy != record }

// jsonList is a list of keys as the ONE argument an `IN (SELECT value FROM
// json_each(?))` takes — the tracker's idiom for a set of keys, which binds
// one parameter however many keys it names.
func jsonList(keys []string) (string, error) {
	if keys == nil {
		keys = []string{}
	}
	b, err := json.Marshal(keys)
	if err != nil {
		return "", fmt.Errorf("code: encode a key list: %w", err)
	}
	return string(b), nil
}

// The applier's statements over a repository's own row and rules: every one
// with a WHERE is registered in plan_test.go, so the pinned driver's planner
// is held to the primary key each is written for.
const (
	purgedSQL      = `SELECT COUNT(*) FROM code_deletions WHERE repo_id = ?`
	repoVersionSQL = `SELECT version, managed_by FROM code_repos WHERE id = ?`
	// settingsSQL settles a repository's settings, its status only where it
	// is active or archived: an importing repository stays importing until
	// its import is done, and a deleted or purging one stays as it is.
	settingsSQL = `UPDATE code_repos SET
			managed_by = ?, owner_kind = ?, owner = ?, visibility = ?, search = ?, default_branch = ?,
			description = ?, mirror_from = ?, mirror_to = ?,
			status = CASE WHEN status IN ('active', 'archived') THEN ? ELSE status END,
			config_epoch = ?, last_activity_at = ?, version = ?
		WHERE id = ?`
	clearProtectionsSQL = `DELETE FROM code_protections WHERE repo_id = ?`
	clearPathOwnersSQL  = `DELETE FROM code_path_owners WHERE repo_id = ?`
	deleteRepoSQL       = `UPDATE code_repos SET
			status = 'deleted', deleted_at = ?, config_epoch = ?, version = ?
		WHERE id = ? AND status IN ('importing', 'active', 'archived')`
	// restoreRepoSQL brings a deleted repository back: IMPORTING where its
	// create imported and the import is not done — it resumes from what of
	// it landed — and otherwise in the status its settings say.
	restoreRepoSQL = `UPDATE code_repos SET
			managed_by = ?, owner_kind = ?, owner = ?, visibility = ?, search = ?, default_branch = ?,
			description = ?, mirror_from = ?, mirror_to = ?,
			status = CASE WHEN import_source != '' AND imported_at = 0 THEN 'importing' ELSE ? END,
			deleted_at = NULL, config_epoch = ?, last_activity_at = ?, version = ?
		WHERE id = ? AND status = 'deleted'`
	// countKindSQL counts one repository of a kind in, and uncountKindSQL
	// one out: code_repo_kinds, by its key.
	countKindSQL = `INSERT INTO code_repo_kinds (kind, repos) VALUES (?, 1)
		ON CONFLICT (kind) DO UPDATE SET repos = repos + 1`
	uncountKindSQL = `UPDATE code_repo_kinds SET repos = repos - 1 WHERE kind = ?`
	// repoKindSQL is a repository's kind, which its purge counts it out of.
	repoKindSQL = `SELECT kind FROM code_repos WHERE id = ?`
)
