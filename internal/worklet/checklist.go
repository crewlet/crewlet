package worklet

import (
	"slices"
	"strings"
)

// The checklist GESTURES: one change to a task's checklists, stated as what
// somebody did rather than as the collection they want to end with.
//
// [TaskPatch.Checklists] is carried whole, formed from a read in another
// transaction, so of two people editing one checklist (the ordinary case) the
// second whole collection silently discards the first's change. As with
// [WatchIntent] and [RelationIntent], the caller states the gesture and
// [settleChecklist] resolves it inside the decide snapshot. NEVER ON THE WIRE:
// the record carries the resolved collection, so a replay writes rows and an
// older node applies it as any checklist write: no record version moves. A gesture that adds names the id it adds,
// minted by the caller from the operation, never here: the decide reruns on
// every round and retry, and an id minted inside it would be a second list.

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
)

// ChecklistOps are the gestures a caller may send — all of them.
var ChecklistOps = []ChecklistOp{
	ChecklistAddList, ChecklistRemoveList, ChecklistRenameList,
	ChecklistAddItem, ChecklistRemoveItem, ChecklistRenameItem,
	ChecklistSetDone, ChecklistAssignItem,
}

// Valid reports whether an op off the wire is one this build resolves.
func (o ChecklistOp) Valid() bool {
	return slices.Contains(ChecklistOps, o)
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

	// Items are `add_list`'s first items, each with its caller-minted id:
	// a gesture per line would be a commit per line.
	Items []ChecklistItem

	// Done is `set_done`'s value.
	Done bool

	// Assignee is `assign_item`'s handle, "" giving the item to nobody, and
	// the optional owner of the item `add_item` adds.
	Assignee string
}

// grows reports whether the gesture can make the collection larger, the only
// case the caps are checked: a shrink or an in-place edit is never refused,
// or a task past a cap could never be brought back under it ([settleWatch]'s
// rule for an unwatch).
func (c ChecklistIntent) grows() bool {
	return c.Op == ChecklistAddList || c.Op == ChecklistAddItem
}

// ApplyChecklist is the checklists a gesture leaves, or the refusal.
//
// PURE OVER VALUES, so the writer's decide (the authority) and a caller
// building the wake's snapshot cannot disagree about what one gesture does.
// The result shares no slice with the input.
func ApplyChecklist(lists []Checklist, c ChecklistIntent) ([]Checklist, error) {
	if !c.Op.Valid() {
		return nil, invalid("%q is not a checklist gesture — the "+
			"gestures are %s", c.Op, checklistOpList())
	}
	out := cloneChecklists(lists)
	listAt := func() (int, error) {
		if c.List == "" {
			return 0, invalid("a %s gesture names no list", c.Op)
		}
		for i := range out {
			if out[i].ID == c.List {
				return i, nil
			}
		}
		return 0, invalid("this task has no checklist %q", c.List)
	}
	itemAt := func() (int, int, error) {
		if c.Item == "" {
			return 0, 0, invalid("a %s gesture names no item", c.Op)
		}
		for l := range out {
			for i := range out[l].Items {
				if out[l].Items[i].ID == c.Item {
					return l, i, nil
				}
			}
		}
		return 0, 0, invalid("this task has no checklist item %q", c.Item)
	}

	switch c.Op {
	case ChecklistAddList:
		switch {
		case c.List == "":
			return nil, invalid("an add_list gesture mints no list id")
		case slices.ContainsFunc(out, func(l Checklist) bool { return l.ID == c.List }):
			// The id is minted from the gesture's operation (the tools'
			// checklistIntent), so a held one is this gesture again: said
			// without the id, which nobody reading it chose.
			return nil, collision("this task already has that checklist. Look at " +
				"the task rather than adding it again")
		}
		if err := checkChecklistName(c.Name, MaxChecklistName, "checklist name"); err != nil {
			return nil, err
		}
		if len(c.Items) > MaxChecklistItems {
			// Past the cap on its own is the request's fault; a list it
			// only pushes past a cap is the task's state, below.
			return nil, invalid("this add_list gesture carries %d lines and a "+
				"list holds at most %d — split it, or file some of them as "+
				"subtasks with %s", len(c.Items), MaxChecklistItems, CreateWorkItemTool)
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
		// AND EVERYTHING NESTED UNDER IT: a child left behind names a
		// parent that no longer exists, a row a renderer can place nowhere.
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
		return ChecklistItem{}, invalid("a checklist item is added with no id")
	}
	taken := slices.ContainsFunc(adding, func(i ChecklistItem) bool { return i.ID == item.ID })
	for _, list := range lists {
		taken = taken || slices.ContainsFunc(list.Items, func(i ChecklistItem) bool {
			return i.ID == item.ID
		})
	}
	if taken {
		// THE SAME GESTURE AGAIN, as a list's id is: said without the id.
		return ChecklistItem{}, collision("this task already has that checklist " +
			"line. Look at the task rather than adding it again")
	}
	if err := checkChecklistName(item.Name, MaxChecklistItemName, "checklist item name"); err != nil {
		return ChecklistItem{}, err
	}
	// ONLY WHAT A NEW LINE CAN CARRY: a name and an owner. Done and nesting
	// are later gestures about a line that exists.
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
		return invalid("a %s is empty — a line nobody can read is "+
			"not something anybody can tick", field)
	case len(name) > limit:
		return invalid("the %s is %d bytes and the maximum is %d — it "+
			"is refused rather than cut; a line longer than that is a task "+
			"of its own", field, len(name), limit)
	}
	return nil
}

// checkChecklistCaps holds a task's checklists to [MaxChecklists],
// [MaxChecklistItems] per list and [MaxChecklistItemsTotal] across them: the
// caps [MaxCommitBytes] sizes one record from.
//
// Every refusal is the task's state ([ErrState]): a gesture adds one list or
// line, or a list [ApplyChecklist] already held to the per-list cap, so what
// crosses a cap is what the task carries, and the gesture lands once something
// is removed. The total is said as what the gesture would leave: one new list
// can cross it from well below, where "already holds 256" would be false.
func checkChecklistCaps(lists []Checklist) error {
	if len(lists) > MaxChecklists {
		return refusedByState("this task already has %d checklists, the most "+
			"a task can. Remove one, or split the work into subtasks",
			MaxChecklists)
	}
	total := 0
	for _, list := range lists {
		if len(list.Items) > MaxChecklistItems {
			return refusedByState("the checklist %q already holds %d lines, the "+
				"most a list can. Remove one, or file some as subtasks", list.Name,
				MaxChecklistItems)
		}
		total += len(list.Items)
	}
	if total > MaxChecklistItemsTotal {
		return refusedByState("that would leave this task's checklists with %d "+
			"lines, and %d is the most a task can hold. Remove some, or file them "+
			"as subtasks", total, MaxChecklistItemsTotal)
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
// INSIDE THE DECIDE SNAPSHOT, as [settleWatch] is, and returning a COPY:
// Decide reruns on a retry, and a resolution folded into the captured patch
// would apply the gesture to its own result.
func settleChecklist(current Task, patch TaskPatch) (TaskPatch, error) {
	if patch.Checklist == nil {
		return patch, nil
	}
	if patch.Checklists != nil {
		// BOTH AT ONCE IS A PROGRAMMING ERROR, refused rather than
		// resolved in some order, as a watch gesture beside a watcher set is.
		return patch, invalid("this patch carries both a %s "+
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
