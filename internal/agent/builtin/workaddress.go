package builtin

import "github.com/crewlet/crewlet/internal/worklet"

// HOW A TOOL NAMES A TASK: two tasks can hold one key (a counter restored
// beside newer work), a key resolves to the task that claimed it first, and
// the others carry `key_collision` and are reached by id. So every name a
// tool hands back is built by [worklet.ItemAddress], in one of three shapes
// (TestNoToolNamesATaskByAKeyOutsideTheAddressRule holds the package to it):
//
//   - a RECEIPT: `item` (the address), `key`, and `key_collision` where the
//     two differ ([receiptItem], [ReceiptOf]);
//   - an INSTRUCTION to call a tool: the bare address, which the caller
//     copies into an argument ([worklet.TaskDetail.Address]);
//   - PROSE: the address with the key beside it and why, because an id alone
//     says nothing about which task it is ([worklet.TaskDetail.Named]).

// The sentences every argument that takes a task reference ends with: in the
// schema, because that is what a model reads before composing a call, and a
// model never told what `key_collision` means types the key it read
// (TestEveryArgumentOfferingAKeySaysWhenToUseTheID).
const (
	// itemRef ends the description of an argument naming ONE task.
	itemRef = "Its key (ENG-42) or its id — and ALWAYS its id when the row " +
		"or answer you read it from carries `key_collision`: that key opens " +
		"a different task, the one that claimed it first."

	// itemRefs ends the description of an argument naming SEVERAL.
	itemRefs = "Each is a key or an id — and the id for any item whose row " +
		"carries `key_collision`: that key opens a different task, the one " +
		"that claimed it first."
)

// receiptItem names on a receipt the task it is about: `item`, the address
// ([worklet.ItemAddress]), `key` beside it, and `key_collision` where the key
// is not the address, so an id standing in for a key does not read as a
// fault. One function for every receipt, and `item` on each
// because it is the argument's own name: handing it back as `item` reaches
// the right task whichever one it was.
func receiptItem(answer map[string]any, id, key string, collision bool) map[string]any {
	answer["item"] = worklet.ItemAddress(id, key, collision)
	if key != "" {
		answer["key"] = key
	}
	if collision && key != "" {
		answer["key_collision"] = true
	}
	return answer
}

// ReceiptOf is [receiptItem] for a task the caller read before writing.
// Exported for the human write surface's rank move and comment edit, which
// write through the tracker themselves, so their receipts name the item by
// the same rule, in the same three fields, as every tool's.
func ReceiptOf(answer map[string]any, before worklet.TaskDetail) map[string]any {
	return receiptItem(answer, before.Task.ID, before.Task.Key, before.KeyCollision)
}
