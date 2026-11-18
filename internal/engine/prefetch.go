package engine

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/version"
	"github.com/crewlet/crewlet/internal/worklet"
)

// What a turn is given before it starts. Assembled here rather than in the
// runner, and that placement IS the freeze: a runner handed strings cannot
// re-fetch, so a self_iterate loop cannot move the system prompt under the
// executor or cost a provider's prompt cache each pass. A turn wanting more
// calls search_knowledge, an ordinary entry in its own log.

// prefetcher builds the fetcher for the current node, per call rather than
// held: its sources move (the knowledge searcher with each tracker reconcile,
// the party registry with each apply, the chat readers with the running
// transports), and building it is cheaper than the mutex a cached one needs.
func (e *Engine) prefetcher(company *Company) *prefetch.Fetcher {
	return prefetch.New(e.prefetchSources(company))
}

// prefetchSources assembles what [Engine.prefetcher] builds its fetcher from.
func (e *Engine) prefetchSources(company *Company) prefetch.Sources {
	src := prefetch.Sources{
		Knowledge: e.Knowledge(),
		// THROUGH THE AUXILIARY SEAM (auxiliary.go), like every completion
		// made on a seat's behalf: the memory filter, knowledge query and
		// episode summary each prompt on EVERY turn, and that spend must
		// reach the seat's counter, ceiling and record.
		Models: e.auxiliaryFor(company),
		// The same seam, as a compactor: the middle of a long chat thread
		// is condensed rather than dropped, on the seat's own aux chain.
		Compact: e.compactorFor(company),
		// The chat surfaces' READ half, how a seat woken in a thread is
		// handed it. Empty on a node with no chat transport, which renders
		// prefetch.UnreadableThreadHint rather than silently nothing.
		Threads: e.ChatThreads(),
		// Gates ONLY the episode summary: a nil provider pool would disable
		// the memory and knowledge filters too.
		SummarizeEpisodes: company.Config.Learning.Reflect.SummarizeEpisodes.Or(true),
	}
	if db := e.backends.Store; db != nil {
		src.Diary = learning.NewDiary(db)
		src.Episodes = learning.NewEpisodes(db)
		src.Counterparties = learning.NewCounterparties(db)
		src.Skills = learning.NewSkills(db)
	}
	// THE SEAT'S OWN OPEN ASKS, on Worklet's tracker alone: a vendor tracker
	// holds no asks this engine can read.
	if e.router != nil && company.Config.TrackerBackendFor() == config.TrackerWorklet {
		src.Asks = ownAsks{work: e.router.Work()}
	}
	// Left unset rather than a nil *Registry, which would satisfy the
	// interface and then be dereferenced; unset renders raw platform ids.
	if reg := e.Registry(); reg != nil {
		src.Parties = reg
	}
	// THE CURRENT EMBEDDER, read at each call: this fetcher is also the pull
	// tools' ([Engine.equip]), built before its epoch stores the embedder.
	// None configured answers learning.ErrNoEmbeddings, degrading memory
	// similarity to recency and episode recall to empty: states, not failures.
	src.Embed = e.embedText
	// THE ATLAS, for the entries a turn's ask names, as its seat reads it.
	src.Systems = atlasSystems{engine: e}
	// WHAT A SEAT'S NOTES ARE HELD TO: this build, and when the seat was last
	// told its access, read for a memory refresh as a turn's start reads it.
	src.Build, src.AccessTold = version.String(), e.accessToldAt
	return src
}

// ownAsks is [prefetch.Asks] over the estate router: a seat's open asks to one
// person, at the level a seat's own reads take, from whichever data node
// serves the tracker.
type ownAsks struct{ work estate.Work }

func (a ownAsks) OwnAsks(ctx context.Context, asker, person string, limit int) (worklet.OwnAsks, error) {
	return a.work.OwnAsks(ctx, worklet.OwnAsksQuery{Asker: asker, AskedOf: person, Limit: limit,
		Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)}, time.Now())
}

// taskReads is the tracker's read of one task, as the estate router serves it.
type taskReads interface {
	Task(ctx context.Context, idOrKey string, want worklet.DetailWants,
		fresh statelog.Freshness) (worklet.TaskDetail, error)
}

// linkedEntries is the atlas entries the turn's work item names (ADR-0140),
// by id, on Worklet's tracker alone: a vendor tracker holds no such links.
func (e *Engine) linkedEntries(ctx context.Context, company *Company, req Request, item *types.WorkItem,
	readsAtlas bool) []uuid.UUID {
	if e.router == nil || company.Config.TrackerBackendFor() != config.TrackerWorklet {
		return nil
	}
	return linkedEntriesOf(ctx, e.router.Work(), req.Handle, item, readsAtlas)
}

// linkedEntriesOf reads item's entries from work: none for a turn that may
// not read the atlas or is on no Worklet item, where the tracker is not read
// at all, and none for an item that could not be read, which costs the
// block's first lines alone and is logged.
func linkedEntriesOf(ctx context.Context, work taskReads, seat string, item *types.WorkItem,
	readsAtlas bool) []uuid.UUID {
	if !readsAtlas || item == nil || item.Backend != types.WorkWorklet || item.ID == "" {
		return nil
	}
	detail, err := work.Task(ctx, item.ID, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)})
	if err != nil {
		log.WarnContext(ctx, "prefetch_linked_entries_unread", "seat", seat, "item", item.ID, "error", err)
		return nil
	}
	var out []uuid.UUID
	for _, raw := range detail.Task.Entries() {
		if id, parseErr := uuid.Parse(raw); parseErr == nil {
			out = append(out, id)
		}
	}
	return out
}

// prefetchFor renders one turn's context blocks. The seat and agent ID come
// off the pinned epoch: a prefetch resolved against a revision the turn is not
// running would surface another company's memory.
func (e *Engine) prefetchFor(ctx context.Context, company *Company, req Request,
	task string, ask prefetch.Ask, aux auxspend.Use, boundary provenance.Boundary,
	told *toldTurn, workingFor *types.WorkingFor, readsAtlas bool, item *types.WorkItem,
) prefetch.Blocks {
	seat := company.Org.AgentSeatByHandle(req.Handle)
	if seat == nil {
		return prefetch.Blocks{}
	}
	if told == nil {
		// A turn told nothing of its access holds nothing back for it.
		told = &toldTurn{}
	}
	agentID, _ := company.Org.AgentIDFor(seat)
	r := prefetch.Request{
		Seat: seat, AgentID: agentID.String(), Org: company.Org,
		// Attributed to the RUN, not the work key: every phase record is
		// filed under the run, so an aux record under the work key would be
		// invisible to the turn view and `GET /events?turn_id=` (ADR-0017).
		Task: task, Aux: aux,
		// WHAT IT WAS ASKED, which every relevance judgement is made against
		// ([prefetch.Request.Ask], [turnAskFor]), others' words labelled as
		// the trigger labelled them. The telemetry's own, read once, so the
		// judgements and the turn's record agree on who wrote it.
		Ask: ask,
		// THE ACCESS CHANGES THE SEAT WAS TOLD, which mark its notes written
		// before them, and when it was last told its access, which holds a
		// capability note written before it back (capability.go).
		Told: told.memory, AccessChangedAt: told.accessAt,
		// OFF THE ASK, for a coalesced conversation the merged digest: the
		// merge decides its senders and recon flag, and re-deriving them
		// from the constituents would be a second answer free to disagree.
		Senders: sendersOf(e.Registry(), req.Ask()),
		// A POINTER TRIGGER gates the three searches judged against the
		// trigger text. Read off the events: only their parser knows whether
		// a body is the context or a reference to it.
		RequiresRecon: requiresRecon(req.Ask()),
		// THE TRIGGER'S OWN THREAD, as the notification layer stamped it:
		// the working indicator, the reply target and this block must never
		// disagree about which thread a turn is in.
		Thread: threadOf(req.Ask()),
		// AND WHICH MESSAGE IN IT WOKE THE TURN, so the block can say which
		// shown messages were waiting for it
		// ([prefetch.Blocks.ThreadContextAnswered], workedthrough.go).
		Message: triggerMessageOf(req.Ask()),
		// WHOM THIS TURN WORKS FOR, its own carrier, from which its ceiling
		// is composed: never re-derived from the notification, whose message
		// keeps its post's carrier across an edit made for somebody else.
		WorkingFor: workingFor,
		// WHETHER IT MAY READ THE ATLAS, decided with the turn's own ceiling
		// (turnReadsAtlas), for the entries its ask names.
		ReadsAtlas: readsAtlas,
		// THE ENTRIES ITS WORK ITEM NAMES, read with the item where the turn
		// may read the atlas at all.
		Linked: e.linkedEntries(ctx, company, req, item, readsAtlas),
		// THE SEGMENT'S FENCE TOKEN, which its tools fence with too: the
		// thread is somebody else's words, handed over in the prompt.
		Boundary: boundary,
	}
	// TIMED here: the stretch between a turn announcing itself and its first
	// phase opening, which nothing else on the record measures.
	began := time.Now()
	blocks := e.prefetcher(company).Fetch(ctx, r)
	took := time.Since(began)
	e.publishPrefetchSummary(ctx, seat, agentID.String(), req.RunID, req.WorkKey, r, blocks,
		began.UTC(), took)
	e.publishPrefetchRead(ctx, seat, agentID.String(), req.RunID, req.WorkKey, blocks)
	return blocks
}

// publishPrefetchRead records the pages the turn-start knowledge block put in
// front of the seat, as a `knowledge_read` with `via: prefetch`.
//
// A second event rather than a list on prefetch_summary: the summary is one row
// per turn about the PIPELINE, this one row per read about the PAGES, as every
// other way a seat reads the knowledge base records, so page reads are one
// event type to count. Names no phase (the prefetch runs before the first);
// publishes nothing when no page surfaced, a hint included; best effort.
func (e *Engine) publishPrefetchRead(ctx context.Context, seat *org.Role,
	agentID, runID, workKey string, b prefetch.Blocks,
) {
	if e.backends == nil || e.backends.Queue == nil || len(b.RelevantKnowledgePages) == 0 {
		return
	}
	read := knowledge.ReadPages(b.RelevantKnowledgePages)
	if len(read) == 0 {
		return
	}
	ev := events.NewFrom(types.KnowledgeRead{
		Agent: agentID, AgentHandle: seat.Handle(), RoleName: seat.Name,
		TurnID: runID, WorkKey: workKey,
		Via:     types.ReadViaPrefetch,
		Backend: b.RelevantKnowledgePages[0].Backend,
		Query:   types.KnowledgeReadQuery(b.RelevantKnowledgeQuery),
		Pages:   read,
	}, tracing.TraceOf(ctx))
	if ev == nil {
		return
	}
	e.publishEvent(ctx, ev, seat.Name)
}

// publishPrefetchSummary reports what each block actually surfaced: the only
// signal this pipeline has. Every block but knowledge and chat-thread degrades
// silently (internal/agent/prefetch), so an unreachable diary, a misconfigured
// auxiliary model and a seat that never stored a memory all start turns with
// none; per-block hit and size, once per turn, tell them apart.
//
// Best effort: a turn must not fail because its telemetry could not publish.
func (e *Engine) publishPrefetchSummary(ctx context.Context, seat *org.Role,
	agentID, runID, workKey string, r prefetch.Request, b prefetch.Blocks,
	startedAt time.Time, took time.Duration,
) {
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	ev := events.NewFrom(types.PrefetchSummary{
		Agent: agentID, AgentHandle: seat.Handle(), RoleName: seat.Name,
		TurnID:                 runID,
		WorkKey:                workKey,
		StartedAt:              startedAt,
		DurationMS:             int(took / time.Millisecond),
		CounterpartyHit:        b.CounterpartyProfile != "",
		CounterpartyBytes:      len(b.CounterpartyProfile),
		OpenAsksHit:            b.OpenAsks != "",
		OpenAsksBytes:          len(b.OpenAsks),
		SystemsHit:             b.Systems != "",
		SystemsNamed:           b.SystemsNamed,
		SystemsBytes:           len(b.Systems),
		SynthesizedSkillsHit:   b.SynthesizedSkills != "",
		SynthesizedSkillsBytes: len(b.SynthesizedSkills),
		EpisodeRecallHit:       b.EpisodeRecall != "",
		EpisodeRecallBytes:     len(b.EpisodeRecall),
		PersonalMemoryHit:      b.PersonalMemory != "",
		PersonalMemoryBytes:    len(b.PersonalMemory),
		RelevantKnowledgeHit:   b.RelevantKnowledge != "",
		RelevantKnowledgeBytes: len(b.RelevantKnowledge),
		// The count the block cannot carry: an empty search still
		// renders the hint, so hit=true with count=0 is "it ran and
		// found nothing" rather than "it surfaced pages".
		RelevantKnowledgeSelectionCount: len(b.RelevantKnowledgePages),
		ThreadContextHit:                b.ThreadContext != "",
		ThreadContextBytes:              len(b.ThreadContext),
		ThreadContextPosts:              b.ThreadContextPosts,
		// THE TWO FACTS THE PROSE CANNOT CARRY: both zero-message paths
		// render a non-empty hint, so hit, bytes and count cannot say whether
		// a backend answered, and a thread read to a bound stops short of the
		// waking message with the same count as a whole one.
		ThreadContextRead:         b.ThreadContextRead,
		ThreadContextStoppedShort: b.ThreadContextStoppedShort,
		TriggerRequiresRecon:      r.RequiresRecon,
		// AND THE FACT THE EPISODE BLOCK CANNOT CARRY: an empty block is
		// both "nothing similar" and "the embedder failed, so nothing was
		// searched".
		TurnEmbedding: b.TurnEmbedding,
	}, tracing.TraceOf(ctx))
	if ev == nil {
		return
	}
	e.publishEvent(ctx, ev, seat.Name)
}

// turnAsk is what a turn was ASKED ([prefetch.Request.Ask]), the text alone of
// [turnAskFor]: DescribeTrigger without the wrapping (a notification's brief is
// the integration's prompt, identical every turn). Per event, in order:
//   - a notification: its subject unless it only names the surface
//     ([types.ExternalNotification.SubjectIsLabel], as chat's does, pulling
//     every chat turn's judgements together), then its salient body, a burst's
//     per sender without re-sent copies (notify's mergedSalient);
//   - anything else stating an ask (a schedule's task, a colleague's question
//     or answer): its brief, the ask itself, naming the colleague who asked.
//
// Any other event adds nothing, not even the type name DescribeTrigger falls
// back to, which would be a word every such turn is judged against.
func turnAsk(evs []*events.Event) string {
	return turnAskFor(notify.Prompts{}, nil, "", evs).Text()
}

// turnAskFor is [turnAsk] part by part, each part able to put the words somebody
// else wrote inside a fence for the auxiliary model that reads it
// ([prefetch.Ask]): a notification's salient body labelled exactly as its
// source labelled the body the trigger fenced ([notify.SalientLabel]) at
// the trust its writers earn from self, and a colleague's ask or answer as its
// fenced brief. A subject line and a schedule's task hold nobody's words but
// the company's and are only neutralised.
func turnAskFor(prompts notify.Prompts, parties notify.Parties, self string,
	evs []*events.Event) prefetch.Ask {

	var ask prefetch.Ask
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if part, ok := eventAsk(prompts, parties, self, ev); ok {
			ask = append(ask, part)
		}
	}
	return ask
}

// askOf is what req's turn was asked, part by part with who wrote each, as the
// seat req names reads it ([turnAskFor] over this node's prompts and parties).
func (e *Engine) askOf(req Request) prefetch.Ask {
	return turnAskFor(e.notifyPrompts(), e.liveParties(), req.Handle, req.Ask())
}

// eventAsk is one trigger event's part of [turnAskFor], or false for an event that
// asked nothing.
func eventAsk(prompts notify.Prompts, parties notify.Parties, self string,
	ev *events.Event) (prefetch.AskPart, bool) {

	if n, ok := events.DataAs[*types.ExternalNotification](ev); ok && n != nil {
		body := strings.TrimSpace(salientBody(n))
		subject := strings.TrimSpace(n.Subject)
		if n.SubjectIsLabel {
			subject = ""
		}
		if subject == "" && body == "" {
			return prefetch.AskPart{}, false
		}
		part := prefetch.AskPart{Lead: subject, Text: body}
		if body != "" {
			label := notify.SalientLabel(prompts, *n, parties, self)
			part.Label = &label
		}
		return part, true
	}
	// A COLLEAGUE'S ASK OR ANSWER is the colleague's words under the line
	// naming them, labelled as the trigger fences them.
	if quoted, ok := ev.Data.(quotedBriefer); ok {
		lead, words, label := quoted.Quoted()
		lead, words = strings.TrimSpace(lead), strings.TrimSpace(words)
		if lead == "" && words == "" {
			return prefetch.AskPart{}, false
		}
		part := prefetch.AskPart{Lead: lead, Text: words}
		if words != "" {
			part.Label = &label
		}
		return part, true
	}
	brief, ok := ev.Data.(events.Briefer)
	if !ok {
		return prefetch.AskPart{}, false
	}
	text := strings.TrimSpace(brief.Brief())
	if text == "" {
		return prefetch.AskPart{}, false
	}
	return prefetch.AskPart{Text: text}, true
}

// quotedBriefer is a wake whose ask is somebody else's words under a line the
// engine writes: its lead, the words, and the label they are fenced under —
// the parts [fencedBriefer] renders.
type quotedBriefer interface {
	Quoted() (lead, words string, label provenance.Label)
}

// requiresRecon reports that ANY constituent of the trigger is a bare pointer:
// a coalesced trigger holding one webhook that only names what changed still
// needs looking behind, and searching the merged text returns noise for it.
func requiresRecon(evs []*events.Event) bool {
	for _, n := range notificationsIn(evs) {
		if n.ContextRequiresRecon {
			return true
		}
	}
	return false
}

// threadOf resolves the chat thread a turn was woken in, or the zero value.
//
// First wins, over each notification's FLAT metadata (its latest
// constituent's): a chat partition is thread-grained wherever a thread exists
// (a direct conversation partitions on the bare channel only for unthreaded
// messages), so a burst never straddles two threads, and the constituents
// would only offer a choice between identical values. A non-chat trigger adds
// nothing: [notify.ThreadOf] keys on the `transport` only a chat parser stamps.
func threadOf(evs []*events.Event) notify.Thread {
	for _, n := range notificationsIn(evs) {
		if t, ok := notify.ThreadOf(n.Metadata); ok {
			return t
		}
	}
	return notify.Thread{}
}

// notificationsIn reads the notifications out of a turn's trigger envelopes.
// Any other waking event (a scheduled fire, a sandbox completion, an
// agent-to-agent ask) has no sender or recon flag and adds nothing, not an
// error.
func notificationsIn(evs []*events.Event) []*types.ExternalNotification {
	out := make([]*types.ExternalNotification, 0, len(evs))
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if n, ok := events.DataAs[*types.ExternalNotification](ev); ok && n != nil {
			out = append(out, n)
		}
	}
	return out
}

// sendersOf resolves every distinct party who triggered the turn, in the order
// they spoke: a turn woken by four people is not about the last. Through the
// party registry, so a sender who IS a seat here is profiled under their
// handle, not a platform id nobody else uses.
func sendersOf(registry *notify.Registry, evs []*events.Event) []learning.Subject {
	var (
		out  []learning.Subject
		seen = map[learning.Subject]bool{}
	)
	add := func(s learning.Subject) {
		if !s.Valid() || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, n := range notificationsIn(evs) {
		for _, s := range subjectsOf(registry, n) {
			add(s)
		}
	}
	return out
}

// subjectsOf reads the senders off one notification: a coalesced event's
// constituents (the flat Sender is only the latest), else its flat fields,
// never both: the flat fields ARE the latest constituent, so taking both would
// put the last speaker first in a list ordered by who spoke.
func subjectsOf(registry *notify.Registry, ev *types.ExternalNotification) []learning.Subject {
	if len(ev.Messages) > 0 {
		out := make([]learning.Subject, 0, len(ev.Messages))
		for _, m := range ev.Messages {
			out = append(out, subjectOf(registry, ev.NotificationSource,
				m.Sender, m.Metadata, m.Writer))
		}
		return out
	}
	return []learning.Subject{
		subjectOf(registry, ev.NotificationSource, ev.Sender, ev.Metadata, ev.Writer),
	}
}

// subjectOf resolves one sender to the identity their profile is keyed on: a
// first-party notice's by the writer its record names ([writerSubject]), a
// vendor's as an id in the vendor's own namespace.
func subjectOf(registry *notify.Registry, source, sender string, metadata map[string]string,
	writer *types.NotificationWriter) learning.Subject {

	external := strings.TrimSpace(metadata[notify.ActorField])
	if external == "" {
		// Every parser stamps the actor, but an engine-authored
		// notification has none — the sender field is then all there is.
		external = strings.TrimSpace(sender)
	}
	if writer != nil {
		return writerSubject(registry, source, sender, external, *writer)
	}
	subject := learning.Subject{
		ExternalID: external, Platform: source, Name: strings.TrimSpace(sender),
	}
	if registry != nil && external != "" {
		if party, ok := registry.ByExternalID(source, external); ok {
			// A COLLEAGUE IS KEYED ON THEIR HANDLE, so chat and the
			// tracker build one profile, not two under two platform ids.
			subject.Handle = party.Handle
			if party.Name != "" {
				subject.Name = party.Name
			}
		}
	}
	return subject
}

// writerSubject is who a FIRST-PARTY notice's writer is to the seat it woke
// ([types.NotificationWriter]), by the kind its record names: such a source has
// no namespace to look an id up in, and its actor may be a seat's handle, a
// credential's login or a node's name alike.
//
// A SEAT'S WRITE (an agent's, or a person's as their seat) is that seat by
// handle while the chart holds it, so the turn's reads are decided for it
// (builtin.ceilingOf) and its profile joins the chat one. THE ENGINE'S OWN
// names nobody: a repair is no one's errand, like a schedule's fire. ANY OTHER
// (a credential acting as no seat, a seat the chart dropped, an unknown kind)
// keeps its platform identity: the closed end, a stranger's turn that may
// read nothing for them.
func writerSubject(registry *notify.Registry, source, sender, external string,
	w types.NotificationWriter) learning.Subject {

	switch w.Kind {
	case iam.ActorSystem:
		return learning.Subject{}
	case iam.ActorAgent, iam.ActorHuman:
		if seat := strings.TrimSpace(w.Seat); registry != nil && seat != "" {
			if party, ok := registry.ByHandle(seat); ok {
				name := party.Name
				if name == "" {
					name = strings.TrimSpace(sender)
				}
				return learning.Subject{Handle: party.Handle, ExternalID: seat,
					Platform: source, Name: name}
			}
		}
	}
	return learning.Subject{ExternalID: external, Platform: source, Name: strings.TrimSpace(sender)}
}

// interactionsOf renders a turn's trigger as the interactions a learning
// worker reads, carried on the event because the reflect dispatcher is a queue
// consumer whose node may never have seen the trigger.
//
// Every constituent of a coalesced trigger, in the order they spoke, as in
// sendersOf. Each body carries its label (who wrote it, at the trust its
// writers earn from self, by [notify.SalientLabels]), decided here where the
// delivery is in hand as the trigger and turn-start judgements read it: the
// worker runs after the turn with no notification to ask, and fences under it.
func (e *Engine) interactionsOf(evs []*events.Event, self string) []types.InboundInteraction {
	return interactionsFor(e.Registry(), e.notifyPrompts(), self, evs)
}

// interactionsFor is [Engine.interactionsOf] over a given registry and the
// prompts that say who wrote each body.
func interactionsFor(registry *notify.Registry, prompts notify.Prompts, self string,
	evs []*events.Event) []types.InboundInteraction {

	// THE REGISTRY IS THE PARTIES, or none: a nil *Registry would satisfy
	// the interface and then be dereferenced.
	var parties notify.Parties
	if registry != nil {
		parties = registry
	}
	var out []types.InboundInteraction
	for _, n := range notificationsIn(evs) {
		labels := notify.SalientLabels(prompts, *n, parties, self)
		if len(n.Messages) == 0 {
			out = append(out, interaction(registry, n.NotificationSource,
				n.SourceEventType, n.Sender, salientBody(n), n.Metadata,
				n.ContextRequiresRecon, n.Writer, labels[0]))
			continue
		}
		// A COALESCED EVENT'S FLAT FIELDS MIRROR THE LATEST constituent:
		// taking them too would double-count that message in every worker.
		for i, m := range n.Messages {
			out = append(out, interaction(registry, n.NotificationSource,
				m.SourceEventType, m.Sender, m.Body, m.Metadata,
				m.ContextRequiresRecon, m.Writer, labels[i]))
		}
	}
	return out
}

// interaction assembles one, resolving its sender the way a profile is keyed.
func interaction(registry *notify.Registry, source, rawType, sender, body string,
	metadata map[string]string, requiresRecon bool, writer *types.NotificationWriter,
	label provenance.Label,
) types.InboundInteraction {
	subject := subjectOf(registry, source, sender, metadata, writer)
	return types.InboundInteraction{
		Sender: types.CanonicalIdentity{
			Handle: subject.Handle, ExternalID: subject.ExternalID,
			Platform: subject.Platform, DisplayName: subject.Name,
			// A CREDENTIAL ACTING AS NO SEAT, by its login: the requester a
			// turn it woke is worked for (ADR-0109).
			Login: credentialLogin(writer),
		},
		Body:       body,
		Provenance: label,
		// UNKNOWN when the source stamped nothing, which is the honest
		// answer for a tracker or a code host rather than a gap — see
		// notify.ChannelKindField.
		ChannelKind:   channelKind(metadata),
		RawEventType:  rawType,
		RequiresRecon: requiresRecon,
	}
}

// credentialLogin is the login a first-party writer names where it is a
// credential acting as no seat, and "" for every other writer and for a
// vendor's sender, which names no writer at all.
func credentialLogin(w *types.NotificationWriter) string {
	if w == nil || w.Kind != iam.ActorOperator {
		return ""
	}
	return strings.TrimSpace(w.Login)
}

// salientBody is the raw inbound message, never the enriched prompt. A NIL
// SalientBody (the producer emits no distinct raw message) falls back to Body;
// an EMPTY one was genuinely empty, and falling back would hand every worker
// triage boilerplate as what the person said.
func salientBody(n *types.ExternalNotification) string {
	if n.SalientBody != nil {
		return *n.SalientBody
	}
	return n.Body
}

// channelKind reads the canonical surface shape a parser stamped. A value this
// build does not recognise reads as unknown: the field is a closed set, and a
// consumer switching on it must never meet a newer producer's member.
func channelKind(metadata map[string]string) types.ChannelKind {
	switch kind := types.ChannelKind(metadata[notify.ChannelKindField]); kind {
	case types.ChannelDM, types.ChannelGroup, types.ChannelPublic, types.ChannelInternal:
		return kind
	default:
		return types.ChannelUnknown
	}
}
