package gitlet

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

// The applier is a PURE FUNCTION of the log: N nodes keep byte-identical
// tables from one stream only while these hold.
//
//  1. NO CONFIG, NO CLOCK: every instant is the broker's
//     ([statelog.ApplyOptions.StoredAt]); every limit is a constant.
//  2. IT WRITES WHAT THE RECORD SAYS: the subject's arbitration guarantees the
//     decide saw these rows, and a second decide here could disagree.
//  3. A GUARD IS A SKIP, NEVER AN ERROR: a redelivery applies as nothing, as
//     a violation would stall the log on every node. Only a record no correct
//     writer forms (arbitrated one object, claims another) is an error.
//  4. EVERY HORIZON IS A FUNCTION OF THE RECORD (its broker instant, its
//     repository's rows, MaxTombstonesPerRepo), never a second-writer sweep.

// Applier writes this node's copy of Gitlet.
//
// It carries NO NODE IDENTITY ([statelog.Applier]): one that knew its node
// could write rows its peers do not; the eviction gate compares
// [statelog.Record.Writer] with the eviction rows instead. Its one tie to its
// node is a listener (internal/inbox's movement.go), which writes no row.
type Applier struct {
	// moved is told, after a committed batch, whose inbox moved on this
	// node. Nil means nobody is listening.
	moved func([]inbox.Movement)

	// inboxed is the notices the batch wrote or removed, filled inside Apply
	// and drained by Committed — see [inbox.Batch] for why it holds no lock.
	inboxed inbox.Batch
}

// NewApplier builds Gitlet's applier for one node. moved is called after a
// COMMITTED batch with every inbox its notices moved (apply_notices.go), never
// inside the transaction: the store re-runs a transiently failed attempt, and
// a movement announced from it would name a notice no row holds.
func NewApplier(moved func([]inbox.Movement)) *Applier { return &Applier{moved: moved} }

// Committed tells THIS node's open dashboards whose inbox the batch's notices
// moved: every data node applies every record and serves its own dashboards.
// Nothing else is told: a seat's wake comes from the code log's feed, and a
// Gitlet node's cache reads the rows when asked.
func (a *Applier) Committed(context.Context) {
	// RESET BEFORE THE CALL: the next batch may be waiting behind a slow listener.
	moved := a.inboxed.Drain(Domain{}.Name())
	if len(moved) > 0 && a.moved != nil {
		a.moved(moved)
	}
}

// Gated reports a record that must produce no rows, read from this
// transaction: the state the record would apply against.
//
//  1. THE EVICTION GATE: a record by a node evicted before its position.
//  2. THE DELETION GATE: a record about a purged repository, for ever, or a
//     late redelivery would resurrect removed rows. Each hit is counted on the
//     marker, so an operator sees a writer nobody stopped. A CHANGE's record
//     is gated by its envelope's scope, never the payload (the apply holds the
//     payload to it), a CHECKS record by its subject ([gatedRepo]).
//
// Eviction first, the knowledge base's order: a record both gates drop is
// dropped either way, so the order decides only which gate COUNTS it.
func (a *Applier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (statelog.Reason, bool, error) {
	if rec.Writer != "" {
		var from, readmitted sql.NullInt64
		err := tx.QueryRowContext(ctx, evictionSQL, rec.Writer).Scan(&from, &readmitted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return "", false, fmt.Errorf("gitlet: read the eviction gate for node %s: %w", rec.Writer, err)
		default:
			if evictedAt(rec.Position.Packed(), from, readmitted) {
				return statelog.ReasonEvicted, true, nil
			}
		}
	}
	repoID, ok := gatedRepo(rec.Subject, rec.Scope)
	if !ok {
		return "", false, nil
	}
	var author sql.NullString
	err := tx.QueryRowContext(ctx, markerSQL, repoID).Scan(&author)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("gitlet: read the deletion gate for repository %s: %w", repoID, err)
	}
	// THE ONE EXCEPTION IS THE RECORD THAT WROTE THE MARKER, by its id, not
	// its op: "any purge" would let a second, row-destroying purge through.
	if author.Valid && author.String == rec.OpID {
		return "", false, nil
	}
	if _, err := tx.ExecContext(ctx, countRejectSQL,
		store.EncodeTime(rec.StoredAt), repoID); err != nil {
		return "", false, fmt.Errorf("gitlet: count a gate hit on the purged repository %s: %w", repoID, err)
	}
	return statelog.ReasonDeleted, true, nil
}

// countRejectSQL counts one record a deletion marker dropped.
const countRejectSQL = `UPDATE code_deletions SET rejects = rejects + 1, last_reject_at = ? WHERE repo_id = ?`

// Apply writes one record's rows and answers how many it wrote or deleted.
//
// The dispatch is on the SUBJECT KIND, every kind with a case (even the two
// that write nothing), so a kind added without one is an error rather than a
// silently ignored record.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	record, err := Decode(rec.Payload)
	if err != nil {
		return 0, fmt.Errorf("gitlet: decode the record at %s: %w", rec.Position, err)
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
		// THE TRIM'S OWN RECORD: the framework removes the anchors below its
		// floor in this transaction (ADR-0080).
		return 0, nil
	case KindEviction:
		return a.applyEviction(ctx, tx, at)
	case KindGeneration:
		return a.applyGeneration(ctx, tx, at)
	case KindCompany:
		return a.applyActivation(ctx, tx, at)
	case KindName:
		// A NAME'S SUBJECT carries a create, a rename onto it, and the
		// release of an old name whose redirect ended.
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
		if record.Op == OpViewed {
			return a.applyViewed(ctx, tx, at)
		}
		return a.applyMarks(ctx, tx, at)
	}
	// AN UNKNOWN KIND at a version this build reads is a writer publishing a
	// kind it never declared; failing makes that mistake visible.
	return 0, fmt.Errorf("gitlet: %s is not a kind this build applies, and the "+
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

	// brokerAt is the broker's instant for this record, the ONLY instant a
	// record's own apply writes.
	brokerAt time.Time

	// now is the batch's instant and epoch the company's settings it applies
	// under: only a notice's horizon reads them, the one DIVERGENT table
	// these records write (apply_notices.go).
	now   time.Time
	epoch map[string]any

	// maxVariables is the engine's probed bind-parameter limit, which sizes
	// a multi-row INSERT and decides nothing about which rows land.
	maxVariables int
}

// at is the broker's instant as the store encodes it.
func (c applyContext) at() int64 { return store.EncodeTime(c.brokerAt) }

// applyEviction records a node's removal from this log, or its readmission as
// an INVERSE COMMIT rather than a delete, so the history survives a replay.
func (a *Applier) applyEviction(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	e, ok := payload.(Eviction)
	if !ok {
		return 0, fmt.Errorf("gitlet: the eviction at %s carries a %T", at.position, payload)
	}
	if e.NodeID == "" {
		return 0, fmt.Errorf("gitlet: the eviction at %s names no node", at.position)
	}
	if e.Readmitted {
		res, execErr := tx.ExecContext(ctx, readmitSQL, at.packed, at.packed, e.NodeID, at.packed)
		if execErr != nil {
			return 0, fmt.Errorf("gitlet: readmit node %s at %s: %w", e.NodeID, at.position, execErr)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	// A SECOND EVICTION CLEARS THE READMISSION, so evicted-readmitted-evicted
	// reads as three facts rather than one window with a hole in it.
	res, err := tx.ExecContext(ctx, evictSQL,
		e.NodeID, at.at(), e.EvictedBy, at.packed, at.packed)
	if err != nil {
		return 0, fmt.Errorf("gitlet: evict node %s at %s: %w", e.NodeID, at.position, err)
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
		return 0, fmt.Errorf("gitlet: the generation record at %s carries a %T", at.position, payload)
	}
	res, err := tx.ExecContext(ctx, generationSQL,
		g.Generation, at.at(), g.By, store.EncodeTime(g.StreamCreatedAt),
		int64(g.PrevHighest), at.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: record generation %d at %s: %w", g.Generation, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// generationSQL is a reanchor's audit row.
const generationSQL = `INSERT INTO code_log_generations
		(generation, at, by, new_stream_created_at, prev_last_seq_seen, record_id)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT (generation) DO NOTHING`

// applyActivation raises the activation fence to the record's activation and
// never lowers it, so an older activation applied late leaves it alone.
//
// Why it is safe: a reconcile publishes its fence record FIRST and decides
// from a snapshot taken after it applied (its append's session floor). A
// config-managed record of an OLDER activation is either earlier in the log,
// so in the rows the reconcile decides from and written back, or later, so
// refused on every node ([fenced]). An activation that changes nothing writes
// only this record, not one per declared repository.
func (a *Applier) applyActivation(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	if at.record.Subject.ID != activationID {
		return 0, fmt.Errorf("gitlet: the activation record at %s arbitrated %s, and "+
			"the fence is %s — a record on another company subject is not one this "+
			"build writes", at.position, at.record.Subject, CompanySubject())
	}
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	act, ok := payload.(Activated)
	if !ok {
		return 0, fmt.Errorf("gitlet: the activation record at %s carries a %T", at.position, payload)
	}
	res, err := tx.ExecContext(ctx, raiseFenceSQL, activationID, act.ConfigEpoch, at.packed)
	if err != nil {
		return 0, fmt.Errorf("gitlet: raise the activation fence to %d at %s: %w",
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

// fenced reports a config-managed record decided below the fence this
// transaction holds, which applies as nothing on every node. Only a
// CONFIG-MANAGED record carries an activation, so a tool's ([ManagedTools],
// activation 0) applies at any fence; [managerRefuses] keeps it off a
// repository the configuration manages.
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
		return false, fmt.Errorf("gitlet: read the activation fence: %w", err)
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
		return 0, fmt.Errorf("gitlet: the create at %s carries a %T", at.position, payload)
	}
	// THE NAME IS THE ADDRESS: a record that arbitrated one name and claims
	// another would take a name nobody contended for.
	if token := NameToken(p.Name); token != at.record.Subject.ID {
		return 0, fmt.Errorf("gitlet: the create at %s arbitrated name %s and its "+
			"payload names %q, whose token is %s", at.position, at.record.Subject.ID, p.Name, token)
	}
	if p.RepoID == "" || !ValidName(p.Name) {
		return 0, fmt.Errorf("gitlet: the create at %s names repository %q as %q, "+
			"which no writer forms", at.position, p.RepoID, p.Name)
	}
	// A PURGED ID IS NEVER REUSED, so its create is a redelivery. The name
	// subject is not under the repository's marker, so the create checks it.
	var purged int
	if err = tx.QueryRowContext(ctx, purgedSQL, p.RepoID).Scan(&purged); err != nil {
		return 0, fmt.Errorf("gitlet: read the deletion marker of %s: %w", p.RepoID, err)
	}
	if purged > 0 {
		return 0, nil
	}
	// A HELD NAME stays held: the broker arbitrated it, so a second create is
	// a redelivery, and refusing it would stall every node.
	var held int
	if err = tx.QueryRowContext(ctx, guardNameSQL, at.record.Subject.ID).Scan(&held); err != nil {
		return 0, fmt.Errorf("gitlet: read the claim on %q: %w", p.Name, err)
	}
	if held > 0 {
		return 0, nil
	}
	if refused, fenceErr := fenced(ctx, tx, p.Settings.ManagedBy, p.Settings.ConfigEpoch); fenceErr != nil || refused {
		return 0, fenceErr
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO code_names (token, name, repo_id) VALUES (?, ?, ?)`,
		at.record.Subject.ID, p.Name, p.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: claim %q at %s: %w", p.Name, at.position, err)
	}
	s := p.Settings
	// WHO MADE IT: the record's decider, nobody for a declaration (the
	// configuration made it). Its idle clock starts at its creation.
	createdBy, createdKind := at.record.Actor, string(at.record.ActorKind)
	if at.record.ActorKind == ActorConfig {
		createdBy = ""
	}
	// A create that imports (its own, or a pull mirror's first fetch) makes
	// the repository importing ([CreateRepo.status]).
	from, to, imports, err := remoteColumns(s, p.importSource())
	if err != nil {
		return 0, fmt.Errorf("gitlet: the create at %s: %w", at.position, err)
	}
	ownersDigest, humanPathsDigest := rowDigests(s)
	res, err := tx.ExecContext(ctx, `INSERT INTO code_repos
			(id, name, status, managed_by, kind, owner_kind, owner, visibility, search, default_branch,
			 description, config_epoch, created_at, created_by, created_by_kind, last_activity_at, version,
			 mirror_from, mirror_to, import_source, owners_digest, human_paths_digest, protected)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		p.RepoID, p.Name, string(p.status()), string(s.ManagedBy), string(s.Kind), string(s.Owner.Kind),
		s.Owner.Ref, string(s.Visibility), string(s.Search), s.DefaultBranch, s.Description, s.ConfigEpoch,
		at.at(), createdBy, createdKind, at.at(), at.packed, from, to, imports, ownersDigest, humanPathsDigest,
		rowProtected(s))
	if err != nil {
		return 0, fmt.Errorf("gitlet: create repository %s at %s: %w", p.RepoID, at.position, err)
	}
	created, _ := res.RowsAffected()
	// COUNTED IN BY KIND only where this record made the row: repo_limit
	// holds a tool's create of a project to it, and the purge counts it out.
	// Its repository tallies start empty: a new repository holds no change.
	tallied := 0
	if created > 0 {
		if _, err = tx.ExecContext(ctx, countKindSQL, string(s.Kind)); err != nil {
			return 0, fmt.Errorf("gitlet: count repository %s in at %s: %w", p.RepoID, at.position, err)
		}
		if tallied, err = retally(ctx, tx, p.RepoID, tallyStanding{}); err != nil {
			return 0, err
		}
	}
	// EVERY RULE A CREATE WRITES IS ITS OWN: no earlier row carries a source.
	protections, err := insertProtections(ctx, tx, at.maxVariables, p.RepoID, s.Protections,
		at.sourcesOf(len(s.Protections)))
	if err != nil {
		return 0, fmt.Errorf("gitlet: write %s's protections at %s: %w", p.RepoID, at.position, err)
	}
	owners, err := insertPathOwners(ctx, tx, at.maxVariables, p.RepoID, s.PathOwners)
	if err != nil {
		return 0, fmt.Errorf("gitlet: write %s's path owners at %s: %w", p.RepoID, at.position, err)
	}
	return 1 + 2*int(created) + tallied + protections + owners, nil
}

// rowDigests is the digests a repository's row carries of the settings s
// writes beside it: its path owners' ([OwnersDigest]) and its rules' human
// paths' ([HumanPathsDigest]). The merge gate and a revise's decide read the
// columns rather than hash rows, and the home's due listing compares them
// with the digests a revision's facts were matched against.
//
// THEY RIDE THE STATEMENT THAT WRITES THE ROW (create, settings, restore):
// the rules and the row land in one transaction, so no reader holds digests
// without their rows, and a statement of their own would rewrite the row and
// cost two rows more of the apply budget ([reconcileRecordRows]).
func rowDigests(s RepoSettings) (owners, humanPaths string) {
	return OwnersDigest(s.PathOwners), HumanPathsDigest(ruleGatesOf(s.Protections))
}

// rowProtected is code_repos.protected (1 or 0): whether a rule protects the
// repository, whose index the protected listing walks ([ReadQuery.Protected],
// [protectedReposKey]). It rides the row's statement, for [rowDigests]' reason.
func rowProtected(s RepoSettings) int {
	if len(s.Protections) > 0 {
		return 1
	}
	return 0
}

// insertPathOwners writes a repository's path owners, one row an owner, its
// globs packed ([packPaths]); their digest rides the row ([rowDigests]). An
// owner named twice, which no writer forms, keeps its first row rather than
// aborting the transaction on every node.
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

// packPaths and unpackPaths are a path owner's globs as its row holds them,
// one a line in order: a glob holds no control character
// (gitref.ParsePathGlob), so the packing is exact both ways.
func packPaths(paths []string) string { return strings.Join(paths, "\n") }

func unpackPaths(packed string) []string { return strings.Split(packed, "\n") }

// insertProtections writes a repository's rules in the record's order, each
// with its gate ([gateColumn]) and the source at its index ([ruleSource]);
// their human paths' digest rides the row ([rowDigests]; humanpaths.go says
// who reads it). A pattern named twice, which no writer forms, keeps its
// first rule rather than aborting the transaction on every node.
func insertProtections(ctx context.Context, tx *sql.Tx, maxVariables int, repoID string,
	rules []Protection, sources []ruleSource) (int, error) {

	if len(sources) != len(rules) {
		return 0, fmt.Errorf("gitlet: write %s's protections: %d rules and %d sources", repoID, len(rules),
			len(sources))
	}

	gates := make([]string, len(rules))
	for i, r := range rules {
		var err error
		if gates[i], err = gateColumn(r.Gate); err != nil {
			return 0, err
		}
	}
	return store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO code_protections (repo_id, pattern, allow_force, allow_delete, direct_push, gate,
			set_by, set_by_kind, set_at) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?)`, `ON CONFLICT (repo_id, pattern) DO NOTHING`, len(rules),
		func(i int) []any {
			r, src := rules[i], sources[i]
			return []any{repoID, r.Pattern, r.AllowForce, r.AllowDelete, string(r.DirectPush), gates[i],
				src.by, string(src.kind), src.at}
		})
}

// ruleSource is who set one rule and when (code_protections' set_by,
// set_by_kind, set_at; replicated 0148): the envelope's decider of the record
// that last CHANGED it, nobody for code.repos' reconcile, and that record's
// broker instant as the store encodes it.
type ruleSource struct {
	by   string
	kind ActorKind
	at   int64
}

// source is the record's own source for a rule it sets: its decider (nobody
// for a declaration, as created_by) and its broker instant.
func (c applyContext) source() ruleSource {
	by := c.record.Actor
	if c.record.ActorKind == ActorConfig {
		by = ""
	}
	return ruleSource{by: by, kind: c.record.ActorKind, at: c.at()}
}

// sourcesOf is the record's own source for each of n rules.
func (c applyContext) sourcesOf(n int) []ruleSource {
	out := make([]ruleSource, n)
	for i := range out {
		out[i] = c.source()
	}
	return out
}

// rulesSourced is the source of each rule a record writes over a
// repository's rules (a settings change, a restore): the replaced row's where
// it holds the SAME rule ([Protection.Equal]), the record's own otherwise.
//
// CARRIED, NOT RESTAMPED: a record carries every rule whole, so restamping
// would name, for each rule it did not touch, somebody who never set it. The
// rows are read in the record's own transaction, so the source stays a pure
// function of the record and the rows before it, and a replay forms it again.
func rulesSourced(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	rules []Protection) ([]ruleSource, error) {

	held, err := sourcedRulesIn(ctx, tx, repoID)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %s's protections at %s: %w", repoID, at.position, err)
	}
	out := make([]ruleSource, len(rules))
	for i, r := range rules {
		if was, ok := held[r.Pattern]; ok && was.rule.Equal(r) {
			out[i] = was.source
			continue
		}
		out[i] = at.source()
	}
	return out, nil
}

// sourcedRule is one rule as its row holds it, with its source.
type sourcedRule struct {
	rule   Protection
	source ruleSource
}

// sourcedRulesIn reads a repository's rules whole, with gate and source, by
// pattern, for [rulesSourced].
func sourcedRulesIn(ctx context.Context, tx *sql.Tx, repoID string) (map[string]sourcedRule, error) {
	rows, err := tx.QueryContext(ctx, sourcedRulesSQL, repoID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]sourcedRule{}
	for rows.Next() {
		var (
			r            sourcedRule
			direct, gate string
			kind         string
		)
		if err = rows.Scan(&r.rule.Pattern, &r.rule.AllowForce, &r.rule.AllowDelete, &direct, &gate,
			&r.source.by, &kind, &r.source.at); err != nil {
			return nil, err
		}
		r.rule.DirectPush, r.source.kind = DirectPush(direct), ActorKind(kind)
		if r.rule.Gate, err = gateFrom(gate); err != nil {
			return nil, fmt.Errorf("rule %s: %w", r.rule.Pattern, err)
		}
		out[r.rule.Pattern] = r
	}
	return out, rows.Err()
}

// applyRepo is every record on a repository's own subject.
//
// THE SUBJECT GUARD: the payload's id must be the subject's, which makes the
// subject the arbitration unit rather than a label.
//
// THE VERSION GUARD (all but the purge, the marker's own): under a strict
// replay the create is below this position, so a missing row is an error; a
// row at or past it marks a redelivery, applied as nothing.
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
	case ExpungeScan:
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
	case CloseSettled:
		payloadRepo = p.RepoID
	case QueueSweep:
		payloadRepo = p.RepoID
	case ViewedKeyRecord:
		payloadRepo = p.RepoID
	default:
		return 0, fmt.Errorf("gitlet: %s is not an op this build applies to a repository "+
			"(the record at %s)", at.record.Op, at.position)
	}
	if payloadRepo != id {
		return 0, fmt.Errorf("gitlet: the record at %s arbitrated repo.%s and its payload "+
			"names %s — a record on one repository's subject may change only that "+
			"repository", at.position, id, payloadRepo)
	}
	if p, ok := payload.(Purge); ok {
		return a.applyPurge(ctx, tx, at, p)
	}
	row, err := repoVersion(ctx, tx, id)
	if err != nil {
		return 0, fmt.Errorf("gitlet: the %s at %s: %w", at.record.Op, at.position, err)
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
	case ExpungeScan:
		return a.applyExpungeScan(ctx, tx, at, p)
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
		n, err := a.applyMerge(ctx, tx, at, p)
		return a.noticed(ctx, tx, at, id, payload, n, err)
	case Enqueue:
		return a.applyEnqueue(ctx, tx, at, p)
	case Dequeue:
		n, err := a.applyDequeue(ctx, tx, at, p)
		return a.noticed(ctx, tx, at, id, payload, n, err)
	case Candidate:
		return a.applyCandidate(ctx, tx, at, p)
	case Eject:
		n, err := a.applyEject(ctx, tx, at, p)
		return a.noticed(ctx, tx, at, id, payload, n, err)
	case Land:
		n, err := a.applyLand(ctx, tx, at, p)
		return a.noticed(ctx, tx, at, id, payload, n, err)
	case Green:
		return a.applyGreen(ctx, tx, at, p)
	case CloseSettled:
		n, err := a.applyCloseSettled(ctx, tx, at, p)
		return a.noticed(ctx, tx, at, id, payload, n, err)
	case Broken:
		n, err := a.applyBroken(ctx, tx, at, p)
		return a.noticed(ctx, tx, at, id, payload, n, err)
	case QueueSweep:
		return a.applyQueueSweep(ctx, tx, at, p)
	case ViewedKeyRecord:
		return a.applyViewedKey(ctx, tx, at, p)
	}
	return 0, fmt.Errorf("gitlet: the record at %s carries a %T", at.position, payload)
}

// noticed writes a record's notices beside its n applied rows
// ([Applier.writeNotices]), none where the apply wrote nothing: a redelivery,
// whose first apply wrote them, or a guard's skip.
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

// repoVersion reads a repository's guard, refusing a missing row: under a
// strict replay its create precedes every record about it, and a PURGED one's
// records were already dropped by its marker.
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

// applySettings replaces a repository's settings whole and moves its idle
// clock. A deleted or purging repository keeps its status: only a restore
// resurrects one. An ACTIVE repository made a pull mirror of a source it does
// not follow yet becomes IMPORTING, its import begun afresh (mirrorImportSQL).
//
// A record of another manager than the repository's applies as nothing
// ([managerRefuses]), as one below the activation fence does.
func (a *Applier) applySettings(ctx context.Context, tx *sql.Tx, at applyContext, p SettingsRecord,
	row versionRow) (int, error) {

	s := p.Settings
	if managerRefuses(row, s.ManagedBy) {
		return 0, nil
	}
	if refused, err := fenced(ctx, tx, s.ManagedBy, s.ConfigEpoch); err != nil || refused {
		return 0, err
	}
	// ITS CHANGE TALLIES before, which its owner and visibility move.
	before, err := tallyStandingIn(ctx, tx, p.RepoID)
	if err != nil {
		return 0, err
	}
	// THE IMPORT A PULL MIRROR STARTS, read off the mirrors held BEFORE the
	// record: a source not followed yet is imported first (M6-D16,
	// [repoRemotes.startsImport]).
	prior, err := remotesIn(ctx, tx, p.RepoID)
	if err != nil {
		return 0, err
	}
	from, to, imports, err := remoteColumns(s, prior.startsImport(s))
	if err != nil {
		return 0, fmt.Errorf("gitlet: the settings at %s: %w", at.position, err)
	}
	ownersDigest, humanPathsDigest := rowDigests(s)
	res, err := tx.ExecContext(ctx, settingsSQL,
		string(s.ManagedBy), string(s.Owner.Kind), s.Owner.Ref, string(s.Visibility), string(s.Search),
		s.DefaultBranch, s.Description, from, to, ownersDigest, humanPathsDigest, rowProtected(s),
		string(s.status()), s.ConfigEpoch, at.at(), at.packed, p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: settle %s's settings at %s: %w", p.RepoID, at.position, err)
	}
	n, _ := res.RowsAffected()
	if imports != "" {
		// The same row the statement above wrote, so no row of its own.
		if _, err = tx.ExecContext(ctx, mirrorImportSQL, imports, p.RepoID); err != nil {
			return 0, fmt.Errorf("gitlet: start %s's import from its pull mirror's source at %s: %w", p.RepoID,
				at.position, err)
		}
	}
	replaced, err := replaceProtections(ctx, tx, at, p.RepoID, s.Protections)
	if err != nil {
		return 0, err
	}
	owners, err := replacePathOwners(ctx, tx, at, p.RepoID, s.PathOwners)
	if err != nil {
		return 0, err
	}
	moved, err := retally(ctx, tx, p.RepoID, before)
	if err != nil {
		return 0, err
	}
	return int(n) + replaced + owners + moved, nil
}

// replacePathOwners deletes a repository's path owners and writes the new set:
// at most MaxPathOwners rows each way.
func replacePathOwners(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	owners []PathOwner) (int, error) {

	res, err := tx.ExecContext(ctx, clearPathOwnersSQL, repoID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: clear %s's path owners at %s: %w", repoID, at.position, err)
	}
	removed, _ := res.RowsAffected()
	written, err := insertPathOwners(ctx, tx, at.maxVariables, repoID, owners)
	if err != nil {
		return 0, fmt.Errorf("gitlet: write %s's path owners at %s: %w", repoID, at.position, err)
	}
	return int(removed) + written, nil
}

// replaceProtections deletes a repository's rules and writes the new set,
// each rule it carries unchanged keeping who set it ([rulesSourced]).
func replaceProtections(ctx context.Context, tx *sql.Tx, at applyContext, repoID string,
	rules []Protection) (int, error) {

	sources, err := rulesSourced(ctx, tx, at, repoID, rules)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, clearProtectionsSQL, repoID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: clear %s's protections at %s: %w", repoID, at.position, err)
	}
	removed, _ := res.RowsAffected()
	written, err := insertProtections(ctx, tx, at.maxVariables, repoID, rules, sources)
	if err != nil {
		return 0, fmt.Errorf("gitlet: write %s's protections at %s: %w", repoID, at.position, err)
	}
	return int(removed) + written, nil
}

// applyDelete marks a repository deleted. A delete of a repository already
// deleted or purging changes nothing and is applied completely as a no-op.
func (a *Applier) applyDelete(ctx context.Context, tx *sql.Tx, at applyContext, p LifecycleRecord,
	row versionRow) (int, error) {

	// A DELETE CARRIES NO SETTINGS, so the repository's own manager decides
	// whether the fence governs it.
	if refused, err := fenced(ctx, tx, row.managedBy, p.ConfigEpoch); err != nil || refused {
		return 0, err
	}
	// ITS CHANGES LEAVE THE TALLIES with it: a deleted repository is in no use.
	before, err := tallyStandingIn(ctx, tx, p.RepoID)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, deleteRepoSQL,
		at.at(), p.ConfigEpoch, at.packed, p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: delete %s at %s: %w", p.RepoID, at.position, err)
	}
	n, _ := res.RowsAffected()
	moved, err := retally(ctx, tx, p.RepoID, before)
	if err != nil {
		return 0, err
	}
	return int(n) + moved, nil
}

// applyRestore brings a deleted repository back with the settings the restore
// carries, and moves its idle clock; its refs and packs were never touched. A
// restore of another manager than the repository's applies as nothing
// ([managerRefuses]).
func (a *Applier) applyRestore(ctx context.Context, tx *sql.Tx, at applyContext, p LifecycleRecord,
	row versionRow) (int, error) {

	if p.Settings == nil {
		return 0, fmt.Errorf("gitlet: the restore at %s carries no settings — a restore "+
			"states what the repository comes back with", at.position)
	}
	s := *p.Settings
	if managerRefuses(row, s.ManagedBy) {
		return 0, nil
	}
	if refused, err := fenced(ctx, tx, s.ManagedBy, p.ConfigEpoch); err != nil || refused {
		return 0, err
	}
	// ITS CHANGES RETURN TO THE TALLIES with it, under the owner it returns with.
	before, err := tallyStandingIn(ctx, tx, p.RepoID)
	if err != nil {
		return 0, err
	}
	from, to, _, err := remoteColumns(s, nil)
	if err != nil {
		return 0, fmt.Errorf("gitlet: the restore at %s: %w", at.position, err)
	}
	ownersDigest, humanPathsDigest := rowDigests(s)
	res, err := tx.ExecContext(ctx, restoreRepoSQL,
		string(s.ManagedBy), string(s.Owner.Kind), s.Owner.Ref, string(s.Visibility), string(s.Search),
		s.DefaultBranch, s.Description, from, to, ownersDigest, humanPathsDigest, rowProtected(s),
		string(s.status()), p.ConfigEpoch, at.at(), at.packed, p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: restore %s at %s: %w", p.RepoID, at.position, err)
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
	moved, err := retally(ctx, tx, p.RepoID, before)
	if err != nil {
		return 0, err
	}
	return int(n) + replaced + owners + moved, nil
}

// managerRefuses reports a settings or restore record whose manager is not
// the repository's, which applies as nothing on every node.
//
// A REPOSITORY'S MANAGER NEVER CHANGES: neither decide reaches a repository
// the other manages (config_managed, [ErrNotConfigManaged]), so such a record
// lands only by racing an activation, and applying it would hand the
// repository over. The fence ignores a tool's record ([fenced]), so this is
// what keeps a tool's settings off a declared repository.
func managerRefuses(row versionRow, record ManagedBy) bool { return row.managedBy != record }

// jsonList is a list of keys as the ONE argument an `IN (SELECT value FROM
// json_each(?))` takes: one bind parameter however many keys.
func jsonList(keys []string) (string, error) {
	if keys == nil {
		keys = []string{}
	}
	b, err := json.Marshal(keys)
	if err != nil {
		return "", fmt.Errorf("gitlet: encode a key list: %w", err)
	}
	return string(b), nil
}

// The applier's statements over a repository's own row and rules: every one
// with a WHERE is registered in plan_test.go, so the pinned driver's planner
// is held to the primary key each is written for.
const (
	purgedSQL      = `SELECT COUNT(*) FROM code_deletions WHERE repo_id = ?`
	repoVersionSQL = `SELECT version, managed_by FROM code_repos WHERE id = ?`
	// settingsSQL settles a repository's settings, its status only where
	// active or archived (an importing one stays so until done, a deleted or
	// purging one as it is), with [rowDigests] and [rowProtected].
	settingsSQL = `UPDATE code_repos SET
			managed_by = ?, owner_kind = ?, owner = ?, visibility = ?, search = ?, default_branch = ?,
			description = ?, mirror_from = ?, mirror_to = ?, owners_digest = ?, human_paths_digest = ?,
			protected = ?, status = CASE WHEN status IN ('active', 'archived') THEN ? ELSE status END,
			config_epoch = ?, last_activity_at = ?, version = ?
		WHERE id = ?`
	// mirrorImportSQL starts the import a pull mirror owes
	// ([repoRemotes.startsImport]) on a repository settingsSQL left ACTIVE,
	// as a create that imports leaves the row. An archived one is never a
	// pull mirror, a deleted or purging one stays, and one already importing
	// keeps its source: what moved meanwhile is its first sync's.
	mirrorImportSQL = `UPDATE code_repos SET
			status = 'importing', import_source = ?, import_seq = 0, imported_at = 0, import_error = ''
		WHERE id = ? AND status = 'active'`
	clearProtectionsSQL = `DELETE FROM code_protections WHERE repo_id = ?`
	// sourcedRulesSQL is a repository's rules as the record replacing them
	// compares them, each with who set it ([sourcedRulesIn]).
	sourcedRulesSQL = `SELECT pattern, allow_force, allow_delete, direct_push, gate, set_by, set_by_kind,
			set_at FROM code_protections WHERE repo_id = ?`
	clearPathOwnersSQL = `DELETE FROM code_path_owners WHERE repo_id = ?`
	deleteRepoSQL      = `UPDATE code_repos SET
			status = 'deleted', deleted_at = ?, config_epoch = ?, version = ?
		WHERE id = ? AND status IN ('importing', 'active', 'archived')`
	// restoreRepoSQL brings a deleted repository back: IMPORTING where its
	// create's import is not done (it resumes from what landed), otherwise in
	// the status its settings say, with [rowDigests] and [rowProtected].
	restoreRepoSQL = `UPDATE code_repos SET
			managed_by = ?, owner_kind = ?, owner = ?, visibility = ?, search = ?, default_branch = ?,
			description = ?, mirror_from = ?, mirror_to = ?, owners_digest = ?, human_paths_digest = ?,
			protected = ?, status = CASE WHEN import_source != '' AND imported_at = 0 THEN 'importing' ELSE ? END,
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
