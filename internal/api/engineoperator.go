package api

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tracker"
)

// engineOperatorWriter is the node's tracker writer acting as one operator.
//
// ONE HELPER FOR THE TEN SEAMS BELOW, because turning a tool-layer actor
// into a writer is a single rule and ten hand-copied spellings of it are
// ten chances for one seam to carry an identity the other nine do not —
// which is exactly what happened to [tracker.Provenance.Seat], the field that
// decides whose person record a write lands on.
func engineOperatorWriter(w *tracker.Writer, actor builtin.Actor) *tracker.Writer {
	// THE CREDENTIAL AND THE PERSON IT NAMES, which are two different
	// facts: the author stays the token, and the seat is only ever the
	// subject of that person's own state. Through the actor's own
	// provenance, the one construction a seat's writers use too, so the two
	// surfaces cannot carry different trails for one actor.
	return w.As(actor.Handle, actor.Kind, actor.Provenance())
}

// NewEngineOperator builds the operator surface — the one tool catalogue every
// operator transport serves — over a running engine.
//
// HERE, beside the seam it fills and beside [NewEngineRuntime], rather than in
// the command that wires the node, for that constructor's reason: `crewlet run`
// and the end-to-end suite must serve `/operator/act` and `/operator/mcp` from
// the same code. A harness that assembled its own catalogue would certify what
// it wired rather than what a node serves — and the replay's session floor is
// read off exactly this surface's answer.
//
// THE SAME DEPS A SEAT'S TOOLS GET, with one field different: the actor. That
// is what makes this one implementation of every tool rather than two — see
// [builtin.WorkDeps.Actor].
//
// The DEFAULTS are deliberately absent. A seat files into its unit's project
// when it names none, because a seat HAS a unit; an operator does not, so the
// argument is required and the tool refuses naming it rather than guessing a
// project on a person's behalf.
//
// Which UNIT the work is filed into is not a default of this surface and no
// longer needs one: the tracker reads it off the project's own row at the
// write, so an operator's item belongs to the team that owns the project it
// named. It used to be stamped from the caller's own team, which an operator
// has not got — so every item filed here read "Filed into: no unit" beside a
// project page naming its unit.
func NewEngineOperator(e *engine.Engine) (*operator.Server, error) {
	// EVERY CALL THAT MAY WRITE is audited onto this node's own queue, so
	// the event store here holds who did what through either transport.
	opts := operator.Options{Audit: e.Backends().Queue}
	// WHAT EACH NODE CAN CARRY OUT, read off the same lease table the seat
	// host heartbeats into, so a verb the node holding a seat has not been
	// upgraded to carry is refused rather than accepted and never done.
	opts.Fleet = coord.FeatureReader{Leases: e.Backends().Coord}
	if c := e.Company(); c != nil && c.Config != nil {
		opts.Company = c.Config.Name
	}
	// THE CHART, resolved per call because a config apply replaces it —
	// and wired UNCONDITIONALLY, which it was not. It used to be set only
	// where the company had a knowledge backend, because `search_knowledge`
	// was the only tool that read it. WHO THE CALLER IS reads it on every
	// call now: the same chart says which seat a token is bound to, and
	// without it a bound founder's own marks, pins and queue were written
	// under their credential's name instead of theirs.
	opts.Org = func() *org.Organization {
		c := e.Company()
		if c == nil {
			return nil
		}
		return c.Org
	}
	// A PARKED CODING RUN IS ANSWERABLE BY ITS TURN from here, on every
	// node and whatever this company's sandbox configuration: the record is
	// the fleet's, and the answer is carried out by the node holding the
	// run's seat, which reads it off that seat's inbox. The person answering
	// is the one the credential names, resolved as every other write here.
	opts.Runs = builtin.RunDeps{
		Desk: sandbox.AnswerDesk{
			Pending: sandbox.NewCoordStore(e.Backends().Fleet),
			Queue:   e.Backends().Queue,
		},
		Actor: operator.WorkActor(opts.Org),
	}
	// AND A SEAT IS PAUSED OR RESUMED from here, on every node: the pause
	// is the fleet's record, and each node holding or later acquiring the
	// seat carries it out from its own watched copy. The change is
	// announced onto this node's queue by whichever caller's write won.
	opts.Pauses = builtin.SeatPauseDeps{
		Pauses:   e.Backends().Fleet,
		Announce: e.Backends().Queue,
		Org:      opts.Org,
		Actor:    operator.WorkActor(opts.Org),
	}
	// AND A NOTE REACHES A RUNNING TURN from here, on every node: it is
	// scattered to the fleet and answered by the node running the turn,
	// which is the only one that can hand it to the turn's next round.
	opts.Steer = builtin.SteerDeps{
		Asker: e.Backends().Queue,
		Actor: operator.WorkActor(opts.Org),
	}
	if reader, writer := e.Tracker(), e.TrackerWriter(); reader != nil && writer != nil {
		opts.Work = builtin.WorkDeps{
			Reader: reader,
			// THE OPERATOR'S OWN CREDENTIAL IS THE PARTY, and it comes
			// from the request's context rather than from the call: a
			// tracker whose author field is chosen by the writer is not
			// an audit trail, and there is deliberately no way to name a
			// seat to act as.
			Writer: func(actor builtin.Actor) builtin.WorkWriter {
				return engineOperatorWriter(writer, actor)
			},
			// AND THE TWO SEQUENCES, which this surface went
			// without — so an operator's assistant was refused
			// `waiting_on` and `blocking` by name on a tool whose
			// own description offers them, and would not have been
			// served the fold at all. Both need the replicated
			// estate, which this writer has; nothing else about
			// them differs from a seat's.
			Dependencies: func(actor builtin.Actor) builtin.WorkDepender {
				return engineOperatorWriter(writer, actor)
			},
			Merges: func(actor builtin.Actor) builtin.WorkMerger {
				return engineOperatorWriter(writer, actor)
			},
			// AND THE CROSS-PROJECT MOVE, a sequence of its own: a
			// top-level item and its subtree re-keyed into another
			// project. A seat holds it too, behind the project lead's
			// gate; see internal/agent/builtin/workmove.go.
			Moves: func(actor builtin.Actor) builtin.WorkMover {
				return engineOperatorWriter(writer, actor)
			},
			// AND THE RANKED SEARCH. It reads, so it takes no actor —
			// the corpus is the same for everybody and there is nothing
			// to attribute — and without it the operator catalogue
			// listed a verb this surface could never register.
			Search: engine.WorkSearcher(e),
			// THE SAVED-VIEW WRITER, which only this surface has: a
			// view is furniture a person arranges, and no seat is
			// given the tools that reach it.
			ViewWriter: func(actor builtin.Actor) builtin.ViewWriter {
				return engineOperatorWriter(writer, actor)
			},
			// AND THE CATALOGUE WRITER: the company's own vocabulary is
			// a person's to set, never a seat's to widen so its own
			// create succeeds.
			CatalogueWriter: func(actor builtin.Actor) builtin.CatalogueWriter {
				return engineOperatorWriter(writer, actor)
			},
			// AND THE PERSON WRITER. Who may write what is the
			// tracker's own rule; what this surface supplies is the
			// identity it is judged against.
			PersonWriter: func(actor builtin.Actor) builtin.PersonWriter {
				return engineOperatorWriter(writer, actor)
			},
			// AND THE INBOX READ. It takes no actor for the reason
			// Search takes none — it reads, and whose inbox is an
			// argument rather than an identity — and it is this
			// surface's alone beside the person writer, because a
			// seat has a mailbox rather than an inbox.
			Inbox: reader,
			// AND THE TRASH. A removal takes an item off every board in
			// the company and a restore puts it back at any age; neither
			// destroys anything, which is what separates both from the
			// purge the CLI guards with a typed confirmation. No seat
			// holds either — see internal/agent/builtin/worktrash.go.
			TrashWriter: func(actor builtin.Actor) builtin.TrashWriter {
				return engineOperatorWriter(writer, actor)
			},
			// AND THE BOARD DRAG. A card's place in its project's order is
			// a person's arrangement, so no seat holds it either — see
			// internal/agent/builtin/workplace.go.
			Placer: func(actor builtin.Actor) builtin.WorkPlacer {
				return engineOperatorWriter(writer, actor)
			},
			// AND A PROJECT'S OWN SETTINGS. Unlike the five above,
			// this one is on every surface — declaring a tag is open
			// to every seat — and what an operator adds here is the
			// credential the archive facet asks for.
			ProjectWriter: func(actor builtin.Actor) builtin.ProjectWriter {
				return engineOperatorWriter(writer, actor)
			},
			// THE ROSTER, so an operator's assistant is refused a
			// handle nobody has rather than silently filing work for
			// one — the same check every seat's tools make.
			Seats: func() []colleague.Seat {
				c := e.Company()
				if c == nil {
					return nil
				}
				return builtin.Corpus(c.Org)
			},
			// AND THE PARTY BEHIND A HANDLE, which the roster above
			// deliberately does not carry: an operator reading a
			// person's inbox or their own state is answered about
			// BOTH the names that person's rows may be filed under,
			// and the credential is an attribution key rather than
			// somewhere an agent could mention them.
			Party: builtin.Parties(opts.Org),
			// AND THE THREE CHART SEAMS THE SEAT SURFACE HAS AND THIS
			// ONE WENT WITHOUT. Their absence was invisible and not
			// harmless: with no Leads, an operator filing an unassigned
			// task woke nobody at all — the lead fallback is what
			// catches exactly that task — and with no Units every
			// project this surface listed read as belonging to no team.
			Leads: engine.LiveLeads(e),
			Units: engine.LiveUnits(e),
			// A PERSON'S CREATE THAT NAMES NO PROJECT lands where their
			// seat's work does — the derivation every seat's surface and
			// the dashboard's `viewer` use, read per call against the
			// current chart because this surface outlives every apply.
			DefaultProject: engine.LiveDefaultProject(e),
			// THE SAME CHART DECIDES WHO IS WRITING: a token bound to
			// a human seat writes that PERSON's own state, while the
			// author on the record stays the token.
			Actor: operator.WorkActor(opts.Org),
			// THE MENTION RESOLVER, which this surface went without: a
			// comment's @-mention is turned into a wake by the tracker's
			// recipients only when the writer resolved it, so an
			// operator writing "@alice can you take this" reached her
			// watchers and never her — while the tool's own description,
			// which their assistant reads, promised it would.
			Mentions: engine.LiveMentions(e),
			// AND THE COMPANY'S CLOCK, which this surface went without
			// entirely: every `due=friday`, `due=overdue` and relative date
			// an operator's assistant wrote resolved on UTC while a seat's
			// own resolved on the company's zone. Read per call, because
			// this surface is built once and an apply can move the clock
			// (ADR-0018).
			Zone:  e.Zone,
			Await: e.WaitCommitted,
		}
	}
	if reader, writer := e.Pages(), e.PagesStore(); reader != nil && writer != nil {
		opts.Pages = builtin.PageDeps{
			Reader: reader, Writer: writer,
			Actor:    operator.PageActor,
			Mentions: engine.LiveMentions(e),
			Reserved: engineReserved(e),
			Await:    e.WaitCommitted,
		}
	}
	// SEARCH IS OFFERED WHENEVER THE COMPANY HAS A BACKEND, native or not:
	// unlike the ten write tools, ranked search over the company's own
	// wiki is exactly as useful to an operator's assistant on Confluence.
	if e.Knowledge() != nil {
		// THE CHART THE SEARCH IS SCOPED AGAINST is already wired
		// above, for every company rather than only this one.
		opts.Knowledge = engineKnowledge{engine: e}
		// AND A PERSON'S QUESTION ANSWERED FROM IT, on the auxiliary model
		// of their own seat and charged to the company's windows — a
		// person has no seat budget. The answers are cached at this node's
		// corpus position, which a company on an external wiki does not
		// have, so there they are never cached.
		opts.Answer = builtin.AnswerDeps{
			Models: engine.AnswerModels(e),
			Budget: engine.AnswerBudget(e),
			Corpus: e.KnowledgeCorpus,
			Actor:  operator.WorkActor(opts.Org),
		}
	}
	// THE LEAD RELATION, which the tracker deliberately does not derive:
	// it holds no org chart, and one it derived would be a second opinion
	// about the hierarchy.
	opts.Leads = engineLeads(e)
	// AND THE PROJECT'S OWN LEAD, which is a different question: one is
	// about a person's line, the other about who plans a container's work.
	opts.LeadsProject = engine.LeadsProjectOf(e)
	return operator.New(opts)
}

// engineLeads answers whether one handle leads another, walking the chart's own
// management chain.
//
// ANY ANCESTOR, not just the direct manager: a founder leads everybody, and an
// authority that stopped at one level would make "a lead may set what somebody
// in their line does next" mean "a lead may, for the people directly under
// them" — which is not what a line is.
//
// A HANDLE THIS BUILD CANNOT RESOLVE ANSWERS FALSE, which is the conservative
// direction: the write is then refused unless it is the person's own.
func engineLeads(e *engine.Engine) builtin.Leads {
	return func(_ context.Context, actor, handle string) bool {
		c := e.Company()
		if c == nil || c.Org == nil || actor == "" || actor == handle {
			return false
		}
		seat := c.Org.SeatByHandle(handle)
		if seat == nil {
			return false
		}
		for _, manager := range c.Org.Ancestors(seat) {
			if manager.Handle() == actor {
				return true
			}
		}
		return false
	}
}

// engineReserved is the containers an operator's assistant may not write to
// directly, on the same terms a seat has them.
func engineReserved(e *engine.Engine) []string {
	c := e.Company()
	if c == nil || c.Config == nil {
		return nil
	}
	var out []string
	for _, key := range []string{c.Config.SkillsContainerKey(), c.Config.RootSpaceKey()} {
		if key = strings.TrimSpace(key); key != "" {
			out = append(out, key)
		}
	}
	return out
}

// engineKnowledge resolves the node's searcher per call, for the reason the
// engine's own liveKnowledge does: an apply REPLACES it, and a value captured
// when the API was assembled searches with a rotated credential's predecessor.
type engineKnowledge struct{ engine *engine.Engine }

func (k engineKnowledge) CanSearch(seat *org.Role, o *org.Organization) bool {
	s := k.engine.Knowledge()
	return s != nil && s.CanSearch(seat, o)
}

func (k engineKnowledge) Search(ctx context.Context, q knowledge.Query) knowledge.Result {
	s := k.engine.Knowledge()
	if s == nil {
		return knowledge.Result{}
	}
	return s.Search(ctx, q)
}
