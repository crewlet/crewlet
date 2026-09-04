package work

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// NewItem is what a caller supplies to file work.
type NewItem struct {
	Project string
	Type    Type
	Title   string
	Body    string

	ParentID string
	Assignee string
	Priority Priority
	Labels   []string
	Due      *time.Time

	// Status overrides the default. EMPTY IS THE USEFUL CASE: an item
	// created with an assignee starts at todo, and one created without
	// starts at triage — where the unit lead is woken for it. That default
	// is what makes an unassigned item somebody's problem rather than
	// nobody's, so a caller that names a status is opting out of it.
	Status Status

	// Watchers are added beyond the automatic ones (the reporter and any
	// assignee).
	Watchers []string

	// Quiet suppresses the wake. For an import, never for ordinary work.
	Quiet bool
}

// Written is what a write reports back.
//
// The REVISION is the ETag a caller conditions its next write on, and the one
// a read-your-writes wait is taken on. Carried on every write rather than
// re-read, because a direct get after a write is not read-your-writes on a
// replicated stream.
type Written struct {
	Item     Item
	Revision uint64

	// ChangeID names the record the wake will be derived from, so a caller
	// can correlate its write with the wake it caused.
	ChangeID string
}

// Create files a new item.
//
// TWO KEYS, IN ORDER: the project counter is compare-and-set first, then the
// item is created under the number it minted. A crash between them leaves a
// NUMBERING GAP — ENG-7 exists, ENG-8 never did, ENG-9 is next — which is
// what Jira does too, and is strictly better than the alternative: minting
// the item first and the number after would let two items share a key, and a
// key is what people paste into chat.
func (s *Store) Create(ctx context.Context, actor Actor, in NewItem) (Written, error) {
	if err := actor.validate(); err != nil {
		return Written{}, err
	}
	if err := s.validateNew(in); err != nil {
		return Written{}, err
	}

	number, err := s.mint(ctx, in.Project)
	if err != nil {
		return Written{}, err
	}

	at := s.now()
	item := Item{
		V: DocumentVersion, ID: s.newID(),
		Key: FormatKey(in.Project, number), Project: in.Project,
		Type: in.Type, ParentID: in.ParentID,
		Title: strings.TrimSpace(in.Title), Body: in.Body,
		Status: in.Status, Priority: in.Priority,
		Reporter: actor.Handle, Assignee: in.Assignee,
		Labels: cleanList(in.Labels), Due: in.Due,
		CreatedAt: at, UpdatedAt: at, ChangeSeq: 1,
	}
	if item.Priority == "" {
		item.Priority = PriorityNone
	}
	if item.Status == "" {
		// The default that makes an unassigned item somebody's problem.
		item.Status = StatusTriage
		if item.Assignee != "" {
			item.Status = StatusTodo
		}
	}
	addWatcher(&item, actor.Handle)
	addWatcher(&item, in.Assignee)
	for _, w := range cleanList(in.Watchers) {
		addWatcher(&item, w)
	}

	change := s.change(actor, item, ChangeCreated, at)
	change.Quiet = in.Quiet
	change.Excerpt = excerpt(item.Title)
	item.LastChange = &change

	data, err := EncodeItem(item)
	if err != nil {
		return Written{}, err
	}
	created, err := s.docs.CreateDocument(ctx, coord.FamilyWork, ItemKey(item.ID), data)
	if err != nil {
		return Written{}, fmt.Errorf("work: create %s: %w", item.Key, err)
	}
	if !created {
		// A minted uuid colliding is not a race a retry fixes, it is a
		// broken id source — and silently reusing the row would file this
		// work onto somebody else's item.
		return Written{}, fmt.Errorf("work: item id %s already exists", item.ID)
	}
	revision, err := s.revisionOf(ctx, ItemKey(item.ID))
	if err != nil {
		return Written{}, err
	}
	change.HeadRevision = revision
	if err := s.writeChange(ctx, change); err != nil {
		return Written{}, err
	}
	return Written{Item: item, Revision: revision, ChangeID: change.ID}, nil
}

// mint takes the next number in a project, compare-and-set.
//
// It NEVER reads the projection. A stale local number would mint a key that
// already exists, and the projection is behind by design.
func (s *Store) mint(ctx context.Context, project string) (int, error) {
	key := CounterKey(project)
	for range casRounds {
		rec, found, err := s.docs.Document(ctx, coord.FamilyWork, key)
		if err != nil {
			return 0, fmt.Errorf("work: read the %s counter: %w", project, err)
		}
		if !found {
			data, err := EncodeCounter(Counter{V: DocumentVersion, Project: project, Last: 1})
			if err != nil {
				return 0, err
			}
			created, err := s.docs.CreateDocument(ctx, coord.FamilyWork, key, data)
			if err != nil {
				return 0, fmt.Errorf("work: start the %s counter: %w", project, err)
			}
			if created {
				return 1, nil
			}
			// Somebody else started it. Re-read and take the next.
			continue
		}
		counter, err := DecodeCounter(rec.Value)
		if err != nil {
			return 0, err
		}
		counter.Last++
		counter.Project = project
		counter.V = DocumentVersion
		data, err := EncodeCounter(counter)
		if err != nil {
			return 0, err
		}
		ok, err := s.docs.UpdateDocument(ctx, coord.FamilyWork, key, data, rec.Version)
		if err != nil {
			return 0, fmt.Errorf("work: advance the %s counter: %w", project, err)
		}
		if ok {
			return counter.Last, nil
		}
	}
	return 0, fmt.Errorf("%w: the %s counter lost %d races in a row",
		ErrConflict, project, casRounds)
}

// revisionOf reads a key's current version.
//
// USED ONLY AFTER A WRITE THIS PROCESS MADE, to learn the revision the write
// produced. It is NOT read-your-writes in general — a direct get is answered
// by whichever replica takes it — so a not-found here is a fault rather than
// an absence, and the caller never concludes the record is gone.
func (s *Store) revisionOf(ctx context.Context, key string) (uint64, error) {
	rec, found, err := s.docs.Document(ctx, coord.FamilyWork, key)
	if err != nil {
		return 0, fmt.Errorf("work: read back %s: %w", key, err)
	}
	if !found {
		return 0, fmt.Errorf("work: %s vanished immediately after it was written", key)
	}
	return rec.Version, nil
}

// change builds a change record for an item's current state.
func (s *Store) change(actor Actor, item Item, kind ChangeKind, at time.Time) Change {
	return Change{
		V: DocumentVersion, ID: s.newSeqID(), ItemID: item.ID, Kind: kind,
		Actor: actor.Name(), ActorKind: actor.Kind, OperatorID: actor.OperatorID,
		TurnID: actor.TurnID, Chain: slices.Clone(actor.Chain),
		Snapshot: snapshotOf(item), CreatedAt: at,
	}
}

// writeChange creates the change key, which is what a wake is derived from.
//
// LAST IN EVERY SEQUENCE, AND CREATE-ONLY. A crash before it leaves a head
// with no change key, which any projector repairs from the head's own
// LastChange record — where a crash before the HEAD write leaves nothing at
// all, which is the correct outcome for a write that did not happen.
func (s *Store) writeChange(ctx context.Context, change Change) error {
	data, err := EncodeChange(change)
	if err != nil {
		return err
	}
	created, err := s.docs.CreateDocument(ctx, coord.FamilyWork,
		ChangeKey(change.ItemID, change.ID), data)
	if err != nil {
		return fmt.Errorf("work: record change %s: %w", change.ID, err)
	}
	if !created {
		// A change key that already exists is a REPAIR that got there
		// first — a peer rebuilding this exact change from the head. The
		// record is identical, so this is success rather than a fault.
		log.DebugContext(ctx, "work_change_already_recorded",
			"item", change.ItemID, "change", change.ID,
			"detail", "a peer repaired this change key from the head first")
	}
	return nil
}

// Edit is a partial update to an item. A nil field is unchanged; the pointer
// is what tells "set this to empty" from "leave it alone", which a plain
// string cannot.
type Edit struct {
	Title    *string
	Body     *string
	Status   *Status
	Priority *Priority
	Assignee *string
	ParentID *string
	Due      **time.Time
	Labels   *[]string

	// CloseReason and DuplicateOf accompany a move to a terminal status.
	CloseReason *CloseReason
	DuplicateOf *string

	// Watch adds or removes the actor from the watcher set. Removing sets
	// the mute, which is what makes the unwatch stick against every
	// automatic re-add.
	Watch *bool

	// AddLinks and RemoveLinks change this item's authored link ends.
	AddLinks    []Link
	RemoveLinks []Link

	// Quiet suppresses the wake, for an import.
	Quiet bool
}

// Update applies an edit, compare-and-set against the head.
//
// IF-MATCH IS OPTIONAL AND MEANS TWO DIFFERENT THINGS. With a version, a
// stale one is REFUSED outright: the caller decided from a state that has
// moved, and a person editing a title against a description somebody else
// rewrote should be told. Without one, the edit is MERGED per field onto the
// freshest head, which is what an agent's tool call needs — it names the two
// fields it means and must not lose a race to a comment somebody added while
// the model was thinking.
func (s *Store) Update(ctx context.Context, actor Actor, itemID string, ifMatch uint64, edit Edit) (Written, error) {
	if err := actor.validate(); err != nil {
		return Written{}, err
	}
	var out Written
	err := s.mutate(ctx, itemID, ifMatch, func(item *Item) (Change, error) {
		return s.applyEdit(actor, item, edit)
	}, &out)
	return out, err
}

// mutate is the read-decide-write loop every head change goes through.
func (s *Store) mutate(ctx context.Context, itemID string, ifMatch uint64,
	decide func(*Item) (Change, error), out *Written) error {
	key := ItemKey(itemID)
	for round := range casRounds {
		rec, found, err := s.docs.Document(ctx, coord.FamilyWork, key)
		if err != nil {
			return fmt.Errorf("work: read %s: %w", itemID, err)
		}
		if !found {
			return fmt.Errorf("%w: item %s", ErrNotFound, itemID)
		}
		if ifMatch != 0 && rec.Version != ifMatch {
			return fmt.Errorf("%w: it is at revision %d, not %d",
				ErrStaleVersion, rec.Version, ifMatch)
		}
		item, err := DecodeItem(rec.Value)
		if err != nil {
			return err
		}
		change, err := decide(&item)
		if err != nil {
			return err
		}
		if change.Kind == "" {
			// Nothing to do. Reporting the current state rather than a
			// no-op error: an agent that sets a status to what it already
			// is has not failed, and telling it so would cost a round.
			*out = Written{Item: item, Revision: rec.Version}
			return nil
		}
		item.UpdatedAt = change.CreatedAt
		item.ChangeSeq++
		change.Snapshot = snapshotOf(item)
		item.LastChange = &change

		data, err := EncodeItem(item)
		if err != nil {
			return err
		}
		ok, err := s.docs.UpdateDocument(ctx, coord.FamilyWork, key, data, rec.Version)
		if err != nil {
			return fmt.Errorf("work: write %s: %w", itemID, err)
		}
		if !ok {
			if ifMatch != 0 {
				// The caller conditioned on a version, and it moved
				// between the read and the write. Refusing rather than
				// retrying is the whole point of If-Match.
				return fmt.Errorf("%w: it changed while this write was in flight",
					ErrStaleVersion)
			}
			log.DebugContext(ctx, "work_head_cas_retry", "item", itemID, "round", round+1)
			continue
		}
		revision, err := s.revisionOf(ctx, key)
		if err != nil {
			return err
		}
		change.HeadRevision = revision
		if err := s.writeChange(ctx, change); err != nil {
			return err
		}
		*out = Written{Item: item, Revision: revision, ChangeID: change.ID}
		return nil
	}
	return fmt.Errorf("%w: %s lost %d races in a row", ErrConflict, itemID, casRounds)
}

// applyEdit decides what an edit changes, and refuses what it may not.
//
// It returns a change with an EMPTY KIND when nothing moved, which the caller
// reads as a no-op rather than as an error.
func (s *Store) applyEdit(actor Actor, item *Item, edit Edit) (Change, error) {
	at := s.now()
	fields := map[string]Delta{}
	kinds := map[ChangeKind]bool{}

	set := func(name string, from, to string, kind ChangeKind) {
		if from == to {
			return
		}
		fields[name] = Delta{From: from, To: to}
		kinds[kind] = true
	}

	if edit.Title != nil {
		title := strings.TrimSpace(*edit.Title)
		if err := s.checkTitle(title); err != nil {
			return Change{}, err
		}
		set("title", item.Title, title, ChangeFields)
		item.Title = title
	}
	if edit.Body != nil {
		if len(*edit.Body) > MaxBody {
			return Change{}, invalid("body",
				"%d bytes, past the %d-byte cap — an item body is refused rather "+
					"than cut, because a specification truncated mid-sentence is "+
					"one an agent acts on the wrong half of", len(*edit.Body), MaxBody)
		}
		if item.Body != *edit.Body {
			// The BODIES ARE NOT IN THE DELTA. A 64 KiB before-and-after
			// in an ageless change bucket is two copies of the item per
			// edit, for ever, to render a line nobody reads.
			fields["body"] = Delta{From: "(previous)", To: excerpt(*edit.Body)}
			kinds[ChangeFields] = true
			item.Body = *edit.Body
		}
	}
	if edit.Priority != nil {
		if !edit.Priority.Valid() {
			return Change{}, invalid("priority", "%q is not one of %v", *edit.Priority, Priorities())
		}
		set("priority", string(item.Priority), string(*edit.Priority), ChangeFields)
		item.Priority = *edit.Priority
	}
	if edit.ParentID != nil {
		set("parent", item.ParentID, *edit.ParentID, ChangeFields)
		item.ParentID = *edit.ParentID
	}
	if edit.Due != nil {
		before, after := formatDue(item.Due), formatDue(*edit.Due)
		set("due", before, after, ChangeFields)
		item.Due = *edit.Due
	}
	if edit.Labels != nil {
		labels := cleanList(*edit.Labels)
		if err := s.checkLabels(labels); err != nil {
			return Change{}, err
		}
		if !slices.Equal(item.Labels, labels) {
			set("labels", strings.Join(item.Labels, ", "), strings.Join(labels, ", "), ChangeFields)
			item.Labels = labels
		}
	}
	if edit.Status != nil {
		if err := s.applyStatus(item, edit, fields, kinds, at); err != nil {
			return Change{}, err
		}
	}
	if edit.Assignee != nil {
		if err := s.applyAssignee(actor, item, *edit.Assignee, fields, kinds); err != nil {
			return Change{}, err
		}
	}
	if edit.Watch != nil {
		s.applyWatch(actor, item, *edit.Watch, kinds)
	}
	if err := s.applyLinks(item, edit, fields, kinds); err != nil {
		return Change{}, err
	}
	if len(kinds) == 0 {
		return Change{}, nil
	}

	// ANY HUMAN WRITE RESETS THE HAND-OFF BUDGET. A person looking at the
	// item is exactly the event the budget is waiting for, and it is reset
	// on any human touch rather than only on a reassignment so that
	// unblocking an item — a comment, a priority bump — hands the agents a
	// fresh budget without a second gesture.
	if actor.IsHuman() {
		item.Reassignments = 0
	}

	change := s.change(actor, *item, dominantKind(kinds), at)
	change.Fields = fields
	change.Quiet = edit.Quiet
	change.Excerpt = excerptOfFields(fields)
	return change, nil
}

// dominantKind picks the one kind a change is reported as.
//
// ONE KIND PER CHANGE, and the order is what a RECIPIENT most needs to be
// told: an edit that moves the assignee and the priority together is an
// assignment — the person now owning it needs to know that first, and the
// priority is in the delta map either way.
func dominantKind(kinds map[ChangeKind]bool) ChangeKind {
	for _, kind := range []ChangeKind{
		ChangeRemoved, ChangeAssignee, ChangeStatus, ChangeLinks,
		ChangeWatchers, ChangeFields,
	} {
		if kinds[kind] {
			return kind
		}
	}
	return ""
}

// excerptOfFields renders a delta map into a card's lead line.
func excerptOfFields(fields map[string]Delta) string {
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		d := fields[name]
		switch {
		case d.From == "":
			parts = append(parts, fmt.Sprintf("%s set to %s", name, d.To))
		case d.To == "":
			parts = append(parts, fmt.Sprintf("%s cleared", name))
		default:
			parts = append(parts, fmt.Sprintf("%s %s → %s", name, d.From, d.To))
		}
	}
	return excerpt(strings.Join(parts, "; "))
}

func formatDue(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *Store) checkTitle(title string) error {
	switch {
	case title == "":
		return invalid("title", "an item needs a title — it is the line every board and every notification shows")
	case len(title) > MaxTitle:
		return invalid("title", "%d bytes, past the %d-byte cap", len(title), MaxTitle)
	}
	return nil
}

func (s *Store) checkLabels(labels []string) error {
	if len(labels) > MaxLabels {
		return invalid("labels", "%d labels, past the cap of %d", len(labels), MaxLabels)
	}
	for _, l := range labels {
		if len(l) > MaxLabelLength {
			return invalid("labels", "%q is %d bytes, past the %d-byte cap",
				l, len(l), MaxLabelLength)
		}
	}
	return nil
}

// applyStatus moves an item's status and keeps the closure fields honest.
func (s *Store) applyStatus(item *Item, edit Edit, fields map[string]Delta,
	kinds map[ChangeKind]bool, at time.Time) error {
	next := *edit.Status
	if !next.Valid() {
		return invalid("status", "%q is not one of %v", next, Statuses())
	}
	if next == item.Status {
		return nil
	}
	if next.Terminal() {
		reason := CloseCompleted
		if edit.CloseReason != nil {
			reason = *edit.CloseReason
		}
		if !reason.Valid() {
			return invalid("close_reason", "%q is not one of %v", reason, CloseReasons())
		}
		if reason == CloseDuplicate {
			// A DUPLICATE MUST NAME THE SURVIVOR. Without it the close is
			// a dead end: a person following the link finds an item that
			// says it is a duplicate of nothing.
			target := ""
			if edit.DuplicateOf != nil {
				target = strings.TrimSpace(*edit.DuplicateOf)
			}
			if target == "" {
				return invalid("duplicate_of",
					"closing as a duplicate must name the item that survives, "+
						"or the close leaves a dead end")
			}
			item.DuplicateOf = target
		}
		item.CloseReason = reason
		closed := at
		item.ClosedAt = &closed
	} else {
		// REOPENING CLEARS THE CLOSURE. A reopened item still claiming it
		// was completed is one a report counts as delivered.
		item.CloseReason = ""
		item.DuplicateOf = ""
		item.ClosedAt = nil
	}
	fields["status"] = Delta{From: string(item.Status), To: string(next)}
	kinds[ChangeStatus] = true
	item.Status = next
	return nil
}

// applyAssignee hands an item over, and is where the hand-off budget bites.
func (s *Store) applyAssignee(actor Actor, item *Item, next string,
	fields map[string]Delta, kinds map[ChangeKind]bool) error {
	next = strings.TrimSpace(next)
	if next == item.Assignee {
		return nil
	}
	if actor.IsAgent() && item.Reassignments+1 > ReassignmentBudget {
		// REFUSED NAMING THE FIELD, and the item is left where it is for a
		// person. Bouncing it once more would cost two turns and land it
		// no closer to being done.
		return fmt.Errorf(
			"%w: %d agent hand-offs since a person last touched %s (the budget is %d). "+
				"It is left with %q for somebody to look at; any human write "+
				"resets the count",
			ErrReassignmentBudget, item.Reassignments, item.Key, ReassignmentBudget,
			item.Assignee)
	}
	if actor.IsAgent() {
		item.Reassignments++
	}
	fields["assignee"] = Delta{From: item.Assignee, To: next}
	kinds[ChangeAssignee] = true
	item.Assignee = next
	addWatcher(item, next)
	return nil
}

// applyWatch adds or removes the actor, setting the mute on a removal.
func (s *Store) applyWatch(actor Actor, item *Item, watch bool, kinds map[ChangeKind]bool) {
	handle := actor.Handle
	if handle == "" {
		return
	}
	if watch {
		item.Muted = slices.DeleteFunc(item.Muted, func(h string) bool { return h == handle })
		if !slices.Contains(item.Watchers, handle) {
			item.Watchers = append(item.Watchers, handle)
			kinds[ChangeWatchers] = true
		}
		return
	}
	before := len(item.Watchers)
	item.Watchers = slices.DeleteFunc(item.Watchers, func(h string) bool { return h == handle })
	if !slices.Contains(item.Muted, handle) {
		item.Muted = append(item.Muted, handle)
	}
	if before != len(item.Watchers) {
		kinds[ChangeWatchers] = true
	}
}

// applyLinks changes the link ends this item authors.
func (s *Store) applyLinks(item *Item, edit Edit, fields map[string]Delta,
	kinds map[ChangeKind]bool) error {
	if len(edit.AddLinks) == 0 && len(edit.RemoveLinks) == 0 {
		return nil
	}
	var added, removed []string
	for _, link := range edit.AddLinks {
		if !link.Kind.Valid() {
			return invalid("links.kind", "%q is not one of %v", link.Kind, LinkKinds())
		}
		if strings.TrimSpace(link.To) == "" {
			return invalid("links.to", "a link needs an item to point at")
		}
		if link.To == item.ID {
			return invalid("links.to", "an item cannot link to itself")
		}
		if slices.Contains(item.Links, link) {
			continue
		}
		if len(item.Links) >= MaxLinks {
			return invalid("links", "%s already has %d links, the cap", item.Key, MaxLinks)
		}
		item.Links = append(item.Links, link)
		added = append(added, string(link.Kind)+" "+link.To)
	}
	for _, link := range edit.RemoveLinks {
		before := len(item.Links)
		item.Links = slices.DeleteFunc(item.Links, func(l Link) bool { return l == link })
		if before != len(item.Links) {
			removed = append(removed, string(link.Kind)+" "+link.To)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	fields["links"] = Delta{From: strings.Join(removed, ", "), To: strings.Join(added, ", ")}
	kinds[ChangeLinks] = true
	return nil
}

func (s *Store) validateNew(in NewItem) error {
	if !ValidProject(in.Project) {
		return invalid("project", "%q is not a project key", in.Project)
	}
	if !in.Type.Valid() {
		return invalid("type", "%q is not one of %v", in.Type, Types())
	}
	if err := s.checkTitle(strings.TrimSpace(in.Title)); err != nil {
		return err
	}
	if len(in.Body) > MaxBody {
		return invalid("body", "%d bytes, past the %d-byte cap", len(in.Body), MaxBody)
	}
	if in.Status != "" && !in.Status.Valid() {
		return invalid("status", "%q is not one of %v", in.Status, Statuses())
	}
	if in.Priority != "" && !in.Priority.Valid() {
		return invalid("priority", "%q is not one of %v", in.Priority, Priorities())
	}
	return s.checkLabels(cleanList(in.Labels))
}

// ValidProject reports whether s is a well-formed project key.
//
// THE SAME GRAMMAR THE CONFIG ENFORCES on a unit's `project`, restated here
// because this edge is reached by input the loader never saw — a REST call, a
// tool argument — and a key minted from it becomes part of every item key
// under it, permanently.
func ValidProject(s string) bool {
	if len(s) < 2 || len(s) > 10 {
		return false
	}
	if s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
