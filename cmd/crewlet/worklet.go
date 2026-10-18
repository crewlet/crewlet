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
// Everything a company does to its tasks is done by seats, through their own
// tools, and a person does the rest through the HTTP write surface the
// dashboard is built on — the same tools again, under a person's own name. The
// one gesture that belongs on a SHELL is the operation no seat may ever
// perform: a purge, which destroys a task and every row it produced on every
// data node, which the write path restricts to a person or an operator token
// precisely because nothing else can be asked to confirm it, and which an
// operator acting on an erasure request runs from a terminal and a runbook
// rather than from a screen. It goes through that same surface's own route.
//
// That operation had no caller anywhere. `worklet.Writer.PurgeTask` existed,
// the applier handled its record, the deletion marker stopped a redelivery
// resurrecting anything — and no CLI verb, no route and no tool reached it. So
// a company could not destroy a task under any circumstances: an erasure
// request had no mechanism, and a credential pasted into a task body stayed in
// the durable rows of every data node for ever. `remove` only hides a task and
// `delete` only stops later records about it; neither takes the body out of a
// single database.

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
// # The confirmation is the item's KEY, and the node checks it
//
// The item may be named by its id, a uuid nobody reads; the KEY is what a
// person sees on the board and in the ticket they were asked to act on, so it
// has to be looked up — which is the whole point of asking. The node compares
// it against the item the first argument resolves to and destroys nothing if
// they differ.
//
// # The project is the node's to know
//
// The record is filed under the item's own project, and the stored row is the
// one place that says which that is, so the command no longer asks for it: a
// flag the caller could set wrong was a flag that could file a purge under a
// project it was not about.
//
// # The reason is required
//
// It is the only thing that survives. The rows are destroyed; what remains is
// the deletion marker, and its reason is the entire account of what used to be
// at that key for whoever reads it a year later.
func workletPurge(environ config.Source, args []string, stdout, stderr io.Writer) error {
	item, rest := splitSubject(args)
	var reason, confirm, opID *string
	client, err := nodeClientFor(environ, rest, "worklet purge", stderr, func(fs *flag.FlagSet) {
		reason = fs.String("reason", "", "why; recorded on the deletion marker")
		confirm = fs.String("confirm", "",
			"the item's KEY — this destroys the item and every row it produced")
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
			"on every data node, and nothing undoes it — name the item's KEY in " +
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
		fmt.Fprintln(stdout, "  The record is durable and this node has not "+
			"applied it yet: the rows go as each data node reaches it. Do not "+
			"run this again.")
	}
	// WHAT A DESTRUCTION DOES NOT REACH, every time rather than in the docs
	// alone. An offline or evicted disk keeps its copy until replay,
	// adoption, replacement or destruction, and there is no duration to
	// state — so an operator acting on an erasure request has to be told
	// here, where they are running the gesture.
	fmt.Fprintln(stdout, "  A node that is offline or evicted keeps its copy "+
		"until it replays, adopts a snapshot, is replaced or is destroyed. "+
		"`crewlet retention status` names which nodes those are.")
}

// logWriteTimeout is how long a command waits for the node's answer to a write
// that is ONE RECORD ON A STATE LOG: a purge, a chat post, an erase, a prune, a
// room's horizon.
//
// SIX RESOLVE BUDGETS. What such a write can legitimately wait on is the write
// path's own: the wait for the answering node to reach the caller's previous
// write, the wait for it to reach a peer's record on the same object, and the
// resolution of its own append — each bounded at
// [statelog.DefaultResolveBudget]. Three of those back to back is fifteen
// seconds, past the ten every other verb waits, which is where a working purge
// was abandoned and reported with no operation id to retry it under; twice
// that leaves room for the arbitration rounds between them and the request
// around them. A coordination write — a seat's pause — is a compare-and-set,
// not a record, and keeps the ordinary ceiling.
const logWriteTimeout = 6 * statelog.DefaultResolveBudget
