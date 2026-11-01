package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
)

// `crewlet seats` — pausing and resuming a seat from a shell.
//
// # Through the act transport, as whoever the token is
//
// The same `pause_seat` and `resume_seat` the dashboard's buttons call, over
// the same route (`POST /operator/act/{tool}`), so a pause from a terminal and
// one from the profile screen are one gesture with one record. The credential
// is CREWLET_API_TOKEN and nothing else, and the node decides the gesture on
// the principal it resolves to — the seat's holder, whoever leads it, or an
// admin — and records it under that principal's own name, its kind and the
// credential, as every other write is recorded. Nobody is refused for being
// bound to no seat: a token with the authority acts under its own login.
//
// # A retry is the same operation
//
// Every invocation mints an operation id before the request and sends it as
// the `Idempotency-Key` header, which the act transport requires; an answer
// whose outcome is unknown — or none at all — prints it, and `-op-id` sends it
// again, so a retry is the first attempt's operation rather than a second one.
// Both gestures are also idempotent on the record itself — pausing a paused
// seat changes nothing — so the id is what keeps the runtime audit to one row
// per gesture.

// runSeats dispatches `crewlet seats`.
func runSeats(environ config.Source, args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "pause":
		return seatsAct(environ, rest, builtin.PauseSeatTool, true, stdout, stderr)
	case "resume":
		return seatsAct(environ, rest, builtin.ResumeSeatTool, false, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, seatsUsage)
		return flag.ErrHelp
	default:
		fmt.Fprintln(stderr, seatsUsage)
		return fmt.Errorf("unknown seats command %q", sub)
	}
}

const seatsUsage = "usage: crewlet seats pause <handle> [-stop] [-reason TEXT] " +
	"[<config.yaml>] [-api URL] [-op-id ID]\n" +
	"       crewlet seats resume <handle> [<config.yaml>] [-api URL] [-op-id ID]\n" +
	"The credential is " + apiTokenEnv + "."

// seatsAct is one pause or resume.
func seatsAct(environ config.Source, args []string, tool string, pausing bool, stdout, stderr io.Writer) error {
	gesture := strings.TrimSuffix(tool, "_seat")
	handle, rest := splitSubject(args)
	var stop *bool
	var reason, opID *string
	client, err := nodeClientFor(environ, rest, "seats "+gesture, stderr,
		func(fs *flag.FlagSet) {
			if pausing {
				stop = fs.Bool("stop", false,
					"also end the seat's running work: the turn it is on at its next round, "+
						"and any coding run it has out at once; neither is run again. Until it "+
						"is resumed, every change its coding runs make is refused")
				reason = fs.String("reason", "", "why, in a line; shown on the seat and in the feed")
			}
			opID = opIDFlag(fs,
				"retry an `unknown` outcome with the id it printed, so the retry is "+
					"the same operation; empty starts a new one")
		})
	if err != nil {
		return err
	}
	handle = strings.TrimPrefix(strings.TrimSpace(handle), "@")
	if handle == "" {
		fmt.Fprintln(stderr, seatsUsage)
		return errors.New("name the seat: its handle, as the org chart spells it")
	}

	// THE OPERATION IS MINTED HERE, BEFORE THE REQUEST, as every keyed write
	// mints its own ([operationFor]): a gesture whose answer never came may
	// well have landed, and the key it was sent under is the only handle on
	// it.
	operation, err := operationFor(gesture, *opID)
	if err != nil {
		return err
	}
	write := keyedWrite{gesture: gesture, operation: operation, check: "the seat"}

	gestureArgs := map[string]any{"handle": handle}
	if pausing {
		if *stop {
			gestureArgs["stop_running"] = true
		}
		if r := strings.TrimSpace(*reason); r != "" {
			gestureArgs["reason"] = r
		}
	}

	var answer struct {
		Outcome string `json:"outcome"`
		Receipt struct {
			Changed     bool   `json:"changed"`
			PausedBy    string `json:"paused_by"`
			OperatorID  string `json:"operator_id"`
			PausedAt    string `json:"paused_at"`
			StopRunning bool   `json:"stop_running"`
		} `json:"receipt"`
	}
	// THE ORDINARY CEILING: a pause is a read and a compare-and-set on the
	// coordination store — the "handful of writes" [nodeRequestTimeout] is
	// sized for — and a stop rides the record rather than a call of its own.
	if err := write.settle(stderr, client.postKeyed(context.Background(),
		"/operator/act/"+url.PathEscape(tool), operation,
		map[string]any{"args": gestureArgs}, &answer)); err != nil {
		return err
	}

	if err := write.confirmed(gesture+" "+handle, answer.Outcome); err != nil {
		return err
	}
	by := describeAuthor(answer.Receipt.PausedBy, answer.Receipt.OperatorID)
	switch {
	case pausing && answer.Receipt.Changed:
		fmt.Fprintf(stdout, "paused %s (by %s at %s)\n", handle, by, answer.Receipt.PausedAt)
		if answer.Receipt.StopRunning {
			fmt.Fprintln(stdout, "  The turn it is on ends at its next round and is not run again; "+
				"a coding run it has out is ended now, its box reclaimed and what it spent charged.")
			fmt.Fprintln(stdout, "  Its coding runs change nothing until it is resumed: their pushes "+
				"and their calls of its tools that write are refused.")
		} else {
			fmt.Fprintln(stdout, "  The turn it is on, if any, finishes first; nothing new starts.")
		}
	case pausing:
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
