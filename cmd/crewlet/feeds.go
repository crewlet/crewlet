package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// `crewlet feeds` — the wake feeds' dead letters: the records no node could
// read in the hour each was handed back, and an operator's discard of one.
//
// # Why through a running node
//
// The entries are kept in the broker's dead-letter stream, `CREWLET_DLQ`,
// which on the default topology is the engine's own embedded broker, binding
// no socket anything outside its process could reach. So `dead-letters` is a
// client of `GET /feeds/dead-letters` and `discard` of its POST, as
// `crewlet fleet broker` is of `/fleet/broker`; every node serving the API
// answers alike, since the entries are the broker's.
//
// # What discard is FOR
//
// A dead letter is a wake nobody has had. The first node whose feed starts
// able to read it derives that wake, and the retention lets it go after
// `stream.event_retention_hours`; what neither covers is an entry no build will
// ever read — a record sealed under a key retired fleet-wide, one no build
// translates and none will — which keeps `feed_dead_lettered` raised for the
// whole retention. Discarding it is the operator's decision that its wake can
// be lost, so the command makes them repeat the id, as every destructive
// gesture of this binary does.

// feedsUsage is the group's usage.
const feedsUsage = "usage: crewlet feeds dead-letters [-json] [<config.yaml>] [-api URL]\n" +
	"       crewlet feeds discard <id> -confirm <id> [<config.yaml>] [-api URL]"

// runFeeds dispatches `crewlet feeds`.
func runFeeds(environ config.Source, args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "dead-letters":
		return feedsDeadLetters(environ, rest, stdout, stderr)
	case "discard":
		return feedsDiscard(environ, rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, feedsUsage)
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown feeds command %q", sub)
	}
}

// deadLetterListing is GET /feeds/dead-letters' answer as this command reads
// it.
//
// A SHAPE OF THIS COMMAND'S OWN, for `retention status`'s reason: a struct
// shared with the writer would tie an older binary to a newer node's answer.
// Its tests serve the engine's own type (engine.FeedDeadLetterListing), so the
// two cannot drift unnoticed.
type deadLetterListing struct {
	At    time.Time `json:"at"`
	Feeds []struct {
		Source    string            `json:"source"`
		Group     string            `json:"group"`
		Subject   string            `json:"subject"`
		Entries   []deadLetterEntry `json:"entries"`
		ListError string            `json:"list_error"`
	} `json:"feeds"`
}

// deadLetterEntry is one entry as this command reads it — the listing's and
// the discard's answer alike.
type deadLetterEntry struct {
	ID             uint64    `json:"id"`
	Source         string    `json:"source"`
	Stream         string    `json:"stream"`
	Position       uint64    `json:"position"`
	Generation     uint64    `json:"generation"`
	Key            string    `json:"key"`
	OpID           string    `json:"op_id"`
	Reason         string    `json:"reason"`
	Detail         string    `json:"detail"`
	StoredAt       time.Time `json:"stored_at"`
	Delivered      uint64    `json:"delivered"`
	DeadLetteredAt time.Time `json:"dead_lettered_at"`
	KeptAt         time.Time `json:"kept_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	DecodeError    string    `json:"decode_error"`
}

// feedsDeadLetters is `crewlet feeds dead-letters`.
//
// A FEED THE NODE COULD NOT LIST EXITS NON-ZERO, in either form: the table and
// the JSON both say which feed and why, and a script reading the exit status
// alone must not take an unread feed for one holding nothing.
func feedsDeadLetters(environ config.Source, args []string, stdout, stderr io.Writer) error {
	var asJSON *bool
	client, err := nodeClientFor(environ, args, "feeds dead-letters", stderr, func(fs *flag.FlagSet) {
		asJSON = fs.Bool("json", false, "print the node's answer as JSON, for a script")
	})
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := client.get(context.Background(), "/feeds/dead-letters", &raw); err != nil {
		return err
	}
	var listing deadLetterListing
	if err := json.Unmarshal(raw, &listing); err != nil {
		return fmt.Errorf("the node's dead-letter answer is not the shape this build reads: %w", err)
	}
	if *asJSON {
		if err := printAnswer(stdout, raw); err != nil {
			return err
		}
	} else {
		renderDeadLetters(stdout, listing)
	}
	var unread []string
	for _, feed := range listing.Feeds {
		if feed.ListError != "" {
			fmt.Fprintf(stderr, "The %s feed's dead letters could not be listed: %s\n",
				feed.Source, feed.ListError)
			unread = append(unread, feed.Source)
		}
	}
	if len(unread) > 0 {
		return fmt.Errorf("%s could not be listed (%s), so this is not every dead letter: "+
			"an unread feed is not an empty one. Run it again",
			words.Count(len(unread), "wake feed", "wake feeds"), words.And(unread))
	}
	return nil
}

// renderDeadLetters prints every entry of every feed the node listed, one row
// each, then in words what each row could not hold: why no node read the
// record, or why this build could not read the entry.
func renderDeadLetters(w io.Writer, l deadLetterListing) {
	var entries []deadLetterEntry
	listed := 0
	for _, feed := range l.Feeds {
		if feed.ListError == "" {
			listed++
		}
		entries = append(entries, feed.Entries...)
	}
	if len(entries) == 0 {
		if listed > 0 {
			fmt.Fprintln(w, "No wake feed holds a dead letter.")
		}
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tFEED\tREASON\tRECORD\tKEY\tKEPT\tEXPIRES")
	for _, e := range entries {
		reason := orDash(e.Reason)
		if e.DecodeError != "" {
			reason = "(unreadable)"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, e.Source, reason, recordOf(e),
			orDash(e.Key), keptAgo(e.KeptAt, l.At), expiresIn(e.ExpiresAt, l.At))
	}
	_ = tw.Flush()
	fmt.Fprintln(w)
	for _, e := range entries {
		switch {
		case e.DecodeError != "":
			fmt.Fprintf(w, "  %d: this build cannot read the entry: %s\n", e.ID, e.DecodeError)
		case e.Detail != "":
			fmt.Fprintf(w, "  %d: %s\n", e.ID, e.Detail)
		}
	}
	fmt.Fprintln(w, "\n  Each is a wake nobody has had: the first node whose feed starts able to "+
		"read one derives it.\n  Discard one whose wake you have decided to lose with "+
		"`crewlet feeds discard <id> -confirm <id>`.")
}

// recordOf is where an entry's record was delivered from: its position as
// every operator surface writes one, where the node that gave up read its
// generation — a sequence alone compares only within one generation — and its
// stream and sequence otherwise.
func recordOf(e deadLetterEntry) string {
	switch {
	case e.Stream == "":
		return "-"
	case e.Generation > 0:
		return statelog.Position{Stream: e.Stream, Generation: uint32(e.Generation),
			Seq: e.Position}.String()
	default:
		return fmt.Sprintf("%s seq %d", e.Stream, e.Position)
	}
}

// keptAgo is how long ago the broker kept an entry, by the answering node's
// clock.
func keptAgo(kept, at time.Time) string {
	if kept.IsZero() || at.IsZero() {
		return "-"
	}
	return statelog.SpokenSpan(at.Sub(kept)) + " ago"
}

// expiresIn is how long an entry has left before its retention lets it go, by
// the answering node's clock.
func expiresIn(expires, at time.Time) string {
	switch {
	case expires.IsZero() || at.IsZero():
		return "-"
	case !expires.After(at):
		return "due now"
	}
	return "in " + statelog.SpokenSpan(expires.Sub(at))
}

// feedsDiscard is `crewlet feeds discard`.
func feedsDiscard(environ config.Source, args []string, stdout, stderr io.Writer) error {
	id, rest := splitSubject(args)
	var confirm *string
	client, err := nodeClientFor(environ, rest, "feeds discard", stderr, func(fs *flag.FlagSet) {
		confirm = fs.String("confirm", "",
			"repeat the id, because the wake the entry holds is lost for good: no node will "+
				"ever derive it")
	})
	if err != nil {
		return err
	}
	if id == "" || *confirm != id {
		fmt.Fprintln(stderr, feedsUsage)
		return errors.New("a discarded dead letter's wake is lost for good: no node will ever " +
			"derive it. Repeat the id in -confirm")
	}
	var entry deadLetterEntry
	path := "/feeds/dead-letters/" + url.PathEscape(id) + "/discard?" +
		url.Values{"confirm": {id}}.Encode()
	if err := client.post(context.Background(), path, &entry); err != nil {
		var lost noAnswer
		if errors.As(err, &lost) {
			fmt.Fprintln(stderr, "The node did not answer, so whether the entry was discarded is "+
				"unknown. `crewlet feeds dead-letters` lists what the feeds still hold; "+
				"discarding an entry already gone is answered as no dead letter.")
		}
		return err
	}
	fmt.Fprintf(stdout, "Discarded dead letter %d of the %s feed (%s).\n", entry.ID, entry.Source,
		recordOf(entry))
	fmt.Fprintln(stdout, "  Its wake is lost for good: no node will derive it.")
	return nil
}
