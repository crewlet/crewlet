package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

// `crewlet objects` — where the company's files are kept, and what the
// object store's collector last found.
//
// # Why through a running node
//
// The collector's record is in the coordination store, which on the default
// topology is the engine's own embedded broker: a command that opened it from
// outside would find nothing — see nodeclient.go. So `status` reads the fleet
// view (`GET /fleet`), whose objects block every node answers from that record.

// runObjects dispatches `crewlet objects`.
func runObjects(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "status":
		return objectsStatus(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, "usage: crewlet objects status [<config.yaml>] [-url] [-token] [-json]")
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown objects command %q", sub)
	}
}

// objectsView is the fleet view's objects block as this command reads it.
//
// A SHAPE OF THIS COMMAND'S OWN, for `retention status`'s reason: a struct
// shared with the writer would tie an older binary to a newer node's answer.
// Its tests serve the WRITER's renderer (queries.RenderObjects), so the two
// cannot drift unnoticed.
type objectsView struct {
	State   string              `json:"state"`
	Backend string              `json:"backend"`
	Node    string              `json:"node"`
	Collect *objectsCollectView `json:"collect"`
	Audit   *objectsAuditView   `json:"audit"`
}

type objectsCollectView struct {
	At        time.Time `json:"at"`
	Completed bool      `json:"completed"`
	Listed    int       `json:"listed"`
	Deleted   int       `json:"deleted"`
	Skipped   string    `json:"skipped"`
	Error     string    `json:"error"`
}

type objectsAuditView struct {
	At            time.Time `json:"at"`
	Completed     bool      `json:"completed"`
	Referenced    int       `json:"referenced"`
	Missing       int       `json:"missing"`
	MissingChunks []string  `json:"missing_chunks"`
	Error         string    `json:"error"`
}

// objectsStatus is `crewlet objects status`.
func objectsStatus(args []string, stdout, stderr io.Writer) error {
	var asJSON *bool
	client, err := nodeClientFor(args, "objects status", stderr, func(fs *flag.FlagSet) {
		asJSON = fs.Bool("json", false,
			"print the fleet view's objects block as the node answered it")
	})
	if err != nil {
		return err
	}
	var answer struct {
		Objects json.RawMessage `json:"objects"`
	}
	if err := client.get(context.Background(), "/fleet", &answer); err != nil {
		return err
	}
	if len(answer.Objects) == 0 {
		return errors.New("this node's fleet view carries no object store: it runs " +
			"none, so ask a node that does with -url")
	}
	if *asJSON {
		_, err := fmt.Fprintf(stdout, "%s\n", answer.Objects)
		return err
	}
	var view objectsView
	if err := json.Unmarshal(answer.Objects, &view); err != nil {
		return fmt.Errorf("the node's objects block is not the shape this build reads: %w", err)
	}
	return renderObjects(stdout, view)
}

// renderObjects prints the block: where the files are, and what the last
// collection and audit found — and, above all, any chunk the store has lost.
func renderObjects(w io.Writer, v objectsView) error {
	switch v.State {
	case "unavailable":
		return errors.New("the object store's record could not be read: the coordination " +
			"store did not answer — ask again in a moment")
	case "not_yet":
		_, err := fmt.Fprintln(w, "The object store's collector has not finished a pass yet: "+
			"it runs on a data node holding the worker duties, hourly.")
		return err
	case "reported":
	default:
		return fmt.Errorf("the node answered an objects state this build does not know (%q)", v.State)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Files are kept in: %s\n", backendName(v.Backend))
	fmt.Fprintf(&b, "Collector:         %s\n", v.Node)
	if c := v.Collect; c != nil {
		fmt.Fprintf(&b, "Last collection:   %s — %d chunks listed, %d deleted%s\n",
			c.At.Format(time.RFC3339), c.Listed, c.Deleted, passNote(c.Completed, c.Skipped, c.Error))
	} else {
		fmt.Fprintln(&b, "Last collection:   none yet")
	}
	if a := v.Audit; a != nil {
		fmt.Fprintf(&b, "Last audit:        %s — %d chunks named, %d missing%s\n",
			a.At.Format(time.RFC3339), a.Referenced, a.Missing, passNote(a.Completed, "", a.Error))
		if a.Missing > 0 {
			fmt.Fprintf(&b, "\n%d chunk(s) the company's files are made of are not in the store. "+
				"Restore them from a backup (docs/guides/backup.md):\n", a.Missing)
			for _, h := range a.MissingChunks {
				fmt.Fprintf(&b, "  %s\n", h)
			}
			if shown := len(a.MissingChunks); shown < a.Missing {
				fmt.Fprintf(&b, "  … and %d more\n", a.Missing-shown)
			}
		}
	} else {
		fmt.Fprintln(&b, "Last audit:        none yet")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// backendName is a recorded backend in an operator's words.
func backendName(identity string) string {
	if rest, ok := strings.CutPrefix(identity, "s3:"); ok {
		return "an S3 bucket (" + rest + ")"
	}
	if identity == "nats" {
		return "the fleet's own NATS bucket (OBJ_crewlet_files)"
	}
	return identity
}

// passNote is what to say after a pass's counts when it did not run in full.
func passNote(completed bool, skipped, failed string) string {
	switch {
	case failed != "":
		return " (stopped: " + failed + ")"
	case skipped != "":
		return " (deleted nothing: " + skipped + ")"
	case !completed:
		return " (incomplete)"
	}
	return ""
}
