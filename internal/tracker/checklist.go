package tracker

import (
	"slices"
	"strings"
)

// The checklist GESTURES: one change to a task's checklists, stated as what
// somebody did rather than as the collection they want to end with.
//
// # Why a gesture and never the collection
//
// [TaskPatch.Checklists] is carried WHOLE — every list, every item, every tick
// — and a caller can only form it from a read it made in another transaction.
// Two people working one checklist are the ordinary case rather than a race
// somebody has to be unlucky to hit: one ticks "migrate the users" while the
// other adds "announce the cut-over", and whichever whole collection lands
// second discards the other's change without a word. It is [WatchIntent]'s
// reason and [RelationIntent]'s, and the answer is theirs: the caller states
// the gesture, and [settleChecklist] resolves it against the task as the
// decide snapshot holds it — the one place this engine guarantees a single
// consistent read of the set the record will carry.
//
// NEVER ON THE WIRE. The record an applier sees carries the resolved
// collection, so a replay writes rows rather than re-deriving a set from
// whatever it had applied by then, and a node that has never heard of a
// gesture applies it exactly as it applies every other checklist write —
// which is why no record version moves for it.
//
// # Why the ids are the caller's
//
// A gesture that ADDS something names the id it adds. It is minted by the
// caller from the operation — never here — because the decide runs again on
// every round and on a retried request, and a list minted a fresh id inside it
// would be a second list the second time.

// ChecklistOp is what one checklist gesture does.
type ChecklistOp string

// The gestures. Each constant IS the wire value a caller sends.
const (
	// ChecklistAddList adds a named list, optionally with its first items.
	ChecklistAddList ChecklistOp = "add_list"
	// ChecklistRemoveList removes a list and every item in it.
	ChecklistRemoveList ChecklistOp = "remove_list"
	// ChecklistRenameList renames a list.
	ChecklistRenameList ChecklistOp = "rename_list"
	// ChecklistAddItem adds one item at the end of a list.
	ChecklistAddItem ChecklistOp = "add_item"
	// ChecklistRemoveItem removes one item, and the items nested under it.
	ChecklistRemoveItem ChecklistOp = "remove_item"
	// ChecklistRenameItem renames one item.
	ChecklistRenameItem ChecklistOp = "rename_item"
	// ChecklistSetDone ticks or unticks one item.
	ChecklistSetDone ChecklistOp = "set_done"
	// ChecklistAssignItem gives one item to somebody, or to nobody.
	ChecklistAssignItem ChecklistOp = "assign_item"
	// ChecklistPromote marks one item as having become a subtask. It is
	// the parent half of [Writer.PromoteItem] and no tool offers it: the
	// subtask it names has to exist, and only the promotion sequence
	// creates one.
	ChecklistPromote ChecklistOp = "promote"
)

// ChecklistOps are the gestures a caller may send — every one but the
// promotion, which is a sequence's own step.
var ChecklistOps = []ChecklistOp{
	ChecklistAddList, ChecklistRemoveList, ChecklistRenameList,
	ChecklistAddItem, ChecklistRemoveItem, ChecklistRenameItem,
	ChecklistSetDone, ChecklistAssignItem,
}

// Valid reports whether an op off the wire is one this build resolves.
func (o ChecklistOp) Valid() bool {
	return o == ChecklistPromote || slices.Contains(ChecklistOps, o)
}

// ChecklistIntent is one checklist gesture, resolved inside the writer's own
// decide snapshot by [settleChecklist].
type ChecklistIntent struct {
	Op ChecklistOp

	// List is the list a gesture acts on — the one `add_item` appends to,
	// the one renamed or removed — or, on `add_list`, the id it mints.
	List string

	// Item is the item a gesture acts on, or the id `add_item` mints.
	// Item ids are unique across the WHOLE task rather than within a
	// list, which is what lets every item gesture name the item alone.
	Item string

	// Name is the new list's or item's name, on the gestures that set one.
	Name string

	// Items are `add_list`'s first items, each with the id its caller
	// minted — a list is usually written down whole, and a gesture per
	// line would be a commit per line.
	Items []ChecklistItem

	// Done is `set_done`'s value.
	Done bool

	// Assignee is `assign_item`'s handle, "" giving the item to nobody, and
	// the optional owner of the item `add_item` adds.
	Assignee string

	// PromotedTo is `promote`'s subtask id.
	PromotedTo string
}

// grows reports whether the gesture can make the collection larger, which is
// the only case the caps are checked against: a gesture that only shrinks or
// edits in place is never refused for a size the collection already had, or a
// task that somehow grew past a cap would be one nobody could bring back
// under it — [settleWatch]'s rule for an unwatch.
func (c ChecklistIntent) grows() bool {
	return c.Op == ChecklistAddList || c.Op == ChecklistAddItem
}

// ApplyChecklist is the checklists a gesture leaves, or the refusal.
//
// PURE OVER VALUES, so the writer's decide and a caller building the wake's
// snapshot run the same function — the snapshot is only the tool's best answer
// about who the change reaches, and the decide is the authority, but they
// cannot disagree about what one gesture does to one collection.
//
// The input is never written through: the result shares no slice with it.
func ApplyChecklist(lists []Checklist, c ChecklistIntent) ([]Checklist, error) {
	if !c.Op.Valid() {
		return nil, invalid("tracker: %q is not a checklist gesture — the "+
			"gestures are %s", c.Op, checklistOpList())
	}
	out := cloneChecklists(lists)
	listAt := func() (int, error) {
		if c.List == "" {
			return 0, invalid("tracker: a %s gesture names no list", c.Op)
		}
		for i := range out {
			if out[i].ID == c.List {
				return i, nil
			}
		}
		return 0, invalid("tracker: this task has no checklist %q", c.List)
	}
	itemAt := func() (int, int, error) {
		if c.Item == "" {
			return 0, 0, invalid("tracker: a %s gesture names no item", c.Op)
		}
		for l := range out {
			for i := range out[l].Items {
				if out[l].Items[i].ID == c.Item {
					return l, i, nil
				}
			}
		}
		return 0, 0, invalid("tracker: this task has no checklist item %q", c.Item)
	}

	switch c.Op {
	case ChecklistAddList:
		switch {
		case c.List == "":
			return nil, invalid("tracker: an add_list gesture mints no list id")
		case slices.ContainsFunc(out, func(l Checklist) bool { return l.ID == c.List }):
			return nil, invalid("tracker: this task already has a checklist %q", c.List)
		}
		if err := checkChecklistName(c.Name, MaxChecklistName, "checklist name"); err != nil {
			return nil, err
		}
		items := make([]ChecklistItem, 0, len(c.Items))
		for _, item := range c.Items {
			fresh, err := newItem(out, items, item)
			if err != nil {
				return nil, err
			}
			items = append(items, fresh)
		}
		out = append(out, Checklist{ID: c.List, Name: strings.TrimSpace(c.Name), Items: items})

	case ChecklistRemoveList:
		l, err := listAt()
		if err != nil {
			return nil, err
		}
		out = slices.Delete(out, l, l+1)

	case ChecklistRenameList:
		l, err := listAt()
		if err != nil {
			return nil, err
		}
		if err := checkChecklistName(c.Name, MaxChecklistName, "checklist name"); err != nil {
			return nil, err
		}
		out[l].Name = strings.TrimSpace(c.Name)

	case ChecklistAddItem:
		l, err := listAt()
		if err != nil {
			return nil, err
		}
		fresh, err := newItem(out, nil, ChecklistItem{
			ID: c.Item, Name: c.Name, Assignee: c.Assignee,
		})
		if err != nil {
			return nil, err
		}
		out[l].Items = append(out[l].Items, fresh)

	case ChecklistRemoveItem:
		l, i, err := itemAt()
		if err != nil {
			return nil, err
		}
		// AND EVERYTHING NESTED UNDER IT. An item's children name it as
		// their parent, and left behind they would point at a line that no
		// longer exists — rows a renderer can place nowhere.
		gone := map[string]bool{out[l].Items[i].ID: true}
		for grew := true; grew; {
			grew = false
			for _, item := range out[l].Items {
				if item.Parent != nil && gone[*item.Parent] && !gone[item.ID] {
					gone[item.ID], grew = true, true
				}
			}
		}
		out[l].Items = slices.DeleteFunc(out[l].Items, func(item ChecklistItem) bool {
			return gone[item.ID]
		})

	case ChecklistRenameItem:
		l, i, err := itemAt()
		if err != nil {
			return nil, err
		}
		if err := checkChecklistName(c.Name, MaxChecklistItemName, "checklist item name"); err != nil {
			return nil, err
		}
		out[l].Items[i].Name = strings.TrimSpace(c.Name)

	case ChecklistSetDone:
		l, i, err := itemAt()
		if err != nil {
			return nil, err
		}
		out[l].Items[i].Done = c.Done

	case ChecklistAssignItem:
		l, i, err := itemAt()
		if err != nil {
			return nil, err
		}
		out[l].Items[i].Assignee = strings.TrimSpace(c.Assignee)

	case ChecklistPromote:
		l, i, err := itemAt()
		if err != nil {
			return nil, err
		}
		if c.PromotedTo == "" {
			return nil, invalid("tracker: a promotion of item %q names no subtask", c.Item)
		}
		promoted := c.PromotedTo
		out[l].Items[i].PromotedTo = &promoted
	}

	if c.grows() {
		if err := checkChecklistCaps(out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// newItem is an item a gesture adds, checked against every item the task
// already carries and every one this gesture has added before it.
func newItem(lists []Checklist, adding []ChecklistItem, item ChecklistItem) (ChecklistItem, error) {
	if item.ID == "" {
		return ChecklistItem{}, invalid("tracker: a checklist item is added with no id")
	}
	taken := slices.ContainsFunc(adding, func(i ChecklistItem) bool { return i.ID == item.ID })
	for _, list := range lists {
		taken = taken || slices.ContainsFunc(list.Items, func(i ChecklistItem) bool {
			return i.ID == item.ID
		})
	}
	if taken {
		return ChecklistItem{}, invalid("tracker: this task already has a checklist item %q", item.ID)
	}
	if err := checkChecklistName(item.Name, MaxChecklistItemName, "checklist item name"); err != nil {
		return ChecklistItem{}, err
	}
	// ONLY WHAT A NEW LINE CAN CARRY: a name and an owner. Done, nesting and
	// a promotion are later gestures about a line that exists.
	return ChecklistItem{
		ID: item.ID, Name: strings.TrimSpace(item.Name),
		Assignee: strings.TrimSpace(item.Assignee),
	}, nil
}

// checkChecklistName refuses an empty or oversized name, naming the field.
func checkChecklistName(name string, limit int, field string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return invalid("tracker: a %s is empty — a line nobody can read is "+
			"not something anybody can tick", field)
	case len(name) > limit:
		return invalid("tracker: the %s is %d bytes and the maximum is %d — it "+
			"is refused rather than cut; a line longer than that is a task "+
			"of its own", field, len(name), limit)
	}
	return nil
}

// checkChecklistCaps holds a task's checklists to [MaxChecklists],
// [MaxChecklistItems] per list and [MaxChecklistItemsTotal] across them.
//
// The caps are what the design maximum of one record is sized from
// ([MaxCommitBytes] counts the whole tree at every cap), so a collection past
// them is a record the arithmetic never allowed for.
func checkChecklistCaps(lists []Checklist) error {
	if len(lists) > MaxChecklists {
		return invalid("tracker: a task carries at most %d checklists and this "+
			"change would leave %d — split the work into subtasks instead",
			MaxChecklists, len(lists))
	}
	total := 0
	for _, list := range lists {
		if len(list.Items) > MaxChecklistItems {
			return invalid("tracker: checklist %q would hold %d items and a "+
				"list holds at most %d — split it, or promote some of them "+
				"into subtasks", list.Name, len(list.Items), MaxChecklistItems)
		}
		total += len(list.Items)
	}
	if total > MaxChecklistItemsTotal {
		return invalid("tracker: a task carries at most %d checklist items "+
			"across its lists and this change would leave %d — promote some "+
			"of them into subtasks", MaxChecklistItemsTotal, total)
	}
	return nil
}

// cloneChecklists is a deep copy, so a gesture never writes through the task
// it was resolved against.
func cloneChecklists(lists []Checklist) []Checklist {
	out := make([]Checklist, len(lists))
	for i, list := range lists {
		out[i] = list
		out[i].Items = slices.Clone(list.Items)
	}
	return out
}

// checklistOpList is the gestures as a refusal names them.
func checklistOpList() string {
	names := make([]string, 0, len(ChecklistOps))
	for _, op := range ChecklistOps {
		names = append(names, string(op))
	}
	return strings.Join(names, ", ")
}

// settleChecklist resolves a checklist gesture into the whole collection the
// record carries.
//
// INSIDE THE DECIDE SNAPSHOT, for [settleWatch]'s reason, and returning a COPY
// for the same one: Decide runs again on a retry, and a resolution folded into
// the captured patch would apply the gesture to its own result.
func settleChecklist(current Task, patch TaskPatch) (TaskPatch, error) {
	if patch.Checklist == nil {
		return patch, nil
	}
	if patch.Checklists != nil {
		// BOTH SPELLINGS AT ONCE IS A PROGRAMMING ERROR, refused rather
		// than resolved in some order, exactly as a watch gesture beside
		// a whole watcher set is.
		return patch, invalid("tracker: this patch carries both a %s "+
			"checklist gesture and a whole checklist collection — a caller "+
			"states one or the other", patch.Checklist.Op)
	}
	lists, err := ApplyChecklist(current.Checklists, *patch.Checklist)
	if err != nil {
		return patch, err
	}
	patch.Checklist = nil
	patch.Checklists = &lists
	return patch, nil
}
