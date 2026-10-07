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
	At         time.Time `json:"at"`
	Completed  bool      `json:"completed"`
	Listed     int       `json:"listed"`
	Deleted    int       `json:"deleted"`
	Abandoned  int       `json:"abandoned"`
	Skipped    string    `json:"skipped"`
	SweepError string    `json:"sweep_error"`
	Error      string    `json:"error"`
}

type objectsAuditView struct {
	At    time.Time           `json:"at"`
	Error string              `json:"error"`
	Found *objectsFindingView `json:"found"`
}

type objectsFindingView struct {
	At           time.Time `json:"at"`
	Completed    bool      `json:"completed"`
	Referenced   int       `json:"referenced"`
	Missing      int       `json:"missing"`
	Damaged      int       `json:"damaged"`
	MissingFiles []struct {
		Object  string `json:"object"`
		NamedBy string `json:"named_by"`
		Damaged bool   `json:"damaged"`
	} `json:"missing_files"`
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
// collection and audit found — and, above all, any object the store has lost.
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
		fmt.Fprintf(&b, "Last collection:   %s — %d objects listed, %d deleted, "+
			"%d unfinished upload(s) abandoned%s\n",
			c.At.Format(time.RFC3339), c.Listed, c.Deleted, c.Abandoned,
			passNote(c.Completed, c.Skipped, c.Error))
		if c.SweepError != "" {
			fmt.Fprintf(&b, "                   unfinished uploads could not be swept: %s\n", c.SweepError)
		}
	} else {
		fmt.Fprintln(&b, "Last collection:   none yet")
	}
	// THE FINDINGS ARE THE LAST AUDIT TO RUN TO ITS END, and an attempt that
	// failed after it is said beside them rather than in their place: what
	// the store has lost does not stop being lost because an audit could
	// not finish.
	a := v.Audit
	switch {
	case a == nil:
		fmt.Fprintln(&b, "Last audit:        none yet")
	case a.Found == nil:
		fmt.Fprintf(&b, "Last audit:        none has finished — the last, at %s, stopped: %s\n",
			a.At.Format(time.RFC3339), a.Error)
	default:
		f := a.Found
		fmt.Fprintf(&b, "Last audit:        %s — %d files named, %d missing, %d damaged%s\n",
			f.At.Format(time.RFC3339), f.Referenced, f.Missing, f.Damaged, passNote(f.Completed, "", ""))
		if a.Error != "" && a.At.After(f.At) {
			fmt.Fprintf(&b, "                   a later audit, at %s, stopped: %s\n",
				a.At.Format(time.RFC3339), a.Error)
		}
		if lost := f.Missing + f.Damaged; lost > 0 {
			fmt.Fprintf(&b, "\n%d file(s) cannot be read: the store does not hold their bytes, "+
				"or holds them wrong. Restore them from a backup (docs/guides/backup.md), or "+
				"upload them again:\n", lost)
			for _, m := range f.MissingFiles {
				what := "missing"
				if m.Damaged {
					what = "damaged"
				}
				fmt.Fprintf(&b, "  %s (%s, object %s)\n", m.NamedBy, what, m.Object)
			}
			if shown := len(f.MissingFiles); shown < lost {
				fmt.Fprintf(&b, "  … and %d more\n", lost-shown)
			}
		}
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
//
// A PASS THAT STOPPED KEEPS ITS COUNTS: a collection that met an estate it
// could not fully read had already deleted what it counted before it stopped,
// so the note says why it stopped and never that it deleted nothing.
func passNote(completed bool, skipped, failed string) string {
	switch {
	case failed != "":
		return " (stopped: " + failed + ")"
	case skipped != "":
		return " (stopped judging: " + skipped + ")"
	case !completed:
		return " (incomplete)"
	}
	return ""
}
