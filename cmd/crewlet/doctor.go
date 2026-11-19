package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/words"
)

const doctorUsage = "usage: crewlet doctor [-json] [-words] [<config.yaml>] [-api URL]"

// exitBlocked is the doctor's status while a finding blocks the company: apart
// from 1, which says the command could not ask or the node could not read a
// source.
const exitBlocked = 2

// runDoctor prints the Advisor's open findings: classes, subjects by kind and
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
	reading, err := readAdvisor(context.Background(), client)
	if err != nil {
		return err
	}
	answer := reading.answer()
	if *asJSON {
		raw, err := json.Marshal(reading)
		if err != nil {
			return err
		}
		if err := printAnswer(stdout, raw); err != nil {
			return err
		}
	} else {
		renderFindings(stdout, answer, *withWords)
		renderRun(stdout, reading.LastRun)
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
	// A SOURCE THE ADVISOR COULD NOT READ FAILS CLOSED, as a failure to ask: a
	// block it holds is missing from the list, so no gate may pass on it. So
	// does an Advisor that has not run yet.
	switch {
	case reading.LastRun == nil:
		if *asJSON {
			return errSilent
		}
		return errors.New("the Advisor has not run yet, so a finding that blocks may be missing")
	case len(answer.Unread) > 0:
		if *asJSON {
			return errSilent
		}
		return fmt.Errorf("the Advisor could not read %s, so a finding that blocks may be missing",
			words.And(answer.Unread))
	}
	return nil
}

// doctorReading is what the doctor reads of the node: every open finding the
// Advisor keeps, and its latest run.
type doctorReading struct {
	Findings []queries.AdvisorFinding   `json:"findings"`
	LastRun  *queries.AdvisorRunSummary `json:"last_run"`
}

// doctorPages bounds how many pages of findings the doctor reads: a company
// with ten thousand open findings has a problem the list says already.
const doctorPages = 100

// readAdvisor reads every open finding, a page at a time, and the latest run.
func readAdvisor(ctx context.Context, client *nodeClient) (doctorReading, error) {
	out := doctorReading{Findings: []queries.AdvisorFinding{}}
	cursor := ""
	for range doctorPages {
		path := "/advisor/findings?state=open&limit=100"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var page queries.AdvisorFindingList
		if err := client.get(ctx, path, &page); err != nil {
			return doctorReading{}, err
		}
		out.Findings = append(out.Findings, page.Findings...)
		out.LastRun = page.LastRun
		if page.NextCursor == nil {
			return out, nil
		}
		cursor = *page.NextCursor
	}
	return out, fmt.Errorf("the node answered more than %d pages of findings", doctorPages)
}

// answer is the open findings a person is to look at, neither snoozed nor
// applied and waiting on their effect, and what the latest run could not read.
func (r doctorReading) answer() advisor.Answer {
	out := advisor.Answer{Findings: []advisor.Finding{}, Unread: []string{}}
	if r.LastRun != nil {
		out.Unread = r.LastRun.Unread
	}
	for _, f := range r.Findings {
		if f.State != advisor.StateOpen || f.SnoozedUntil != nil {
			continue
		}
		g := advisor.Finding{Class: advisor.Class(f.Class), Severity: f.Severity, Actor: f.Actor, Area: f.Area,
			Subject: advisor.Subject{Kind: f.Subject.Kind, Seat: f.Subject.Seat, Unit: f.Subject.Unit,
				Schedule: f.Subject.Schedule, Name: f.Subject.Name},
			Title: f.Title, Why: f.Why, Item: f.Item, Names: f.Names, Path: f.Path, Fix: f.Fix}
		for _, fact := range f.Evidence.Facts {
			g.Evidence = append(g.Evidence, advisor.Fact{Key: fact.Key, Value: fact.Value})
		}
		out.Findings = append(out.Findings, g)
	}
	return out
}

// renderRun says when the Advisor last checked, or that it has not.
func renderRun(w io.Writer, run *queries.AdvisorRunSummary) {
	switch {
	case run == nil:
		fmt.Fprintln(w, "The Advisor has not run yet.")
	case run.EndedAt == nil:
		fmt.Fprintf(w, "A run asked for at %s has not ended yet.\n", run.StartedAt.UTC().Format(time.RFC3339))
	default:
		fmt.Fprintf(w, "Last checked at %s.\n", run.EndedAt.UTC().Format(time.RFC3339))
	}
}

// renderFindings prints one row per finding, the most costly first.
func renderFindings(w io.Writer, a advisor.Answer, withWords bool) {
	if len(a.Findings) == 0 {
		fmt.Fprintln(w, "No open findings.")
		renderUnread(w, a.Unread)
		return
	}
	subjects := ordinals(a.Findings)
	if withWords {
		for i, f := range a.Findings {
			subjects[i] = subjectHandles(f.Subject)
		}
	}
	rows := folded(a.Findings)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tSEVERITY\tACTOR\tCLASS\tSUBJECT\tEVIDENCE")
	for i, row := range rows {
		f := a.Findings[row[0]]
		var who, facts []string
		for _, j := range row {
			who = append(who, subjects[j])
			facts = append(facts, factsText(a.Findings[j].Evidence))
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", i+1, f.Severity, f.Actor, f.Class,
			strings.Join(who, ","), strings.Join(facts, "; "))
	}
	_ = tw.Flush()
	if withWords {
		fmt.Fprintln(w)
		for i, row := range rows {
			f := a.Findings[row[0]]
			fmt.Fprintf(w, "%d. %s %s\n", i+1, f.Title, f.Why)
			// A CLASS SAYS ONE THING; each subject's specifics are listed under it.
			for _, j := range row {
				g := a.Findings[j]
				line := "   " + subjects[j]
				if g.Item != "" {
					line += ": " + g.Item
				}
				if len(g.Names) > 0 {
					line += " (" + words.And(g.Names) + ")"
				}
				fmt.Fprintln(w, line)
			}
		}
	}
	fmt.Fprintln(w, "\nAn owner finding's fix is applied from the Advisor page, which re-checks it first; "+
		"an engine finding is a pattern the engine is reducing.")
	renderUnread(w, a.Unread)
}

// folded is the findings as rows, one per class, words, cost and fix kind, as
// Settings › Advisor folds them: a class said for several subjects is one row.
func folded(findings []advisor.Finding) [][]int {
	var rows [][]int
	at := map[[5]string]int{}
	for i, f := range findings {
		key := [5]string{string(f.Class), f.Title, f.Why, string(f.Severity), string(f.Fix.Kind)}
		if r, ok := at[key]; ok {
			rows[r] = append(rows[r], i)
			continue
		}
		at[key] = len(rows)
		rows = append(rows, []int{i})
	}
	return rows
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
