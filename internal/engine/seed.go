package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE BOOT SEED: a company FILE's org chart becoming rows.
//
// # Why a file still writes a chart at all
//
// The chart is a log: every later change to it is a record with its own
// history and its own author, and a stored config revision cannot carry one at
// all. But a company has to START somewhere, and what an operator has on the
// first run is a file — `crewlet run -company acme.yaml`. Without this a fresh
// deployment boots with an empty chart, which is a company with no seats: no
// mailbox is attached, no placement claims anything, and the dashboard renders
// an organisation of nobody. The file would validate, the node would come up,
// and nothing at all would say why.
//
// # ONLY WHEN THE CHART IS EMPTY
//
// This is a SEED and not an import. It answers "this company has never had a
// chart, and a file describes one", and nothing else. The moment the chart
// holds anything — a seat somebody hired through the API, a unit moved with
// a chart write, a peer's import — the file stops being the authority and
// this does nothing at all.
//
// The alternative was to seed on every boot and let the ledger make it a
// no-op. That is wrong in one direction and it is the expensive one: an
// operator who still passes `-company` on a node that restarts would re-place
// every object the file names, so a seat moved to another unit last week
// would silently move back on the next restart. A chart edited after the seed
// belongs to whoever edited it.
//
// Changing the chart from a file afterwards is `crewlet config import`, which
// diffs the file against the rows, says what it would remove, and asks.
//
// # It is an IMPORT RECORD, keyed on the file's own content
//
// [chart.Writer.WriteImport] is one record on the structure's subject carrying
// the whole authored chart, and the apply is a no-op when its revision is
// already in `chart_import_ledger`. That ledger is the FLEET's half of the
// emptiness rule above: two nodes booting the same file at the same moment
// both see an empty chart, both publish, and the second apply writes nothing.
// The key is a hash of the AUTHORED CHART rather than of the file or of the
// revision id, so the two publishes are one record:
//
//   - The FILE is the wrong key because two things change it that the chart
//     does not care about — a reworded mission, a rotated `${VAR}` — and each
//     would make the two nodes disagree about what they were seeding.
//   - The REVISION ID is the wrong key for the same reason and one more: the
//     rotation gesture is re-activating an UNCHANGED revision, and a new id
//     over an identical chart is not a different structure.
//   - The CHART's own content is right because the question the ledger
//     answers is "has this structure already landed".

// seedChart publishes the company file's authored chart, once.
//
// It runs at boot, after the state log is up and before the first epoch is
// installed, so the view the first composition carries is already the file's.
//
// A NODE WITH NO CHART WRITER DOES NOTHING, which is the `crewlet validate`
// shape and not a failure: that engine applies to nothing and opens no log.
func (e *Engine) seedChart(ctx context.Context, cfg *config.Company) error {
	writer := e.ChartWriter()
	if writer == nil || cfg == nil {
		return nil
	}
	// THE EMPTINESS CHECK FIRST, and at this node's own applied level: a
	// linearizable read would put a broker round trip and a barrier append
	// into every boot, to answer a question whose wrong answer the fleet's
	// import ledger already absorbs.
	held, err := e.Chart().Read(ctx, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		return fmt.Errorf("engine: read the chart before seeding it: %w", err)
	}
	if len(held.Units) > 0 || len(held.Seats) > 0 {
		return nil
	}

	authored := config.AuthoredChart(cfg)
	edges := authored.Edges()
	if len(edges) == 0 {
		// A COMPANY WITH NO UNITS AND NO SEATS is a real authoring
		// state — an operator who has written their providers and not
		// their people — and importing nothing would write a ledger row
		// saying an empty structure had landed, which the next edited
		// file would then have to be distinguished from.
		return nil
	}
	// THE DOMAIN'S OWN KEY, which `crewlet config import` computes
	// the same way: two importers of one file must agree, or each
	// rewrites every row the other already wrote.
	revision := chart.ImportKey(authored)

	// THE OP ID IS THE REVISION, so a retry of a seed that published and
	// then lost its answer is the same operation rather than a second one.
	// The ledger makes the APPLY idempotent on every node; this makes the
	// PUBLISH idempotent on this one.
	result, err := writer.WriteImport(ctx, "seed:"+revision, revision, edges)
	if err != nil {
		return fmt.Errorf("engine: seed the org chart from the company file: %w", err)
	}
	switch result.Outcome {
	case statelog.OutcomeApplied, statelog.OutcomePending:
	default:
		// UNKNOWN IS NOT A FAILURE HERE. The broker may have taken the
		// record and not answered, and the ledger is what decides — so a
		// boot that refused on this would refuse on a seed that landed.
		log.WarnContext(ctx, "chart_seed_unknown", "revision", revision,
			"detail", "the broker did not say whether the seed landed; the "+
				"import ledger makes a re-publish a no-op, so the next boot "+
				"settles it")
	}

	// AND THEN THE CONTENT, one record per object.
	//
	// THE IMPORT CARRIES STRUCTURE ONLY — where each object sits and who
	// leads each unit — because that is the one thing that has to be
	// arbitrated as a whole: it is one graph, and a cycle has to be
	// impossible rather than merely detectable. Everything else about an
	// object is its own, contends with nothing, and is a record on its own
	// subject.
	//
	// So a seeded chart without this half is a company of empty seats:
	// every handle in the right unit, with no model, no credentials and no
	// name. The two halves are ONE gesture from an operator's point of
	// view, which is why a failure in the second is reported with the same
	// revision the first was keyed on.
	last, err := e.seedContent(ctx, writer, authored, revision)
	if err != nil {
		return err
	}

	// AND THEN WAIT FOR THIS NODE TO APPLY ITS OWN SEED, by reading at a
	// FLOOR: the position the last record landed at.
	//
	// # Why the seed is the one read here that names a position
	//
	// Publishing is not applying. Every record above is on the log and the
	// applier reaches it a moment later, on its own goroutine — so the view
	// rebuild that runs next reads at the applier's cursor and finds an
	// EMPTY chart. The node then boots a company with no seats: it installs
	// its first epoch from that empty view, wires every integration against
	// a roster of nobody, and converges only when the periodic rebuild
	// fires. Measured, that was a node reporting `config_applied seats=0`
	// and `gitlab_wired seat_identities=0` on a company whose file has a
	// seat in it, and then serving that for up to half a minute.
	//
	// Everything else reads stale with no floor, for the reason
	// [Engine.rebuildChart] gives: what is being derived IS this node's
	// view, and asking the fleet whether it is caught up would put a round
	// trip on every committed record. This is not that question — the floor
	// is a position this node's own writes returned, so the wait is for its
	// own applier and for nothing else.
	//
	// A ZERO FLOOR WAITS FOR NOTHING, which is the honest answer when every
	// write came back `unknown`: there is no position to wait for, and the
	// import ledger settles it on the next boot.
	//
	// A FAILURE HERE IS NOT A FAILED SEED. The records are published and
	// the applier will reach them; what is lost is the boot's head start,
	// so it is reported and the boot continues.
	if _, err := e.Chart().Read(ctx, statelog.Freshness{
		Level: statelog.ReadStale, MinPosition: last,
	}); err != nil {
		log.WarnContext(ctx, "chart_seed_not_applied_yet", "revision", revision,
			"error", err,
			"detail", "the seed is published and this node has not applied it "+
				"yet, so its first epoch may carry no seats; the periodic "+
				"rebuild converges within 30s")
	}
	log.InfoContext(ctx, "chart_seeded", "revision", revision,
		"units", len(authored.Units), "seats", len(authored.Seats),
		"detail", "the company file's units and seats were published to the "+
			"chart log; the chart is no longer empty, so nothing seeds it again")
	return nil
}

// seedContent publishes each object's own content after the structure.
//
// # Why every record is keyed on the seed's revision
//
// The op ids are `seed:<revision>:<kind>:<key>`, which makes each one
// idempotent across a retry and across two nodes seeding at once: the
// operation ledger collapses the second publish of an id it has already
// applied. Without that, two nodes booting the same file at the same instant
// would write every seat twice — harmless in the rows, because the second is
// identical, and not harmless in the history, which would read as somebody
// having edited every seat in the company.
//
// # A failure here stops rather than continuing
//
// A partial content seed is a company where some seats can think and others
// cannot, which is worse than one where none can: the first looks like a
// working company with a few broken people in it. The error names the object
// it stopped on, and the next boot resumes — the ledger makes what landed a
// no-op, so there is nothing to undo.
func (e *Engine) seedContent(ctx context.Context, writer *chart.Writer,
	authored chart.Authored, revision string) (statelog.Position, error) {

	// THE FURTHEST POSITION ANY OF THESE REACHED, which is what the caller
	// waits on. The LAST one is not necessarily it — a write that came back
	// `unknown` carries a zero position — so the maximum is the only floor
	// that is true of every record this seed placed.
	var last statelog.Position
	reached := func(at statelog.Position) {
		if at.Packed() > last.Packed() {
			last = at
		}
	}

	for _, unit := range authored.Units {
		result, err := writer.WriteUnit(ctx, seedOpID(revision, "u", unit.Key),
			chart.UnitContent{
				Key: unit.Key, Name: unit.Name, Type: unit.Type,
				Purpose: unit.Purpose, Goals: unit.Goals,
				Channel: unit.Channel, Project: unit.Project,
				Space: unit.Space, KnowledgeRefs: unit.KnowledgeRefs,
				Runtime: unit.Runtime,
			})
		if err != nil {
			return last, fmt.Errorf("engine: seed the content of unit %q: %w",
				unit.Key, err)
		}
		reached(result.Position)
	}
	for _, seat := range authored.Seats {
		result, err := writer.WriteSeat(ctx, seedOpID(revision, "s", seat.Handle),
			chart.SeatContent{
				Handle: seat.Handle, Kind: seat.Kind, Unit: seat.Unit,
				Name: seat.Name, Email: seat.Email,
				Backstory: seat.Backstory, Goal: seat.Goal,
				Responsibilities:     seat.Responsibilities,
				BehavioralGuidelines: seat.BehavioralGuidelines,
				Manages:              seat.Manages,
				Project:              seat.Project, Space: seat.Space,
				Runtime: seat.Runtime,
			})
		if err != nil {
			return last, fmt.Errorf("engine: seed the content of seat %q: %w",
				seat.Handle, err)
		}
		reached(result.Position)
	}
	return last, nil
}

// seedOpID is one seeded object's operation id.
func seedOpID(revision, kind, key string) string {
	return "seed:" + revision + ":" + kind + ":" + chart.NormalizeKey(key)
}

// seedTimeout bounds the seed's publish.
//
// A BOOT-SHAPED BUDGET rather than the write path's own: this runs before the
// node serves anything, and a broker that has taken the record and gone quiet
// would otherwise hold the whole node up with no probe answering to say why.
// Thirty seconds is the same figure [jsprovision] gives a clustered create,
// which is the slowest thing that has already happened by the time this runs.
const seedTimeout = 30 * time.Second

// publishStagedChart publishes a chart an OFFLINE `crewlet config import`
// left in this node's own database, and clears it.
//
// # Why this exists at all
//
// `crewlet config import` writes both halves of a company file when it can
// reach a running node. With the engine STOPPED it can write only one: a
// revision is a row in the node's database, and a chart is a record on an
// ordered log that needs a broker no command-line process opens. So the
// offline route STAGES the chart, and this is where the stage is redeemed.
//
// # It is NOT the seed, and the difference is the whole point
//
// [Engine.seedChart] runs only while the chart is EMPTY: it is a first
// deployment's bootstrap, and a file that re-seeded a live company would
// revert every hire made since. A stage is the opposite — an operator's
// explicit "this file is the chart again", performed at a stopped node — so
// it publishes whatever the chart currently holds, exactly as the API route
// would have.
//
// # Published once, and safely more than once
//
// The stage is TAKEN in one transaction before the publish, so the ordinary
// path writes one record. A crash between the take and the publish loses the
// stage, which is why the take happens first: losing it costs an operator a
// re-run of a command they still have, where publishing it twice would write
// a second record on the subject every structural write in the company
// serialises behind. The import ledger absorbs the duplicate either way.
func (e *Engine) publishStagedChart(ctx context.Context) error {
	writer := e.ChartWriter()
	if writer == nil || e.backends == nil || e.backends.Store == nil {
		return nil
	}
	staged, found, err := e.backends.Store.StagedCharts().Take(ctx)
	if err != nil || !found {
		return err
	}
	body, err := secrets.Open(e.cipher, staged.Payload)
	if err != nil {
		return fmt.Errorf("engine: open the chart staged from %s: %w",
			staged.SourcePath, err)
	}
	var authored chart.Authored
	if err := json.Unmarshal(body, &authored); err != nil {
		return fmt.Errorf("engine: decode the chart staged from %s: %w",
			staged.SourcePath, err)
	}
	edges := authored.Edges()
	if len(edges) == 0 {
		return nil
	}
	// THE OP ID IS THE KEY, so a retry of a publish that lost its answer is
	// the same operation rather than a second one.
	result, err := writer.WriteImport(ctx, "staged:"+staged.ID, staged.ID, edges)
	if err != nil {
		return fmt.Errorf("engine: publish the chart staged from %s: %w",
			staged.SourcePath, err)
	}
	last, err := e.seedContent(ctx, writer, authored, staged.ID)
	if err != nil {
		return err
	}
	if last.Seq == 0 {
		last = result.Position
	}
	log.InfoContext(ctx, "chart_staged_published",
		"source", staged.SourcePath, "key", staged.ID,
		"units", len(authored.Units), "seats", len(authored.Seats),
		"position", last.String(), "staged_by", staged.StagedBy)
	// AND WAIT FOR THIS NODE'S OWN APPLIER, at the floor the last record
	// landed at — [Engine.seedChart]'s own reason, one gesture along: the
	// epoch installed next reads at the applier's cursor, and without the
	// wait it reads a chart this publish has not reached.
	if _, err := e.Chart().Read(ctx, statelog.Freshness{
		Level: statelog.ReadStale, MinPosition: last,
	}); err != nil {
		log.WarnContext(ctx, "chart_staged_not_applied_yet", "key", staged.ID,
			"error", err,
			"detail", "the staged chart is published and this node has not "+
				"applied it yet; the periodic rebuild converges within 30s")
	}
	return nil
}

// publishStagedChartAtBoot is [Engine.publishStagedChart] under the seed's own
// deadline, and it never fails the boot — for [Engine.seedChartAtBoot]'s
// reason, and with the same requirement that it never pass silently.
func (e *Engine) publishStagedChartAtBoot(ctx context.Context) {
	at, cancel := context.WithTimeout(ctx, seedTimeout)
	defer cancel()
	if err := e.publishStagedChart(at); err != nil {
		log.WarnContext(ctx, "chart_staged_publish_failed", "error", err,
			"detail", "this node could not publish the org chart an offline "+
				"`crewlet config import` staged for it. The stage is spent, so "+
				"re-run that import — against a running node this time, which "+
				"publishes it directly")
	}
}

// PublishStagedChartForTest redeems a stage from a test in this package's
// external suite.
//
// EXPORTED FOR THE SUITE and nowhere else: the boot calls
// [Engine.publishStagedChartAtBoot], which swallows every failure by design,
// so a case driving the boot could not tell a publish that worked from one
// that warned. A test that asserted the LOG LINE instead would be asserting
// the wording rather than the behaviour.
func (e *Engine) PublishStagedChartForTest(ctx context.Context) error {
	return e.publishStagedChart(ctx)
}

// seedChartAtBoot is [Engine.seedChart] under its own deadline, and it never
// fails the boot.
//
// # Why a failure is a warning rather than a refusal
//
// A node that cannot publish the seed still has to come up. Its peers may
// already hold the chart — the ledger would make this a no-op anyway — and the
// alternative is a fleet that cannot start because one node's broker was slow.
// What it must NOT do is pass silently: a company whose chart never landed has
// no seats, and this line is the only thing that says which of the two
// happened.
func (e *Engine) seedChartAtBoot(ctx context.Context, cfg *config.Company) {
	seed, cancel := context.WithTimeout(ctx, seedTimeout)
	defer cancel()
	if err := e.seedChart(seed, cfg); err != nil {
		log.WarnContext(ctx, "chart_seed_failed", "error", err,
			"detail", "this node could not publish the company file's org "+
				"chart. If no peer has published it either, the company has "+
				"no seats: check the broker and re-run, or publish the chart "+
				"from a node that can reach it")
	}
}
