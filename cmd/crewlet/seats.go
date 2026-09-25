package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// `crewlet seats` — pausing and resuming a seat from a shell.
//
// # Through the act transport, as the person
//
// The same `pause_seat` and `resume_seat` the dashboard's buttons call, over
// the same route (`POST /operator/act/{tool}`), so a pause from a terminal and
// one from the profile screen are one gesture with one record: the token names
// the person, the seat it is bound to rides beside it, and the call leaves the
// same `operator_acted` row. A token nobody bound is refused `unbound` with the
// remedy, which is the act transport's one rule — a pause is made by somebody.
//
// # A retry is the same request
//
// Every invocation mints a request id and prints it when the answer is
// `unknown`; `-request-id` sends it again, so a retry is the first attempt's
// operation rather than a second one. Both gestures are also idempotent on the
// record itself — pausing a paused seat changes nothing — so the id is what
// keeps the runtime audit to one row per gesture.

// runSeats dispatches `crewlet seats`.
func runSeats(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "pause":
		return seatsAct(rest, "pause_seat", true, stdout, stderr)
	case "resume":
		return seatsAct(rest, "resume_seat", false, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, seatsUsage)
		return flag.ErrHelp
	default:
		fmt.Fprintln(stderr, seatsUsage)
		return fmt.Errorf("unknown seats command %q", sub)
	}
}

const seatsUsage = "usage: crewlet seats pause <handle> [-stop] [-reason TEXT] " +
	"[<config.yaml>] [-url] [-token] [-request-id]\n" +
	"       crewlet seats resume <handle> [<config.yaml>] [-url] [-token] [-request-id]"

// seatsAct is one pause or resume.
func seatsAct(args []string, tool string, pausing bool, stdout, stderr io.Writer) error {
	handle, rest := splitSubject(args)
	var stop *bool
	var reason, requestID *string
	client, err := nodeClientFor(rest, "seats "+strings.TrimSuffix(tool, "_seat"), stderr,
		func(fs *flag.FlagSet) {
			if pausing {
				stop = fs.Bool("stop", false,
					"also end the turn the seat is on at its next round; it is not run again")
				reason = fs.String("reason", "", "why, in a line; shown on the seat and in the feed")
			}
			requestID = fs.String("request-id", "",
				"retry an `unknown` outcome with the id it printed, so the retry is the "+
					"same request")
		})
	if err != nil {
		return err
	}
	handle = strings.TrimPrefix(strings.TrimSpace(handle), "@")
	if handle == "" {
		fmt.Fprintln(stderr, seatsUsage)
		return errors.New("name the seat: its handle, as the org chart spells it")
	}
	id := strings.TrimSpace(*requestID)
	if id == "" {
		id = uuid.NewString()
	}
	body := map[string]any{"handle": handle}
	if pausing {
		if *stop {
			body["stop_running"] = true
		}
		if r := strings.TrimSpace(*reason); r != "" {
			body["reason"] = r
		}
	}

	var answer struct {
		Outcome string `json:"outcome"`
		Receipt struct {
			Changed      bool   `json:"changed"`
			PausedBy     string `json:"paused_by"`
			PausedBySeat string `json:"paused_by_seat"`
			PausedAt     string `json:"paused_at"`
			StopRunning  bool   `json:"stop_running"`
		} `json:"receipt"`
	}
	if err := client.postJSON(context.Background(), "/operator/act/"+url.PathEscape(tool),
		map[string]any{"request_id": id, "args": body}, &answer); err != nil {
		return err
	}

	switch {
	case answer.Outcome == "unknown":
		fmt.Fprintf(stdout, "%s %s: unknown — the node could not confirm the write. "+
			"Retry with -request-id %s\n", strings.TrimSuffix(tool, "_seat"), handle, id)
	case pausing && answer.Receipt.Changed:
		by := firstNonEmpty(answer.Receipt.PausedBySeat, answer.Receipt.PausedBy)
		fmt.Fprintf(stdout, "paused %s (by %s at %s)\n", handle, by, answer.Receipt.PausedAt)
		if answer.Receipt.StopRunning {
			fmt.Fprintln(stdout, "  The turn it is on ends at its next round and is not run again.")
		} else {
			fmt.Fprintln(stdout, "  The turn it is on, if any, finishes first; nothing new starts.")
		}
	case pausing:
		by := firstNonEmpty(answer.Receipt.PausedBySeat, answer.Receipt.PausedBy)
		fmt.Fprintf(stdout, "%s was already paused (by %s at %s); nothing changed\n",
			handle, by, answer.Receipt.PausedAt)
	case answer.Receipt.Changed:
		fmt.Fprintf(stdout, "resumed %s\n  What waited on its inbox is delivered first, in order.\n",
			handle)
	default:
		fmt.Fprintf(stdout, "%s was not paused; nothing changed\n", handle)
	}
	return nil
}
