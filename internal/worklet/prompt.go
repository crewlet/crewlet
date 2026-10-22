package worklet

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

// Prompt is what a change to a task asks of the seat it reached.
//
// # It tailors to the ROUTING REASON, not to the change kind
//
// One assignment reaches the new assignee, several watchers and possibly the
// unit lead, and it asks each of them for something different: the assignee
// owns the work, a watcher is following it, and the lead is there only because
// nobody else was named. A prompt keyed on the change kind would have to tell
// all three the same thing, which in practice means telling all three the
// weakest thing. The parser already decided which a copy is and stamped it, so
// this reads that back rather than re-deriving a judgement the router made
// with more information.
//
// # Eighteen reasons, THREE FRAMES
//
// The router distinguishes eighteen reasons because the routing rules
// genuinely differ — a blocker's dependent and a task's watcher are reached by
// different paths. What a RECIPIENT needs to be told collapses to three:
// somebody is asking you, this is your work, or this is activity you follow. A
// prompt with eighteen openers would be eighteen places for one sentence to
// drift, and the reason itself is rendered in the header either way.
//
// # This prompt DOES name tools, and that is the exception rather than a lapse
//
// The tool-capabilities doctrine says the engine must not name tools, because
// a deployed MCP server's names are not knowable by the engine. That reasoning
// does not reach here: these tools are shipped by this build, registered by
// this build under names this build chose, and present on every seat that can
// read this notification at all.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Source }

// RequiresRecon implements [notify.Prompt].
//
// TRUE FOR A POINTER, FALSE FOR PROSE, and the split matters more than it
// looks: the flag suppresses the turn-start relevance filtering AND the
// personal-memory and episode recall that go with it, so setting it on a wake
// that already carries what it means costs the seat its own context for
// nothing.
//
// A COMMENT CARRIES ITS TEXT — the excerpt is what somebody actually said — so
// the seat can begin reasoning from the trigger. A bare transition carries no
// prose at all and is a pointer at a task the seat must read first.
func (Prompt) RequiresRecon(n notify.Inbound) bool {
	if metaAddress(n.Metadata) == "" {
		return false
	}
	switch ChangeKind(n.EventType) {
	case ChangeComment, ChangeCommentEdited:
		return strings.TrimSpace(n.Body) == ""
	case ChangePrioritised:
		// NOT A POINTER. A `prioritised` wake names the task, the person
		// who put it there and the position — so the seat can begin
		// reasoning from the trigger, and turning the recon on would cost
		// it the personal memory and episode recall that the flag
		// suppresses, for a fetch of something it was already told.
		//
		// `prioritised` reaches this switch at all only because it is the
		// one non-task wake that DOES carry a task key: the task at the
		// top of the list. Without this arm the key alone would have
		// turned it into a pointer.
		return false
	}
	return true
}

// SubjectIsLabel implements [notify.Prompt]: never — the subject names the
// task and its title, which on a comment is the only place the topic is named
// at all.
func (Prompt) SubjectIsLabel(notify.Inbound) bool { return false }

// Addressed implements [notify.Prompt]: somebody is waiting on THIS seat.
//
// The eight primary reasons and no others. A watcher is following the task
// rather than being asked about it, and a fallback is the task landing
// somewhere rather than on somebody — a seat obliged to answer either would
// comment on every field change in its unit's projects.
func (Prompt) Addressed(n notify.Inbound) bool {
	return Reason(n.Metadata[MetaVia]).Addressed()
}

// Owes is the chat surface a wake OBLIGES its seat to post on, or "".
//
// ONE WAKE AND ONE ONLY: the answer to a decision whose asker said it would
// report the outcome somewhere ([Decision.Inform]), reaching that asker under
// `answered`. Every other copy of the same record — a watcher's, the item's
// assignee's — carries the inform in its metadata too, and owes nothing,
// because the promise was the asker's.
//
// A SURFACE, never a channel: the engine enforces the obligation by holding
// the turn open until a tool on that surface has delivered
// ([turn.DeliveredTo]), and a tool reports the surface it reached rather than
// the channel it posted in — so the channel is the prompt's to name and the
// asker's to honour. Read from the metadata the parser stamped, so the parser
// ([Parser.Parse]) and the partition key below answer from one rule.
func Owes(metadata map[string]string) string {
	if Reason(metadata[MetaVia]) != ReasonAnswered {
		return ""
	}
	var inform decision.Inform
	if raw := metadata[MetaInform]; raw == "" ||
		json.Unmarshal([]byte(raw), &inform) != nil || !inform.Surface.Valid() {
		return ""
	}
	return string(inform.Surface)
}

// owedPartition is the separator between a conversation and the surface a
// partition of it owes. Neither a conversation identity (`task:<id>` or
// `<object>:<id>`) nor a surface name carries it.
const owedPartition = "#owes:"

// PartitionKey implements [notify.Prompt]: the task is the conversation, and
// a wake that owes a chat surface is a finer cut of it.
//
// THE FINER CUT IS WHAT KEEPS AN OBLIGATION WHOLE THROUGH A MERGE. A turn owes
// at most ONE surface ([turn.Reply] names one), and a coalesced trigger is one
// turn: two answers on one item informing two different surfaces, merged,
// would have had to drop one of the two promises. Partitioned apart, every
// partition owes one surface or none, and the merge carries it
// ([notify.Coalesce]).
func (p Prompt) PartitionKey(metadata map[string]string, subject string) string {
	key := p.ConversationIdentity(metadata, subject)
	if surface := Owes(metadata); surface != "" && key != "" {
		return key + owedPartition + surface
	}
	return key
}

// ConversationIdentity implements [notify.Prompt]: the TASK is the
// conversation, and it is named by its ID. The partition key above is this,
// or a finer cut of it — the invariant [notify.Prompt.ConversationIdentity]
// requires.
//
// # The id and never the key
//
// A key is an ADDRESS — what a person pastes into chat and types into a tool
// call — and an address is not one task for the life of the deployment:
//
//   - TWO TASKS CAN HOLD ONE KEY. A counter restored beside newer work mints
//     numbers those tasks already hold, and the applier flags every holder but
//     the claimant `key_collision` rather than refuse a record the broker
//     committed. Keyed on the key, wakes about the two coalesced into ONE
//     digest and their turns shared ONE ledger, so a seat woken about the
//     duplicate was handed the claimant's thread as its own history.
//   - A KEY MOVES. A cross-project move re-keys the task (ENG-7 becomes OPS-3),
//     and keyed on the key its conversation started over under the new one:
//     the ledger of every turn the seat had taken on it was filed where its
//     next turn never looked, and a coding run parked on a question asked
//     from the task could not be matched by the answer posted on it.
//
// The id is the one name a task never shares and never gives up. The key was
// chosen once so "a chat thread about ENG-42 and the tracker activity on it
// land in one ledger" — which never happened: the spine namespaces every
// source-local key ([notify.Namespaced]), so `work:ENG-42` and a chat
// thread's key were never one string.
//
// `task:<id>`, IN THE GRAMMAR A NON-TASK WAKE'S OBJECT ALREADY USES, so the two
// can never collide, and a task-subject wake keys exactly as its own object
// would. The id is the one the WAKE points at ([Snapshot.TaskID]) rather than
// the subject's: a `prioritised` wake is written on a person and is about the
// task at the top of their list, so it joins that task's conversation, as it
// did while both keyed on the task's key.
func (Prompt) ConversationIdentity(metadata map[string]string, _ string) string {
	if id := metadata[MetaTaskID]; id != "" {
		return string(KindTask) + ":" + id
	}
	// A NON-TASK WAKE KEYS ON ITS OWN OBJECT. Falling through to an empty
	// key would put every person-scoped wake in the company into ONE
	// ledger — a conversation key is what separates threads, and a shared
	// empty one merges them all.
	if id := metadata[MetaObjectID]; id != "" {
		return metadata[MetaObject] + ":" + id
	}
	return ""
}

// WorkItem is the task a wake is about, which is the item the turn it wakes
// is charged to.
//
// THE TASK ID IS THE IDENTITY, read off [MetaTaskID] — the snapshot's task,
// which the parser writes only when the wake is genuinely about one. A wake
// about a person's own list names no task and so names no item: charging that
// turn to the object id would charge it to a handle.
func (Prompt) WorkItem(metadata map[string]string) (types.WorkItem, bool) {
	id := metadata[MetaTaskID]
	if id == "" {
		return types.WorkItem{}, false
	}
	return types.WorkItem{
		Backend: types.WorkWorklet, ID: id,
		Key: metadata[MetaTaskKey], Project: metadata[MetaProject],
	}, true
}

// WakesActor implements [notify.Prompt].
//
// ONE REASON DOES, and it is the one the router already names: a person who
// closed a blocker learns from this that the task it was blocking is now
// workable, which is a CONSEQUENCE of their own write rather than a repeat of
// it. Every other change here is something the actor already knows.
//
// READ OFF THE ROUTING REASON this recipient's copy carries ([MetaVia]), never
// the event type: the unblocked notice to the actor and every other copy of the
// same status change share one event type (`status`), and only the reason the
// parser stamped per recipient tells them apart — the reason [Route] already
// let through by the same rule ([Reason.WakesActor]).
func (Prompt) WakesActor(n notify.Inbound) bool {
	return Reason(n.Metadata[MetaVia]).WakesActor()
}

// Provenance implements [notify.Prompt]: what the parser stated — the comment,
// the description or the change note the body is, by the change's author
// ([notify.StatedHosted]) — and, for a wake built without the parser, the
// surface the change kind produces. HOSTED, because only company principals
// write the engine's own tracker: a change whose record names nobody is read
// as a colleague's, as every other door this text comes through reads it
// ([provenance.TrustForHosted]).
func (Prompt) Provenance(n notify.Inbound, parties notify.Parties) notify.Provenance {
	return notify.StatedHosted(n, parties, excerptSource(ChangeKind(n.EventType)))
}

// DigestBody implements [notify.Prompt]: comments keep their text, field and
// status changes collapse to their lead.
//
// A comment is something a person SAID and each one is different. A field
// change's body is a delta line the digest's own lead already states, so five
// of them in a coalesced trigger is the same sentence five times, burying the
// comment underneath.
func (Prompt) DigestBody(eventType, body string) string {
	switch ChangeKind(eventType) {
	case ChangeComment, ChangeCommentEdited:
		return body
	}
	return ""
}

// Build implements [notify.Prompt].
func (Prompt) Build(n notify.Inbound, parties notify.Parties) string {
	meta := n.Metadata
	// THE OBJECT DECIDES THE FRAME, before the reason does. Every opener
	// below says "A task…", the header is labelled **Task:** and the
	// context block sends the reader to get_work_item — none of which is
	// true of a person's priority list.
	//
	// A TASK IS THE DEFAULT FRAME: every other object kind names itself in
	// `object`, which the parser always sets — so a wake built without the
	// key, by hand rather than by the parser, renders as a task.
	if kind := ObjectKind(meta[MetaObject]); kind != "" && kind != KindTask {
		return buildObjectPrompt(kind, n, parties)
	}
	reason := Reason(meta[MetaVia])
	var b strings.Builder

	promptOpener(&b, n, parties, reason)
	promptDecision(&b, meta, reason)

	promptChanged(&b, n)
	promptContext(&b, meta)
	if isFallback(reason) {
		promptWhyYou(&b, meta[MetaProject])
	}
	promptHandling(&b, meta, reason)
	return b.String()
}

// isFallback reports a reason that reached this seat because nobody else was
// named, rather than because of anything about them.
//
// ONE REASON, and it is named rather than derived from the candidate's own
// FallbackOnly flag: the flag is gone by the time a prompt runs — routing
// resolved it and stamped what survived — so the prompt reads the reason it
// was told, which is the only thing it has.
func isFallback(reason Reason) bool { return reason == ReasonLeadFallback }

// promptOpener writes the one sentence that says why this seat is reading
// this, and — for the reasons that are ABOUT something somebody said — the
// text itself.
//
// ONE SWITCH, and the shape is deliberate. This was three functions chosen by
// [Reason.Primary], and that split put six arms where their own reason could
// never reach them: `blocking` was written in the "following" half while
// Primary sends it to the "owned" one, and `unblocked` and `collaborator` the
// other way round — so a blocker's assignee read "a task you are named on
// changed" and somebody whose work had just become startable read "a task you
// are watching changed". Each arm looked right beside the others in its own
// function, and nothing could see the pairing was wrong.
//
// EVERY REASON HAS AN ARM, and the default is the honest sentence for one a
// newer build routed that this one does not know: a rolling upgrade puts such
// a wake on the wire, and "a task you are named on changed" is true of every
// reason there is.
func promptOpener(b *strings.Builder, n notify.Inbound, parties notify.Parties,
	reason Reason) {

	quoted := false
	switch reason {
	case ReasonMention:
		b.WriteString("You were @-mentioned in a comment on a task.")
		quoted = true
	case ReasonAsked:
		b.WriteString("You were asked a question on a task, and the person " +
			"who asked is waiting for an answer.")
		quoted = true
	case ReasonAnswered:
		b.WriteString("A question you asked on a task was answered.")
		quoted = true
	case ReasonThread:
		b.WriteString("A comment thread you are part of has a new reply.")
		quoted = true
	case ReasonPrioritised:
		b.WriteString("Somebody else set the order of your work queue.")
	case ReasonAssignee:
		switch ChangeKind(n.EventType) {
		case ChangeCreated:
			b.WriteString("A new task was filed with you as its assignee.")
		case ChangeRemoved:
			b.WriteString("A task you were assigned to was removed.")
		case ChangeAssignee:
			b.WriteString("A task was assigned to you.")
		default:
			b.WriteString("A task you are assigned to changed.")
		}
	case ReasonUnassigned:
		b.WriteString("A task you were assigned to went to somebody else.")
	case ReasonReporter:
		b.WriteString("A task you filed changed.")
	case ReasonUnblocked:
		b.WriteString("A task you are waiting on is now workable — every " +
			"blocker on it has finished.")
	case ReasonBlocking:
		// THE BLOCKER'S OWN ASSIGNEE, and the sentence has to say whose
		// problem it is: this seat holds the task somebody else is now
		// waiting for, which is a call on THEIR time rather than news
		// about somebody else's work.
		b.WriteString("Somebody's work now waits on a task of yours.")
	case ReasonRoutedTo:
		b.WriteString("Work routes to your team now: a task was pointed at it.")
	case ReasonParentAssignee:
		b.WriteString("A subtask of a task you hold finished or was reopened.")
	case ReasonChecklist:
		b.WriteString("A checklist item assigned to you changed.")
	case ReasonCollaborator:
		b.WriteString("A task you are collaborating on changed.")
	case ReasonWatcher:
		b.WriteString("A task you are watching changed.")
	case ReasonUnwatched:
		b.WriteString("Somebody removed your watch on a task.")
	case ReasonPurged:
		// NOT "changed". The task and everything on it were destroyed,
		// and a reader who opens this expecting an edit goes looking for
		// what moved — on a row that is gone.
		b.WriteString("A task in your project was permanently destroyed.")
	case ReasonLeadFallback:
		b.WriteString("A task in your team's project has activity and nobody " +
			"here is named on it.")
	default:
		b.WriteString("A task you are named on changed.")
	}
	promptHeader(b, n, parties)
	// A COMMENT IS QUOTED WHATEVER BROUGHT IT HERE. The four reasons above
	// are ABOUT something somebody said, but a remark reaches a task's
	// assignee, reporter and watchers too, and to them the change IS the
	// remark: [promptChanged] leaves a comment's body to this opener, and
	// [Prompt.RequiresRecon] reports a comment wake as carrying its text —
	// so a copy that rendered only "a comment was added" sent its seat to
	// read the task for words the trigger was built to hand it.
	switch ChangeKind(n.EventType) {
	case ChangeComment, ChangeCommentEdited:
		quoted = true
	}
	if !quoted {
		return
	}
	body := n.Body
	if body == "" {
		body = "(no text)"
	}
	b.WriteString("\n**Comment:**\n" + body + "\n")
}

// promptDecision renders a structured ask to the person asked, and the choice
// to the person who asked.
//
// THE ANSWERING CALL IS WRITTEN OUT WHOLE, ids included, because every part
// of it is something a seat otherwise has to go and find: the ask's comment id
// is in a thread it would read for no other reason, and an option's id is
// typed back exactly or refused. A seat told "answer with `choice`" and not
// shown the call spends a round reading the item to learn what it was already
// holding.
//
// ONLY FOR THE TWO REASONS THE DECISION IS ADDRESSED TO. A watcher on the item
// sees the question in the comment like any other remark; the options are for
// the person who owes the answer, and the choice for the person waiting on it.
// A decision that does not decode renders nothing rather than half a block —
// the comment above still carries what was said.
func promptDecision(b *strings.Builder, meta map[string]string, reason Reason) {
	switch reason {
	case ReasonAsked:
		var decision decision.Decision
		raw := meta[MetaDecision]
		if raw == "" || json.Unmarshal([]byte(raw), &decision) != nil ||
			len(decision.Options) == 0 {
			return
		}
		promptAskedDecision(b, meta, decision)
	case ReasonAnswered:
		promptAnsweredDecision(b, meta)
	}
}

// promptAskedDecision is the decision block the person asked reads.
func promptAskedDecision(b *strings.Builder, meta map[string]string, d decision.Decision) {
	b.WriteString("\n## The decision you are asked for\n")
	b.WriteString("**Question:** " + d.Question + "\n")
	switch d.Role {
	case decision.RoleApprover:
		b.WriteString("**You are asked as:** approver — your answer IS the " +
			"decision.\n")
	case decision.RoleContributor:
		b.WriteString("**You are asked as:** contributor — your answer is an " +
			"input to a decision somebody else makes.\n")
	}
	b.WriteString("**Options:**\n")
	for _, option := range d.Options {
		line := "- `" + option.ID + "` — " + option.Label
		if option.Detail != "" {
			line += ": " + option.Detail
		}
		if option.ID == d.Recommended {
			line += " *(recommended)*"
		}
		b.WriteString(line + "\n")
	}
	if d.Rationale != "" {
		b.WriteString("**Why the asker recommends it:** " + d.Rationale + "\n")
	}
	if len(d.Evidence) > 0 {
		b.WriteString("**What the asker looked at:**\n")
		for _, evidence := range d.Evidence {
			line := "- " + string(evidence.Kind) + " " + evidence.Ref
			if evidence.Label != "" {
				line += " — " + evidence.Label
			}
			b.WriteString(line + "\n")
		}
	}
	if d.Inform != nil {
		b.WriteString("**Where the outcome goes:** the asker will report it in " +
			d.Inform.Channel + " on " + string(d.Inform.Surface) + ".\n")
	}
	// THE RECOMMENDED OPTION IN THE EXAMPLE, because it is the one the
	// asker thought most likely — and the first when there is none. The
	// sentence after the call says it is an example to change, and the
	// list of ids beside it is what the choice is checked against.
	choice := d.Recommended
	if choice == "" {
		choice = d.Options[0].ID
	}
	// THE ADDRESS, never the bare key: a key another task claimed first
	// opens THAT task, so an answer sent to it would land on the claimant.
	call := answeringCall{
		Item: metaAddress(meta), Answers: meta[MetaCommentID],
		Choice: choice, Body: "<why, in a sentence or two>",
	}
	b.WriteString("\nAnswer with ONE call to `" + CommentOnWorkTool + "`, " +
		"naming the option you choose by its id in `choice` (one of: " +
		strings.Join(d.OptionIDs(), ", ") + "):\n\n" +
		"```json\n" + jsonText(call) + "\n```\n\n" +
		"If none of the options is right, answer without `choice` and say in " +
		"`body` what you would do instead — that is an answer too. The asker " +
		"is woken with whatever you send, so send it once.\n")
}

// answeringCall is the literal arguments of the call that answers an ask, in
// the order a person reads them.
type answeringCall struct {
	Item    string `json:"item"`
	Answers string `json:"answers,omitempty"`
	Choice  string `json:"choice"`
	Body    string `json:"body"`
}

// promptAnsweredDecision is the choice block the asker reads.
//
// IT SAYS WHAT HAPPENS NEXT, because the asker is the one that stopped: the
// escalation guidance tells a seat to end its turn blocked on the branch it
// asked about, and this wake is what that branch was waiting for.
func promptAnsweredDecision(b *strings.Builder, meta map[string]string) {
	var option decision.Option
	chose := meta[MetaChoice] != "" &&
		json.Unmarshal([]byte(meta[MetaChoice]), &option) == nil && option.ID != ""
	var inform decision.Inform
	informs := meta[MetaInform] != "" &&
		json.Unmarshal([]byte(meta[MetaInform]), &inform) == nil && inform.Channel != ""
	if !chose && !informs && meta[MetaQuestion] == "" {
		return
	}
	b.WriteString("\n## The decision\n")
	if question := meta[MetaQuestion]; question != "" {
		b.WriteString("**You asked:** " + question + "\n")
	}
	if chose {
		b.WriteString("**They chose:** " + option.Label + " (`" + option.ID + "`)\n")
	} else {
		b.WriteString("**They answered in prose** rather than choosing one of " +
			"your options — read the comment above for what they would do " +
			"instead.\n")
	}
	if informs {
		b.WriteString("**You said you would report the outcome in** " +
			inform.Channel + " on " + string(inform.Surface) + " — post it " +
			"there in this turn. The turn is not finished until you have: " +
			"the person who answered was told you would.\n")
	}
	b.WriteString("\nCarry on from this answer: it unblocks the branch you " +
		"stopped on when you asked.\n")
}

// promptHeader is the identifying block every opener shares.
func promptHeader(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	meta := n.Metadata
	b.WriteString("\n\n**Task:** " + n.Subject)
	promptCollision(b, meta)
	if lead := changeLead(meta, promptSender(n, parties)); lead != "" {
		b.WriteString("\n**What happened:** " + lead)
	}
	b.WriteString("\n**By:** " + promptSender(n, parties))
	if project := meta[MetaProject]; project != "" {
		b.WriteString("\n**Project:** " + project)
	}
	if status := meta[MetaStatus]; status != "" {
		b.WriteString("\n**Status:** " + status)
	}
	if assignee := meta[MetaAssignee]; assignee != "" {
		b.WriteString("\n**Assignee:** " + assignee)
	}
	if meta[MetaLate] == "true" {
		// WHY THEY ARE HEARING THIS NOW. A repair reaches somebody hours
		// after the change, and a reader who cannot tell that from a
		// fresh event re-reads a thread looking for what just moved.
		b.WriteString("\n**Note:** this notice is a repair — the change " +
			"itself happened earlier and the wake for it did not reach you.")
	}
	b.WriteString("\n")
}

// changeLead names what happened in one line.
//
// Worth stating plainly because the kind alone does not: a seat that had to
// infer "assigned to you" from a delta map would guess.
func changeLead(meta map[string]string, actor string) string {
	by := ""
	if actor != "" {
		by = " by " + actor
	}
	switch ChangeKind(meta[MetaChangeKind]) {
	case ChangeCreated:
		return "The task was filed" + by + "."
	case ChangeAssignee:
		return "The assignee changed" + by + "."
	case ChangeStatus:
		return "The status changed" + by + "."
	case ChangeComment:
		return "A comment was added" + by + "."
	case ChangeCommentEdited:
		return "A comment was edited" + by + "."
	case ChangeCommentResolved:
		return "A comment was resolved" + by + "."
	case ChangeCommentRemoved:
		return "A comment was removed" + by + "."
	case ChangeRelations:
		return "The task's relations changed" + by + "."
	case ChangeWatchers:
		return "The watchers changed" + by + "."
	case ChangeCollaborators:
		return "The collaborators changed" + by + "."
	case ChangeFields:
		return "Fields were edited" + by + "."
	case ChangeTags:
		return "The tags changed" + by + "."
	case ChangeChecklist:
		return "A checklist changed" + by + "."
	case ChangeReparented:
		return "The task was re-parented" + by + "."
	case ChangeMoved:
		return "The task moved project" + by + "."
	case ChangeRouted:
		return "The task was routed to another unit" + by + "."
	case ChangeArchived:
		return "The task was archived" + by + "."
	case ChangeRemoved:
		return "The task was removed" + by + "."
	case ChangeRestored:
		return "The task was restored" + by + "."
	case ChangePurged:
		// THE ONE WITH NO INVERSE, and the line says so: every other kind
		// here describes something a reader could undo or answer.
		return "The task and everything on it were permanently destroyed" +
			by + ". This cannot be undone."
	}
	return ""
}

// promptChanged renders the deltas the record already carries, and the excerpt
// beside them.
//
// NOT ON A COMMENT, whose body is the comment itself and is rendered by the
// asked opener — repeating it under a "What changed" heading reads as two
// different things having happened.
//
// THE DELTAS FIRST, THE EXCERPT AFTER. The excerpt is prose about the change
// (a purge's reason, a new task's description); the deltas are the change. A
// seat reads the heading for what moved and the paragraph for why, which is
// the order it would ask the two questions in.
func promptChanged(b *strings.Builder, n notify.Inbound) {
	switch ChangeKind(n.EventType) {
	case ChangeComment, ChangeCommentEdited:
		return
	}
	moved, said := n.Metadata[MetaDeltas], n.Body
	if moved == "" && said == "" {
		return
	}
	b.WriteString("\n## What changed\n")
	if moved != "" {
		b.WriteString(moved + "\n")
	}
	if said != "" {
		if moved != "" {
			b.WriteString("\n")
		}
		b.WriteString(said + "\n")
	}
}

// changedText is one `field: from → to` line per delta, in the engine's own
// field names.
//
// WHY THIS EXISTS AT ALL. [Notify.Fields] is documented as the deltas a card
// renders and no card rendered it: the wake said "The status changed by ada."
// and stopped, so the seat's only route to the value was a `get_work_item`
// round — which cannot recover the side it moved FROM, because that side is
// nowhere on the task. A wake that names the kind and not the move is the
// same failure the history rows were fixed for, one surface along.
//
// THE STORED TEXT, NOT A RENDERING. `deltas.go` says a value's presentation
// belongs to a surface that knows the reader's zone and the company's live
// vocabulary; this surface knows neither — a prompt is assembled on whichever
// node won the delivery — and a model reads `in_review` and a whole RFC3339
// instant perfectly well. What it must never do is invent a label, because
// the seat's own tools answer in these same names.
//
// SORTED BY FIELD NAME, because a map's range order differs between
// iterations: two nodes rendering one record, or one node rendering a
// redelivery of it, would otherwise build two different prompts from one
// change — and a prompt is kept in the event store exactly as the model saw
// it, so the difference is permanent and unexplainable.
//
// BOUNDED BY CONSTRUCTION: [MaxDeltas] caps the entries at the write, a value
// side is whole only up to [MaxDeltaValue] and described by its size past it,
// and a set's entry names only what joined and what left — so the block cannot
// outgrow its prompt and nothing here needs a cut of its own. Fields the card
// had no room for are counted on a line of their own.
//
// A SET IS ITS MOVES — "watchers: added ada, bob; removed carol" — because
// that is what the commit changed, and a model handed both whole sides has to
// diff them itself to find it.
func changedText(fields map[string]Delta, omitted int) string {
	if len(fields) == 0 && omitted == 0 {
		return ""
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, name := range names {
		delta := fields[name]
		if len(delta.Added) > 0 || len(delta.Removed) > 0 {
			var moves []string
			if len(delta.Added) > 0 {
				moves = append(moves, "added "+strings.Join(delta.Added, ", "))
			}
			if len(delta.Removed) > 0 {
				moves = append(moves, "removed "+strings.Join(delta.Removed, ", "))
			}
			b.WriteString("- " + name + ": " + strings.Join(moves, "; ") + "\n")
			continue
		}
		// AN EMPTY SIDE IS AN EM DASH, which is what every other surface
		// draws for one: "assignee:  → ada" reads as a rendering fault
		// where "assignee: — → ada" reads as an assignment.
		b.WriteString("- " + name + ": " + orDash(delta.From) + " → " +
			orDash(delta.To) + "\n")
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "- and %d more field(s) this card had no room for\n", omitted)
	}
	return strings.TrimRight(b.String(), "\n")
}

// orDash is a value, or the em dash that stands for its absence.
func orDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

// metaAddress is the reference that opens the task a wake points at: its key,
// or its id where another task claimed the key first ([ItemAddress]). Empty for
// a wake that points at no task.
//
// EVERY REFERENCE A PROMPT HANDS A SEAT IS BUILT HERE, because a seat acts on
// what it is shown: a wake about the duplicate of a key that named the key
// sent the seat to read — and comment on, and reassign — the task that
// claimed it.
func metaAddress(meta map[string]string) string {
	return ItemAddress(meta[MetaTaskID], meta[MetaTaskKey],
		meta[MetaKeyCollision] == "true")
}

// promptCollision says, under the task's own name, why a wake names it by its
// id: its key is one another task claimed first, and opens that task.
//
// SAID RATHER THAN LEFT TO THE ID, because a seat that sees an id where it is
// used to a key, and the key in the deltas or the title, reaches for the key.
func promptCollision(b *strings.Builder, meta map[string]string) {
	if meta[MetaKeyCollision] != "true" || meta[MetaTaskKey] == "" {
		return
	}
	key, id := meta[MetaTaskKey], meta[MetaTaskID]
	b.WriteString("\n**Key:** " + key + " — another task claimed this key " +
		"first, so **" + key + "** opens THAT task. This one is reached only " +
		"by its id, **" + id + "**: use the id in every call about it.")
}

// promptContext is the recon pointer.
func promptContext(b *strings.Builder, meta map[string]string) {
	key := metaAddress(meta)
	if key == "" {
		return
	}
	switch ChangeKind(meta[MetaChangeKind]) {
	case ChangeRemoved, ChangePurged:
		// NOTHING TO FETCH. Sending a seat to read a task that no longer
		// exists costs it a round and a failed tool call — and on a
		// purge the row is not in the trash either, so the tool answers
		// `not_found` rather than offering a restore.
		return
	}
	b.WriteString("\n## Get full context" +
		"\nRead **" + key + "** with `" + GetWorkItemTool + "` before " +
		"deciding anything: this notice carries what changed, not the task " +
		"and its comment thread.\n")
	if url := meta["url"]; url != "" {
		b.WriteString("Link: " + url + "\n")
	}
}

// promptWhyYou is the delegate / take it / escalate decision.
//
// REACHED ONLY FROM A FALLBACK ROUTING. A directed routing carries its own
// signal — being assigned or named says what is wanted — while a fallback says
// only that nobody else here was available, which is a fact about the org
// chart rather than about the work. Left unexplained, a lead reads it as "this
// is mine" and quietly absorbs every unowned task in their project.
func promptWhyYou(b *strings.Builder, project string) {
	where := "this project"
	if project != "" {
		where = "the **" + project + "** project"
	}
	b.WriteString("\n## Why you received this" +
		"\nYou lead the team that owns " + where + ", and the task names" +
		" nobody here — no assignee on your team, no watcher, no mention. You" +
		" are its owner by FALLBACK, not because the work is yours, and nobody" +
		" else is following it." +
		"\n\nDecide one of:" +
		"\n- **Delegate** — set the assignee to the right teammate with `" +
		UpdateWorkItemTool + "`; later changes route to them." +
		"\n- **Take it yourself** — only if the work clearly falls to you:" +
		" assign it to yourself." +
		"\n- **Escalate** — if it is out of scope or you cannot tell whose it" +
		" is, hand it to your own manager: mention them with `" +
		CommentOnWorkTool + "`, or reassign it to them.\n")
}

// promptHandling is the mechanics block.
//
// Tracker conventions only — what a shared multi-agent surface needs that a
// model would not assume: who stays out when nobody asked, the status and
// assignee that tell everyone the work started, one summary rather than a
// running log, and how a seat hands back a task it holds.
// Role-specific behaviour — ownership, tone, the quality bar — belongs in the
// role's behavioural guidelines, and repeating it here would put one team's
// standards on every seat. Generic diligence (read the comments first, ask
// rather than guess) is the model's own and is not restated.
func promptHandling(b *strings.Builder, meta map[string]string, reason Reason) {
	if ChangeKind(meta[MetaChangeKind]) == ChangePurged {
		// A DIFFERENT INSTRUCTION FROM A REMOVAL, because the remedies
		// differ: a removed task is in the trash and `restore_task`
		// brings it back, and a purged one is gone from every data node
		// with no gesture that returns it. Telling a lead to "record any
		// progress worth keeping" is right in both; telling them the
		// task can come back is right in only one.
		b.WriteString("\n## How to handle this" +
			"\nThe task, its comments, its history and its turn records were" +
			" destroyed on every node, and nothing restores them. Stop any" +
			" in-flight work on it. If anybody on your team had progress worth" +
			" keeping, it now exists only where they wrote it down." +
			"\n\nYou are hearing this because you lead the project it was" +
			" filed in. No action is required of you beyond knowing.\n")
		return
	}
	if ChangeKind(meta[MetaChangeKind]) == ChangeRemoved {
		b.WriteString("\n## How to handle this" +
			"\nThe task no longer exists — stop any in-flight work on it. If" +
			" you had progress worth keeping, record it where your team tracks" +
			" such things; otherwise no action is needed.\n")
		return
	}
	if isFallback(reason) {
		// The delegate / take it / escalate block above already IS this
		// seat's instruction, and a second list under a second heading
		// makes a lead read two sets of steps and follow neither.
		return
	}

	b.WriteString("\n## How to handle this")
	if !reason.Addressed() {
		// STAYING OUT IS FOR A WAKE NOBODY IS WAITING ON. On an addressed
		// one the delivery check refuses a silent end, so telling that
		// seat to stay silent — because the task, as it usually is on a
		// question or a mention, is a colleague's — is an instruction the
		// engine sends straight back for a second round.
		b.WriteString("\n- If its assignee is neither you nor on your team," +
			" stay silent unless you hold information that blocks their" +
			" decision.")
	}
	// TAKING THE TASK ON, not acting on the wake: a seat answering a
	// question on a colleague's task acts on it too, and must not come
	// away holding the assignee field.
	b.WriteString("\n- If you take the task on, move it to an active status" +
		" with `" + UpdateWorkItemTool + "`, make sure the assignee names" +
		" you, and post ONE summary with `" + CommentOnWorkTool + "` when" +
		" you are done. Other people read this surface: no running" +
		" commentary, acknowledgements or agreement-only comments.")
	if holdsTask(reason) {
		// THE DECLINE GESTURE, AND ONLY FOR THE TASK'S HOLDER. That an
		// addressed wake may not end in silence is the decoder's refusal,
		// and that a decline names the right owner on the surface the ask
		// came from is the executor's contract, so neither is restated;
		// what only the tracker knows is that a task the seat HOLDS is
		// declined by moving it. Every other reason is about a
		// task that is ordinarily somebody else's — the person asked,
		// mentioned or answered, the reporter, a thread participant — and
		// telling that seat to reassign it hands a colleague's work away.
		b.WriteString("\n- If the work is not yours, reassign it to the" +
			" right teammate — your manager if you cannot tell.")
	}
	b.WriteString("\n")
}

// holdsTask reports the reasons whose seat holds the task the wake is about,
// which are the two a task-level decline — reassigning it — is open to.
//
// `blocking` IS ONE: its only handle is the blocker's own assignee, and the
// wake is written on the blocker, so the task in the prompt is this seat's.
// Both are addressed ([Reason.Addressed]); nothing outside that set is offered
// a decline, because a seat nobody is waiting on has nothing to decline.
func holdsTask(reason Reason) bool {
	return reason == ReasonAssignee || reason == ReasonBlocking
}

// promptSender renders the actor as a colleague.
//
// Through the party registry first, because the actor is a HANDLE here and a
// handle is not what a person is called — a prompt rendering "eng" where a
// reader expects "Engineer (eng)" makes the roster and the notification look
// like two different companies.
func promptSender(n notify.Inbound, parties notify.Parties) string {
	actor := n.Metadata[notify.ActorField]
	if actor == "" {
		actor = n.Sender
	}
	if parties != nil && actor != "" {
		if party, ok := parties.ByHandle(actor); ok {
			if label := party.Label(); label != "" {
				return label
			}
		}
	}
	if actor == "" {
		return "someone"
	}
	return actor
}
