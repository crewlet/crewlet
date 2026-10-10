package builtin

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/tracker"
)

// The two arguments update_work_item grew for the item's own page: a REASON a
// hand-off or a send-back carries, and a CHECKLIST gesture.

// MaxChangeReason is how long the one line a hand-off or a send-back carries
// may be, in characters.
//
// FIVE HUNDRED because it is a sentence to the person receiving the work —
// "you own the rollout now, ops has the rest", "the timeout path has no
// test" — and anything longer is the conversation, which has a comment thread
// of its own. It rides the change's excerpt ([tracker.Notify.Excerpt]), which
// is what the history row stores and the wake renders, so it is also held to
// that excerpt's byte ceiling ([tracker.MaxExcerpt]) and REFUSED past either
// rather than cut: a reason silently truncated is a reason somebody reads as
// finished.
const MaxChangeReason = 500

// changeReason is the `reason` argument checked against the change the call
// makes, or the refusal.
//
// TWO CHANGES CARRY ONE, and both hand the work to somebody with something to
// say about it: an ASSIGNMENT, which says why the work is the new assignee's
// now, and a SEND-BACK — a status move back to an open status from one
// further along ([tracker.SendsBack]) — which says what to do again, and is
// how a person reviewing work returns it. On any other edit a reason would
// land on a history row as an excerpt nothing announced and nobody asked for:
// a move forward is reported by the move, and the explanation of a close
// belongs in a comment, which the tool's own description already asks for.
func changeReason(args map[string]any, before tracker.Task, patch tracker.TaskPatch) (string, string) {
	raw, held := args["reason"]
	if !held {
		return "", ""
	}
	reason := strings.TrimSpace(argString(map[string]any{"v": raw}, "v"))
	if reason == "" {
		return "", ""
	}
	_, assigning := args["assignee"]
	sendingBack := patch.Status != nil && tracker.SendsBack(before.Status, *patch.Status)
	if !assigning && !sendingBack {
		return "", fmt.Sprintf("`reason` explains a hand-off (an `assignee`) or "+
			"work SENT BACK (a `status` back to an open status from one further "+
			"along — %s is %s), and this call makes neither. To explain any other "+
			"change, comment on the item with comment_on_work_item.",
			before.Key, before.Status)
	}
	if n := utf8.RuneCountInString(reason); n > MaxChangeReason {
		return "", fmt.Sprintf("`reason` is %d characters and a hand-off or a "+
			"send-back carries at most %d — say it in a sentence, and put the "+
			"rest in a comment.", n, MaxChangeReason)
	}
	if len(reason) > tracker.MaxExcerpt {
		return "", fmt.Sprintf("`reason` is %d bytes and the line a hand-off "+
			"or a send-back carries holds at most %d — shorten it, and put the "+
			"rest in a comment.", len(reason), tracker.MaxExcerpt)
	}
	return reason, ""
}

// checklistSchema is the `checklist` argument's shape.
func checklistSchema() map[string]any {
	ops := make([]any, 0, len(tracker.ChecklistOps))
	for _, op := range tracker.ChecklistOps {
		ops = append(ops, string(op))
	}
	return map[string]any{
		"type": "object",
		"description": "ONE change to the item's checklists. add_list {name, " +
			"items?}; remove_list {list}; rename_list {list, name}; add_item " +
			"{list, name, assignee?}; remove_item {item}; rename_item {item, " +
			"name}; set_done {item, done}; assign_item {item, assignee} " +
			"(\"\" for nobody). Lists and items are named by the ids " +
			"get_work_item shows; the change is applied to the checklists as " +
			"they are when it lands, so somebody else's tick in the meantime " +
			"is kept.",
		"properties": map[string]any{
			"op":       map[string]any{"type": "string", "enum": ops},
			"list":     map[string]any{"type": "string", "description": "The checklist's id."},
			"item":     map[string]any{"type": "string", "description": "The checklist item's id."},
			"name":     map[string]any{"type": "string"},
			"items":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "add_list only: the new list's first items, in order."},
			"done":     map[string]any{"type": "boolean"},
			"assignee": map[string]any{"type": "string", "description": "A seat's handle."},
		},
		"required": []any{"op"},
	}
}

// checklistIntent reads the `checklist` argument into a gesture, minting the
// ids an addition names, or answers the refusal.
//
// THE IDS ARE DERIVED FROM THE OPERATION, the way a comment's is ([commentID]):
// a retried request carries the same operation, so it names the same list
// rather than adding a second one, and two different gestures in one turn are
// two operations with ids of their own.
func (d WorkDeps) checklistIntent(args map[string]any, opID string) (*tracker.ChecklistIntent, string) {
	raw, held := args["checklist"]
	if !held || raw == nil {
		return nil, ""
	}
	spec, ok := raw.(map[string]any)
	if !ok {
		return nil, "`checklist` is one change, as an object: " +
			"{\"op\": \"set_done\", \"item\": \"<id>\", \"done\": true}."
	}
	op := tracker.ChecklistOp(strings.TrimSpace(argString(spec, "op")))
	if !op.Valid() {
		return nil, fmt.Sprintf("%q is not a checklist change. The changes are: %s.",
			clip(string(op)), checklistOps())
	}
	intent := &tracker.ChecklistIntent{
		Op:   op,
		List: strings.TrimSpace(argString(spec, "list")),
		Item: strings.TrimSpace(argString(spec, "item")),
		Name: strings.TrimSpace(argString(spec, "name")),
	}
	switch op {
	case tracker.ChecklistAddList:
		intent.List = mintedID(opID, "list")
		for i, name := range argStrings(spec, "items") {
			intent.Items = append(intent.Items, tracker.ChecklistItem{
				ID: mintedID(opID, "item-"+strconv.Itoa(i)), Name: name,
			})
		}
	case tracker.ChecklistAddItem:
		intent.Item = mintedID(opID, "item")
	case tracker.ChecklistSetDone:
		done, held := spec["done"].(bool)
		if !held {
			return nil, "set_done needs `done`: true to tick the item, false to untick it."
		}
		intent.Done = done
	}
	if _, named := spec["assignee"]; named &&
		(op == tracker.ChecklistAddItem || op == tracker.ChecklistAssignItem) {
		handle, refusal := d.resolveHandle(UpdateWorkItemTool,
			"`checklist.assignee`", argString(spec, "assignee"))
		if refusal != "" {
			return nil, refusal
		}
		intent.Assignee = handle
	} else if op == tracker.ChecklistAssignItem {
		return nil, "assign_item needs `assignee` — a seat's handle, or \"\" to give the item to nobody."
	}
	return intent, ""
}

// checklistNamespace is the uuid namespace a checklist gesture's new ids are
// derived under. Fixed for the life of the deployment: a changed namespace
// would mint a retried request a second list.
var checklistNamespace = uuid.MustParse("3b1e8f4a-7c2d-5e6f-9a0b-1c2d3e4f5a6b")

// mintedID is one id a checklist gesture adds, derived from its operation.
func mintedID(opID, what string) string {
	return uuid.NewSHA1(checklistNamespace, []byte(opID+"\x00"+what)).String()
}

// checklistOps is the gestures as a refusal names them.
func checklistOps() string {
	names := make([]string, 0, len(tracker.ChecklistOps))
	for _, op := range tracker.ChecklistOps {
		names = append(names, string(op))
	}
	return strings.Join(names, ", ")
}
