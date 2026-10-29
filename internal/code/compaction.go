package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A COMPACTION, DECIDED: a git node that merged some of a repository's packs
// into fewer asks the repository's committer to record it, and the committer
// decides it in ONE snapshot, alone, as a record of its own (OpCompact) on the
// repository's subject — so everything that moves a repository's packs has one
// writer, its committer, and contends on one subject.
//
// # What the decide holds a compaction to
//
// Against the rows of one snapshot, in this order; the first rule that fails
// is the answer, and none of them writes anything:
//
//  1. the repository: absent, deleted or being purged is repo_deleted, in the
//     sentence of the state it is in ([RepoGone]); archived is decided, since
//     an archived repository is still read and every read of it pays for
//     every pack it holds;
//  2. every input: exactly one row, of part `pack`, live ([liveInput]) and not
//     full ([PackRow.Full]), and named by no published layer the record does
//     not replace — else stale, naming the first that is not: a compaction,
//     an expunge or a purge moved it first, an expunge left it dropping, a
//     pack grew full under the plan's feet, or a publication put it in a
//     layer, and the next pass plans again from the rows;
//  3. every output: a bundle [PackRef.Check] admits — the pack and both its
//     indexes within MaxPackBytes, the one rule every holder of a row applies
//     — else pack_too_large, and its key minted within objstore.RecordWithin
//     of the decision ([uploadExpired]) — else upload_expired;
//  4. the arithmetic: the outputs hold no more objects than the inputs' ROWS
//     count — else the decision is an error ([ErrInvalidProposal]): a builder
//     that invented objects is a bug, refused before any record and logged.
//     No lower bound, since inputs that share objects are merged into outputs
//     that hold each once. And no size rule (code.limits.repo_max_bytes): a
//     compaction adds no content, and the one way its outputs can outweigh its
//     inputs — a delta whose base went to an earlier output of a cut, written
//     whole — moves a repository already near its ceiling by an object, where
//     refusing it would leave that repository unable to compact at all.
//
// A compaction that carries a published layer — a PUBLICATION — is held to
// the layer's own seven rules (publication.go): 6g, no expunge open, with the
// repository's row, and 6a–6f after the rules above. Nothing else is read — no
// ref, no protection, no push — so a decide costs the repository's row, its
// published chain (at most MaxPublishedLayers rows, by the key) and a seek of
// the key an input, and a publication its live packs besides; the rows of two
// snapshots are never paired.
//
// # A merge makes no reachability judgement
//
// Every object of the inputs is in the outputs ([Compaction], ADR-0060), so no
// push decided against the inputs can land a ref whose closure a compaction
// removed: a compaction is decided against the inputs alone, never against the
// refs, and the subject's compare-and-set orders it with every push.

// Validate refuses a proposal no committer could decide ([ErrInvalidProposal],
// naming the field), before it is queued: a repository id that is no
// repository's, a compaction id that is no bare UUIDv7 (the record's operation
// id, whose instant the ledger and every output id read), a node id config
// does not admit, a Seen that is no position on the code log, inputs that are
// not sorted and distinct or number fewer than two or more than
// MaxCompactInputs, outputs that number none, more than MaxCompactOutputs or
// more than the inputs, an output whose pack id is not its derivation
// ([DeriveOutputPackID]) or whose bundle [PackRef.Check] refuses, and a
// compaction past what one record may carry. A PUBLICATION — one carrying a
// Layer — may carry no inputs and no outputs at all, and is refused for a
// layer [LayerRef.Check] refuses (with [ErrInvalidLayer] beside it), an
// output that is not one of the layer's packs, or as many replaced layers as
// a chain holds. What the inputs and the chain hold is the decide's to judge,
// from the rows: the proposal does not carry it.
func (p CompactionProposal) Validate() error {
	_, err := p.measure()
	return err
}

// ValidateUnstored is [CompactionProposal.Validate] of a proposal whose
// outputs are not yet STORED — each output's sizes, object count and checksum
// known from the files its build wrote ([LayBundle]), its object's key and
// digest not until its put returns: every output is measured and checked as it
// will be once stored, a key and a digest having one spelling's length each,
// exactly as [Proposal.ValidateUnstored] measures a push's pack. So the git
// node that built a compaction holds it to every rule its committer will
// before a byte of it is uploaded (internal/code/compact), and a compaction
// no committer could take — an output past the bundle rule, more outputs than
// it may write, a record past its bytes — stores nothing. An output already
// stored is refused: Validate is the check of that.
//
// A PUBLICATION'S LAYER is held the same way: its parts are not stored either,
// so the layer carries none, and each is stood in by the part the layer's
// sections cut into — its size the cut's, its key and digest one spelling's
// length each — so the layer is held to every rule its row must meet
// ([LayerRef.Check]) and the record to its bytes with every part named. A
// layer that already names a part is refused, as a stored output is.
func (p CompactionProposal) ValidateUnstored() error {
	stand := largestEntry().Pack
	add := make([]PackRef, len(p.Compaction.Add))
	for i, out := range p.Compaction.Add {
		if !out.Object.IsZero() || out.Hash != "" {
			return fmt.Errorf("%w: add[%d]: already stored as %s — Validate checks a stored output",
				ErrInvalidProposal, i, out.Object)
		}
		out.Object, out.Hash = stand.Object, stand.Hash
		add[i] = out
	}
	p.Compaction.Add = add
	if l := p.Compaction.Layer; l != nil {
		if len(l.Parts) > 0 {
			return fmt.Errorf("%w: layer.parts: already stored as %d parts — Validate checks a stored layer",
				ErrInvalidProposal, len(l.Parts))
		}
		layer := *l
		sections := layer.Sections()
		for i := int64(0); i*LayerPartBytes < layer.Size(); i++ {
			layer.Parts = append(layer.Parts, LayerPart{Object: stand.Object, Hash: stand.Hash,
				Size: layerPart(sections, int(i)).size})
		}
		p.Compaction.Layer = &layer
	}
	return p.Validate()
}

// measure validates p and answers its queued byte charge: the compaction
// encoded, which is what its record carries, held under [EntryRoom] — what one
// proposal may take of a record beside the record's own overhead.
func (p CompactionProposal) measure() (int, error) {
	if err := p.validFields(); err != nil {
		return 0, err
	}
	encoded, err := json.Marshal(p.Compaction)
	if err != nil {
		return 0, fmt.Errorf("%w: compaction: encode it: %w", ErrInvalidProposal, err)
	}
	if len(encoded) > EntryRoom {
		return 0, fmt.Errorf("%w: compaction: %d bytes, past the %d one proposal may take of its record",
			ErrInvalidProposal, len(encoded), EntryRoom)
	}
	return len(encoded), nil
}

// validFields is every rule of [CompactionProposal.Validate] but the record's
// bytes.
func (p CompactionProposal) validFields() error {
	c := p.Compaction
	bad := func(field, format string, args ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrInvalidProposal, field, fmt.Sprintf(format, args...))
	}
	if id, err := uuid.Parse(c.RepoID); err != nil || id.String() != c.RepoID {
		return bad("repo_id", "%q is not a UUID in its canonical lowercase spelling", c.RepoID)
	}
	if id, err := uuid.Parse(c.CompactionID); err != nil || id.Version() != 7 || id.String() != c.CompactionID {
		return bad("compaction_id", "%q is not a UUIDv7 in its canonical lowercase spelling", c.CompactionID)
	}
	switch {
	case !config.ValidNodeID(c.Node):
		return bad("node", "%q is not a node id", c.Node)
	case c.Seen.IsZero():
		return bad("seen", "a compaction names the code log's position it was planned at")
	case c.Seen.Stream != topics.CodeLogStream:
		return bad("seen", "%s is a position on another log than the code log (%s)", c.Seen,
			topics.CodeLogStream)
	case c.Seen.Valid() != nil:
		return fmt.Errorf("%w: seen: %w", ErrInvalidProposal, c.Seen.Valid())
	case c.Layer != nil && len(c.Retire) == 0 && len(c.Add) == 0:
		// A LAYER OVER THE PACKS AS THEY STAND: a publication with no repack
		// retires nothing and writes nothing but its layer.
	case len(c.Retire) < 2:
		return bad("retire", "%d packs; a compaction merges two at the least", len(c.Retire))
	case len(c.Retire) > MaxCompactInputs:
		return bad("retire", "%d packs, past the %d one compaction merges (MaxCompactInputs)",
			len(c.Retire), MaxCompactInputs)
	case len(c.Add) == 0:
		return bad("add", "a compaction writes one output at the least")
	case len(c.Add) > MaxCompactOutputs:
		return bad("add", "%d outputs, past the %d one compaction writes (MaxCompactOutputs)",
			len(c.Add), MaxCompactOutputs)
	case len(c.Add) > len(c.Retire):
		return bad("add", "%d outputs of %d packs; a merge writes no more packs than it replaces",
			len(c.Add), len(c.Retire))
	}
	for i, id := range c.Retire {
		switch {
		case id == "":
			return bad(fmt.Sprintf("retire[%d]", i), "a pack is named by its id")
		case i > 0 && c.Retire[i-1] >= id:
			// SORTED AND DISTINCT, so one plan forms one record on every
			// node, and no input is counted twice.
			return bad(fmt.Sprintf("retire[%d]", i), "%q is not after %q — the inputs are sorted and "+
				"distinct", id, c.Retire[i-1])
		}
	}
	for i, out := range c.Add {
		if want := DeriveOutputPackID(c.CompactionID, i); out.PackID != want {
			return bad(fmt.Sprintf("add[%d].pack_id", i), "%q, and output %d of compaction %s is %s",
				out.PackID, i, c.CompactionID, want)
		}
		if err := out.Check(); err != nil {
			return fmt.Errorf("%w: add[%d].%w", ErrInvalidProposal, i, err)
		}
	}
	if c.Layer != nil {
		return c.validLayerFields()
	}
	return nil
}

// ProposeCompaction decides one compaction as a batch of its own in the
// repository's queue and answers what became of it — the push's queue and
// bounds ([Committer.Propose]): a full queue or a node holding its bytes is
// busy at once, a compaction whose caller's deadline leaves too little for a
// batch and its answer is busy undecided, a stopping committer answers
// [ErrNotServing], and a proposal that is not one answers
// [ErrInvalidProposal]. ctx bounds only this caller's wait.
//
// APPLIED IS ANSWERED FROM THE ROWS: the outputs' rows the record applied, at
// its position — or, collapsed onto an earlier record of the same compaction
// id, at that record's — never from the decision. A refusal that decided
// nothing appends nothing: the outputs are named by no row, and the collector
// takes them past its grace.
func (c *Committer) ProposeCompaction(ctx context.Context, p CompactionProposal) (CompactResult, error) {
	bytes, err := p.measure()
	if err != nil {
		return CompactResult{}, err
	}
	deadline, _ := ctx.Deadline()
	a := c.queue(ctx, &queued{compaction: &p, bytes: bytes,
		rows:    compactDecidedRows(len(p.Compaction.Retire), len(p.Compaction.Add), p.Compaction.Layer),
		arrived: c.clock(), deadline: deadline, gone: ctx.Done(), answer: make(chan answered, 1)})
	return a.compact, a.err
}

// compactDecision is one round's decision of a compaction: its refusal and
// sentence where it is refused, nothing where it forms the record, and the
// version the repository's row was read at either way.
type compactDecision struct {
	refusal Refusal
	detail  string
	version int64
}

// decideCompaction is one round's decision of c over the rows tx holds — see
// this file's head for the rules, in their order, and publication.go's for a
// publication's. Pure over (the rows, c, retention, now): retention is the
// deciding epoch's code.deleted_retention_days, which a repo_deleted refusal
// names the purge's earliest instant by, and now the decide's clock, which an
// output's key and a layer part's are judged by.
func decideCompaction(ctx context.Context, tx *sql.Tx, c Compaction, retention time.Duration,
	now time.Time) (compactDecision, error) {

	repo, found, err := pushRepoIn(ctx, tx, c.RepoID)
	if err != nil {
		return compactDecision{}, err
	}
	d := compactDecision{version: repo.version}
	if !found || repo.status == RepoStatusDeleted || repo.status == RepoStatusPurging {
		if d.detail, err = goneDetail(ctx, tx, c.RepoID, repo, found, retention); err != nil {
			return compactDecision{}, err
		}
		d.refusal = RefusalRepoDeleted
		return d, nil
	}
	if c.Layer != nil && repo.expungeID != "" {
		// RULE 6g, read off the repository's row before anything else of a
		// publication (publication.go).
		d.refusal, d.detail = RefusalStale, expungingLayerDetail(repo.name, repo.expungeID)
		return d, nil
	}

	// THE PUBLISHED CHAIN, READ FOR EVERY COMPACTION: no input may be a pack
	// a layer names, unless the record replaces that layer.
	chain, err := layersOf(ctx, tx, c.RepoID)
	if err != nil {
		return compactDecision{}, err
	}
	var replaces []string
	if c.Layer != nil {
		replaces = c.Layer.Replaces
	}
	published := publishedBy(chain, replaces)

	inputs, err := namedPackRows(ctx, tx, c.RepoID, c.Retire)
	if err != nil {
		return compactDecision{}, fmt.Errorf("code: read the packs compaction %s retires: %w", c.CompactionID, err)
	}
	rows := make(map[string][]namedPack, len(inputs))
	for _, p := range inputs {
		rows[p.id] = append(rows[p.id], p)
	}
	var held int64
	retired := make(map[string]bool, len(c.Retire))
	for _, id := range c.Retire {
		var why string
		switch named := rows[id]; {
		case liveInput(named) != "":
			why = liveInput(named)
		case named[0].row().Full():
			why = "is full"
		case published[named[0].checksum] != "":
			why = fmt.Sprintf("is in published layer %s, and a compaction retires a published pack only with "+
				"the layer it replaces", published[named[0].checksum])
		default:
			held += named[0].objects
			retired[id] = true
			continue
		}
		d.refusal, d.detail = RefusalStale, fmt.Sprintf("pack %s %s; the next pass plans again", id, why)
		return d, nil
	}

	var made int64
	for i, out := range c.Add {
		if err := out.Check(); err != nil {
			d.refusal, d.detail = RefusalPackTooLarge, fmt.Sprintf("output %d (pack %s): %v", i, out.PackID, err)
			return d, nil
		}
		if detail, expired := uploadExpired("the pack", out.Object, now, "the next pass builds it again"); expired {
			d.refusal, d.detail = RefusalUploadExpired, fmt.Sprintf("output %d (pack %s): %s", i, out.PackID,
				detail)
			return d, nil
		}
		made += out.Objects
	}
	if made > held {
		return compactDecision{}, fmt.Errorf("%w: add: the outputs hold %d objects and the %d packs they "+
			"replace %d — a merge writes no object its inputs did not hold", ErrInvalidProposal, made,
			len(c.Retire), held)
	}
	if c.Layer != nil {
		if d.refusal, d.detail, err = decideLayer(ctx, tx, c, chain, retired, now); err != nil {
			return compactDecision{}, err
		}
	}
	return d, nil
}

// withinCompaction refuses a decision past a record's bytes or rows, which
// [CompactionProposal.Validate]'s bounds keep from ever forming — the check
// makes the invariant the record's own, as [withinRecord] does a push batch's.
func withinCompaction(d statelog.Decision, c Compaction) error {
	switch rows := compactDecidedRows(len(c.Retire), len(c.Add), c.Layer); {
	case len(d.Payload) > MaxRecordBytes:
		return fmt.Errorf("%w: %d bytes, past MaxRecordBytes (%d)", ErrRecordCap, len(d.Payload), MaxRecordBytes)
	case rows > MaxRecordRows:
		return fmt.Errorf("%w: up to %d rows, past MaxRecordRows (%d)", ErrRecordCap, rows, MaxRecordRows)
	}
	return nil
}

// compact publishes one compaction as ONE record and answers it — the
// compaction's batch of its own, under the committer's own lifetime and one
// CommitBudget, as a push batch's ([Committer.commit]).
//
// ITS OPERATION ID IS THE COMPACTION'S OWN, never one minted for the batch: a
// compaction its asker hands to the repository's runner-up after this node
// went silent is the same compaction there, and the ledger collapses it onto
// the record that landed rather than deciding it again — which would find its
// inputs retired and answer it stale. ITS SESSION IS ITS SEEN: decided from a
// snapshot that holds the rows its builder planned from, or — where this node
// cannot reach them in time — refused behind, about this node, so the runner-up
// decides it.
func (c *Committer) compact(repoID string, q *queued) {
	ctx, cancel := context.WithTimeout(c.life, CommitBudget)
	defer cancel()
	comp := q.compaction.Compaction
	retention := c.retention()
	subject := RepoSubject(repoID)
	scope := []string{RepoPath(repoID)}
	var (
		last    compactDecision
		decided bool
	)
	res, err := c.pub.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: comp.CompactionID, Pattern: statelog.PatternArbitrated, Session: comp.Seen,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// RESET AT THE TOP OF EVERY ROUND: only the last round says
			// what the publish decided.
			last, decided = compactDecision{}, false
			now := c.now()
			d, err := decideCompaction(ctx, tx, comp, retention, now)
			if err != nil {
				return statelog.Decision{}, err
			}
			last, decided = d, true
			if d.refusal != "" {
				return statelog.Decision{Version: d.version}, nil
			}
			decision, err := recordFor(stamp, subject, OpCompact, scope, comp.CompactionID,
				Actor{ID: c.nodeID, Kind: ActorSystem}, comp, now)
			if err != nil {
				return statelog.Decision{}, err
			}
			if err := withinCompaction(decision, comp); err != nil {
				return statelog.Decision{}, err
			}
			decision.Version = d.version
			return decision, nil
		},
	})
	out, answerErr := c.compactAnswer(ctx, comp, last, decided, res, err)
	q.answer <- answered{compact: out, err: answerErr}
}

// compactAnswer is a compaction's answer from the publish's result and the
// last round's decision.
func (c *Committer) compactAnswer(ctx context.Context, comp Compaction, last compactDecision, decided bool,
	res statelog.Result, err error) (CompactResult, error) {

	if err != nil {
		return c.refusedCompaction(ctx, comp, err)
	}
	switch res.Outcome {
	case statelog.OutcomeApplied:
		if !res.Position.IsZero() {
			return c.appliedCompaction(ctx, comp, res.Position)
		}
		// NOTHING WAS APPENDED: the decide refused it, and its last round
		// says why. A publish that answered with no record and no decision
		// broke the publisher's contract, and says so rather than answering
		// from nothing.
		if !decided || last.refusal == "" {
			return CompactResult{}, fmt.Errorf("code: the publish of compaction %s answered applied with "+
				"no record and decided no refusal", comp.CompactionID)
		}
		return CompactResult{Refusal: last.refusal, Detail: last.detail}, nil
	case statelog.OutcomePending:
		return CompactResult{Outcome: statelog.OutcomePending, Position: res.Position}, nil
	}
	return CompactResult{Outcome: statelog.OutcomeUnknown}, nil
}

// appliedCompaction answers a compaction whose record applied at at, FROM THE
// ROWS: every output a row of part `pack` added at at — in code_packs, or in
// code_packs_retired where a later compaction has since retired it, which
// moves the row and keeps when it was added — and a publication's layer a row
// of code_layers added at at. One read, in one transaction.
//
// An output or a layer that is in none is a record that applied and wrote
// rows this node does not hold: no committer of this domain forms that, so it
// is logged at ERROR and answered unknown — never applied, which would hand
// its builder outputs and a layer to adopt that no row names. A layer a later
// publication replaced before this answer read the rows is answered so too:
// its rows are gone, since a replaced layer keeps none, and the one publisher
// of a repository plans its next publication only once this one is answered.
func (c *Committer) appliedCompaction(ctx context.Context, comp Compaction, at statelog.Position) (CompactResult,
	error) {

	var missing []string
	if err := c.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		missing, err = outputsMissing(ctx, tx, comp, at)
		return err
	}); err != nil {
		failed := fmt.Errorf("code: the record of compaction %s applied at %s and its output rows could not "+
			"be read: %w", comp.CompactionID, at, err)
		c.log.WarnContext(ctx, "code_commit_failed", "kind", kindCompaction, "repo", comp.RepoID,
			"op_id", comp.CompactionID, "error", failed)
		return CompactResult{}, failed
	}
	if len(missing) > 0 {
		c.log.ErrorContext(ctx, "code_compaction_unanswerable", "repo", comp.RepoID,
			"compaction_id", comp.CompactionID, "position", at.String(), "missing", missing)
		return CompactResult{Outcome: statelog.OutcomeUnknown, Detail: fmt.Sprintf("the compaction's record "+
			"applied at %s and what it wrote is not in the rows this node holds: %s", at,
			strings.Join(missing, ", "))}, nil
	}
	return CompactResult{Outcome: statelog.OutcomeApplied, Position: at}, nil
}

// refusedCompaction answers a compaction whose publish failed — the push's
// mapping ([Committer.refused]) in a compaction's words, since what each
// refusal says about the node and the record is the same.
func (c *Committer) refusedCompaction(ctx context.Context, comp Compaction, err error) (CompactResult, error) {
	var refusal *statelog.Unavailable
	switch {
	case errors.Is(err, ErrInvalidProposal):
		// THE BUILDER'S BUG, the decide's rule 4 — or a publication's 6b or
		// 6e, its planner's: refused before any record, and said where an
		// operator reads the node that decided, under the name of what built
		// it.
		msg := "code_compaction_invalid"
		if comp.Layer != nil {
			msg = "code_publication_invalid"
		}
		c.log.ErrorContext(ctx, msg, "repo", comp.RepoID,
			"compaction_id", comp.CompactionID, "node", comp.Node, "error", err)
		return CompactResult{}, err
	case errors.Is(err, statelog.ErrConflict):
		c.countBusy(comp.RepoID, kindCompaction, busyContended, 1)
		return CompactResult{Refusal: RefusalBusy, Detail: "the repository kept changing for every round " +
			"the write may take; the next pass plans again"}, nil
	case errors.As(err, &refusal) && refusal.Reason == statelog.ReasonDeleted:
		// A FACT ABOUT THE REPOSITORY: its deletion marker drops every
		// record about it, and the position is named only where the marker
		// dropped THIS record.
		var at statelog.Position
		if refusal.Landed() {
			at = refusal.Position
		}
		return CompactResult{Refusal: RefusalRepoDeleted, Position: at,
			Detail: c.purgedDetail(ctx, comp.RepoID, comp.CompactionID)}, nil
	case errors.As(err, &refusal) && refusal.Landed():
		return CompactResult{Refusal: RefusalNotApplied, Position: refusal.Position,
			Detail: fmt.Sprintf("the compaction applies nowhere: its record at %s was durable and the code "+
				"log's %s gate drops it on every node; the next pass plans again", refusal.Position,
				refusal.Reason)}, nil
	case errors.As(err, &refusal) && nodeRefusals[refusal.Reason]:
		return CompactResult{}, fmt.Errorf("%w: %w", ErrNotServing, refusal)
	case errors.As(err, &refusal):
		return CompactResult{Refusal: RefusalUnavailable,
			Detail: "the code log refused the write: " + strings.TrimPrefix(refusal.Error(), "statelog: ")}, nil
	}
	c.log.WarnContext(ctx, "code_commit_failed", "kind", kindCompaction, "repo", comp.RepoID,
		"op_id", comp.CompactionID, "error", err)
	return CompactResult{}, fmt.Errorf("code: compaction %s: %w", comp.CompactionID, err)
}

// partPack is the one part every pack this build stores is: a pack is ONE
// bundle, one row.
const partPack = "pack"

// namedPack is one row of code_packs as it is stored — what a rewrite's
// decide judges an input by and its apply moves into code_packs_retired whole:
// content is the position its content dates from as a purge reads it
// ([PackRow.Content]).
type namedPack struct {
	id, part, object, hash, checksum                        string
	kind                                                    PackKind
	state                                                   PackState
	size, indexBytes, reverseBytes, objects, added, content int64
}

// row is the pack as a snapshot serves it, for the rules a [PackRow] answers.
func (p namedPack) row() PackRow {
	return PackRow{PackID: p.id, Part: p.part, Size: p.size, IndexBytes: p.indexBytes,
		ReverseBytes: p.reverseBytes, GitChecksum: p.checksum, Objects: p.objects, Kind: p.kind, State: p.state}
}

// liveInput is why rows — every row code_packs holds of one pack id — are no
// pack a rewrite may take as an input, or "" for one it may: exactly one row,
// of part `pack`, LIVE. ONE READING for every rewrite's decide, a
// compaction's and an expunge's begin, so a pack one takes is a pack the other
// may: a dropping pack is neither's, since its expunge's commit retires it.
func liveInput(rows []namedPack) string {
	switch {
	case len(rows) == 0:
		return "is no longer live: a compaction, an expunge or a purge moved it first"
	case len(rows) == 1 && rows[0].state == PackDropping:
		return "is dropping: an expunge left it for its commit to retire"
	case len(rows) == 1 && rows[0].state != PackLive:
		return fmt.Sprintf("is %q, no state this build rewrites", rows[0].state)
	case len(rows) > 1 || rows[0].part != partPack:
		return "is split into parts"
	}
	return ""
}

// namedPackRows is every row of code_packs of repoID whose pack id is among
// ids, each id once however often it is named — ONE statement over the key,
// driven from the list (planner.go).
func namedPackRows(ctx context.Context, tx *sql.Tx, repoID string, ids []string) ([]namedPack, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	list, err := jsonList(distinct(ids))
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, namedPacksSQL, repoID, list)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []namedPack
	for rows.Next() {
		var p namedPack
		if err := rows.Scan(&p.id, &p.part, &p.object, &p.hash, &p.size, &p.indexBytes, &p.reverseBytes,
			&p.checksum, &p.objects, &p.kind, &p.state, &p.added, &p.content); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// distinct is ids with every repeat after its first dropped, in order.
func distinct(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// outputsMissing is everything comp wrote at at that no table holds, each
// named ("output <pack id>", "layer <layer id>"): an output not in code_packs
// and not in code_packs_retired, where a row a later compaction retired keeps
// the position it was added at — the retired table read only for the outputs
// code_packs lacks — and a publication's layer not in code_layers as added at
// at.
func outputsMissing(ctx context.Context, tx *sql.Tx, comp Compaction, at statelog.Position) ([]string, error) {
	var missing []string
	if comp.Layer != nil {
		chain, err := layersOf(ctx, tx, comp.RepoID)
		if err != nil {
			return nil, err
		}
		if !layerApplied(chain, comp, at) {
			missing = append(missing, "layer "+comp.Layer.LayerID)
		}
	}
	lacking, err := packsLacking(ctx, tx, comp.RepoID, idsOf(comp.Add), at)
	if err != nil {
		return nil, fmt.Errorf("code: read compaction %s's outputs: %w", comp.CompactionID, err)
	}
	for _, id := range lacking {
		missing = append(missing, "output "+id)
	}
	return missing, nil
}

// idsOf is the pack ids of refs, in order.
func idsOf(refs []PackRef) []string {
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = r.PackID
	}
	return ids
}

// packsLacking is every pack of ids whose row added at at no pack table of
// repoID holds — in code_packs in any state, or in code_packs_retired, where a
// row a later record retired keeps the position it was added at, that table
// read only for the ids code_packs lacks — by pack id: what a rewrite's
// answer reads of the outputs its record applied ([outputsMissing], and an
// expunge's).
func packsLacking(ctx context.Context, tx *sql.Tx, repoID string, ids []string, at statelog.Position) (
	[]string, error) {

	if len(ids) == 0 {
		return nil, nil
	}
	live, err := namedPackRows(ctx, tx, repoID, ids)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(ids))
	for _, p := range live {
		if p.part == partPack && p.added == at.Packed() {
			present[p.id] = true
		}
	}
	var lacking []string
	for _, id := range ids {
		if !present[id] {
			lacking = append(lacking, id)
		}
	}
	if len(lacking) == 0 {
		return nil, nil
	}
	list, err := jsonList(distinct(lacking))
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, retiredAddedSQL, repoID, list)
	if err != nil {
		return nil, fmt.Errorf("the retired rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id, part string
			added    int64
		)
		if err := rows.Scan(&id, &part, &added); err != nil {
			return nil, fmt.Errorf("the retired rows: %w", err)
		}
		if part == partPack && added == at.Packed() {
			present[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("the retired rows: %w", err)
	}
	var missing []string
	for _, id := range lacking {
		if !present[id] {
			missing = append(missing, id)
		}
	}
	return missing, nil
}

// The compaction's reads, each registered in plan_test.go: the rows of the
// packs a list names — a decide's inputs, an apply's inputs and outputs, an
// answer's outputs — and the retired rows of the outputs an answer did not
// find live, each driven from its list over the table's whole key.
const (
	namedPacksSQL = `SELECT p.pack_id, p.part, p.object, p.hash, p.size, p.index_bytes, p.reverse_bytes,
		p.git_checksum, p.objects, p.kind, p.state, p.added_position, p.content_position
		FROM json_each(?2) AS j CROSS JOIN code_packs AS p INDEXED BY ` + packsKey + `
		WHERE p.repo_id = ?1 AND p.pack_id = CAST(j.value AS TEXT)`
	retiredAddedSQL = `SELECT r.pack_id, r.part, r.added_position
		FROM json_each(?2) AS j CROSS JOIN code_packs_retired AS r INDEXED BY ` + retiredKey + `
		WHERE r.repo_id = ?1 AND r.pack_id = CAST(j.value AS TEXT)`
)
