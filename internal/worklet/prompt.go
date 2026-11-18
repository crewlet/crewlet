package worklet

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/words"
)

// Prompt is what a change to a task asks of the seat it reached.
//
// It tailors to the routing reason the parser stamped, not the change kind:
// one assignment reaches the assignee, watchers and perhaps the unit lead,
// each owing something different, and a prompt keyed on the kind tells all
// three the weakest thing. The eighteen reasons collapse to three frames
// (somebody is asking you, this is your work, this is activity you follow):
// eighteen openers would be eighteen places for one sentence to drift.
//
// It names tools, the exception to the tool-capabilities doctrine: this build
// ships and registers them, under names it chose, on every seat that can read
// this notification.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Source }

// RequiresRecon implements [notify.Prompt]: true for a pointer, false for
// prose. The flag also suppresses personal-memory and episode recall, so
// setting it on a wake that carries its meaning costs the seat its context for
// nothing. A comment carries what somebody said; a bare transition is a
// pointer at a task the seat must read first.
func (Prompt) RequiresRecon(n notify.Inbound) bool {
	if metaAddress(n.Metadata) == "" {
		return false
	}
	switch ChangeKind(n.EventType) {
	case ChangeComment, ChangeCommentEdited:
		return strings.TrimSpace(n.Body) == ""
	case ChangePrioritised:
		// Not a pointer: it names the task, who put it there and the position.
		// It reaches this switch only as the one non-task wake carrying a task
		// key (the top of the list), which alone would make it a pointer.
		return false
	}
	return true
}

// SubjectIsLabel implements [notify.Prompt]: never — the subject names the
// task and its title, which on a comment is the only place the topic is named
// at all.
func (Prompt) SubjectIsLabel(notify.Inbound) bool { return false }

// Addressed implements [notify.Prompt]: somebody is waiting on THIS seat —
// the eight primary reasons only. A watcher follows the task and a fallback is
// the task landing somewhere; a seat obliged to answer either would comment on
// every field change in its unit's projects.
func (Prompt) Addressed(n notify.Inbound) bool {
	return Reason(n.Metadata[MetaVia]).Addressed()
}

var _ notify.LaneRule = Prompt{}

// PersonWaits implements [notify.LaneRule]: a person's comment waits on a
// reply; a task they filed, assigned or moved is the seat's work, which the
// work pickup hands in its order (ADR-0159).
func (Prompt) PersonWaits(n notify.Inbound) bool {
	switch ChangeKind(n.EventType) {
	case ChangeComment, ChangeCommentEdited:
		return strings.TrimSpace(n.Body) != ""
	}
	return false
}

// Owes is the chat surface a wake OBLIGES its seat to post on, or "".
//
// Only the answer to a decision whose asker promised to report the outcome
// ([Decision.Inform]), reaching that asker under `answered`; every other copy
// carries the inform too and owes nothing, since the promise was the asker's.
// A surface, never a channel: the engine holds the turn open until a tool on
// that surface delivers ([turn.DeliveredTo]), and a tool reports the surface,
// not the channel. Read from the parser's metadata so [Parser.Parse] and the
// partition key answer from one rule.
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
// a wake that owes a chat surface is a finer cut of it. The cut keeps an
// obligation whole through a merge: a turn owes at most one surface
// ([turn.Reply]), so two answers informing two surfaces, coalesced, would drop
// a promise; apart, each partition owes one or none ([notify.Coalesce]).
func (p Prompt) PartitionKey(metadata map[string]string, subject string) string {
	key := p.ConversationIdentity(metadata, subject)
	if surface := Owes(metadata); surface != "" && key != "" {
		return key + owedPartition + surface
	}
	return key
}

// ConversationIdentity implements [notify.Prompt]: the TASK is the
// conversation, named `task:<id>`. The partition key is this or a finer cut
// of it, as [notify.Prompt.ConversationIdentity] requires.
//
// The id, never the key: a key is an address. Two tasks can hold one (a
// restored counter re-mints numbers, flagged `key_collision`), so their wakes
// would share one digest and ledger; and a cross-project move re-keys a task,
// orphaning its ledger and any question parked on it. A non-task wake's
// object uses the same `<object>:<id>` grammar, so the two never collide. The
// id is the one the wake points at ([Snapshot.TaskID]), so a `prioritised`
// wake, written on a person, joins the task at the top of their list.
func (Prompt) ConversationIdentity(metadata map[string]string, _ string) string {
	if id := metadata[MetaTaskID]; id != "" {
		return ItemConversation(id)
	}
	// A non-task wake keys on its own object: a shared empty key would merge
	// every person-scoped wake in the company into one ledger.
	if id := metadata[MetaObjectID]; id != "" {
		return metadata[MetaObject] + ":" + id
	}
	return ""
}

// WorkItem is the task a wake is about, which is the item the turn it wakes
// is charged to: [MetaTaskID], which the parser writes only when the wake is
// about a task. A wake about a person's own list names no item, since
// charging it to the object id would charge it to a handle.
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

// WakesActor implements [notify.Prompt]: only for the reason that tells an
// actor a consequence of their own write, a closed blocker's task becoming
// workable ([Reason.WakesActor], the rule [Route] lets it through by). Read
// off this copy's routing reason ([MetaVia]), never the event type: every
// copy of that status change shares the type `status`.
func (Prompt) WakesActor(n notify.Inbound) bool {
	return Reason(n.Metadata[MetaVia]).WakesActor()
}

// Provenance implements [notify.Prompt]: what the parser stated (the comment,
// description or change note, by the change's author; [notify.StatedHosted]),
// or for a wake built without the parser, the surface its change kind
// produces. Hosted, because only company principals write the engine's own
// tracker, so a record naming nobody reads as a colleague's
// ([provenance.TrustForHosted]).
func (Prompt) Provenance(n notify.Inbound, parties notify.Parties) notify.Provenance {
	return notify.StatedHosted(n, parties, ExcerptSource(ChangeKind(n.EventType)))
}

// DigestBody implements [notify.Prompt]: comments keep their text, field and
// status changes collapse to their lead. Each comment differs; a field
// change's body repeats the digest's lead, and five of them bury the comment.
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
	// The object decides the frame before the reason: the task frame's
	// openers, **Task:** header and get_work_item pointer are untrue of a
	// priority list. A task is the default, since the parser always sets
	// `object`; a hand-built wake without it renders as a task.
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
// named, rather than because of anything about them. Named, not derived from
// the candidate's FallbackOnly flag, which routing has resolved before a
// prompt runs.
func isFallback(reason Reason) bool { return reason == ReasonLeadFallback }

// promptOpener writes the one sentence that says why this seat is reading
// this, and — for the reasons that are ABOUT something somebody said — the
// text itself.
//
// One switch, not one function per [Reason.Primary] half: split, arms sat
// where their reason could never reach them and nothing showed the mispairing.
// The default is for a reason a newer peer routed during a rolling upgrade:
// "a task you are named on changed" is true of every reason.
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
		default:
			// Read off [MetaMoved], not the record's kind: a hand-off written
			// with a status is a `status` record, and a reassertion to the
			// holder hands it nothing.
			if slices.Contains(metaMoved(n.Metadata), ChangeAssignee) {
				b.WriteString("A task was assigned to you.")
			} else {
				b.WriteString("A task you are assigned to changed.")
			}
		}
	case ReasonUnassigned:
		b.WriteString("A task you were assigned to went to somebody else.")
	case ReasonReporter:
		b.WriteString("A task you filed changed.")
	case ReasonUnblocked:
		b.WriteString("A task you are waiting on is now workable — every " +
			"blocker on it has finished.")
	case ReasonBlocking:
		// The blocker's own assignee: say that somebody waits on this seat's
		// task, a call on its time rather than news.
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
		// Not "changed": a reader expecting an edit looks for what moved, on a
		// row that is gone.
		b.WriteString("A task in your project was permanently destroyed.")
	case ReasonLeadFallback:
		b.WriteString("A task in your team's project has activity and nobody " +
			"here is named on it.")
	default:
		b.WriteString("A task you are named on changed.")
	}
	promptHeader(b, n, parties)
	// A comment is quoted whatever brought it here: to an assignee, reporter
	// or watcher the change IS the remark. [promptChanged] leaves a comment's
	// body to this opener and [Prompt.RequiresRecon] reports it as carried,
	// so dropping it would send the seat to read the task for it.
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
// The answering call is written out whole, ids included: the ask's comment id
// sits in a thread the seat has no other reason to read, and an option id is
// typed back exactly or refused. Only for the two reasons the decision is
// addressed to; a watcher sees the question as a comment. An undecodable
// decision renders nothing rather than half a block.
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
	// The recommended option in the example, as the asker's likeliest, else
	// the first; the text after the call says to change it, and the id list
	// beside it is what the choice is checked against.
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
		"naming the option you choose by its id in `choice` (one of " +
		words.Or(d.OptionIDs()) + "):\n\n" +
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

// promptAnsweredDecision is the choice block the asker reads. It says what
// happens next: the escalation guidance ended the asker's turn blocked on this
// branch, and this wake is what it was waiting for.
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
		// A repair arrives hours after the change; unmarked, it sends the
		// reader through the thread for what just moved.
		b.WriteString("\n**Note:** this notice is a repair — the change " +
			"itself happened earlier and the wake for it did not reach you.")
	}
	b.WriteString("\n")
}

// changeLead names what happened in one line, since a seat inferring
// "assigned to you" from a delta map would guess. It names every kind of
// change the write made ([MetaMoved]), not only the record's one kind ("assign
// it to ana and set it to todo" is a `status` record): the record's kind leads
// where its field moved or it names its own event, and the others follow.
func changeLead(meta map[string]string, actor string) string {
	by := ""
	if actor != "" {
		by = " by " + actor
	}
	kind, moved := ChangeKind(meta[MetaChangeKind]), metaMoved(meta)
	if kind.MovedFields() != nil && !slices.Contains(moved, kind) {
		// A field kind whose field did not move is a reassertion beside the
		// write's real move, another kind's or an unnamed edit (a priority, a
		// date). A record that moved nothing keeps its kind's own line.
		switch {
		case len(moved) > 0:
			kind = moved[0]
		case meta[MetaDeltas] != "":
			kind = ChangeFields
		}
	}
	lead := kindLead(kind, by)
	var also []words.Said
	for _, other := range moved {
		if other != kind {
			also = append(also, words.Said(movedNoun(other)))
		}
	}
	if lead == "" || len(also) == 0 {
		return lead
	}
	others := words.And(also)
	return lead + " " + strings.ToUpper(others[:1]) + others[1:] + " changed too."
}

// metaMoved is [MetaMoved] read back: the kinds of change a wake's write made,
// nil where it names none. A member this build has no move for is skipped, as a
// newer peer may name a kind of change this one cannot say.
func metaMoved(meta map[string]string) []ChangeKind {
	raw := meta[MetaMoved]
	if raw == "" {
		return nil
	}
	var out []ChangeKind
	for _, part := range strings.Split(raw, ",") {
		if kind := ChangeKind(part); kind.Valid() && movedNoun(kind) != "" {
			out = append(out, kind)
		}
	}
	return out
}

// movedNoun names what a kind of change moves, for the sentence that lists a
// write's other moves ([changeLead]); "" for a kind whose change is no one
// field's move ([ChangeKind.MovedFields]).
func movedNoun(kind ChangeKind) string {
	switch kind {
	case ChangeAssignee:
		return "the assignee"
	case ChangeStatus:
		return "the status"
	case ChangeCollaborators:
		return "the collaborators"
	case ChangeWatchers:
		return "the watchers"
	case ChangeTags:
		return "the tags"
	case ChangeRouted:
		return "the team"
	case ChangeRelations:
		return "the task's relations"
	}
	return ""
}

// kindLead is the line one kind of change is told in, by its actor.
func kindLead(kind ChangeKind, by string) string {
	switch kind {
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
	case ChangeLinkSettled:
		return "A pull request this task waits on was merged or closed. Update the task: " +
			"its status, what is left, or what follows."
	case ChangePurged:
		// The one with no inverse, and the line says so.
		return "The task and everything on it were permanently destroyed" +
			by + ". This cannot be undone."
	}
	return ""
}

// promptChanged renders the deltas the record already carries, and the excerpt
// beside them. Not on a comment, whose body the opener renders; repeating it
// here reads as two things having happened. Deltas first: they are what moved,
// and the excerpt (a purge's reason, a description) is why.
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
// field names: without it the seat must spend a `get_work_item` round, which
// cannot recover the FROM side.
//
// The stored text, not a rendering: this node knows neither the reader's zone
// nor the live vocabulary (`deltas.go`), and an invented label would not match
// the seat's own tools. Sorted by field name, since a prompt is stored as the
// model saw it and two renderings of one record must agree. Bounded at the
// write ([MaxDeltas], [MaxDeltaValue]), so no cut is needed here; fields the
// card omitted are counted. A set is its moves ("watchers: added ada; removed
// carol"), so the model need not diff two whole sides.
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
		// An empty side is an em dash, as on every other surface:
		// "assignee:  → ada" reads as a rendering fault.
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
// a wake that points at no task. Every reference a prompt hands a seat is built
// here, since a seat acts on what it is shown and a collided key opens the
// claimant.
func metaAddress(meta map[string]string) string {
	return ItemAddress(meta[MetaTaskID], meta[MetaTaskKey],
		meta[MetaKeyCollision] == "true")
}

// promptCollision says, under the task's own name, why a wake names it by its
// id: its key opens another task that claimed it first. Said outright, since a
// seat seeing an id where it expects a key, with the key nearby, reaches for it.
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
		// Nothing to fetch: the read would fail, and a purged row is not in the
		// trash, so the tool answers `not_found` with no restore.
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

// promptWhyYou is the delegate / take it / escalate decision, for a fallback
// routing only: a directed one says what is wanted, while a fallback says only
// that nobody else was available, and unexplained a lead absorbs every unowned
// task in their project.
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
		CommentOnWorkTool + "` and `asks: true`, or reassign it to them.\n")
}

// promptHandling is the mechanics block: tracker conventions a model would not
// assume on a shared surface (who stays out, the status and assignee that show
// work started, one summary not a running log, how to hand a task back).
// Role-specific behaviour belongs in the role's guidelines, and generic
// diligence is the model's own.
func promptHandling(b *strings.Builder, meta map[string]string, reason Reason) {
	if ChangeKind(meta[MetaChangeKind]) == ChangePurged {
		// Not a removal's instruction: a removed task is in the trash and
		// `restore_task` returns it; a purged one is gone from every data node.
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
		// The fallback block above is this seat's instruction; a second list
		// makes a lead follow neither.
		return
	}

	b.WriteString("\n## How to handle this")
	if !reason.Addressed() {
		// Staying out is for a wake nobody waits on: on an addressed one the
		// delivery check refuses a silent end and sends it back for a second round.
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
		// The decline gesture, for the task's holder only: the decoder refuses a
		// silent end and the executor's contract names the owner, so only the
		// tracker's part is said. Other reasons are about a colleague's task,
		// which reassigning would hand away.
		b.WriteString("\n- If the work is not yours, reassign it to the" +
			" right teammate — your manager if you cannot tell.")
	}
	b.WriteString("\n")
}

// holdsTask reports the reasons a decline by reassignment is open to: those
// whose seat holds the task the wake is about ([Reason.Held]) and is waited on
// ([Reason.Addressed]); a seat nobody waits on has nothing to decline.
func holdsTask(reason Reason) bool {
	return reason.Held() && reason.Addressed()
}

// promptSender renders the actor as a colleague: through the party registry
// first, since the actor is a handle ("eng") and the roster calls them
// "Engineer (eng)".
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

// ItemConversation is the conversation a task IS, named by its id: what every
// wake about the task is filed under ([Prompt.ConversationIdentity]), and what
// the work pickup stamps its wake with, so a pickup's turn continues the
// task's own conversation (ADR-0112).
func ItemConversation(id string) string { return string(KindTask) + ":" + id }
