package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
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
			"retry an outcome the node could not establish with the id it "+
				"printed, so the retry cannot append a second purge")
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
	err = client.postKeyed(context.Background(), path, *opID, &answer)
	var unsettled *unsettledWrite
	if errors.As(err, &unsettled) {
		// THE ONE OUTCOME TO RETRY, and only under the SAME id: a fresh
		// one would append a second purge of an item the first may
		// already have destroyed.
		return fmt.Errorf("%w\n\nNo acknowledgement — this is the one "+
			"outcome to retry, and retrying with the same operation id is "+
			"what stops a second record: -op-id %s", err, unsettled.opID)
	}
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "purge %s in %s (%s): %s at %s\n", answer.Key,
		answer.Project, answer.Task, answer.Outcome, answer.Position)
	if answer.Outcome == "pending" {
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
