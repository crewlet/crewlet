package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/words"
)

const doctorUsage = "usage: crewlet doctor [-json] [-words] [<config.yaml>] [-api URL]"

// runDoctor prints the setup advisor's findings: classes, subjects and counts by
// default, so its output can be pasted anywhere; titles and reasons with -words.
func runDoctor(environ config.Source, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stderr, doctorUsage)
		return flag.ErrHelp
	}
	var asJSON, withWords *bool
	client, err := nodeClientFor(environ, args, "doctor", stderr, func(fs *flag.FlagSet) {
		asJSON = fs.Bool("json", false, "print the node's answer as JSON, for a script")
		withWords = fs.Bool("words", false, "print each finding's title and why, which name the company's own seats and schedules")
	})
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := client.get(context.Background(), "/advisor", &raw); err != nil {
		return err
	}
	var answer advisor.Answer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return fmt.Errorf("the node's advisor answer is not the shape this build reads: %w", err)
	}
	if *asJSON {
		if err := printAnswer(stdout, raw); err != nil {
			return err
		}
	} else {
		renderFindings(stdout, answer, *withWords)
	}
	var blocking []string
	for _, f := range answer.Findings {
		if f.Severity == advisor.SeverityBlocks {
			blocking = append(blocking, string(f.Class))
		}
	}
	if len(blocking) > 0 {
		if *asJSON {
			return errSilent
		}
		return fmt.Errorf("%s the company cannot work as configured: %s",
			words.Count(len(blocking), "finding says", "findings say"), words.And(blocking))
	}
	return nil
}

// renderFindings prints one row per finding, the most costly first.
func renderFindings(w io.Writer, a advisor.Answer, withWords bool) {
	if len(a.Findings) == 0 {
		fmt.Fprintf(w, "No findings at revision %s.\n", a.Revision)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SEVERITY\tACTOR\tCLASS\tSUBJECT\tEVIDENCE")
	for _, f := range a.Findings {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", f.Severity, f.Actor, f.Class, subjectText(f.Subject), factsText(f.Evidence))
		if withWords {
			fmt.Fprintf(tw, "\t\t%s %s\t\t\n", f.Title, f.Why)
		}
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\nAt revision %s. An owner finding's fix is sent from the dashboard or with "+
		"PUT /config; an engine finding is a pattern the engine is reducing.\n", a.Revision)
}

func subjectText(s advisor.Subject) string {
	parts := []string{string(s.Kind)}
	for _, p := range []string{s.Seat, s.Unit, s.Schedule} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ":")
}

func factsText(facts []advisor.Fact) string {
	if len(facts) == 0 {
		return "-"
	}
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		out = append(out, fmt.Sprintf("%s=%d", f.Key, f.Value))
	}
	return strings.Join(out, " ")
}
