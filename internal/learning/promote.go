package learning

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Cross-agent promotion: what a whole team worked out, written where a person
// can read it.
//
// # Why this is a knowledge-base page and not a skill row
//
// Every other skill here is AGENT-SCOPE: one seat's row, in one seat's
// catalogue, loaded into one seat's prompt. That is deliberate — a skill is a
// procedure a particular seat follows, and there is no unit-scope skill table
// because a procedure four seats share is a procedure the TEAM has, which
// makes it documentation rather than per-agent memory.
//
// So the output is a DRAFT PAGE under the unit's `Auto-Drafted Skills`
// parent, which the turn-start knowledge search deliberately excludes: an
// unvetted draft never reaches another agent. A unit lead reviews it and
// publishes by moving it out of that parent, at which point it is an ordinary
// knowledge-base page every seat can find. Nothing here promotes itself past
// a person.
//
// # What "cross-agent" has to mean
//
// DISTINCT SEATS, not skills. One seat that drafted four near-identical
// skills is a catalogue that needs curating, not a team convergence — and
// counting rows rather than owners would promote it, presenting one agent's
// habit as what the unit does. `min_sibling_count` is a count of AGENTS.
//
// # A convergence is drafted once, and the fleet remembers it
//
// The pass re-clusters the same persisted skills every tick, so a convergence
// it drafted yesterday is in front of it again today, and so is one a lead
// rejected. What stops it drafting either again is the [PromotionLedger]: one
// record per convergence, in the coordination store because the pass is a
// fleet singleton on a lease and the next holder of the duty has to see what
// the last one did. The ledger is read BEFORE any model call, so a unit whose
// convergences all have a record costs one coordination read and nothing
// else.
//
// A RECORD IS MATCHED BY WHAT THE CONVERGENCE IS, not by what anybody called
// it. The model names a draft, and at the temperature it runs at a renamed
// answer is a second draft of the same procedure, so a name is no identity. The
// cluster itself has none either: pooling is greedy, so the skill that leads a
// cluster and the set of skills in it move whenever a seat writes one or the
// curator archives one — which is exactly while a team is converging, when a
// rejection must hold. What survives re-clustering is the question pooling asks, "is
// this the same kind of work": a record carries the tool set of the cluster
// it was written for, and it stands for any later cluster whose leading tool
// run is within `jaccard_threshold` of that set — the measure the pool itself
// was formed with, so the two cannot disagree about what one convergence is.
// The FINGERPRINT, a digest of that tool set, is only the record's address.
//
// And the pool is formed OLDEST SKILL FIRST, so a cluster's leader is the
// first skill anybody wrote of the procedure and a seat that later writes
// another joins that cluster rather than displacing its leader.
//
// A RECORD IS IN ONE OF FOUR STATES. Drafting: the pass has decided what the
// page says and has not yet seen the page it made, so the next pass makes it
// from the record, with no model call. Drafted: the page exists, and the next
// pass asks its knowledge base whether a lead rejected it. Rejected: a lead
// did, by the gesture the draft's own page describes ([PromotionWriter]). And
// declined: the model looked and found no shared procedure, which holds until
// more seats have converged than it saw — a decline answered the evidence it
// was shown, and paying again for the same evidence buys the same answer.
//
// NO RECORD EXPIRES (see [coord.Promotions]). A rejection holds for the life
// of the deployment, and so does a draft for as long as the company drafts
// into the knowledge base the draft is in, because each is a decision about a
// procedure rather than an observation that goes stale. What ends one is the
// procedure changing: seats whose tools drift past the threshold are a
// different convergence, and that one is drafted.
//
// # The knobs this closes
//
// `skill_promotion.enabled`, `min_sibling_count`, `jaccard_threshold` and
// `budget_tokens` all validated, shipped in the example company, and had no
// reader outside internal/config: `Skills.ListFor` existed for this pass and
// had no caller anywhere.

// PromotionInterval is how often the promotion pass runs.
//
// DAILY, unlike the clustering pass's hourly tick, and for a reason the
// cadences make plain: clustering waits for one seat to repeat itself, which
// can happen in an afternoon, while this waits for THREE SEATS to
// independently arrive at the same procedure — a thing that takes weeks. An
// hourly tick would pay a catalogue scan per unit per hour to reach the same
// answer twenty-four times.
const PromotionInterval = 24 * time.Hour

// DefaultPromotionTokens caps one promotion draft.
//
// Larger than a single-turn synthesis draft, because the prompt carries
// several agents' whole procedures and the answer is a page a person reads
// rather than a body a model follows.
const DefaultPromotionTokens = 4000

// DefaultMinSiblings is how many distinct seats must converge. Below three it
// is a coincidence — two seats sharing a procedure is as likely to be two
// seats copying one trigger as it is a team practice.
const DefaultMinSiblings = 3

// DefaultPromotionJaccard pools two seats' skills. Deliberately the same
// value clustering uses: the question is identical — "is this the same kind
// of work" — and two constants for one question drift.
const DefaultPromotionJaccard = 0.6

// PromotionUnit is one unit the pass considers, resolved fresh per tick.
//
// CONTAINER RESOLVED BY THE CALLER, not by the writer: which space or project
// a unit files into is a fact about the ORG, and an integration package that
// read it would have to know what a unit is. The writer's job is to create a
// page in a container it is handed.
type PromotionUnit struct {
	// ID is the unit's name, which is its stable identity in the event and
	// the address its records are filed under in the [PromotionLedger].
	ID string

	// Lead is a representative role for the unit — its lead — so a
	// dashboard can file the event somewhere. Not an author: a promotion
	// has several.
	Lead *org.Role

	// Handles are the unit's agent seats, whose catalogues are pooled.
	Handles []string

	// Container is the unit's configured knowledge container — its `space`,
	// which is a Confluence space key or a container of the engine's own
	// pages, whichever knowledge base the company runs. Empty when it has
	// none.
	Container string

	// Hint names what an operator must set when Container is empty. A unit
	// with nowhere to file is SOFT-SKIPPED with this in the log rather than
	// failing the pass: a company that configured knowledge for one team
	// and not another is a supported state, and a hard failure would stop
	// the configured team's promotions too.
	Hint string
}

// PromotionWriter creates the draft in whichever knowledge base the company
// runs, and says what a lead did with it.
//
// THE GESTURE AND ITS CHECK ARE ONE TYPE'S. How a lead rejects a draft
// depends on what the knowledge base lets a person do to a page, and the
// sentence the draft carries to say so ([PromotionWriter.Rejection]) and the
// read that recognises it ([PromotionWriter.Rejected]) are declared side by
// side so the two cannot describe different gestures.
type PromotionWriter interface {
	// Backend names the knowledge base this writer drafts into, in the
	// words its knowledge search reports ([knowledge.Searcher]'s Backend).
	// A record carries it, because a draft in a knowledge base the company
	// has since left is one nobody reviews, and its page id means nothing
	// to the writer the company uses now.
	Backend() string

	// Rejection is how a lead rejects a draft here, as the clause a draft's
	// page completes its sentence with: "To reject it, " + Rejection() + ".".
	Rejection() string

	// CreateDraft creates the draft under the container's auto-drafted
	// parent, or returns the page already there under this title.
	//
	// The bool reports whether this call CREATED it: a pass announces
	// SkillPromoted only for a page it made, and a page it found is either
	// one an earlier attempt made or one somebody else holds the title of.
	CreateDraft(ctx context.Context, container, name, markdown string) (knowledge.DraftPage, bool, error)

	// Rejected reports whether a lead has rejected a draft this writer
	// created, and how, in words for the log: empty while the draft stands,
	// whether it is still under review or has been published.
	//
	// AN ERROR IS NEVER A REJECTION. A draft the knowledge base could not be
	// asked about is one nothing can be concluded about, and a pass that read
	// an outage as a rejection would record, for good, a decision nobody
	// made.
	Rejected(ctx context.Context, container, pageID string) (string, error)
}

// PromotionLedger is the fleet's record of the convergences this pass has
// acted on: [coord.Promotions], which the coordination store serves.
type PromotionLedger interface {
	Promotions(ctx context.Context, unit string) ([]coord.PromotionRecord, error)
	CreatePromotion(ctx context.Context, rec coord.PromotionRecord) (coord.PromotionRecord, bool, error)
	UpdatePromotion(ctx context.Context, rec coord.PromotionRecord) (coord.PromotionRecord, bool, error)
}

// PromotionUnits lists the units a pass walks, read fresh each tick.
//
// A FUNCTION for the same reason the background roster is one: an apply
// changes the org, and a pass holding the units it started with would keep
// promoting into a space the company has moved off.
type PromotionUnits func() []PromotionUnit

// PromotionWriterFor resolves the knowledge base to draft into, or nil and why
// there is none.
//
// A FUNCTION, for the reason [PromotionUnits] is one: the answer is a
// function of the live epoch, and a writer captured when the pass was built
// is a writer captured before the knowledge backend was wired — the
// background passes are armed after the node exists and BEFORE the inbound
// service builds its integration clients.
//
// Nil is an ordinary answer and the pass says so once rather than failing.
// THE REASON TRAVELS WITH IT because only the resolver knows which reason it
// is: a company that runs no knowledge base, one whose knowledge base this
// node is not serving, and one whose connection did not build are three
// different things for an operator to fix, and a pass that guessed would send
// the second and the third to configure a setting that is already right.
type PromotionWriterFor func() (PromotionWriter, string)

// Promoter distils what several seats in a unit independently learned.
type Promoter struct {
	writer PromotionWriterFor
	ledger PromotionLedger
	skills *Skills
	models Models
	units  PromotionUnits

	minSiblings int
	poolAt      float64
	timeout     time.Duration
	maxTokens   int
}

// PromoterOptions configures the pass. Zero values take the shipped defaults,
// which are the numbers config.DefaultLearning carries.
type PromoterOptions struct {
	Writer PromotionWriterFor
	Ledger PromotionLedger
	Skills *Skills
	Models Models
	Units  PromotionUnits

	// MinSiblings is how many DISTINCT seats must converge; zero takes
	// DefaultMinSiblings.
	MinSiblings int

	// JaccardThreshold pools two seats' skills, and matches a convergence
	// to the ledger's records; zero takes DefaultPromotionJaccard.
	JaccardThreshold float64

	// CallTimeout bounds one auxiliary call; zero takes DefaultAuxTimeout.
	CallTimeout time.Duration

	// MaxTokens caps one promotion draft; zero takes
	// DefaultPromotionTokens.
	MaxTokens int
}

// NewPromoter builds the pass.
func NewPromoter(opts PromoterOptions) (*Promoter, error) {
	switch {
	case opts.Writer == nil:
		// The RESOLVER is missing, which is a wiring mistake rather than a
		// configuration one: a company with no knowledge base has a
		// resolver that answers nil, and the pass reports that itself.
		return nil, fmt.Errorf("learning: skill promotion needs a writer resolver")
	case opts.Ledger == nil:
		// Without it every tick drafts again what the last one drafted,
		// and what a lead rejected.
		return nil, fmt.Errorf("learning: skill promotion needs the fleet's promotion ledger")
	case opts.Skills == nil:
		return nil, fmt.Errorf("learning: skill promotion needs a skill store to read")
	case opts.Models == nil:
		return nil, fmt.Errorf("learning: skill promotion needs a model registry")
	case opts.Units == nil:
		return nil, fmt.Errorf("learning: skill promotion needs the company's units")
	}
	p := &Promoter{
		writer: opts.Writer, ledger: opts.Ledger, skills: opts.Skills,
		models: opts.Models, units: opts.Units,
		minSiblings: opts.MinSiblings, poolAt: opts.JaccardThreshold,
		timeout: opts.CallTimeout, maxTokens: opts.MaxTokens,
	}
	if p.minSiblings <= 0 {
		p.minSiblings = DefaultMinSiblings
	}
	if p.poolAt <= 0 {
		p.poolAt = DefaultPromotionJaccard
	}
	if p.timeout <= 0 {
		p.timeout = DefaultAuxTimeout
	}
	if p.maxTokens <= 0 {
		p.maxTokens = DefaultPromotionTokens
	}
	return p, nil
}

// Pass walks every unit, writing at most one draft in each.
//
// Returns the events to publish. A unit whose write FAILED contributes
// nothing and is not an error the pass reports upward: the next tick
// re-clusters the same rows and tries again, which is the retry, and failing
// the pass would take the units after it down with the one that broke.
func (p *Promoter) Pass(ctx context.Context) []events.Payload {
	// RESOLVED ONCE, before the walk: which knowledge base a company drafts
	// into cannot change inside one pass, and asking per unit would ask the
	// engine for the same answer once per team.
	writer, unserved := p.writer()
	if writer == nil {
		log.InfoContext(ctx, "skill_promotion_idle",
			"reason", unserved,
			"detail", "a promoted skill is a draft page a unit lead reviews, "+
				"and there is nowhere to put one; the reason says what to "+
				"fix, or set learning.skill_promotion.enabled: false")
		return nil
	}
	var out []events.Payload
	for _, unit := range p.units() {
		if ctx.Err() != nil {
			return out
		}
		payload, err := p.promoteUnit(ctx, writer, unit)
		if err != nil {
			log.WarnContext(ctx, "skill_promotion_failed", "unit", unit.ID, "error", err.Error())
			continue
		}
		if payload != nil {
			out = append(out, payload)
		}
	}
	return out
}

// promoteUnit drafts the widest convergence in one unit that the ledger holds
// no record for, or reports why there is none.
func (p *Promoter) promoteUnit(ctx context.Context, writer PromotionWriter, unit PromotionUnit) (events.Payload, error) {
	if len(unit.Handles) < p.minSiblings {
		// Fewer seats than the threshold, so no cluster in this unit can
		// ever reach it. Checked before the catalogue read because it is
		// free and the read is not.
		return nil, nil
	}
	if unit.Container == "" {
		// SOFT SKIP, with the remediation in the log. A company that
		// configured knowledge for one team and not another is supported,
		// and failing here would stop the configured team's promotions.
		log.InfoContext(ctx, "skill_promotion_skipped", "unit", unit.ID,
			"reason", "no_knowledge_container", "detail", unit.Hint)
		return nil, nil
	}

	skills, err := p.skills.ListFor(ctx, unit.Handles, ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing %s's skills: %w", unit.ID, err)
	}
	clusters := poolSiblings(skills, p.poolAt)
	candidates := converging(clusters, p.minSiblings)
	if len(candidates) == 0 {
		log.DebugContext(ctx, "skill_promotion_found_nothing", "unit", unit.ID,
			"skills", len(skills), "clusters", len(clusters),
			"min_siblings", p.minSiblings)
		return nil, nil
	}

	// THE LEDGER BEFORE ANY MODEL CALL, and a ledger that cannot be read
	// drafts nothing: "no record" is the answer that pays for a draft of a
	// procedure a lead rejected.
	held, err := p.ledger.Promotions(ctx, unit.ID)
	if err != nil {
		return nil, fmt.Errorf("reading %s's promotion records: %w", unit.ID, err)
	}
	records, err := decodePromotions(held)
	if err != nil {
		return nil, fmt.Errorf("reading %s's promotion records: %w", unit.ID, err)
	}

	// A RECORD IS SETTLED ONCE A PASS, however many convergences it stands
	// for: a second settle would ask the knowledge base again and write at a
	// version the first one already moved.
	settled := map[string]bool{}
	for _, c := range candidates {
		standing := p.standingFor(records, c, writer.Backend())
		if len(standing) == 0 {
			return p.draft(ctx, writer, unit, c, records)
		}
		for _, rec := range standing {
			if settled[rec.fingerprint] {
				continue
			}
			settled[rec.fingerprint] = true
			payload, spent, err := p.settle(ctx, writer, unit, c, rec)
			if spent || err != nil {
				return payload, err
			}
		}
	}
	log.DebugContext(ctx, "skill_promotion_all_recorded", "unit", unit.ID,
		"convergences", len(candidates), "records", len(records),
		"detail", "every convergence here has been drafted, rejected or "+
			"declined, so nothing was drafted and no model was asked")
	return nil, nil
}

// standingFor is every record that stands for a convergence, so the pass
// drafts nothing for it.
//
// A record stands when its tool set is within the pooling threshold of the
// convergence's leading run and its state still answers the question:
//
//   - a REJECTION always does, whatever knowledge base the company runs now;
//     it is a person's decision about a procedure;
//   - a DECLINE does while no more seats have converged than the model saw;
//   - a DRAFT does while the company drafts into the knowledge base the draft
//     is in — one in a knowledge base it has left is a page nobody reviews;
//   - a record this build cannot read — a newer version, a state it does not
//     know — always does, because drafting over it would overwrite what a
//     newer build decided.
func (p *Promoter) standingFor(records []promotionEntry, c SiblingCluster, backend string) []promotionEntry {
	var out []promotionEntry
	for _, rec := range records {
		if toolJaccard(rec.Tools, c.Sequence) < p.poolAt {
			continue
		}
		switch {
		case !rec.readable():
			out = append(out, rec)
		case rec.State == promotionRejected:
			out = append(out, rec)
		case rec.State == promotionDeclined:
			if c.DistinctAgents() <= rec.Agents {
				out = append(out, rec)
			}
		case rec.Backend == backend:
			out = append(out, rec)
		}
	}
	return out
}

// settle brings one standing record up to date, reporting whether that spent
// this unit's one write for the tick.
func (p *Promoter) settle(ctx context.Context, writer PromotionWriter, unit PromotionUnit,
	c SiblingCluster, rec promotionEntry) (events.Payload, bool, error) {

	if !rec.readable() {
		return nil, false, nil
	}
	switch rec.State {
	case promotionDrafting:
		payload, err := p.finish(ctx, writer, unit, c, rec)
		return payload, true, err
	case promotionDrafted:
		p.observe(ctx, writer, unit, rec)
	}
	return nil, false, nil
}

// observe asks the knowledge base whether a lead rejected a draft, and records
// it when one did.
//
// Nothing a lead did is lost when this fails: the draft's record stands for
// the convergence either way, so it is not drafted again, and the next pass
// asks again.
func (p *Promoter) observe(ctx context.Context, writer PromotionWriter, unit PromotionUnit, rec promotionEntry) {
	if rec.PageID == "" {
		return
	}
	how, err := writer.Rejected(ctx, rec.Container, rec.PageID)
	if err != nil {
		log.WarnContext(ctx, "skill_promotion_draft_unread", "unit", unit.ID,
			"page_id", rec.PageID, "title", rec.Title, "error", err.Error(),
			"detail", "whether a lead rejected this draft is not known until a "+
				"pass can ask; it is not drafted again meanwhile")
		return
	}
	if how == "" {
		return
	}
	rec.State, rec.Rejection, rec.At = promotionRejected, how, time.Now().UTC()
	if ok, err := p.write(ctx, unit, rec); err != nil || !ok {
		log.WarnContext(ctx, "skill_promotion_rejection_unrecorded", "unit", unit.ID,
			"page_id", rec.PageID, "title", rec.Title, "error", errString(err),
			"detail", "the next pass records it; until then the draft's own "+
				"record keeps it from being drafted again")
		return
	}
	log.InfoContext(ctx, "skill_promotion_rejected", "unit", unit.ID,
		"page_id", rec.PageID, "title", rec.Title, "container", rec.Container,
		"how", how)
}

// draft asks the model for the shared procedure of one convergence nothing
// stands for, files what it answered, and makes the page.
func (p *Promoter) draft(ctx context.Context, writer PromotionWriter, unit PromotionUnit,
	c SiblingCluster, records []promotionEntry) (events.Payload, error) {

	tools := convergenceTools(c.Sequence)
	fingerprint := promotionFingerprint(tools)
	// THE RECORD AT THIS ADDRESS, when there is one, is one this build read
	// as standing for nothing any more — a decline more seats have since
	// outgrown, or a draft in a knowledge base the company has left — and
	// it is replaced at the version it was read at, so a holder of the duty
	// that raced this one loses. One this build cannot read is left alone:
	// its tools may be where this build does not look for them, and writing
	// over it would erase what a newer build decided.
	var prior *promotionEntry
	for i := range records {
		if records[i].fingerprint == fingerprint {
			prior = &records[i]
		}
	}
	if prior != nil && !prior.readable() {
		log.DebugContext(ctx, "skill_promotion_record_unreadable", "unit", unit.ID,
			"fingerprint", fingerprint, "version", prior.V, "state", string(prior.State))
		return nil, nil
	}

	member, err := auxiliary(p.models, unit.Lead, Attribution{Worker: PromotionWorker})
	if err != nil {
		return nil, fmt.Errorf("no auxiliary model for promotion: %w", err)
	}
	call, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	completion, err := member.Provider.Complete(call, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: PromotionSystemPrompt},
			{Role: llm.RoleUser, Content: buildPromotionPrompt(unit, c)},
		},
		Temperature: llm.Temp(auxTemperature),
		MaxTokens:   p.maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("drafting a promotion for %s: %w", unit.ID, err)
	}
	now := time.Now().UTC()

	draft, ok := parseSkillDraft(ctx, completion)
	if !ok {
		// The model looked at several seats' near-identical procedures and
		// could not name a shared one. Rare and still not an error — and
		// RECORDED, so the same evidence is not paid for again tomorrow.
		declined := promotionEntry{V: promotionEntryVersion, State: promotionDeclined,
			Tools: tools, Agents: c.DistinctAgents(), At: now}
		if _, filed, fileErr := p.file(ctx, unit, fingerprint, declined, prior); fileErr != nil || !filed {
			log.WarnContext(ctx, "skill_promotion_decline_unrecorded", "unit", unit.ID,
				"error", errString(fileErr),
				"detail", "the next pass asks the model about this convergence again")
		}
		log.DebugContext(ctx, "skill_promotion_declined", "unit", unit.ID,
			"distinct_agents", c.DistinctAgents())
		return nil, nil
	}

	// THE PREFIX IS THE BACKSTOP THAT HIDES IT. The knowledge search leaves
	// out every page under the auto-drafted parent, and where a backend
	// could not read a hit's parent chain it leaves out a hit titled with
	// the prefix ([knowledge.Excludes]) — so a draft without it would reach
	// every agent in the company unreviewed on exactly that answer.
	title := knowledge.AutoDraftTitlePrefix + draft.Name
	drafting := promotionEntry{V: promotionEntryVersion, State: promotionDrafting,
		Tools: tools, Agents: c.DistinctAgents(), Backend: writer.Backend(),
		Container: unit.Container, Title: title,
		Body: renderPromotion(unit, c, draft, writer.Rejection()), At: now}
	// FILED BEFORE THE PAGE IS MADE. A draft that lands with no record is a
	// convergence the next pass pays for again, and names again — possibly
	// differently, which is a second page. Filed first, a failure between
	// the two leaves a record the next pass finishes with no model call.
	stored, ok, err := p.file(ctx, unit, fingerprint, drafting, prior)
	if err != nil {
		return nil, fmt.Errorf("recording the draft %q for %s before making it: %w",
			title, unit.ID, err)
	}
	if !ok {
		log.InfoContext(ctx, "skill_promotion_raced", "unit", unit.ID, "title", title,
			"detail", "another holder of the promotion duty recorded this "+
				"convergence first; the next pass reads what it recorded")
		return nil, nil
	}
	return p.finish(ctx, writer, unit, c, stored)
}

// finish makes the page a drafting record describes, and records the page.
func (p *Promoter) finish(ctx context.Context, writer PromotionWriter, unit PromotionUnit,
	c SiblingCluster, rec promotionEntry) (events.Payload, error) {

	page, created, err := writer.CreateDraft(ctx, rec.Container, rec.Title, rec.Body)
	if err != nil {
		return nil, fmt.Errorf("drafting %q into %s: %w", rec.Title, rec.Container, err)
	}
	rec.State, rec.PageID, rec.Body, rec.At = promotionDrafted, page.ID, "", time.Now().UTC()
	if ok, err := p.write(ctx, unit, rec); err != nil || !ok {
		// THE PAGE EXISTS and the record still says drafting, so the next
		// pass finishes it again: the writer finds the page by its title
		// and reports it not created, and only the record moves.
		log.WarnContext(ctx, "skill_promotion_page_unrecorded", "unit", unit.ID,
			"page_id", page.ID, "title", page.Title, "error", errString(err))
	}
	if !created {
		// A page already holds this title — made by an earlier attempt
		// whose record did not move, or somebody else's. Announcing it
		// would put a promotion in the feed that this pass did not make.
		log.DebugContext(ctx, "skill_promotion_already_drafted", "unit", unit.ID,
			"page_id", page.ID, "title", page.Title)
		return nil, nil
	}

	log.InfoContext(ctx, "skill_promoted", "unit", unit.ID, "title", page.Title,
		"page_id", page.ID, "container", rec.Container,
		"siblings", len(c.Skills), "distinct_agents", c.DistinctAgents())
	return types.SkillPromoted{
		RoleName:       seatName(unit.Lead),
		UnitID:         unit.ID,
		SkillName:      strings.TrimPrefix(rec.Title, knowledge.AutoDraftTitlePrefix),
		PageID:         page.ID,
		PageTitle:      page.Title,
		ContainerKey:   rec.Container,
		SiblingCount:   len(c.Skills),
		DistinctAgents: c.DistinctAgents(),
	}, nil
}

// file writes a new record at a convergence's address — over the record that
// was there, at the version it was read at, when there was one.
func (p *Promoter) file(ctx context.Context, unit PromotionUnit, fingerprint string,
	rec promotionEntry, prior *promotionEntry) (promotionEntry, bool, error) {

	value, err := rec.encode()
	if err != nil {
		return promotionEntry{}, false, err
	}
	out := coord.PromotionRecord{Unit: unit.ID, Fingerprint: fingerprint, Value: value}
	var stored coord.PromotionRecord
	var ok bool
	if prior != nil {
		out.Version = prior.version
		stored, ok, err = p.ledger.UpdatePromotion(ctx, out)
	} else {
		stored, ok, err = p.ledger.CreatePromotion(ctx, out)
	}
	if err != nil || !ok {
		return promotionEntry{}, ok, err
	}
	rec.fingerprint, rec.version = fingerprint, stored.Version
	return rec, true, nil
}

// write moves one record to its next state, at the version it was read at,
// reporting false when that version no longer held.
//
// Each caller writes a record once and reads the ledger afresh on its next
// pass, so the version the write returns is not kept.
func (p *Promoter) write(ctx context.Context, unit PromotionUnit, rec promotionEntry) (bool, error) {
	value, err := rec.encode()
	if err != nil {
		return false, err
	}
	_, ok, err := p.ledger.UpdatePromotion(ctx, coord.PromotionRecord{
		Unit: unit.ID, Fingerprint: rec.fingerprint, Value: value, Version: rec.version,
	})
	return ok, err
}

// errString is an error's text for a log line, or empty for a lost race.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---- the ledger's records ---------------------------------------------- //

// promotionState is where one convergence's record stands. See the package
// doc's "A convergence is drafted once".
type promotionState string

const (
	promotionDrafting promotionState = "drafting"
	promotionDrafted  promotionState = "drafted"
	promotionRejected promotionState = "rejected"
	promotionDeclined promotionState = "declined"
)

// Valid reports whether s is a state this build knows.
func (s promotionState) Valid() bool {
	switch s {
	case promotionDrafting, promotionDrafted, promotionRejected, promotionDeclined:
		return true
	}
	return false
}

// promotionEntryVersion is the version of a record's value this build writes
// and fully reads.
const promotionEntryVersion = 1

// promotionEntry is one record's value: this package's vocabulary, which
// coordination stores without reading.
//
// SHARED BETWEEN BUILDS, so it is read losslessly: a field a newer build
// added is carried back on every write this build makes to the same record,
// and a record at a newer version, or in a state this build does not know,
// stands for its convergence and is never written by this build at all.
type promotionEntry struct {
	V     int            `json:"v"`
	State promotionState `json:"state"`

	// Tools is the tool set of the convergence the record was written for,
	// distinct and sorted: what a later convergence is matched against.
	Tools []string `json:"tools"`

	// Agents is how many distinct seats had converged when it was written.
	// A decline holds while no more have.
	Agents int `json:"agents"`

	// Backend is the knowledge base a draft was made in
	// ([PromotionWriter.Backend]); empty on a decline.
	Backend string `json:"backend,omitempty"`

	// Container, Title and PageID are where the draft is. PageID is empty
	// until the page is made.
	Container string `json:"container,omitempty"`
	Title     string `json:"title,omitempty"`
	PageID    string `json:"page_id,omitempty"`

	// Body is the draft's page, held only while it is being made so the
	// next pass can make it without asking the model again; the page holds
	// it after that.
	Body string `json:"body,omitempty"`

	// At is when the record entered its state.
	At time.Time `json:"at"`

	// Rejection is how a lead's rejection was seen, in the writer's words.
	Rejection string `json:"rejection,omitempty"`

	// carried is every field of the stored value this build does not know,
	// written back as it was read.
	carried map[string]json.RawMessage

	// fingerprint and version are the record's address and the store's
	// version of it as read.
	fingerprint string
	version     uint64
}

// promotionFields are the keys [promotionEntry] reads, which every write
// replaces rather than carries.
var promotionFields = []string{
	"v", "state", "tools", "agents", "backend", "container", "title",
	"page_id", "body", "at", "rejection",
}

// readable reports whether this build may act on the record: a version it
// writes, in a state it knows.
func (e promotionEntry) readable() bool {
	return e.V <= promotionEntryVersion && e.State.Valid()
}

// encode renders the record, with the fields a newer build wrote carried back.
func (e promotionEntry) encode() ([]byte, error) {
	type wire promotionEntry
	known, err := json.Marshal(wire(e))
	if err != nil {
		return nil, fmt.Errorf("learning: encode a promotion record: %w", err)
	}
	if len(e.carried) == 0 {
		return known, nil
	}
	merged := maps.Clone(e.carried)
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(known, &fields); err != nil {
		return nil, fmt.Errorf("learning: encode a promotion record: %w", err)
	}
	maps.Copy(merged, fields)
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("learning: encode a promotion record: %w", err)
	}
	return out, nil
}

// decodePromotions reads a unit's records.
//
// A RECORD THAT DOES NOT DECODE STOPS THE UNIT rather than being skipped: a
// record skipped is a convergence with no record, which the pass drafts — and
// the record it could not read may be a lead's rejection.
func decodePromotions(held []coord.PromotionRecord) ([]promotionEntry, error) {
	out := make([]promotionEntry, 0, len(held))
	for _, rec := range held {
		var entry promotionEntry
		if err := json.Unmarshal(rec.Value, &entry); err != nil {
			return nil, fmt.Errorf("the promotion record %s of unit %s does not "+
				"decode, and it may be a lead's rejection: %w",
				rec.Fingerprint, rec.Unit, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rec.Value, &fields); err != nil {
			return nil, fmt.Errorf("the promotion record %s of unit %s does not "+
				"decode: %w", rec.Fingerprint, rec.Unit, err)
		}
		for _, known := range promotionFields {
			delete(fields, known)
		}
		if len(fields) > 0 {
			entry.carried = fields
		}
		entry.fingerprint, entry.version = rec.Fingerprint, rec.Version
		out = append(out, entry)
	}
	return out, nil
}

// convergenceTools is a tool run as the set a convergence is judged by:
// distinct and sorted, because pooling compares SETS ([toolJaccard]) and a
// record compared by order would see two runs of the same tools as different
// work.
func convergenceTools(run []string) []string {
	out := slices.Clone(run)
	slices.Sort(out)
	return slices.Compact(out)
}

// promotionFingerprint is a record's address within its unit: a digest of the
// convergence's tool set. See the package doc for why it addresses a record
// and does not identify a convergence.
//
// Sixteen bytes of SHA-256, hex: a key segment the coordination grammar
// carries literally, with no collision a unit's handful of convergences could
// reach.
func promotionFingerprint(tools []string) string {
	sum := sha256.Sum256([]byte(strings.Join(tools, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// ---- the pool ---------------------------------------------------------- //

// SiblingCluster is one procedure several seats arrived at independently.
type SiblingCluster struct {
	// Sequence is the representative tool run: the cluster's leader's, which
	// is the first skill anybody in the unit wrote of this procedure.
	Sequence []string
	// Skills are the contributing agent-scope rows, oldest first.
	Skills []Skill
}

// DistinctAgents is how many different seats contributed.
//
// THE NUMBER THAT DECIDES, not len(Skills): one seat with four near-identical
// skills is a catalogue that needs curating, and promoting it would present
// one agent's habit as the team's practice.
func (c SiblingCluster) DistinctAgents() int {
	seen := map[string]struct{}{}
	for _, sk := range c.Skills {
		seen[sk.AgentHandle] = struct{}{}
	}
	return len(seen)
}

// poolSiblings groups a unit's skills by how similar their tool runs are.
//
// The same greedy single pass the episode clustering uses, over skills rather
// than turns: each skill joins the first cluster whose leader it is close
// enough to, or leads one.
//
// OLDEST FIRST, whatever order the skills arrive in. A cluster's leader is
// what the ledger's records are matched against, so it has to be the skill
// least likely to move: the first one written of the procedure. Newest first,
// every skill a seat wrote would take over the lead of its cluster, and a
// rejection matched against the leader it displaced could stop holding.
func poolSiblings(skills []Skill, threshold float64) []SiblingCluster {
	ordered := slices.Clone(skills)
	slices.SortStableFunc(ordered, func(a, b Skill) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	var clusters []SiblingCluster
	for _, sk := range ordered {
		if len(sk.ToolSequence) == 0 {
			// A skill with no recorded run cannot be compared to one, and
			// pooling it by name would group two procedures that share a
			// word. Skipped rather than made its own singleton cluster.
			continue
		}
		joined := false
		for i := range clusters {
			if toolJaccard(sk.ToolSequence, clusters[i].Sequence) >= threshold {
				clusters[i].Skills = append(clusters[i].Skills, sk)
				joined = true
				break
			}
		}
		if !joined {
			clusters = append(clusters, SiblingCluster{
				Sequence: slices.Clone(sk.ToolSequence),
				Skills:   []Skill{sk},
			})
		}
	}
	return clusters
}

// converging is every cluster at least minAgents distinct seats arrived at,
// the widest first.
//
// EVERY ONE, IN ORDER, rather than the widest alone. The pass drafts at most
// one per unit per tick — each is an auxiliary call plus a page write — and it
// walks this list past every convergence the ledger already stands for, so a
// unit with two gets the wider one drafted on one tick and the other on the
// next. The widest alone would be the same drafted cluster on every tick, and
// the other would never be reached.
func converging(clusters []SiblingCluster, minAgents int) []SiblingCluster {
	var out []SiblingCluster
	for _, c := range clusters {
		if c.DistinctAgents() >= minAgents {
			out = append(out, c)
		}
	}
	slices.SortStableFunc(out, func(a, b SiblingCluster) int {
		return cmp.Compare(b.DistinctAgents(), a.DistinctAgents())
	})
	return out
}

// ---- what the model and the lead read ---------------------------------- //

// promotionPromptSkills is how many of a cluster's skills reach the prompt.
//
// Four is enough to show what the seats agree on and where they differ, which
// is the whole question. Past that the bodies are repetition at prompt prices.
// The prompt says how many it left out; every contributing skill is named on
// the draft's own page under "Where it came from", and each is whole in its
// seat's catalogue.
const promotionPromptSkills = 4

// PromotionSystemPrompt asks for the practice a team shares.
const PromotionSystemPrompt = `Several agents on one team independently wrote
down nearly the same procedure. Write the version the team should share, as a
page a person will review before it is published.

Answer with a JSON object and nothing else:
{"name":"kebab-case-name","description":"one line on when to use it",
 "content":"the procedure, as numbered steps"}

Write what the agents AGREE on. Where they differ, say what the difference
depends on rather than picking one — a reviewer needs to see the choice. Keep
it general: no ticket numbers, no names, no dates, nothing about one agent.

Answer exactly {} if they do not actually share a procedure — similar tools do
not always mean the same work.`

// buildPromotionPrompt renders the converging skills for the model.
//
// ONE SKILL PER SEAT BEFORE ANY SEAT'S SECOND, because the question is what
// the SEATS agree on: a cluster in which one seat wrote several versions would
// otherwise fill the prompt with that seat and leave out the colleagues whose
// agreement is the evidence.
func buildPromotionPrompt(unit PromotionUnit, c SiblingCluster) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Team: %s\n%d agents wrote %d skills around this tool run:\n%s\n",
		unit.ID, c.DistinctAgents(), len(c.Skills), strings.Join(c.Sequence, " -> "))
	for i, sk := range seatFirst(c.Skills) {
		if i >= promotionPromptSkills {
			fmt.Fprintf(&b, "\n(and %d more, all similar)\n", len(c.Skills)-promotionPromptSkills)
			break
		}
		fmt.Fprintf(&b, "\n### %s (by %s)\n%s\n\n%s\n",
			sk.Name, sk.AgentHandle, sk.Description, sk.Content)
	}
	return b.String()
}

// seatFirst orders skills so each seat's first comes before any seat's
// second, keeping their order otherwise.
func seatFirst(skills []Skill) []Skill {
	out := make([]Skill, 0, len(skills))
	var rest []Skill
	seen := map[string]bool{}
	for _, sk := range skills {
		if seen[sk.AgentHandle] {
			rest = append(rest, sk)
			continue
		}
		seen[sk.AgentHandle] = true
		out = append(out, sk)
	}
	return append(out, rest...)
}

// renderPromotion is the page body, in markdown.
//
// THE PROVENANCE IS PART OF THE PAGE, not just of the event. A reviewer
// opening an auto-drafted page needs to know who converged on it and from
// what before deciding whether the team should adopt it — and the event that
// carried those numbers is in a feed they are not reading.
//
// SO IS WHAT THE REVIEWER CAN DO, in the knowledge base's own gesture
// (rejection, from [PromotionWriter.Rejection]), and what happens after: the
// page is the only place the reviewer is told, and a reviewer who believed a
// deleted draft would come back would have no reason to delete it.
func renderPromotion(unit PromotionUnit, c SiblingCluster, draft skillDraft, rejection string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "> **Auto-drafted, not reviewed.** %d agents in %s "+
		"independently arrived at this procedure. No agent's knowledge search "+
		"returns this page while it sits under %q.\n>\n",
		c.DistinctAgents(), unit.ID, knowledge.AutoDraftedParent)
	fmt.Fprintf(&b, "> **To adopt it**, move it out from under %q. "+
		"**To reject it**, %s. A rejection is recorded for the whole company, "+
		"and whatever you do with this page, the promotion pass does not draft "+
		"this procedure for %s in this knowledge base again.\n\n",
		knowledge.AutoDraftedParent, rejection, unit.ID)
	fmt.Fprintf(&b, "%s\n\n## Procedure\n\n%s\n\n## Where it came from\n\n",
		draft.Description, draft.Content)
	for _, sk := range c.Skills {
		fmt.Fprintf(&b, "- %q, by `%s`\n", sk.Name, sk.AgentHandle)
	}
	fmt.Fprintf(&b, "\nCommon tool run: `%s`\n", strings.Join(c.Sequence, " -> "))
	return b.String()
}
