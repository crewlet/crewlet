package builtin

import "github.com/crewlet/crewlet/internal/tracker"

// HOW A TOOL NAMES A TASK TO THE CALLER WHO WILL ACT ON THE NAME.
//
// A task has two names, and only one of them is always its own. Its KEY is
// what a person reads and pastes and what a model types back — but two tasks
// can hold one key (a counter restored beside newer work mints numbers they
// already hold), and every read resolves the key to the task that claimed it
// first. The other holders are flagged `key_collision` and are reached by
// their ID alone. So everything a tool hands back to name a task is built by
// one rule, [tracker.ItemAddress] — the key, unless the flag is set, and then
// the id — and the tools here used to echo the key whatever the flag said: a
// seat that commented on the duplicate was answered `item: ENG-7`, told to
// "Read ENG-7's thread", and its next call landed on the claimant.
//
// Three shapes, one per kind of text:
//
//   - A RECEIPT names the task in `item` — the reference to hand back, the
//     value every `item` argument takes — beside `key`, what the task is
//     called, and `key_collision` where the two differ, so a reader can tell
//     an id standing in for a key from a fault ([receiptItem]).
//   - An INSTRUCTION to call a tool on the task names its address, bare,
//     because the caller copies it into an argument ([tracker.TaskDetail.Address]).
//   - PROSE about the task names its address with the key beside it and why
//     ([tracker.TaskDetail.Named]), because an id alone says nothing about
//     which task it is.
//
// A gate holds the package to it: outside the functions listed in its own
// allowance, nothing here reads a task's key off a detail
// (TestNoToolNamesATaskByAKeyOutsideTheAddressRule).

// The sentences every argument that takes a task reference ends with.
//
// IN THE SCHEMA, because the schema is what a model reads before it composes a
// call: the flag on a row is only half of the rule, and a model that has never
// been told what it means types the key it read beside it. A gate holds every
// argument offering a key to carrying one of these
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

// receiptItem puts on a receipt how it names the task it is about: `item`, the
// address ([tracker.ItemAddress]), `key` beside it, and `key_collision` where
// the key is not the address.
//
// ONE FUNCTION for every receipt, so a create, a move and an edit cannot name
// the task three ways — and `item` on every one of them, because it is the
// argument's own name: a caller that hands back the field called `item` as the
// argument called `item` is right whichever task it was.
func receiptItem(answer map[string]any, id, key string, collision bool) map[string]any {
	answer["item"] = tracker.ItemAddress(id, key, collision)
	if key != "" {
		answer["key"] = key
	}
	if collision && key != "" {
		answer["key_collision"] = true
	}
	return answer
}

// receiptOf is [receiptItem] for a task the tool read before writing.
func receiptOf(answer map[string]any, before tracker.TaskDetail) map[string]any {
	return receiptItem(answer, before.Task.ID, before.Task.Key, before.KeyCollision)
}
