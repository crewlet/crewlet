package code

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

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN EXPUNGE, DECIDED: the one rewrite that drops objects, in two records of
// their own on a repository's subject — its BEGIN ([ExpungeBegin]) and its
// COMMIT ([ExpungeCommit]) — beside a third that only asks for one, a person's
// REQUEST ([ExpungeRequest]). Each is proposed to the repository's committer
// ([Committer.ProposeExpunge]) and decided by it in ONE snapshot, alone, as a
// compaction is: everything that moves a repository's packs has one writer,
// and contends on one subject.
//
// # Two records, so nothing starves the drop
//
// The BEGIN rewrites some of the repository's live packs — its inputs, chosen
// by the repository's maintainer, a full pack among them — into packs holding
// what a ref reached at the maintainer's scan, which stay live, and packs
// holding the rest, which are DROPPING ([PackDropping]); and it moves the
// repository's DROP EPOCH. Its outputs hold every object of its inputs, so as
// far as any reader is concerned it is a merge: a cache serves a dropping pack
// beside the live ones. What changes is what a NEW push may rely on: a
// receiver's view of the repository leaves the dropping packs out, and a push
// whose receiver read the repository at an earlier epoch is refused
// `expunging` by its committer, since its objects were checked against packs
// the begin may since have made dropping. The COMMIT then retires the dropping
// packs, keeping as a live RESCUE whatever of them a push landed between the
// scan and the begin made reachable again; its one precondition is that its
// expunge is still open, which no push falsifies, so a repository pushed to
// all day still commits. One record with a precondition on the refs would be
// refused again by every push that landed while it was built. A purged
// object leaves the store a [RetireGrace] after the commit, once the sweep of
// the retired rows and the collector's next pass take it.
//
// # What the decide holds each to
//
// Against the rows of one snapshot, in this order; the first rule that fails
// is the answer, and none of them writes anything. Every record's first rule
// is the repository: absent, deleted or being purged is repo_deleted, in the
// sentence of the state it is in ([RepoGone]); archived is decided, since an
// archived repository still holds what was pushed to it.
//
// A REQUEST then meets no rule of its own: it forms its record whatever the
// repository holds open, and its apply WIDENS a request already open to its
// own id and position ([Applier.applyExpungeRequest]). The open request cannot
// stand in for it: it is served once nothing that dates from before ITS
// position is left ([PackRow.Content]), and a later request asks about more —
// a pack pushed between the two, such as the push that leaked what the later
// person is purging, and whatever a ref dropped between them left, which a
// cycle whose scan read the refs before the later request kept as reachable.
// A request asked again under its own id is the same request, collapsed by
// the operation ledger.
//
// A BEGIN:
//
//  1. no expunge open — else stale: a repository expunges once at a time;
//  2. its epoch the row's plus one — else stale: another expunge began since
//     the plan;
//  3. every input exactly one row, of part `pack`, live ([liveInput]) — full
//     or not — else stale, naming the first that is not: a compaction, an
//     expunge or a purge moved it first;
//  4. the layers it drops ([ExpungeBegin.DropLayers]) the chain's top from the
//     lowest layer naming an input, in chain order — else stale: a
//     publication moved the chain since the plan;
//  5. every output a bundle [PackRef.Check] admits — else pack_too_large —
//     whose key was minted within objstore.RecordWithin of the decision —
//     else upload_expired ([uploadExpired], the push's own rule);
//  6. the arithmetic: its outputs hold no more objects than its inputs' rows
//     count — else the decision is an error ([ErrInvalidProposal]): a builder
//     that invented objects is a bug, refused before any record and logged.
//
// A COMMIT:
//
//  1. its expunge the open one — else stale: it committed already;
//  2. its epoch the repository's — else stale;
//  3. the packs it retires exactly the repository's dropping packs, every one
//     — else stale;
//  4. every rescue pack held to the begin's rule 5;
//  5. the arithmetic: its rescue holds no more objects than the dropping
//     packs' rows count — else [ErrInvalidProposal].
//
// NO REF IS READ, and no protection and no push: a request costs the
// repository's row; a begin the row, a seek of the key an input and the
// published chain; a commit the row and the repository's own pack rows. The
// rows of two snapshots are never paired.

// Validate refuses a proposal no committer could decide ([ErrInvalidProposal],
// naming the field), before it is queued: anything but exactly one of a
// request, a begin and a commit; a repository id that is no repository's in
// its canonical spelling; a request whose id is no operation id this engine
// accepts (statelog.CheckCallerOpID), or whose asker is no person bound to a
// seat; a begin or a commit whose own id is no bare UUIDv7, whose node config
// does not admit, whose Seen is no position on the code log or whose epoch is
// below one, and whose request is "" or an operation id; a begin whose grace
// is not PruneGrace on its own schedule and 0 serving a request, whose inputs
// are not sorted and distinct or number none or past MaxExpungeInputs, whose
// outputs number none or past MaxExpungeOutputs, or whose layers to drop are
// not distinct layer ids or number past a chain; a commit whose expunge id is
// no bare UUIDv7, or whose retirements are not sorted and distinct or number,
// as its rescue may, past MaxExpungeOutputs; an output whose pack id is not
// its derivation ([DeriveOutputPackID]) or whose bundle [PackRef.Check]
// refuses; and a proposal past what one record may carry. What the inputs, the
// chain and the dropping packs hold is the decide's to judge, from the rows.
func (p ExpungeProposal) Validate() error {
	_, err := p.measure()
	return err
}

// ValidateUnstored is [ExpungeProposal.Validate] for a begin or a commit whose
// outputs are laid out and not yet stored: each with no key and no digest, as
// a build lays them out, and judged as it will be once its put has filled them
// in — every rule Validate holds a stored proposal to, the record's bytes
// among them, with each output standing at the widest key and digest a store
// mints. What the maintainer that built one holds it to before a byte of it is
// uploaded (internal/code/compact), so a builder's own bug is refused before
// it costs the store anything. An output already stored, and a request, which
// stores nothing, are refused: Validate is the check of those.
func (p ExpungeProposal) ValidateUnstored() error {
	stand := largestEntry().Pack
	unstored := func(field string, outs []PackRef) ([]PackRef, error) {
		filled := make([]PackRef, len(outs))
		for i, out := range outs {
			if !out.Object.IsZero() || out.Hash != "" {
				return nil, fmt.Errorf("%w: %s[%d]: already stored as %s — Validate checks a stored output",
					ErrInvalidProposal, field, i, out.Object)
			}
			out.Object, out.Hash = stand.Object, stand.Hash
			filled[i] = out
		}
		return filled, nil
	}
	var err error
	switch {
	case p.Request != nil:
		return fmt.Errorf("%w: a request stores nothing — Validate checks it", ErrInvalidProposal)
	case p.Begin != nil && p.Commit == nil:
		b := *p.Begin
		if b.Add, err = unstored("begin.add", b.Add); err != nil {
			return err
		}
		if b.Dropping, err = unstored("begin.dropping", b.Dropping); err != nil {
			return err
		}
		p.Begin = &b
	case p.Commit != nil && p.Begin == nil:
		c := *p.Commit
		if c.Add, err = unstored("commit.add", c.Add); err != nil {
			return err
		}
		p.Commit = &c
	}
	return p.Validate()
}

// measure validates p and answers its queued byte charge: the payload its
// record carries, encoded, held under [EntryRoom] — what one proposal may take
// of a record beside the record's own overhead.
func (p ExpungeProposal) measure() (int, error) {
	if err := p.validFields(); err != nil {
		return 0, err
	}
	op, _, _, payload := p.record()
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: encode it: %w", ErrInvalidProposal, op, err)
	}
	if len(encoded) > EntryRoom {
		return 0, fmt.Errorf("%w: %s: %d bytes, past the %d one proposal may take of its record",
			ErrInvalidProposal, op, len(encoded), EntryRoom)
	}
	return len(encoded), nil
}

// record is the record p is: its op, its operation id — the set field's own
// id — its session floor, and its payload. p is one of the three, which
// [ExpungeProposal.validFields] holds it to before anything reads this.
func (p ExpungeProposal) record() (OpKind, string, statelog.Position, any) {
	switch {
	case p.Begin != nil:
		return OpExpungeBegin, p.Begin.ExpungeID, p.Begin.Seen, *p.Begin
	case p.Commit != nil:
		return OpExpungeCommit, p.Commit.CommitID, p.Commit.Seen, *p.Commit
	case p.Request != nil:
		return OpExpungeRequest, p.Request.RequestID, statelog.Position{}, *p.Request
	}
	return "", "", statelog.Position{}, nil
}

// RepoID is the repository p is about, by whichever of its three records it
// carries — "" for one that carries none, which the estate's router refuses
// as it refuses a push naming no repository. It is the ONE answer the router
// keys p by, the committer queues it under and the estate's seam raises the
// repository's floor for, so the three never name different repositories.
func (p ExpungeProposal) RepoID() string {
	switch {
	case p.Begin != nil:
		return p.Begin.RepoID
	case p.Commit != nil:
		return p.Commit.RepoID
	case p.Request != nil:
		return p.Request.RepoID
	}
	return ""
}

// again is what p's asker does after a refusal that decided nothing: a person
// asks for the purge again, and a maintainer's next pass reads the rows and
// plans again.
func (p ExpungeProposal) again() string {
	if p.Request != nil {
		return "ask for the purge again"
	}
	return "the next pass plans again"
}

// decidedRows is the most rows p's record makes its apply touch: a request's
// one, the repository's row; a begin's [expungeBeginRows]; a commit's
// [expungeCommitRows].
func (p ExpungeProposal) decidedRows() int {
	switch {
	case p.Begin != nil:
		return expungeBeginRows(len(p.Begin.Retire), len(p.Begin.Add)+len(p.Begin.Dropping),
			len(p.Begin.DropLayers))
	case p.Commit != nil:
		return expungeCommitRows(len(p.Commit.Retire), len(p.Commit.Add))
	}
	return 1
}

// validFields is every rule of [ExpungeProposal.Validate] but the record's
// bytes.
func (p ExpungeProposal) validFields() error {
	set := 0
	for _, isSet := range []bool{p.Request != nil, p.Begin != nil, p.Commit != nil} {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("%w: an expunge proposal is exactly one of a request, a begin and a commit, and "+
			"this one sets %d", ErrInvalidProposal, set)
	}
	switch {
	case p.Request != nil:
		return p.Request.validFields()
	case p.Begin != nil:
		return p.Begin.validFields()
	}
	return p.Commit.validFields()
}

// badField is a field of an expunge proposal refused: [ErrInvalidProposal], the
// field, and why.
func badField(field, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalidProposal, field, fmt.Sprintf(format, args...))
}

// validFields holds a request to [ExpungeProposal.Validate]'s rules.
func (r ExpungeRequest) validFields() error {
	if err := validRepoID("request.repo_id", r.RepoID); err != nil {
		return err
	}
	if err := statelog.CheckCallerOpID(r.RequestID); err != nil {
		return badField("request.request_id", "%v", err)
	}
	switch why := r.By.invalid(); {
	case why != "":
		return badField("request.by", "%s", why)
	case r.By.Kind != PrincipalOperator || r.By.Seat == "":
		// A PERSON'S, through a credential bound to them: a purge drops what
		// the company's history held, and a seat or a token bound to nobody
		// is no one a purge may be answered to.
		return badField("request.by", "a purge is a person's, through the credential bound to their human seat "+
			"(%s with a seat)", PrincipalOperator)
	}
	return nil
}

// validFields holds a begin to [ExpungeProposal.Validate]'s rules.
func (b ExpungeBegin) validFields() error {
	if err := validRewrite("begin", b.RepoID, "expunge_id", b.ExpungeID, b.RequestID, b.Node, b.Seen,
		b.DropEpoch); err != nil {
		return err
	}
	switch {
	case b.RequestID == "" && b.Grace != PruneGrace:
		return badField("begin.grace", "%s; an expunge on its maintainer's own schedule keeps what a ref drop "+
			"left for PruneGrace (%s)", b.Grace, PruneGrace)
	case b.RequestID != "" && b.Grace != 0:
		return badField("begin.grace", "%s; an expunge serving a request — a purge — keeps nothing a ref drop "+
			"left", b.Grace)
	case len(b.Retire) == 0:
		return badField("begin.retire", "an expunge rewrites a pack at the least")
	case len(b.Retire) > MaxExpungeInputs:
		return badField("begin.retire", "%d packs, past the %d one expunge rewrites (MaxExpungeInputs)",
			len(b.Retire), MaxExpungeInputs)
	case len(b.Add)+len(b.Dropping) == 0:
		return badField("begin.add", "an expunge writes an output at the least: its inputs hold objects, kept "+
			"or dropping")
	case len(b.Add)+len(b.Dropping) > MaxExpungeOutputs:
		return badField("begin.add", "%d outputs, past the %d one expunge writes (MaxExpungeOutputs)",
			len(b.Add)+len(b.Dropping), MaxExpungeOutputs)
	case len(b.DropLayers) > MaxPublishedLayers:
		return badField("begin.drop_layers", "%d layers, past the %d a chain holds (MaxPublishedLayers)",
			len(b.DropLayers), MaxPublishedLayers)
	}
	if err := sortedIDs("begin.retire", b.Retire); err != nil {
		return err
	}
	for i, id := range b.DropLayers {
		switch {
		case !layerName(id):
			return badField(fmt.Sprintf("begin.drop_layers[%d]", i), "%q is not a layer's checksum", id)
		case slices.Contains(b.DropLayers[:i], id):
			return badField(fmt.Sprintf("begin.drop_layers[%d]", i), "%s is named twice", id)
		}
	}
	if err := derivedOutputs("begin.add", b.ExpungeID, 0, b.Add); err != nil {
		return err
	}
	return derivedOutputs("begin.dropping", b.ExpungeID, len(b.Add), b.Dropping)
}

// validFields holds a commit to [ExpungeProposal.Validate]'s rules.
func (c ExpungeCommit) validFields() error {
	if err := validRewrite("commit", c.RepoID, "commit_id", c.CommitID, c.RequestID, c.Node, c.Seen,
		c.DropEpoch); err != nil {
		return err
	}
	if err := bareUUIDv7("commit.expunge_id", c.ExpungeID); err != nil {
		return err
	}
	switch {
	case c.CommitID == c.ExpungeID:
		return badField("commit.commit_id", "%s is the expunge's own id, which its begin's record carries", c.CommitID)
	case len(c.Retire) > MaxExpungeOutputs:
		return badField("commit.retire", "%d packs, past the %d one begin leaves dropping (MaxExpungeOutputs)",
			len(c.Retire), MaxExpungeOutputs)
	case len(c.Add) > MaxExpungeOutputs:
		return badField("commit.add", "%d rescue packs, past the %d one commit writes (MaxExpungeOutputs)",
			len(c.Add), MaxExpungeOutputs)
	}
	if err := sortedIDs("commit.retire", c.Retire); err != nil {
		return err
	}
	return derivedOutputs("commit.add", c.CommitID, 0, c.Add)
}

// validRewrite is the rules a begin and a commit share, the record's own id
// named by idField: a repository id, the id a bare UUIDv7, the request it
// serves "" or an operation id, a node config admits, a Seen on the code log
// and an epoch of at least one.
func validRewrite(what, repoID, idField, id, requestID, node string, seen statelog.Position,
	epoch int64) error {

	if err := validRepoID(what+".repo_id", repoID); err != nil {
		return err
	}
	if err := bareUUIDv7(what+"."+idField, id); err != nil {
		return err
	}
	if requestID != "" {
		if err := statelog.CheckCallerOpID(requestID); err != nil {
			return badField(what+".request_id", "%v", err)
		}
	}
	switch {
	case !config.ValidNodeID(node):
		return badField(what+".node", "%q is not a node id", node)
	case seen.IsZero():
		return badField(what+".seen", "an expunge's record names the code log's position its builder read at")
	case seen.Stream != topics.CodeLogStream:
		return badField(what+".seen", "%s is a position on another log than the code log (%s)", seen,
			topics.CodeLogStream)
	case seen.Valid() != nil:
		return fmt.Errorf("%w: %s.seen: %w", ErrInvalidProposal, what, seen.Valid())
	case epoch < 1:
		return badField(what+".drop_epoch", "%d; a begin moves a repository's drop epoch to one at the least",
			epoch)
	}
	return nil
}

// validRepoID refuses a repository id that is no UUID in its canonical
// spelling.
func validRepoID(field, id string) error {
	if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id {
		return badField(field, "%q is not a UUID in its canonical lowercase spelling", id)
	}
	return nil
}

// bareUUIDv7 refuses an id that is no bare UUIDv7 in its canonical spelling —
// what a rewrite's own id is, the record's operation id and its outputs'
// derivation's ([DeriveOutputPackID]).
func bareUUIDv7(field, id string) error {
	if parsed, err := uuid.Parse(id); err != nil || parsed.Version() != 7 || parsed.String() != id {
		return badField(field, "%q is not a UUIDv7 in its canonical lowercase spelling", id)
	}
	return nil
}

// sortedIDs refuses pack ids that are not sorted, distinct and non-empty — so
// one plan forms one record on every node, and no pack is counted twice.
func sortedIDs(field string, ids []string) error {
	for i, id := range ids {
		switch {
		case id == "":
			return badField(fmt.Sprintf("%s[%d]", field, i), "a pack is named by its id")
		case i > 0 && ids[i-1] >= id:
			return badField(fmt.Sprintf("%s[%d]", field, i), "%q is not after %q — the packs are sorted and "+
				"distinct", id, ids[i-1])
		}
	}
	return nil
}

// derivedOutputs holds outputs to their derivation from rewriteID, the first
// counted as output from, and to the bundle [PackRef.Check] admits.
func derivedOutputs(field, rewriteID string, from int, outputs []PackRef) error {
	for i, out := range outputs {
		if want := DeriveOutputPackID(rewriteID, from+i); out.PackID != want {
			return badField(fmt.Sprintf("%s[%d].pack_id", field, i), "%q, and output %d of %s is %s", out.PackID,
				from+i, rewriteID, want)
		}
		if err := out.Check(); err != nil {
			return fmt.Errorf("%w: %s[%d].%w", ErrInvalidProposal, field, i, err)
		}
	}
	return nil
}

// ProposeExpunge decides one expunge record — a request, a begin or a commit —
// as a batch of its own in the repository's queue and answers what became of
// it: the push's queue and bounds ([Committer.Propose]), as a compaction's are
// ([Committer.ProposeCompaction]) — a full queue or a node holding its bytes
// busy at once, a proposal whose caller's deadline leaves too little for a
// batch and its answer busy undecided, a stopping committer [ErrNotServing],
// and a proposal that is not one [ErrInvalidProposal]. ctx bounds only this
// caller's wait.
//
// ITS OPERATION ID IS THE RECORD'S OWN — a request's RequestID, a begin's
// ExpungeID, a commit's CommitID — never one minted for the batch, so a
// proposal its asker hands the repository's runner-up after this node went
// silent is the same record, collapsed by the ledger. A BEGIN'S AND A
// COMMIT'S SESSION IS THEIR SEEN: decided from a snapshot that holds the rows
// their builder read, or refused behind, about this node, so the runner-up
// decides them. A begin's and a commit's applied is answered from the rows of
// the packs the record added, never from the decision; a request's with its
// own id at its own record, whatever request the repository held open.
func (c *Committer) ProposeExpunge(ctx context.Context, p ExpungeProposal) (ExpungeResult, error) {
	bytes, err := p.measure()
	if err != nil {
		return ExpungeResult{}, err
	}
	deadline, _ := ctx.Deadline()
	a := c.queue(ctx, &queued{expunge: &p, bytes: bytes, rows: p.decidedRows(), arrived: c.clock(),
		deadline: deadline, gone: ctx.Done(), answer: make(chan answered, 1)})
	return a.expunge, a.err
}

// expungeDecision is one round's decision of an expunge record: its refusal
// and sentence where it is refused, nothing where it forms the record, and
// the version the repository's row was read at either way.
type expungeDecision struct {
	refusal Refusal
	detail  string
	version int64
}

// decideExpunge is one round's decision of p over the rows tx holds — see
// this file's head for the rules, in their order. Pure over (the rows, p,
// retention, now): retention is the deciding epoch's
// code.deleted_retention_days, which a repo_deleted refusal names the purge's
// earliest instant by, and now the decide's clock, which an output's key is
// judged by.
func decideExpunge(ctx context.Context, tx *sql.Tx, p ExpungeProposal, retention time.Duration,
	now time.Time) (expungeDecision, error) {

	repoID := p.RepoID()
	repo, found, err := pushRepoIn(ctx, tx, repoID)
	if err != nil {
		return expungeDecision{}, err
	}
	d := expungeDecision{version: repo.version}
	if !found || repo.status == RepoStatusDeleted || repo.status == RepoStatusPurging {
		if d.detail, err = goneDetail(ctx, tx, repoID, repo, found, retention); err != nil {
			return expungeDecision{}, err
		}
		d.refusal = RefusalRepoDeleted
		return d, nil
	}
	switch {
	case p.Begin != nil:
		return decideBegin(ctx, tx, *p.Begin, repo, d, now)
	case p.Commit != nil:
		return decideCommit(ctx, tx, *p.Commit, repo, d, now)
	}
	// A REQUEST forms its record, whatever request is open: its apply widens
	// that one to itself.
	return d, nil
}

// decideBegin is a begin's rules 1–6 over the repository's row as repo holds
// it and the rows tx holds.
func decideBegin(ctx context.Context, tx *sql.Tx, b ExpungeBegin, repo pushRepo, d expungeDecision,
	now time.Time) (expungeDecision, error) {

	switch {
	case repo.expungeID != "":
		d.refusal, d.detail = RefusalStale, fmt.Sprintf("expunge %s is open on %s and has not committed, "+
			"and a repository expunges once at a time; the next pass plans again", repo.expungeID, repo.name)
		return d, nil
	case b.DropEpoch != repo.dropEpoch+1:
		d.refusal, d.detail = RefusalStale, fmt.Sprintf("%s is at drop epoch %d, and the plan's begin moves "+
			"it to %d: another expunge began since the plan; the next pass plans again", repo.name,
			repo.dropEpoch, b.DropEpoch)
		return d, nil
	}
	inputs, err := namedPackRows(ctx, tx, b.RepoID, b.Retire)
	if err != nil {
		return expungeDecision{}, fmt.Errorf("code: read the packs expunge %s rewrites: %w", b.ExpungeID, err)
	}
	rows := make(map[string][]namedPack, len(inputs))
	for _, p := range inputs {
		rows[p.id] = append(rows[p.id], p)
	}
	var held int64
	retiring := make(map[string]bool, len(b.Retire))
	for _, id := range b.Retire {
		named := rows[id]
		if why := liveInput(named); why != "" {
			d.refusal, d.detail = RefusalStale, fmt.Sprintf("pack %s %s; the next pass plans again", id, why)
			return d, nil
		}
		held += named[0].objects
		retiring[named[0].checksum] = true
	}
	chain, err := layersOf(ctx, tx, b.RepoID)
	if err != nil {
		return expungeDecision{}, err
	}
	if want := LayersOver(chain, retiring); !slices.Equal(want, b.DropLayers) {
		d.refusal, d.detail = RefusalStale, fmt.Sprintf("the published layers from the lowest over the "+
			"packs it rewrites are %s and the plan drops %s: a publication moved the chain since the plan; "+
			"the next pass plans again", layerList(want), layerList(b.DropLayers))
		return d, nil
	}
	made, refused, detail := decideOutputs(append(slices.Clone(b.Add), b.Dropping...), now)
	if refused != "" {
		d.refusal, d.detail = refused, detail
		return d, nil
	}
	if made > held {
		return expungeDecision{}, fmt.Errorf("%w: add: the kept and dropping packs hold %d objects and the "+
			"%d packs they replace %d — an expunge's begin writes no object its inputs did not hold",
			ErrInvalidProposal, made, len(b.Retire), held)
	}
	return d, nil
}

// decideCommit is a commit's rules 1–5 over the repository's row as repo holds
// it and the rows tx holds.
func decideCommit(ctx context.Context, tx *sql.Tx, c ExpungeCommit, repo pushRepo, d expungeDecision,
	now time.Time) (expungeDecision, error) {

	switch {
	case repo.expungeID == "":
		d.refusal, d.detail = RefusalStale, fmt.Sprintf("no expunge is open on %s: expunge %s committed "+
			"already; the next pass reads the rows again", repo.name, c.ExpungeID)
		return d, nil
	case repo.expungeID != c.ExpungeID:
		d.refusal, d.detail = RefusalStale, fmt.Sprintf("expunge %s is open on %s, not %s; the next pass "+
			"reads the rows again", repo.expungeID, repo.name, c.ExpungeID)
		return d, nil
	case repo.dropEpoch != c.DropEpoch:
		d.refusal, d.detail = RefusalStale, fmt.Sprintf("%s is at drop epoch %d and the commit names %d; "+
			"the next pass reads the rows again", repo.name, repo.dropEpoch, c.DropEpoch)
		return d, nil
	}
	dropping, err := droppingPacksOf(ctx, tx, c.RepoID)
	if err != nil {
		return expungeDecision{}, err
	}
	var held int64
	ids := make([]string, len(dropping))
	for i, p := range dropping {
		ids[i] = p.id
		held += p.objects
	}
	if !slices.Equal(ids, c.Retire) {
		d.refusal, d.detail = RefusalStale, fmt.Sprintf("the commit retires %d packs and %s holds %d "+
			"dropping (%s); the next pass reads the rows again", len(c.Retire), repo.name, len(ids),
			strings.Join(ids, " "))
		return d, nil
	}
	made, refused, detail := decideOutputs(c.Add, now)
	if refused != "" {
		d.refusal, d.detail = refused, detail
		return d, nil
	}
	if made > held {
		return expungeDecision{}, fmt.Errorf("%w: add: the rescue holds %d objects and the %d dropping packs "+
			"it is cut from %d — a rescue writes no object its dropping packs did not hold",
			ErrInvalidProposal, made, len(ids), held)
	}
	return d, nil
}

// decideOutputs is the begin's rule 5 over outputs, which a commit's rescue
// meets too: the objects they hold, and the refusal and its sentence of the
// first a bundle check refuses or whose key was minted too long before now.
func decideOutputs(outputs []PackRef, now time.Time) (int64, Refusal, string) {
	var made int64
	for i, out := range outputs {
		if err := out.Check(); err != nil {
			return 0, RefusalPackTooLarge, fmt.Sprintf("output %d (pack %s): %v", i, out.PackID, err)
		}
		if detail, expired := uploadExpired("the pack", out.Object, now, "the next pass builds it again"); expired {
			return 0, RefusalUploadExpired, fmt.Sprintf("output %d (pack %s): %s", i, out.PackID, detail)
		}
		made += out.Objects
	}
	return made, "", ""
}

// LayersOver is the layers of chain a begin retiring the packs whose
// checksums retiring names must drop, in chain order: the chain's top from the
// lowest layer naming one of them, and none where no layer does. The one
// reading of a begin's rule 4 — exported for the maintainer that plans the
// begin (internal/code/compact), so a plan and the decide that judges it never
// disagree about the chain — and of what its apply drops.
func LayersOver(chain []LayerRow, retiring map[string]bool) []string {
	for k, row := range chain {
		if slices.ContainsFunc(row.Layer.Packs, func(p string) bool { return retiring[p] }) {
			out := make([]string, 0, len(chain)-k)
			for _, above := range chain[k:] {
				out = append(out, above.Layer.LayerID)
			}
			return out
		}
	}
	return nil
}

// layerList words a list of layers for a refusal.
func layerList(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return "[" + strings.Join(ids, " ") + "]"
}

// expungingLayerDetail is the sentence of a publication refused stale while an
// expunge is open on the repository — the publication's rule 6g
// (publication.go).
func expungingLayerDetail(name, expungeID string) string {
	return fmt.Sprintf("expunge %s is open on %s, and no layer is published until it commits; the next "+
		"pass plans again", expungeID, name)
}

// droppingPack is one dropping pack row as a commit's decide reads it.
type droppingPack struct {
	id      string
	objects int64
}

// droppingPacksOf is every dropping pack row of a repository, by pack id — one
// read of the repository's own rows, by the key's prefix: what a commit
// retires, all of them.
func droppingPacksOf(ctx context.Context, tx *sql.Tx, repoID string) ([]droppingPack, error) {
	rows, err := tx.QueryContext(ctx, droppingPacksSQL, repoID)
	if err != nil {
		return nil, fmt.Errorf("code: read %s's dropping packs: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []droppingPack
	for rows.Next() {
		var p droppingPack
		if err := rows.Scan(&p.id, &p.objects); err != nil {
			return nil, fmt.Errorf("code: read %s's dropping packs: %w", repoID, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("code: read %s's dropping packs: %w", repoID, err)
	}
	return out, nil
}

// withinExpunge refuses a decision past a record's bytes or rows, which
// [ExpungeProposal.Validate]'s bounds keep from ever forming — the check
// makes the invariant the record's own, as [withinRecord] does a push batch's.
func withinExpunge(d statelog.Decision, p ExpungeProposal) error {
	switch rows := p.decidedRows(); {
	case len(d.Payload) > MaxRecordBytes:
		return fmt.Errorf("%w: %d bytes, past MaxRecordBytes (%d)", ErrRecordCap, len(d.Payload), MaxRecordBytes)
	case rows > MaxRecordRows:
		return fmt.Errorf("%w: up to %d rows, past MaxRecordRows (%d)", ErrRecordCap, rows, MaxRecordRows)
	}
	return nil
}

// expunge publishes one expunge record and answers it — the record's batch of
// its own, under the committer's own lifetime and one CommitBudget, as a
// compaction's ([Committer.compact]).
func (c *Committer) expunge(repoID string, q *queued) {
	ctx, cancel := context.WithTimeout(c.life, CommitBudget)
	defer cancel()
	p := *q.expunge
	op, opID, session, payload := p.record()
	retention := c.retention()
	subject := RepoSubject(repoID)
	scope := []string{RepoPath(repoID)}
	var (
		last    expungeDecision
		decided bool
	)
	res, err := c.pub.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated, Session: session,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// RESET AT THE TOP OF EVERY ROUND: only the last round says
			// what the publish decided.
			last, decided = expungeDecision{}, false
			now := c.now()
			d, err := decideExpunge(ctx, tx, p, retention, now)
			if err != nil {
				return statelog.Decision{}, err
			}
			last, decided = d, true
			if d.refusal != "" {
				return statelog.Decision{Version: d.version}, nil
			}
			decision, err := recordFor(stamp, subject, op, scope, opID, Actor{ID: c.nodeID, Kind: ActorSystem},
				payload, now)
			if err != nil {
				return statelog.Decision{}, err
			}
			if err := withinExpunge(decision, p); err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = d.version
			return decision, nil
		},
	})
	out, answerErr := c.expungeAnswer(ctx, p, opID, last, decided, res, err)
	q.answer <- answered{expunge: out, err: answerErr}
}

// expungeAnswer is an expunge record's answer from the publish's result and
// the last round's decision.
func (c *Committer) expungeAnswer(ctx context.Context, p ExpungeProposal, opID string, last expungeDecision,
	decided bool, res statelog.Result, err error) (ExpungeResult, error) {

	if err != nil {
		return c.refusedExpunge(ctx, p, opID, err)
	}
	switch res.Outcome {
	case statelog.OutcomeApplied:
		if !res.Position.IsZero() {
			return c.appliedExpunge(ctx, p, opID, res.Position)
		}
		// NOTHING WAS APPENDED: the decide refused it, and its last round
		// says why. A publish that answered with no record and no decision
		// broke the publisher's contract, and says so rather than answering
		// from nothing.
		if !decided || last.refusal == "" {
			return ExpungeResult{}, fmt.Errorf("code: the publish of operation %s answered applied with no "+
				"record and decided no refusal", opID)
		}
		return ExpungeResult{Refusal: last.refusal, Detail: last.detail}, nil
	case statelog.OutcomePending:
		out := ExpungeResult{Outcome: statelog.OutcomePending, Position: res.Position}
		if p.Request != nil {
			out.Request = p.Request.RequestID
		}
		return out, nil
	}
	return ExpungeResult{Outcome: statelog.OutcomeUnknown}, nil
}

// appliedExpunge answers an expunge record that applied at at. A REQUEST is
// answered with its own id at its record's position: its payload is the same
// whichever round decided it, and what it wrote — the open request — a
// commit may already have served. A BEGIN AND A COMMIT are answered FROM THE
// ROWS: every pack the record added — a begin's kept and dropping packs, a
// commit's rescue — a row added at at, in code_packs or, retired since, in
// code_packs_retired. A pack in neither is a record that applied and wrote
// rows this node does not hold: no committer of this domain forms that, so it
// is logged at ERROR and answered unknown — never applied, which would hand
// its builder packs no row names.
func (c *Committer) appliedExpunge(ctx context.Context, p ExpungeProposal, opID string,
	at statelog.Position) (ExpungeResult, error) {

	var added []string
	switch {
	case p.Request != nil:
		return ExpungeResult{Outcome: statelog.OutcomeApplied, Position: at, Request: p.Request.RequestID}, nil
	case p.Begin != nil:
		added = append(idsOf(p.Begin.Add), idsOf(p.Begin.Dropping)...)
	default:
		added = idsOf(p.Commit.Add)
	}
	var missing []string
	if err := c.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		missing, err = packsLacking(ctx, tx, p.RepoID(), added, at)
		return err
	}); err != nil {
		failed := fmt.Errorf("code: the record of operation %s applied at %s and the rows of the packs it "+
			"added could not be read: %w", opID, at, err)
		c.log.WarnContext(ctx, "code_commit_failed", "kind", kindExpunge, "repo", p.RepoID(), "op_id", opID,
			"error", failed)
		return ExpungeResult{}, failed
	}
	if len(missing) > 0 {
		c.log.ErrorContext(ctx, "code_expunge_unanswerable", "repo", p.RepoID(), "op_id", opID,
			"position", at.String(), "missing", missing)
		return ExpungeResult{Outcome: statelog.OutcomeUnknown, Detail: fmt.Sprintf("the expunge's record "+
			"applied at %s and the packs it added are not in the rows this node holds: %s", at,
			strings.Join(missing, ", "))}, nil
	}
	return ExpungeResult{Outcome: statelog.OutcomeApplied, Position: at}, nil
}

// refusedExpunge answers an expunge record whose publish failed — the push's
// mapping ([Committer.refused]) in an expunge's words, as a compaction's is
// ([Committer.refusedCompaction]).
func (c *Committer) refusedExpunge(ctx context.Context, p ExpungeProposal, opID string,
	err error) (ExpungeResult, error) {

	var refusal *statelog.Unavailable
	switch {
	case errors.Is(err, ErrInvalidProposal):
		// THE BUILDER'S BUG, a begin's or a commit's rule of arithmetic:
		// refused before any record, and said where an operator reads the
		// node that decided.
		c.log.ErrorContext(ctx, "code_expunge_invalid", "repo", p.RepoID(), "op_id", opID, "error", err)
		return ExpungeResult{}, err
	case errors.Is(err, statelog.ErrConflict):
		c.countBusy(p.RepoID(), kindExpunge, busyContended, 1)
		return ExpungeResult{Refusal: RefusalBusy, Detail: "the repository kept changing for every round the " +
			"write may take; " + p.again()}, nil
	case errors.As(err, &refusal) && refusal.Reason == statelog.ReasonDeleted:
		// A FACT ABOUT THE REPOSITORY: its deletion marker drops every
		// record about it, and the position is named only where the marker
		// dropped THIS record.
		var at statelog.Position
		if refusal.Landed() {
			at = refusal.Position
		}
		return ExpungeResult{Refusal: RefusalRepoDeleted, Position: at,
			Detail: c.purgedDetail(ctx, p.RepoID(), opID)}, nil
	case errors.As(err, &refusal) && refusal.Landed():
		return ExpungeResult{Refusal: RefusalNotApplied, Position: refusal.Position,
			Detail: fmt.Sprintf("the expunge's record applies nowhere: it was durable at %s and the code log's "+
				"%s gate drops it on every node; %s", refusal.Position, refusal.Reason, p.again())}, nil
	case errors.As(err, &refusal) && nodeRefusals[refusal.Reason]:
		return ExpungeResult{}, fmt.Errorf("%w: %w", ErrNotServing, refusal)
	case errors.As(err, &refusal):
		return ExpungeResult{Refusal: RefusalUnavailable,
			Detail: "the code log refused the write: " + strings.TrimPrefix(refusal.Error(), "statelog: ")}, nil
	}
	c.log.WarnContext(ctx, "code_commit_failed", "kind", kindExpunge, "repo", p.RepoID(), "op_id", opID,
		"error", err)
	return ExpungeResult{}, fmt.Errorf("code: expunge operation %s: %w", opID, err)
}

// The expunge's own read, registered in plan_test.go: a repository's dropping
// packs, by the key's prefix — at most the outputs one begin wrote.
const droppingPacksSQL = `SELECT pack_id, objects FROM code_packs
	WHERE repo_id = ? AND part = 'pack' AND state = 'dropping' ORDER BY pack_id`
