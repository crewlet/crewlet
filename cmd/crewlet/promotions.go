package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
)

// runPromotions is `crewlet promotions`: the skill-promotion ledger.
//
// # Why it exists
//
// The ledger is the fleet's record of every convergence the promotion pass
// acted on, and it keeps each for good, because a record is what stops the
// pass drafting again a procedure a lead rejected. That makes a record the
// pass got wrong wrong for good as well — a Confluence draft a page
// restriction hides from the org token reads exactly as a deleted one — so an
// operator needs to see the ledger and to clear one record of it.
//
// THROUGH A NODE, for the reason `budgets` goes through one: the ledger lives
// in the coordination store, which on the default topology is inside the
// engine's own process. `list` reads GET /learning/promotions and `clear`
// posts to POST /learning/promotions/clear.
func runPromotions(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "list":
		return promotionsList(rest, stdout, stderr)
	case "clear":
		return promotionsClear(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, "usage: crewlet promotions list [<config.yaml>] [-url] "+
			"[-token] [-unit] [-json]\n"+
			"       crewlet promotions clear [<config.yaml>] [-url] [-token] "+
			"-unit UNIT -fingerprint FINGERPRINT")
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown promotions command %q", sub)
	}
}

// promotionsList prints the ledger, one record at a time.
//
// EVERY FIELD WHOLE, one per line, rather than a table: a title, a rejection
// and the reason a record cannot be read are prose, and a column would have
// to cut them. `-json` prints the node's answer as it came, draft bodies
// included.
func promotionsList(args []string, stdout, stderr io.Writer) error {
	var unit *string
	var asJSON *bool
	client, err := nodeClientFor(args, "promotions list", stderr, func(fs *flag.FlagSet) {
		unit = fs.String("unit", "", "list one unit's records; empty lists every unit's")
		asJSON = fs.Bool("json", false, "print the node's answer as JSON, draft bodies included")
	})
	if err != nil {
		return err
	}
	path := "/learning/promotions"
	if *unit != "" {
		path += "?unit=" + url.QueryEscape(*unit)
	}
	var answer struct {
		Promotions []learning.PromotionReport `json:"promotions"`
	}
	if err := client.get(context.Background(), path, &answer); err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(answer)
	}
	if len(answer.Promotions) == 0 {
		fmt.Fprintln(stdout, "The ledger holds no record: the promotion pass has "+
			"acted on no convergence yet.")
		return nil
	}
	for i, rec := range answer.Promotions {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		printPromotion(stdout, rec)
	}
	return nil
}

// printPromotion writes one record: its address and state on the first line,
// and each field it carries on a line of its own.
func printPromotion(w io.Writer, rec learning.PromotionReport) {
	state := rec.State
	if state == "" {
		state = "unreadable"
	}
	fmt.Fprintf(w, "%s  %s  %s", rec.Unit, rec.Fingerprint, state)
	if !rec.At.IsZero() {
		fmt.Fprintf(w, "  %s", rec.At.UTC().Format(time.RFC3339))
	}
	fmt.Fprintln(w)
	field := func(name, value string) {
		if value != "" {
			fmt.Fprintf(w, "  %-11s %s\n", name+":", value)
		}
	}
	field("title", rec.Title)
	if rec.PageID != "" || rec.Container != "" {
		field("page", fmt.Sprintf("%s in %s (%s)",
			dashIfEmpty(rec.PageID), dashIfEmpty(rec.Container), dashIfEmpty(rec.Backend)))
	}
	if len(rec.Tools) > 0 {
		field("tools", fmt.Sprintf("%s (%d seats)", strings.Join(rec.Tools, ", "), rec.Agents))
	}
	field("rejection", rec.Rejection)
	field("refused", rec.Refused)
	field("unreadable", rec.Unreadable)
	field("raw", rec.Raw)
}

// promotionsClear deletes one record, naming what it cleared.
//
// NOTHING IS UNDONE AT THE KNOWLEDGE BASE. A page the record named stays
// where it is, and the next pass drafts the convergence as though it had never
// been drafted — beside that page, under a title of its own when the model
// names it the same.
func promotionsClear(args []string, stdout, stderr io.Writer) error {
	var unit, fingerprint *string
	client, err := nodeClientFor(args, "promotions clear", stderr, func(fs *flag.FlagSet) {
		unit = fs.String("unit", "", "the unit the record is filed under (required)")
		fingerprint = fs.String("fingerprint", "",
			"the record's fingerprint, as `promotions list` prints it (required)")
	})
	if err != nil {
		return err
	}
	if *unit == "" || *fingerprint == "" {
		return fmt.Errorf("name the record with -unit and -fingerprint, as " +
			"`crewlet promotions list` prints them")
	}
	path := "/learning/promotions/clear?unit=" + url.QueryEscape(*unit) +
		"&fingerprint=" + url.QueryEscape(*fingerprint)
	var answer struct {
		Cleared learning.PromotionReport `json:"cleared"`
	}
	if err := client.post(context.Background(), path, &answer); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Cleared:")
	printPromotion(stdout, answer.Cleared)
	fmt.Fprintln(stdout, "\nThe next promotion pass drafts this convergence again "+
		"if its seats still converge. A page the record named is left where it is.")
	return nil
}
