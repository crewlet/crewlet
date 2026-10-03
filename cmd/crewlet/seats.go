package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// `crewlet seats` — pausing and resuming a seat from a shell.
//
// # Through the act transport, as whoever the token is
//
// The same `pause_seat` and `resume_seat` the dashboard's buttons call, over
// the same route (`POST /operator/act/{tool}`), so a pause from a terminal and
// one from the profile screen are one gesture with one record. The credential
// is CREWLET_API_TOKEN and nothing else, and the node decides the gesture on
// the principal it resolves to — the seat's holder, whoever leads it, or a
// `fleet:operate` holder — and records it under that principal's own name, its
// kind and the credential, as every other write is recorded. Nobody is refused
// for being bound to no seat: a token with the authority acts under its own
// login.
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
	"[<config.yaml>] [-url URL] [-op-id ID]\n" +
	"       crewlet seats resume <handle> [<config.yaml>] [-url URL] [-op-id ID]\n" +
	"The credential is " + apiTokenEnv + "."

// seatsAct is one pause or resume.
func seatsAct(args []string, tool string, pausing bool, stdout, stderr io.Writer) error {
	gesture := strings.TrimSuffix(tool, "_seat")
	handle, rest := splitSubject(args)
	var stop *bool
	var reason, opID *string
	client, err := nodeClientFor(rest, "seats "+gesture, stderr,
		func(fs *flag.FlagSet) {
			if pausing {
				stop = fs.Bool("stop", false,
					"also end the turn the seat is on at its next round; it is not run again")
				reason = fs.String("reason", "", "why, in a line; shown on the seat and in the feed")
			}
			opID = fs.String("op-id", "",
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

	// THE OPERATION ID IS MINTED HERE, BEFORE THE REQUEST, as `work purge`
	// mints one and for its reason: a gesture whose answer never came may
	// well have landed, and the key it was sent under is the only handle on
	// it. One the operator brought is held to the rule the node holds it to
	// before anything is sent, so the refusal names the flag.
	operation := strings.TrimSpace(*opID)
	if operation == "" {
		operation = statelog.NewOpID(time.Now(), gesture)
	} else if err = statelog.CheckCallerOpID(operation); err != nil {
		return fmt.Errorf("-op-id: %w", err)
	}

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
	err = client.postKeyed(context.Background(),
		"/operator/act/"+url.PathEscape(tool), operation,
		map[string]any{"args": gestureArgs}, &answer)
	retry := fmt.Sprintf("run the same command again with -op-id %s, so the "+
		"retry is this operation rather than a second one", operation)
	var lost noAnswer
	if errors.As(err, &lost) {
		fmt.Fprintf(stderr, "The node did not answer, so whether the %s landed is "+
			"unknown: %s.\n", gesture, retry)
		return err
	}
	var refused *nodeRefusal
	if errors.As(err, &refused) && refused.Status == http.StatusServiceUnavailable &&
		(refused.Unsettled || refused.OpID != "") {
		if refused.Unvouched {
			return fmt.Errorf("%w\n\nThis node cannot tell whether it landed and "+
				"will answer the same way until it can: check the seat, or run it "+
				"through another node with -url <that node> -op-id %s — never a "+
				"fresh id", err, operation)
		}
		return fmt.Errorf("%w\n\nTo find out, %s", err, retry)
	}
	if err != nil {
		return err
	}

	by := describeAuthor(answer.Receipt.PausedBy, answer.Receipt.OperatorID)
	switch {
	case answer.Outcome == string(statelog.OutcomeUnknown):
		fmt.Fprintf(stdout, "%s %s: unknown — the node could not confirm the write. "+
			"To find out, %s\n", gesture, handle, retry)
	case pausing && answer.Receipt.Changed:
		fmt.Fprintf(stdout, "paused %s (by %s at %s)\n", handle, by, answer.Receipt.PausedAt)
		if answer.Receipt.StopRunning {
			fmt.Fprintln(stdout, "  The turn it is on ends at its next round and is not run again.")
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
