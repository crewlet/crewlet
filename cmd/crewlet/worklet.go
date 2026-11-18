package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
)

// `crewlet worklet`: the gestures on Worklet's work items that belong to a
// PERSON rather than to a seat.
//
// # Why there is exactly one of them
//
// Seats act through their own tools; a person does the rest through the HTTP
// write surface. The one gesture that belongs on a SHELL is the purge, which
// no seat may perform: it destroys a task and its rows on every data node, the
// write path restricts it to a person or operator token, and an erasure
// request is run from a runbook. `remove` only hides a task and `delete` only
// stops later records; neither takes a pasted credential out of a database.

// runWorklet dispatches `crewlet worklet`.
func runWorklet(environ config.Source, args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "purge":
		return workletPurge(environ, rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, workletPurgeUsage)
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown worklet command %q", sub)
	}
}

// workletPurgeUsage is the one usage line, printed wherever the command is
// refused for want of an argument.
const workletPurgeUsage = "usage: crewlet worklet purge <item> -reason TEXT " +
	"-confirm <item-key> [-op-id ID] [<config.yaml>] [-api URL]"

// workletPurge is `crewlet worklet purge`.
//
// The confirmation is the item's KEY, which the node checks: the item may be
// named by an unreadable uuid, but the key is what a person sees on the board,
// so asking for it makes them look. A mismatch destroys nothing.
//
// There is no project flag: the record is filed under the item's own project,
// which only the stored row says, so a caller cannot set it wrong.
//
// The reason is required: the rows are destroyed, and the deletion marker's
// reason is the whole account of what used to be at that key.
func workletPurge(environ config.Source, args []string, stdout, stderr io.Writer) error {
	item, rest := splitSubject(args)
	var reason, confirm, opID *string
	client, err := nodeClientFor(environ, rest, "worklet purge", stderr, func(fs *flag.FlagSet) {
		reason = fs.String("reason", "", "why; recorded on the deletion marker")
		confirm = fs.String("confirm", "",
			"the item's KEY: this destroys the item and every row it produced")
		opID = opIDFlag(fs,
			"retry a purge whose outcome is unknown with the id it printed, so "+
				"the retry cannot append a second purge; empty starts a new one")
	})
	if err != nil {
		return err
	}
	switch {
	case item == "" || strings.TrimSpace(*confirm) == "":
		fmt.Fprintln(stderr, workletPurgeUsage)
		return fmt.Errorf("a purge destroys the item and every row it produced " +
			"on every data node, and nothing undoes it. Name the item's KEY in " +
			"-confirm to run it")
	case strings.TrimSpace(*reason) == "":
		return fmt.Errorf("state why in -reason: the rows are destroyed and " +
			"the marker's reason is the only account of them that survives")
	}

	// THE OPERATION IS MINTED HERE, BEFORE THE REQUEST, when the operator
	// brought none — see [operationFor]. A purge whose request timed out or
	// dropped may well have landed, and the key it was sent under is the
	// only handle on it.
	operation, err := operationFor("purge", *opID)
	if err != nil {
		return err
	}
	write := keyedWrite{gesture: "purge", operation: operation,
		check: "whether " + item + " is gone"}

	var answer struct {
		Task     string `json:"task"`
		Key      string `json:"key"`
		Project  string `json:"project"`
		Outcome  string `json:"outcome"`
		Position string `json:"position"`
		OpID     string `json:"op_id"`
	}
	path := fmt.Sprintf("/worklet/items/%s/purge?confirm=%s&reason=%s",
		url.PathEscape(item), url.QueryEscape(strings.TrimSpace(*confirm)),
		url.QueryEscape(strings.TrimSpace(*reason)))
	// PATIENTLY, for the reason [nodeClient.patiently] names: a purge waits on
	// the write path's own waits, not on the network. And EVERY RETRY IS
	// UNDER THE SAME ID, as [keyedWrite.settle] tells the operator: a fresh
	// one would append a second purge of an item an earlier attempt may
	// already have destroyed.
	if err := write.settle(stderr, client.patiently(logWriteTimeout).postKeyed(
		context.Background(), path, operation, nil, &answer)); err != nil {
		return err
	}
	if err := write.confirmed("purge "+item, answer.Outcome); err != nil {
		return err
	}

	reportDestruction(stdout, fmt.Sprintf("purge %s in %s (%s)", answer.Key,
		answer.Project, answer.Task), answer.Outcome, answer.Position,
		firstNonEmpty(answer.OpID, operation))
	return nil
}

// reportDestruction prints what a destruction answered — `worklet purge`, `chatlet
// erase` and `chatlet prune` alike: `what` names the gesture, and `operation` the
// operation it was made under. Only an applied or a pending one reaches it:
// [keyedWrite.settle] and [keyedWrite.confirmed] fail every other.
func reportDestruction(stdout io.Writer, what, outcome, position, operation string) {
	at := ""
	if position != "" {
		at = " at " + position
	}
	fmt.Fprintf(stdout, "%s: %s%s (operation %s)\n", what, outcome, at, operation)
	if outcome == string(statelog.OutcomePending) {
		// THE THREE-VALUED OUTCOME, said plainly and NOT as a failure.
		// The record is on the log and every data node applies it as it
		// reaches it; running the gesture again would append a second
		// destruction of rows the first one already destroyed.
		fmt.Fprintln(stdout, "  Saved, but not on this node yet. It's deleted on "+
			"each node as that node catches up. Don't run this again.")
	}
	// WHAT A DESTRUCTION DOES NOT REACH, every time rather than in the docs
	// alone. An offline or evicted disk keeps its copy until replay,
	// adoption, replacement or destruction, and there is no duration to
	// state — so an operator acting on an erasure request has to be told
	// here, where they are running the gesture.
	fmt.Fprintln(stdout, "  A node that's offline or evicted keeps its copy "+
		"until it catches up, is replaced or is destroyed. "+
		"`crewlet retention status` lists those nodes.")
}

// logWriteTimeout is how long a command waits for the node's answer to a write
// that is ONE RECORD ON A STATE LOG: a purge, a chat post, an erase, a prune, a
// room's horizon.
//
// SIX RESOLVE BUDGETS. Such a write can wait on the answering node reaching the
// caller's previous write, reaching a peer's record on the same object, and
// resolving its own append, each bounded at [statelog.DefaultResolveBudget].
// Three is fifteen seconds, past the ten every other verb waits, which
// abandoned a working purge with no operation id to retry; twice that leaves
// room for arbitration rounds. A coordination write (a seat's pause) is a
// compare-and-set, not a record, and keeps the ordinary ceiling.
const logWriteTimeout = 6 * statelog.DefaultResolveBudget
