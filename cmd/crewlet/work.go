package main

import (
	"context"
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
// tools, and that is the design: an engine whose operator edits work by hand is
// one whose org chart is decoration. The exception is the operation no seat may
// ever perform — a purge, which deletes a task's own rows and every row naming
// it on every node and empties the content of its history, and which the write
// path restricts to a person or an operator token precisely because nothing
// else can be asked to confirm it.
//
// No seat tool reaches it, so this verb and the route behind it are how a
// company destroys a task at all: an erasure request needs a mechanism, and a
// credential pasted into a task body is otherwise in the durable rows of every
// node for ever. `remove` only hides a task, and takes nothing out of a single
// database.

// runWork dispatches `crewlet work`.
func runWork(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "purge":
		return workPurge(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, "usage: crewlet work purge <task-id> "+
			"-project KEY -reason TEXT -confirm <task-key> "+
			"[<config.yaml>] [-url] [-token]")
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown work command %q", sub)
	}
}

// workPurge is `crewlet work purge`.
//
// # The confirmation is the task's KEY, and the reason is required
//
// An id is a uuid nobody reads and which the command line already carries;
// repeating it confirms nothing. The KEY is what a person sees on the board and
// in the ticket they were asked to act on, so it has to be looked up — which is
// the whole point of asking.
//
// The reason is required because the content is destroyed and the reason is
// the account of why. The node that takes the purge logs it beside the key
// (`task_purged`), and it travels whole on the purge's own line in the activity
// feed — the line the project's lead is notified with, when there is one —
// which is why the node refuses one too long to fit that line
// (`reason_too_long`, naming how many bytes do) rather than cutting it, lead or
// no lead.
func workPurge(args []string, stdout, stderr io.Writer) error {
	id, rest := splitSubject(args)
	var project, reason, confirm, opID *string
	client, err := nodeClientFor(rest, "work purge", stderr, func(fs *flag.FlagSet) {
		project = fs.String("project", "", "the task's project key; required")
		reason = fs.String("reason", "", "why; required, and refused rather "+
			"than cut if too long to travel whole")
		confirm = fs.String("confirm", "",
			"the task's KEY — this deletes the task's rows and every row naming "+
				"it, and empties its history")
		opID = fs.String("op-id", "",
			"retry an `unknown` outcome with the id it printed, so the retry "+
				"cannot append a second purge")
	})
	if err != nil {
		return err
	}
	switch {
	case id == "" || strings.TrimSpace(*confirm) == "":
		fmt.Fprintln(stderr, "usage: crewlet work purge <task-id> "+
			"-project KEY -reason TEXT -confirm <task-key>")
		return fmt.Errorf("a purge deletes the task's rows and every row naming " +
			"it on every node, empties the content of its history and moves its " +
			"subtasks onto its parent, and nothing undoes it — name the task's " +
			"KEY in -confirm to run it")
	case strings.TrimSpace(*project) == "":
		return fmt.Errorf("name the task's project in -project: it is the " +
			"container the record arbitrates under, and a purge filed under " +
			"the wrong one blocks writes to a project it is not about")
	case strings.TrimSpace(*reason) == "":
		return fmt.Errorf("state why in -reason: the content is destroyed and " +
			"the reason, kept on the line the purge leaves in the activity " +
			"feed, is the account of why")
	}

	var answer struct {
		Task     string `json:"task"`
		Key      string `json:"key"`
		Project  string `json:"project"`
		Outcome  string `json:"outcome"`
		Position struct {
			Stream     string `json:"stream"`
			Generation uint32 `json:"generation"`
			Seq        uint64 `json:"seq"`
		} `json:"position"`
		OpID string `json:"op_id"`
	}
	path := fmt.Sprintf("/work/%s/purge?confirm=%s&project=%s&reason=%s",
		url.PathEscape(id), url.QueryEscape(strings.TrimSpace(*confirm)),
		url.QueryEscape(strings.TrimSpace(*project)),
		url.QueryEscape(strings.TrimSpace(*reason)))
	if given := strings.TrimSpace(*opID); given != "" {
		path += "&op_id=" + url.QueryEscape(given)
	}
	if err := client.post(context.Background(), path, &answer); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "purge %s (%s): %s at %s %d\n", answer.Key, id,
		answer.Outcome, answer.Position.Stream, answer.Position.Seq)
	switch answer.Outcome {
	case "pending":
		// THE THREE-VALUED OUTCOME, said plainly and NOT as a failure.
		// The record is on the log and every node applies it as it
		// reaches it; running the gesture again would append a second
		// purge of a task the first one already destroyed.
		fmt.Fprintln(stdout, "  The record is durable and this node has not "+
			"applied it yet: the rows go as each node reaches it. Do not "+
			"run this again.")
	case "unknown":
		fmt.Fprintf(stdout, "  No acknowledgement — this is the one outcome to "+
			"retry, and retrying with the same operation id is what stops a "+
			"second record: -op-id %s\n", answer.OpID)
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
