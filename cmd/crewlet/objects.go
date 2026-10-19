package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
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
// And `migrate` asks a data node to copy the files into a bucket
// (`POST /objects/migrate`): only the engine reads the objects, the estate
// naming them and the record the final pass moves.

// runObjects dispatches `crewlet objects`.
func runObjects(environ config.Source, args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "status":
		return objectsStatus(environ, rest, stdout, stderr)
	case "migrate":
		return objectsMigrate(environ, rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, "usage: crewlet objects status [<config.yaml>] [-api URL] [-json]")
		fmt.Fprintln(stderr, "       crewlet objects migrate -to s3 -target <absolute path on the "+
			"engine's host> [-final] [<config.yaml>] [-api URL] [-json]")
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
func objectsStatus(environ config.Source, args []string, stdout, stderr io.Writer) error {
	var asJSON *bool
	client, err := nodeClientFor(environ, args, "objects status", stderr, func(fs *flag.FlagSet) {
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
			"none, so ask a node that does with -api")
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

// migrateWait is how long `crewlet objects migrate` waits for a pass.
//
// SIX HOURS, a ceiling rather than an expectation: a pass is bounded by the
// bytes it copies, seconds for a company that has just started and hours for
// one holding the hundred gibibytes the broker's bucket is sized for, at the
// pace a link to a bucket sustains. The node finishes a pass whatever this
// command does, so a command that stopped waiting has lost only the answer,
// and running it again reports where the copy stands (it copies only what the
// bucket does not hold).
const migrateWait = 6 * time.Hour

// migrateView is the node's answer to POST /objects/migrate, in a shape of
// this command's own (see [objectsView] for why).
type migrateView struct {
	From         string `json:"from"`
	To           string `json:"to"`
	Final        bool   `json:"final"`
	Sealed       bool   `json:"sealed"`
	Moved        bool   `json:"moved"`
	DirectUpload string `json:"direct_upload"`
	Next         string `json:"next"`
	Pass         struct {
		Referenced  int   `json:"referenced"`
		Present     int   `json:"present"`
		Copied      int   `json:"copied"`
		CopiedBytes int64 `json:"copied_bytes"`
		Unreadable  int   `json:"unreadable"`
		Complete    bool  `json:"complete"`
		Files       []struct {
			Object  string `json:"object"`
			NamedBy string `json:"named_by"`
			Damaged bool   `json:"damaged"`
		} `json:"unreadable_files"`
	} `json:"pass"`
}

// objectsMigrate is `crewlet objects migrate --to s3`: a copy pass on a live
// fleet, or with -final, the pass that moves it (see
// docs/concepts/object-store.md).
func objectsMigrate(environ config.Source, args []string, stdout, stderr io.Writer) error {
	var to, target *string
	var final, asJSON *bool
	var wait *time.Duration
	client, err := nodeClientFor(environ, args, "objects migrate", stderr, func(fs *flag.FlagSet) {
		to = fs.String("to", "", "the backend the files move to; s3 is the one there is")
		target = fs.String("target", "", "absolute path ON THE ENGINE'S HOST to a Tier A file "+
			"whose store.objects block names the bucket; required")
		final = fs.Bool("final", false, "run the final pass, which moves the fleet; every node "+
			"must be running with -mode maintenance")
		wait = fs.Duration("wait", migrateWait, "how long to wait for the pass before giving up on the answer")
		asJSON = fs.Bool("json", false, "print the node's report as it answered it")
	})
	if err != nil {
		return err
	}
	if *to != "s3" {
		return fmt.Errorf("name where the files move to with -to s3 (got %q): an S3-compatible "+
			"bucket is the one other store there is", *to)
	}
	if strings.TrimSpace(*target) == "" {
		return errors.New("name the bucket with -target: an absolute path, on the engine's " +
			"host, to a Tier A file whose store.objects block names it")
	}
	// POSIX semantics, for the backup's -dir reason: the path is judged on
	// the engine's host.
	if !path.IsAbs(*target) {
		return fmt.Errorf("-target %s is relative, and it is read on the engine's host rather "+
			"than in this shell; give an absolute path", *target)
	}
	query := "/objects/migrate?target=" + url.QueryEscape(*target)
	if *final {
		query += "&final=true"
	}
	var raw json.RawMessage
	if err := client.patiently(*wait).post(context.Background(), query, &raw); err != nil {
		return err
	}
	if *asJSON {
		_, err := fmt.Fprintf(stdout, "%s\n", raw)
		return err
	}
	var view migrateView
	if err := json.Unmarshal(raw, &view); err != nil {
		return fmt.Errorf("the node's migration report is not the shape this build reads: %w", err)
	}
	return renderMigrate(stdout, view)
}

// renderMigrate prints what a pass did and what to do next.
func renderMigrate(w io.Writer, v migrateView) error {
	var b strings.Builder
	fmt.Fprintf(&b, "From:              %s\n", backendName(v.From))
	fmt.Fprintf(&b, "To:                %s\n", backendName(v.To))
	p := v.Pass
	fmt.Fprintf(&b, "Objects named:     %d (%d already in the bucket, %d copied, %s)\n",
		p.Referenced, p.Present, p.Copied, humanBytes(p.CopiedBytes))
	fmt.Fprintf(&b, "Direct uploads:    %s\n", v.DirectUpload)
	if !p.Complete {
		fmt.Fprintln(&b, "                   this node holds a record it could not apply, so "+
			"an object it did not see may be named")
	}
	if p.Unreadable > 0 {
		fmt.Fprintf(&b, "\n%d file(s) could not be read from the fleet's own store and were not "+
			"copied; no copy can bring them back (see `crewlet objects status`):\n", p.Unreadable)
		for _, f := range p.Files {
			what := "missing"
			if f.Damaged {
				what = "damaged"
			}
			fmt.Fprintf(&b, "  %s (%s, object %s)\n", f.NamedBy, what, f.Object)
		}
		if shown := len(p.Files); shown < p.Unreadable {
			fmt.Fprintf(&b, "  and %d more\n", p.Unreadable-shown)
		}
	}
	switch {
	case v.Moved:
		fmt.Fprintf(&b, "\nThe fleet now keeps its files in the bucket%s.\n", sealedNote(v.Sealed))
	case v.Final:
		fmt.Fprintln(&b, "\nThe fleet was not moved.")
	}
	if v.Next != "" {
		fmt.Fprintf(&b, "\n%s\n", v.Next)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// sealedNote says the old bucket was sealed, where it was.
func sealedNote(sealed bool) string {
	if !sealed {
		return ""
	}
	return ", and the old bucket on the broker is sealed: it is read, and never written again"
}
