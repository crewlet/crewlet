package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// `crewlet work` — the gestures on the company's own work items that belong to
// a PERSON rather than to a seat.
//
// # Why there is exactly one of them
//
// Everything a company does to its tasks is done by seats, through their own
// tools, and a person does the rest through the HTTP write surface the
// dashboard is built on — the same tools again, under a person's own name. The
// one gesture that belongs on a SHELL is the operation no seat may ever
// perform: a purge, which destroys a task and every row it produced on every
// node, which the write path restricts to a person or an operator token
// precisely because nothing else can be asked to confirm it, and which an
// operator acting on an erasure request runs from a terminal and a runbook
// rather than from a screen. It goes through that same surface's own route.
//
// That operation had no caller anywhere. `tracker.Writer.PurgeTask` existed,
// the applier handled its record, the deletion marker stopped a redelivery
// resurrecting anything — and no CLI verb, no route and no tool reached it. So
// a company could not destroy a task under any circumstances: an erasure
// request had no mechanism, and a credential pasted into a task body stayed in
// the durable rows of every node for ever. `remove` only hides a task and
// `delete` only stops later records about it; neither takes the body out of a
// single database.

// runWork dispatches `crewlet work`.
func runWork(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "purge":
		return workPurge(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, workPurgeUsage)
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown work command %q", sub)
	}
}

// workPurgeUsage is the one usage line, printed wherever the command is
// refused for want of an argument.
const workPurgeUsage = "usage: crewlet work purge <item> -reason TEXT " +
	"-confirm <item-key> [-op-id ID] [<config.yaml>] [-url]"

// workPurge is `crewlet work purge`.
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
func workPurge(args []string, stdout, stderr io.Writer) error {
	item, rest := splitSubject(args)
	var reason, confirm, opID *string
	client, err := nodeClientFor(rest, "work purge", stderr, func(fs *flag.FlagSet) {
		reason = fs.String("reason", "", "why; recorded on the deletion marker")
		confirm = fs.String("confirm", "",
			"the item's KEY — this destroys the item and every row it produced")
		opID = fs.String("op-id", "",
			"retry a purge whose outcome is unknown with the id it printed, so "+
				"the retry cannot append a second purge; empty starts a new one")
	})
	if err != nil {
		return err
	}
	switch {
	case item == "" || strings.TrimSpace(*confirm) == "":
		fmt.Fprintln(stderr, workPurgeUsage)
		return fmt.Errorf("a purge destroys the item and every row it produced " +
			"on every node, and nothing undoes it — name the item's KEY in " +
			"-confirm to run it")
	case strings.TrimSpace(*reason) == "":
		return fmt.Errorf("state why in -reason: the rows are destroyed and " +
			"the marker's reason is the only account of them that survives")
	}

	// THE OPERATION ID IS MINTED HERE, BEFORE THE REQUEST, when the operator
	// brought none — the gate verbs' rule, for their reason. A purge whose
	// request timed out or dropped may well have landed, and the key it was
	// sent under is the only handle on it: minted by the node, it was lost
	// with the answer, and the only way on was a second purge under a fresh
	// one. It is minted as the node would mint it ([statelog.NewOpID]),
	// carrying the instant a node judges the retry by.
	//
	// AND ONE THE OPERATOR BROUGHT IS HELD TO THE RULE EVERY SURFACE HOLDS
	// IT TO ([statelog.CheckCallerOpID]) before anything is sent, and sent
	// as given: the node refuses an id it did not mint or one altered on the
	// way back `400 op_id_invalid`, and refused here the error names the
	// flag rather than a header this command composed.
	operation := *opID
	if operation == "" {
		operation = statelog.NewOpID(time.Now(), "purge")
	} else if err = statelog.CheckCallerOpID(operation); err != nil {
		return fmt.Errorf("-op-id: %w", err)
	}

	var answer struct {
		Task     string `json:"task"`
		Key      string `json:"key"`
		Project  string `json:"project"`
		Outcome  string `json:"outcome"`
		Position string `json:"position"`
		OpID     string `json:"op_id"`
	}
	path := fmt.Sprintf("/work/items/%s/purge?confirm=%s&reason=%s",
		url.PathEscape(item), url.QueryEscape(strings.TrimSpace(*confirm)),
		url.QueryEscape(strings.TrimSpace(*reason)))
	// PATIENTLY, for the reason [nodeClient.patiently] names: a purge waits on
	// the write path's own waits, not on the network.
	err = client.patiently(purgeRequestTimeout).postKeyed(context.Background(), path,
		operation, &answer)
	var lost noAnswer
	if errors.As(err, &lost) {
		fmt.Fprintf(stderr, "The node did not answer, so whether the purge "+
			"landed is unknown. Run the same command again with -op-id %s — "+
			"if the record landed, the retry answers from it rather than "+
			"appending a second purge.\n", operation)
		return err
	}
	var refused *nodeRefusal
	if errors.As(err, &refused) && refused.OpID != "" &&
		refused.Status == http.StatusServiceUnavailable {
		// EVERY RETRY IS UNDER THE SAME ID, and the answer says which
		// one: a fresh one would append a second purge of an item an
		// earlier attempt may already have destroyed. The id is the
		// node's answer, which is the one this command sent.
		switch {
		case refused.Unvouched:
			// NOT THROUGH THIS NODE: its operation ledger cannot
			// vouch for the operation, so it answers the same way
			// until the record reaches it.
			return fmt.Errorf("%w\n\nThis node cannot tell whether it landed "+
				"and will answer the same way until it can: check whether %s "+
				"is gone, or run it through another node with -url <that node> "+
				"-op-id %s — never a fresh id", err, item, refused.OpID)
		case refused.Unsettled:
			return fmt.Errorf("%w\n\nNo acknowledgement — this is the one "+
				"outcome to retry, and retrying with the same operation id is "+
				"what stops a second record: -op-id %s", err, refused.OpID)
		default:
			// A REFUSAL, which wrote nothing: what the node names above
			// is what clears it, and waiting may.
			return fmt.Errorf("%w\n\nThis attempt wrote nothing. Once the "+
				"node can take it, run it again with -op-id %s, so every "+
				"attempt stays the one operation", err, refused.OpID)
		}
	}
	if err != nil {
		return err
	}

	position := ""
	if answer.Position != "" {
		position = " at " + answer.Position
	}
	fmt.Fprintf(stdout, "purge %s in %s (%s): %s%s (operation %s)\n", answer.Key,
		answer.Project, answer.Task, answer.Outcome, position,
		firstNonEmpty(answer.OpID, operation))
	if answer.Outcome == string(statelog.OutcomePending) {
		// THE THREE-VALUED OUTCOME, said plainly and NOT as a failure.
		// The record is on the log and every node applies it as it
		// reaches it; running the gesture again would append a second
		// purge of an item the first one already destroyed.
		fmt.Fprintln(stdout, "  The record is durable and this node has not "+
			"applied it yet: the rows go as each node reaches it. Do not "+
			"run this again.")
	}
	// WHAT A PURGE DOES NOT REACH, every time rather than in the docs
	// alone. An offline or evicted disk keeps its copy until replay,
	// adoption, replacement or destruction, and there is no duration to
	// state — so an operator acting on an erasure request has to be told
	// here, where they are running the gesture.
	fmt.Fprintln(stdout, "  A node that is offline or evicted keeps its copy "+
		"until it replays, adopts a snapshot, is replaced or is destroyed. "+
		"`crewlet retention status` names which nodes those are.")
	return nil
}

// purgeRequestTimeout is how long `work purge` waits for the node's answer.
//
// SIX RESOLVE BUDGETS. A purge is one record, and what it can legitimately
// wait on is the write path's own: the wait for this node to reach the
// caller's previous write, the wait for it to reach a peer's record on the
// same task, and the resolution of its own append — each bounded at
// [statelog.DefaultResolveBudget]. Three of those back to back is fifteen
// seconds, past the ten every other verb waits, which is where a working purge
// was abandoned and reported with no operation id to retry it under; twice
// that leaves room for the arbitration rounds between them and the request
// around them.
const purgeRequestTimeout = 6 * statelog.DefaultResolveBudget
