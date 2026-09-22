package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
)

// `crewlet chart` — the company's org chart, from the command line.
//
// # Why every one of these goes through a running node
//
// The chart is a LOG, not a table this process can open. A node's replicated
// estate is derived from it by an applier the engine runs, and the store file
// is exclusive to whichever process holds it — so an offline read would be a
// second applier, and an offline write would be a record nothing published.
//
// `crewlet config` opens the store directly because a revision IS a row; this
// command cannot, and that difference is the whole shape of it.
//
// # What it is for
//
// Reading, and one write: the export that makes a cold break a round trip.
// The editing gestures — hire, move, rename — are the dashboard's and the
// API's, because each of them is a decision somebody makes about a person and
// none of them is improved by being typed.

// runChart dispatches the subcommands.
func runChart(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "show":
		return chartShow(rest, stdout, stderr)
	case "check":
		return chartCheck(rest, stdout, stderr)
	case "history":
		return chartHistory(rest, stdout, stderr)
	case "export":
		return chartExport(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr,
			"usage: crewlet chart show|check|history|export [<config.yaml>] "+
				"[-url] [-token] [-out]")
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown chart command %q", sub)
	}
}

// chartRead is the shape every read here decodes.
type chartRead struct {
	Units []struct {
		Key     string `json:"key"`
		Name    string `json:"name"`
		Type    string `json:"type"`
		Parent  string `json:"parent"`
		Lead    string `json:"lead"`
		Purpose string `json:"purpose"`
	} `json:"units"`
	Seats []struct {
		Handle string `json:"handle"`
		Kind   string `json:"kind"`
		Unit   string `json:"unit"`
		Name   string `json:"name"`
		Goal   string `json:"goal"`
	} `json:"seats"`
	Answer struct {
		Level    string `json:"level"`
		Position string `json:"position"`
		Lag      *int64 `json:"lag"`
	} `json:"answer"`
	Runtime bool `json:"runtime"`
}

// chartShow prints the company's units and seats.
func chartShow(args []string, stdout, stderr io.Writer) error {
	client, err := nodeClientFor(args, "chart show", stderr, nil)
	if err != nil {
		return err
	}
	var got chartRead
	if err := client.get(context.Background(), "/chart", &got); err != nil {
		return err
	}
	// THE POSITION FIRST, because everything under it is true only as of
	// one: an operator reading a chart they just changed needs to know
	// whether this node has caught up with them.
	fmt.Fprintf(stdout, "as of %s (%s)", got.Answer.Position, got.Answer.Level)
	if got.Answer.Lag != nil {
		fmt.Fprintf(stdout, ", %d record(s) behind", *got.Answer.Lag)
	}
	fmt.Fprintln(stdout)

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\nUNIT\tPARENT\tLEAD\tNAME")
	sort.Slice(got.Units, func(i, j int) bool { return got.Units[i].Key < got.Units[j].Key })
	for _, u := range got.Units {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", u.Key, dash(u.Parent), dash(u.Lead), u.Name)
	}
	fmt.Fprintln(w, "\nSEAT\tKIND\tUNIT\tNAME")
	sort.Slice(got.Seats, func(i, j int) bool { return got.Seats[i].Handle < got.Seats[j].Handle })
	for _, s := range got.Seats {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Handle, s.Kind, dash(s.Unit), s.Name)
	}
	return w.Flush()
}

// chartCheck prints the continuous report.
//
// IT EXITS NON-ZERO ON AN ERROR-CLASS FINDING, which is what makes it usable
// in a deploy: a seat with no model at all is a company that does not work,
// and a command that reported one and exited 0 would be read as a pass by
// every pipeline that ran it.
func chartCheck(args []string, stdout, stderr io.Writer) error {
	client, err := nodeClientFor(args, "chart check", stderr, nil)
	if err != nil {
		return err
	}
	var got struct {
		Report struct {
			Findings []struct {
				Kind     string `json:"kind"`
				Severity string `json:"severity"`
				Object   string `json:"object"`
				Names    string `json:"names"`
				Detail   string `json:"detail"`
				Remedy   string `json:"remedy"`
			} `json:"findings"`
			Seats     int  `json:"seats"`
			Units     int  `json:"units"`
			Evaluated bool `json:"evaluated"`
		} `json:"report"`
		Worst string `json:"worst"`
	}
	if err := client.get(context.Background(), "/chart/check", &got); err != nil {
		return err
	}
	// ABSENT EVIDENCE IS NOT A CLEAN BILL, and it is the first thing said:
	// no findings from a node that read nothing is the most misleading
	// answer this command could print.
	if !got.Report.Evaluated {
		return errors.New("this node holds no chart view or has applied no " +
			"settings epoch, so it evaluated nothing — ask a node that is " +
			"serving, and do not read this as a pass")
	}
	if len(got.Report.Findings) == 0 {
		fmt.Fprintf(stdout, "no findings over %d unit(s) and %d seat(s)\n",
			got.Report.Units, got.Report.Seats)
		return nil
	}
	for _, f := range got.Report.Findings {
		fmt.Fprintf(stdout, "%s  %s  %s", strings.ToUpper(f.Severity), f.Kind, f.Object)
		if f.Names != "" {
			fmt.Fprintf(stdout, " -> %s", f.Names)
		}
		fmt.Fprintf(stdout, "\n    %s\n", f.Detail)
		if f.Remedy != "" {
			fmt.Fprintf(stdout, "    remedy: %s\n", f.Remedy)
		}
	}
	if got.Worst == "error" {
		return fmt.Errorf("%d finding(s), the worst of them an error",
			len(got.Report.Findings))
	}
	fmt.Fprintf(stdout, "\n%d finding(s), none of them an error\n",
		len(got.Report.Findings))
	return nil
}

// chartHistory prints the company-wide reorganisation feed.
func chartHistory(args []string, stdout, stderr io.Writer) error {
	client, err := nodeClientFor(args, "chart history", stderr, nil)
	if err != nil {
		return err
	}
	var got struct {
		Changes []struct {
			Object struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
			} `json:"object"`
			Kind      string `json:"kind"`
			Actor     string `json:"actor"`
			ActorKind string `json:"actor_kind"`
			Summary   string `json:"summary"`
			CreatedAt string `json:"created_at"`
		} `json:"changes"`
	}
	if err := client.get(context.Background(), "/chart/history", &got); err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WHEN\tWHAT\tOBJECT\tWHO\tSUMMARY")
	for _, c := range got.Changes {
		fmt.Fprintf(w, "%s\t%s\t%s/%s\t%s\t%s\n", c.CreatedAt, c.Kind,
			c.Object.Kind, c.Object.ID, actorOf(c.Actor, c.ActorKind), c.Summary)
	}
	return w.Flush()
}

// chartExport writes the chart as an authored document.
//
// THE UNSTRIPPED ONE, which is why it takes the grant that reads the company
// document: this is a ROUND TRIP — the file an operator edits and imports
// back — so a stripped export would be one that silently deletes half of
// every seat the moment somebody uses it.
func chartExport(args []string, stdout, stderr io.Writer) error {
	var out *string
	client, err := nodeClientFor(args, "chart export", stderr, func(fs *flag.FlagSet) {
		out = fs.String("out", "",
			"write to this path instead of standard output")
	})
	if err != nil {
		return err
	}
	var got json.RawMessage
	if err := client.get(context.Background(), "/company/export", &got); err != nil {
		return err
	}
	pretty, err := json.MarshalIndent(json.RawMessage(got), "", "  ")
	if err != nil {
		return err
	}
	pretty = append(pretty, '\n')
	if out == nil || *out == "" {
		_, err = stdout.Write(pretty)
		return err
	}
	// 0600, LIKE EVERY OTHER FILE THIS BINARY WRITES: an export carries
	// the runtime half of every seat, which is the names of every
	// credential the company holds.
	if err := os.WriteFile(*out, pretty, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote the chart to %s\n", *out)
	return nil
}

// dash renders an empty cell as something a reader can see.
//
// A BLANK COLUMN IS AMBIGUOUS: an empty parent is the ORG ROOT and an empty
// lead is a unit inheriting one, and both are real states rather than missing
// data — so the table says so rather than leaving a gap that reads as a bug.
func dash(in string) string {
	if in == "" {
		return "—"
	}
	return in
}

// actorOf renders who made a change, kind included.
//
// THE KIND IS A COLUMN and never a prefix on the name: one person shown as
// two people three rows apart is what a prefix bought the audit feed the last
// time somebody tried it.
func actorOf(name, kind string) string {
	switch {
	case name == "":
		return "—"
	case kind == "":
		return name
	}
	return name + " (" + kind + ")"
}
