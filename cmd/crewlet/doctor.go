package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/words"
)

const doctorUsage = "usage: crewlet doctor [-json] [-words] [<config.yaml>] [-api URL]"

// exitBlocked is the doctor's status while a finding blocks the company: apart
// from 1, which says the command could not ask or the node could not read a
// source.
const exitBlocked = 2

// runDoctor prints the setup advisor's findings: classes, subjects by kind and
// ordinal, and counts by default, so its output can be pasted anywhere; the
// subjects' handles, titles and reasons only with -words.
func runDoctor(environ config.Source, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stderr, doctorUsage)
		return flag.ErrHelp
	}
	var asJSON, withWords *bool
	client, err := nodeClientFor(environ, args, "doctor", stderr, func(fs *flag.FlagSet) {
		asJSON = fs.Bool("json", false, "print the node's answer as JSON, for a script")
		withWords = fs.Bool("words", false, "print each finding's subject by its handles, and its title and why: the company's own names")
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
	count := 0
	for _, f := range answer.Findings {
		if f.Severity == advisor.SeverityBlocks {
			count++
			if !slices.Contains(blocking, string(f.Class)) {
				blocking = append(blocking, string(f.Class))
			}
		}
	}
	if count > 0 {
		if *asJSON {
			return &exitError{code: exitBlocked, err: errSilent}
		}
		return &exitError{code: exitBlocked, err: fmt.Errorf("%s the company cannot work as configured: %s",
			words.Count(count, "finding says", "findings say"), words.And(blocking))}
	}
	// A SOURCE THE NODE COULD NOT READ FAILS CLOSED, as a failure to ask: a
	// block it holds is missing from the list, so no gate may pass on it.
	if len(answer.Unread) > 0 {
		if *asJSON {
			return errSilent
		}
		return fmt.Errorf("the node could not read %s, so a finding that blocks may be missing",
			words.And(answer.Unread))
	}
	return nil
}

// renderFindings prints one row per finding, the most costly first.
func renderFindings(w io.Writer, a advisor.Answer, withWords bool) {
	if len(a.Findings) == 0 {
		fmt.Fprintf(w, "No findings at revision %s.\n", a.Revision)
		renderUnread(w, a.Unread)
		return
	}
	subjects := ordinals(a.Findings)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tSEVERITY\tACTOR\tCLASS\tSUBJECT\tEVIDENCE")
	for i, f := range a.Findings {
		subject := subjects[i]
		if withWords {
			subject = subjectHandles(f.Subject)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", i+1, f.Severity, f.Actor, f.Class, subject, factsText(f.Evidence))
	}
	_ = tw.Flush()
	if withWords {
		fmt.Fprintln(w)
		for i, f := range a.Findings {
			fmt.Fprintf(w, "%d. %s %s\n", i+1, f.Title, f.Why)
		}
	}
	fmt.Fprintf(w, "\nAt revision %s. An owner finding's fix is sent from the dashboard or with "+
		"PUT /config; an engine finding is a pattern the engine is reducing.\n", a.Revision)
	renderUnread(w, a.Unread)
}

// renderUnread names what the node could not read, so a short list never
// reads as all clear.
func renderUnread(w io.Writer, unread []string) {
	if len(unread) > 0 {
		fmt.Fprintf(w, "Not read: %s. The list may be incomplete.\n", words.And(unread))
	}
}

// ordinals names each finding's subject by its kind and the order the answer
// first names it in (seat#1, schedule#2), never by a handle the company chose.
func ordinals(findings []advisor.Finding) []string {
	seen := map[string]string{}
	next := map[advisor.SubjectKind]int{}
	out := make([]string, len(findings))
	for i, f := range findings {
		if f.Subject.Kind == advisor.SubjectCompany {
			out[i] = string(f.Subject.Kind)
			continue
		}
		key := subjectHandles(f.Subject)
		if _, ok := seen[key]; !ok {
			next[f.Subject.Kind]++
			seen[key] = fmt.Sprintf("%s#%d", f.Subject.Kind, next[f.Subject.Kind])
		}
		out[i] = seen[key]
	}
	return out
}

// subjectHandles is a subject by its kind and handles: seat:sre:health-check.
// An integration is named by its product, which the company did not choose.
func subjectHandles(s advisor.Subject) string {
	parts := []string{string(s.Kind)}
	handles := []string{s.Seat, s.Unit, s.Schedule}
	if s.Kind == advisor.SubjectIntegration {
		handles = []string{s.Name}
	}
	for _, p := range handles {
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
