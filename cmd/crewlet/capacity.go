package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// `crewlet retention set-capacity`, `maintenance`, `reanchor` and
// `verify --restore`.
//
// # What this file states, and what it deliberately does not
//
// The verb, its arguments, the modes it runs in, the refusals an operator will
// see, and what it costs. It states NO transition, NO evidence rule, NO
// ordering and NO count of steps — those are `internal/statelog`'s
// [statelog.PermitPhase] and the table around it, and a CLI help string that
// restated one would be a second state machine.
//
// That is a rule rather than a preference: this procedure was written four
// times in prose beside the mechanism, and each time a later change amended
// the mechanism and left the prose. The copies then disagreed about which
// nodes had to acknowledge, when the baseline was recorded, and whether an
// applied operation could be confirmed without a seal — and the prose is what
// an implementer follows.

// capacityUsage is the synopsis, and it is also where an operator learns the
// shape of the thing: three fleet-wide restarts, and this verb run inside two
// of them.
const capacityUsage = `usage: crewlet retention set-capacity <stream> <bytes> -confirm <bytes>

  A stream's byte ceiling cannot change while anything is publishing, so this
  runs with the whole fleet in a maintenance mode. It costs THREE fleet-wide
  restarts on a first attempt, and two more per retry:

    1. restart every node with  crewlet run -mode maintenance
    2. crewlet retention set-capacity <stream> <bytes> -confirm <bytes>
    3. restart every node with  crewlet run -mode seal
    4. crewlet retention set-capacity <stream> <bytes> -confirm <bytes>
    5. once every log's window is closed, restart every node with no -mode

  crewlet retention maintenance status -stream <stream>  says where it stands
  and what is holding it. The procedure itself is documented once, in
  docs/guides/retention.md.`

// retentionSetCapacity is `crewlet retention set-capacity`.
func retentionSetCapacity(environ config.Source, args []string, stdout, stderr io.Writer) error {
	stream, rest := splitSubject(args)
	target, rest := splitSubject(rest)
	var confirm *string
	var assert *bool
	client, err := nodeClientFor(environ, rest, "retention set-capacity", stderr,
		func(fs *flag.FlagSet) {
			confirm = fs.String("confirm", "",
				"repeat the byte count: this restarts the whole fleet three times")
			assert = fs.Bool("i-have-excluded-all-publishers", false,
				"on an external NATS cluster only: your explicit assertion that "+
					"nothing else holds a connection to that broker, which the "+
					"engine cannot establish because it does not run it")
		})
	if err != nil {
		return err
	}
	bytes, parseErr := strconv.ParseUint(target, 10, 64)
	if stream == "" || parseErr != nil || bytes == 0 || *confirm != target {
		fmt.Fprintln(stderr, capacityUsage)
		return errors.New("name the stream and the byte ceiling, and repeat the " +
			"ceiling in -confirm: a target is chosen once for the life of an " +
			"operation and never changed")
	}

	path := fmt.Sprintf("/worklet/retention/capacity?stream=%s&bytes=%d&confirm=%d",
		url.QueryEscape(stream), bytes, bytes)
	if *assert {
		path += "&assert_excluded=true"
	}
	var answer struct {
		Mode      string             `json:"mode"`
		Operation *capacityOperation `json:"operation"`
		windowsLeft
	}
	if err := client.post(context.Background(), path, &answer); err != nil {
		return err
	}
	if answer.Operation == nil {
		fmt.Fprintln(stdout, "no capacity operation is open")
		return nil
	}
	printOperation(stdout, answer.Mode, *answer.Operation, pageVerb, answer.windowsLeft)
	return nil
}

// windowsLeft is what a capacity gesture's answer names as still standing
// between the fleet and service: every window open on any log, and every log
// whose window the node couldn't read.
type windowsLeft struct {
	Open   []windowLeft `json:"open_windows"`
	Unread []string     `json:"unread_windows"`
}

// windowLeft is one window still open, with what names the command that
// finishes it.
type windowLeft struct {
	Stream         string `json:"stream"`
	OperationID    string `json:"operation_id"`
	Phase          string `json:"phase"`
	TargetMaxBytes uint64 `json:"target_max_bytes"`
}

// returnToService is the step after a window closed, as the sentence that
// follows the one saying so.
//
// THE FLEET GOES BACK TO SERVICE ONLY ONCE NO LOG HAS A WINDOW OPEN, and only
// once the node could read whether one has: a node won't start in service
// otherwise. Said whenever a window closed, it sent an operator resizing two
// logs into a fleet-wide restart that every node refused, with the second
// window still open.
func returnToService(left windowsLeft) string {
	if len(left.Open) == 0 && len(left.Unread) == 0 {
		return "Restart every node with no -mode to return the fleet to service."
	}
	var b strings.Builder
	b.WriteString("Don't restart in service yet: a node won't start while a log " +
		"has a window open.\n")
	for _, w := range left.Open {
		fmt.Fprintf(&b, "  %s's window is still open, at %s. Finish it with "+
			"`crewlet retention set-capacity %s %d -confirm %d`.\n",
			w.Stream, w.Phase, w.Stream, w.TargetMaxBytes, w.TargetMaxBytes)
	}
	for _, stream := range left.Unread {
		fmt.Fprintf(&b, "  This node couldn't read whether %s has a window open. "+
			"Check with `crewlet retention maintenance status -stream %s`.\n",
			stream, stream)
	}
	b.WriteString("Once every window is closed, restart every node with no -mode.")
	return b.String()
}

// capacityOperation is the operation as this command reads it.
//
// DECODED INTO A SHAPE OF THIS COMMAND'S OWN, so an older binary does not
// refuse a newer node's answer over a field it does not print.
type capacityOperation struct {
	Stream            string            `json:"stream"`
	OperationID       string            `json:"operation_id"`
	TargetMaxBytes    uint64            `json:"target_max_bytes"`
	OriginalMaxBytes  uint64            `json:"original_max_bytes"`
	ObservedMaxBytes  uint64            `json:"observed_max_bytes"`
	Phase             string            `json:"phase"`
	Attempt           int               `json:"attempt"`
	Participants      []string          `json:"participants"`
	Excluded          []string          `json:"excluded"`
	WriteIncarnations map[string]string `json:"write_incarnations"`
	Journal           []struct {
		Attempt    int       `json:"attempt"`
		By         string    `json:"by"`
		State      string    `json:"state"`
		At         time.Time `json:"at"`
		ResolvedAt time.Time `json:"resolved_at"`
		Evidence   string    `json:"evidence"`
	} `json:"journal"`
	Blocked    string    `json:"blocked"`
	EnteredAt  time.Time `json:"entered_at"`
	By         string    `json:"by"`
	OperatorID string    `json:"operator_id"`

	// ExcludedBy and AbandonedBy are who made those two gestures, each
	// an author and the credential they made it through.
	ExcludedBy  map[string]capacityParty `json:"excluded_by"`
	AbandonedBy capacityParty            `json:"abandoned_by"`
}

// capacityParty is who made one gesture on a window.
type capacityParty struct {
	By         string `json:"by"`
	OperatorID string `json:"operator_id"`
}

// describe is the party in the words every trail this CLI prints uses.
//
// No stand-in for an empty party: every exclusion records its party in the
// update that excludes the node, and an abandonment is printed only once it
// names one.
func (p capacityParty) describe() string {
	return describeAuthor(p.By, p.OperatorID)
}

// capacityPage is which command prints an operation, which decides the lines
// that differ between them.
type capacityPage int

const (
	// pageVerb is set-capacity, whose answer at `confirmed` is a window its
	// own drive has just closed: an unreleased one comes back as a refusal.
	pageVerb capacityPage = iota
	// pageStatus is `maintenance status`, which already names what holds
	// the window and so never points at itself.
	pageStatus
	// pageGesture is an exclusion or an abandonment, which changes the
	// window and closes nothing.
	pageGesture
)

// printOperation renders one operation, and what an operator does next.
//
// THE NEXT STEP NAMES THE VERB IN FULL. The same lines follow set-capacity,
// `maintenance status`, `exclude` and `abandon`, so "run this verb again" sent
// a reader of the status page back to the page, and a reader of an exclusion
// back to the exclusion, when the next step after either is set-capacity.
//
// AND THE LAST STEP IS SAID TOO. A window set-capacity confirmed and closed
// printed its table and nothing after it, so the one gesture left, returning
// the fleet to service, was in no answer the operator was given. That step is
// the fleet's rather than the window's, so it is said from what the answer
// names as still open ([returnToService]). A confirmed window any other page
// shows is still open, so there the next step is the verb that closes it.
func printOperation(stdout io.Writer, mode string, op capacityOperation, page capacityPage,
	left windowsLeft) {

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "OPERATION\t%s\ton %s\n", op.OperationID, op.Stream)
	fmt.Fprintf(w, "PHASE\t%s\tattempt %d\n", op.Phase, op.Attempt)
	fmt.Fprintf(w, "TARGET\t%s\twas %s\n",
		humanBytes(int64(op.TargetMaxBytes)), humanBytes(int64(op.OriginalMaxBytes)))
	if op.ObservedMaxBytes > 0 {
		fmt.Fprintf(w, "OBSERVED\t%s\t\n", humanBytes(int64(op.ObservedMaxBytes)))
	}
	fmt.Fprintf(w, "OPENED\t%s\tby %s\n", stampOrDash(op.EnteredAt),
		describeAuthor(op.By, op.OperatorID))
	fmt.Fprintf(w, "THIS NODE\tmode %s\t\n", mode)
	if op.Blocked != "" {
		fmt.Fprintf(w, "BLOCKED\t%s\t\n", op.Blocked)
	}
	if op.AbandonedBy.By != "" {
		fmt.Fprintf(w, "ABANDONED\tby %s\t\n", op.AbandonedBy.describe())
	}
	_ = w.Flush()

	// WHAT TO DO NEXT, derived from the phase and NOT a restatement of
	// the transitions: an operator needs the next gesture, and a list of
	// evidence rules is the thing that forks.
	fmt.Fprintln(stdout)
	verb := fmt.Sprintf("`crewlet retention set-capacity %s %d -confirm %d`",
		op.Stream, op.TargetMaxBytes, op.TargetMaxBytes)
	status := ""
	if page != pageStatus {
		status = fmt.Sprintf(" `crewlet retention maintenance status -stream %s` "+
			"names anything that stops it.", op.Stream)
	}
	switch op.Phase {
	case "opened":
		// NOTHING HAS BEEN SENT FOR THIS ATTEMPT, so the next step is the
		// baseline, which only maintenance mode takes: a seal restart from
		// here changes nothing and costs the fleet a restart. A LATER
		// ATTEMPT is back here because the last one's request left the
		// ceiling somewhere other than the target, so "nothing has been
		// sent" is true of this attempt only.
		switch {
		case op.Attempt <= 1:
			fmt.Fprintln(stdout, "Nothing has been sent yet.")
		case op.ObservedMaxBytes > 0:
			fmt.Fprintf(stdout, "This attempt hasn't sent anything yet. The last "+
				"one left the ceiling at %s, not the target.\n",
				humanBytes(int64(op.ObservedMaxBytes)))
		default:
			fmt.Fprintln(stdout, "This attempt hasn't sent anything yet. The last "+
				"one didn't leave the ceiling at the target.")
		}
		fmt.Fprintf(stdout, "Next: with every node started with `crewlet run "+
			"-mode maintenance`, run %s again.%s\n", verb, status)
	case "baselined":
		fmt.Fprintf(stdout, "Next: restart every node with `crewlet run -mode "+
			"seal`, then run %s again.\n", verb)
	case "applied":
		fmt.Fprintf(stdout, "The ceiling is applied and NOT yet sealed: another "+
			"request may still be in flight.\n"+
			"Next: restart every node with `crewlet run -mode seal`, then run "+
			"%s again.\n", verb)
	case "sealing":
		if page == pageStatus {
			fmt.Fprintln(stdout, "Waiting for the barrier.")
		} else {
			fmt.Fprintf(stdout, "Waiting for the barrier. `crewlet retention "+
				"maintenance status -stream %s` names who it's still waiting on.\n",
				op.Stream)
		}
	case "sealed":
		fmt.Fprintf(stdout, "Sealed. Run %s again to verify and confirm.\n", verb)
	case "confirmed":
		// AN ABANDONED WINDOW IS CONFIRMED AT THE CEILING ITS SEAL FOUND,
		// which it no longer compares with the target, so saying it was
		// verified at the target would claim a ceiling the log may not have.
		abandoned := op.Blocked == statelog.BlockedAbandoned
		switch {
		case page == pageVerb && abandoned:
			fmt.Fprintf(stdout, "The window was abandoned and is closed, with the "+
				"ceiling at %s. %s\n", humanBytes(int64(op.ObservedMaxBytes)),
				returnToService(left))
		case page == pageVerb:
			fmt.Fprintln(stdout, "The ceiling is verified at the target, and the "+
				"window is closed. "+returnToService(left))
		case abandoned:
			fmt.Fprintf(stdout, "The window was abandoned, and its seal found the "+
				"ceiling at %s. Run %s again to close it there.\n",
				humanBytes(int64(op.ObservedMaxBytes)), verb)
		default:
			fmt.Fprintf(stdout, "The ceiling is verified at the target, and the "+
				"window is still open. Run %s again to close it.\n", verb)
		}
	}
}

// maintenanceUsage is `crewlet retention maintenance`'s synopsis: the three
// verbs, their arguments and what each one is your word for.
const maintenanceUsage = `usage: crewlet retention maintenance status|abandon|exclude -stream <name>

  status   where the window stands and what is holding it
  exclude  -node <node-id> -confirm <node-id>
           your word that the node's process is stopped and holds no
           outstanding request. It waives the node's acknowledgement and
           withdraws its admission, so a node that will never boot again stops
           holding the window.
  abandon  -confirm <operation-id>
           stop trying to reach the window's target. From opened it clears at
           once; from anywhere else the fleet still restarts with -mode seal.`

// retentionMaintenance is `crewlet retention maintenance`.
func retentionMaintenance(environ config.Source, args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "status":
		return maintenanceStatus(environ, rest, stdout, stderr)
	case "abandon":
		return maintenanceAbandon(environ, rest, stdout, stderr)
	case "exclude":
		return maintenanceExclude(environ, rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, maintenanceUsage)
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown maintenance command %q", sub)
	}
}

// maintenanceStatus prints the durable state, and what is holding it.
//
// It is the ONE place an operator can see why a fleet is still excluded: the
// operation, its phase and attempt, the process each participant was running
// as the newest request left against the one it acknowledged as, which
// acknowledgements are missing, and any admission stopping the window's first
// request.
func maintenanceStatus(environ config.Source, args []string, stdout, stderr io.Writer) error {
	var stream *string
	client, err := nodeClientFor(environ, args, "retention maintenance status", stderr,
		func(fs *flag.FlagSet) {
			stream = fs.String("stream", "", "which log; required")
		})
	if err != nil {
		return err
	}
	if strings.TrimSpace(*stream) == "" {
		fmt.Fprintln(stderr,
			"usage: crewlet retention maintenance status -stream <name>")
		return errors.New("name the log")
	}
	var answer struct {
		Stream             string   `json:"stream"`
		Open               bool     `json:"open"`
		Mode               string   `json:"mode"`
		Sealed             bool     `json:"sealed"`
		Blocking           string   `json:"blocking"`
		RestartsMissing    []string `json:"restarts_missing"`
		AdmissionsBlocking []string `json:"admissions_blocking"`
		// AcksUnread and AdmissionsUnread say the node read the window
		// and couldn't read that register beside it: what is formed from
		// it is absent, and this page says so rather than printing it as
		// empty.
		AcksUnread       bool               `json:"acks_unread"`
		AdmissionsUnread bool               `json:"admissions_unread"`
		Operation        *capacityOperation `json:"operation"`
		Acks             []struct {
			NodeID      string `json:"node_id"`
			OperationID string `json:"operation_id"`
			Attempt     int    `json:"attempt"`
			Incarnation string `json:"incarnation"`
			Mode        string `json:"mode"`
		} `json:"acks"`
		Admissions []struct {
			NodeID      string `json:"node_id"`
			Incarnation string `json:"incarnation"`
		} `json:"admissions"`
	}
	if err := client.get(context.Background(),
		"/worklet/retention/maintenance?stream="+url.QueryEscape(*stream), &answer); err != nil {
		// A WINDOW THE NODE COULDN'T READ IS ITS ANSWER ABOUT THE WINDOW, a
		// coordination read every node shares, and never a node that can't
		// serve this page: framed as the second, it sent an operator to find
		// another node, which reads the same register.
		var refused *nodeRefusal
		if errors.As(err, &refused) &&
			refused.Code == string(httpjson.CodeMaintenanceUnreadable) {
			return refused.asJudgement()
		}
		return err
	}
	if !answer.Open {
		fmt.Fprintf(stdout, "No capacity window is open on %s. "+
			"This node's mode is %s.\n", answer.Stream, answer.Mode)
		if answer.AdmissionsUnread {
			fmt.Fprintln(stdout, "It couldn't read the admissions just now, so it "+
				"can't say whether a node would keep a window from opening. Ask "+
				"again in a moment.")
			return nil
		}
		if len(answer.Admissions) == 0 {
			return nil
		}
		if answer.Mode == string(statelog.ModeNormal) || answer.Mode == "" {
			fmt.Fprintf(stdout, "%d node(s) hold an admission, which is normal "+
				"for a fleet in service.\n", len(answer.Admissions))
			return nil
		}
		// A FLEET ON ITS WAY INTO A WINDOW SHOULD HOLD NONE: every boot into
		// a maintenance mode withdraws the admission its node's last process
		// left once that process isn't running. So each one here is a node
		// still running in service, or one that stopped without giving it
		// back and hasn't been restarted into maintenance mode since: the
		// first keeps a window from opening at all, and the second keeps an
		// open one from sending anything. Said before the window rather than
		// after it.
		held := make([]string, 0, len(answer.Admissions))
		for _, a := range answer.Admissions {
			held = append(held, a.NodeID)
		}
		if len(held) == 1 {
			fmt.Fprintf(stdout, "%s holds an admission: it's still running in "+
				"service, or it stopped without giving it back and hasn't been "+
				"restarted with `-mode maintenance` since. Restart it that way "+
				"before opening a window: while it runs, a window won't open, and "+
				"if it never comes back, exclude it once one is open.\n",
				words.And(held))
			return nil
		}
		fmt.Fprintf(stdout, "%s hold an admission: each is still running in "+
			"service, or stopped without giving it back and hasn't been "+
			"restarted with `-mode maintenance` since. Restart each that way "+
			"before opening a window: while one runs, a window won't open, and "+
			"one that never comes back is excluded once one is open.\n",
			words.And(held))
		return nil
	}
	printOperation(stdout, answer.Mode, *answer.Operation, pageStatus, windowsLeft{})

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	// RUNNING AT REQUEST is the process the window recorded for each node as
	// its newest request left, which a seal acknowledgement has to differ
	// from: recorded again with every request, it is not the baseline's.
	fmt.Fprintln(w, "\nPARTICIPANT\tRUNNING AT REQUEST\tACKNOWLEDGED AS\tATTEMPT\tMODE")
	acked := map[string]int{}
	for i, ack := range answer.Acks {
		acked[ack.NodeID] = i
	}
	// AN ACKNOWLEDGEMENT NOBODY COULD READ IS NOT A MISSING ONE: printed as a
	// dash beside every participant, it read as no node having restarted.
	noAck := "—"
	if answer.AcksUnread {
		noAck = "couldn't read"
	}
	for _, node := range answer.Operation.Participants {
		baseline := dashIfEmpty(answer.Operation.WriteIncarnations[node])
		if containsString(answer.Operation.Excluded, node) {
			fmt.Fprintf(w, "%s\t%s\texcluded by %s\t\t\n", node, baseline,
				answer.Operation.ExcludedBy[node].describe())
			continue
		}
		i, ok := acked[node]
		if !ok {
			fmt.Fprintf(w, "%s\t%s\t%s\t\t\n", node, baseline, noAck)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", node, baseline,
			answer.Acks[i].Incarnation,
			ackAttempt(answer.Acks[i].OperationID, answer.Acks[i].Attempt,
				answer.Operation.OperationID),
			answer.Acks[i].Mode)
	}
	// ONE ROW PER REQUEST, with when it was sent: an attempt sends one
	// request per run of the verb that sends one, a resumed window's
	// included, so a row named by its attempt alone read like the row
	// beside it.
	if len(answer.Operation.Journal) > 0 {
		fmt.Fprintln(w, "\nREQUEST\tSENT\tSTATE\tBY\tEVIDENCE")
		for _, record := range answer.Operation.Journal {
			fmt.Fprintf(w, "attempt %d\t%s\t%s\t%s\t%s\n",
				record.Attempt, stampOrDash(record.At), record.State, record.By,
				dashIfEmpty(record.Evidence))
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(stdout)
	// WHAT HOLDS THE WINDOW IS THE NEXT STEP'S, and the node answers each
	// phase's own list: at opened the baseline's (a restart into a
	// maintenance mode from every participant, and no admission), past it the
	// seal's. The seal's list at opened named every node restarted before
	// the window as owing a seal restart nobody had asked for yet.
	switch {
	case answer.Sealed:
		fmt.Fprintln(stdout, "The barrier holds.")
	case answer.Blocking != "":
		fmt.Fprintf(stdout, "Not sealed: %s\n", answer.Blocking)
	}
	// WHAT THE NODE COULDN'T READ BESIDE THE WINDOW is said as that, so the
	// page never reads as a window waiting on nobody.
	if answer.AcksUnread {
		fmt.Fprintln(stdout, "This node couldn't read which nodes have restarted "+
			"for the window just now, so it can't say what the window is waiting "+
			"on. Ask again in a moment.")
	}
	if answer.AdmissionsUnread && answer.Operation.Phase == "opened" {
		fmt.Fprintln(stdout, "This node couldn't read the admissions just now, so "+
			"it can't say whether one is holding the window. Ask again in a moment.")
	}
	if missing := answer.RestartsMissing; len(missing) > 0 {
		exclude := "<node-id>"
		if len(missing) == 1 {
			exclude = missing[0]
		}
		fmt.Fprintf(stdout, "No restart into a maintenance mode is recorded for "+
			"%s, so the baseline can't run yet. Restart %s with `-mode "+
			"maintenance`, or if %s gone for good, exclude %s with `crewlet "+
			"retention maintenance exclude -stream %s -node %s -confirm %s`.\n",
			words.And(missing), itOrEach(missing), oneIsOrItIs(missing),
			itOrIt(missing), answer.Stream, exclude, exclude)
	}
	if held := answer.AdmissionsBlocking; len(held) > 0 {
		// AN `admitted` KEY IS A NODE THAT MAY BE PUBLISHING, which is
		// the one thing that stops the window's first request and the one
		// an operator cannot deduce from anything else on this page.
		exclude := "<node-id>"
		if len(held) == 1 {
			exclude = held[0]
		}
		fmt.Fprintf(stdout, "%s, so nothing is sent while %s. Restart %s with "+
			"`-mode maintenance`, or if %s gone for good, exclude %s with "+
			"`crewlet retention maintenance exclude -stream %s -node %s "+
			"-confirm %s`.\n",
			holdsAnAdmission(held), itStandsOrOneStands(held), itOrEach(held),
			oneIsOrItIs(held), itOrIt(held), answer.Stream, exclude, exclude)
	}
	return nil
}

// itOrEach and the words beside it say a set of nodes in the number it has,
// so a page about one node never says "these nodes" of it.
func itOrEach(nodes []string) string {
	if len(nodes) == 1 {
		return "it"
	}
	return "each"
}

func itOrIt(nodes []string) string {
	if len(nodes) == 1 {
		return "it"
	}
	return "that one"
}

func oneIsOrItIs(nodes []string) string {
	if len(nodes) == 1 {
		return "it's"
	}
	return "one is"
}

func holdsAnAdmission(nodes []string) string {
	if len(nodes) == 1 {
		return words.And(nodes) + " holds an admission"
	}
	return words.And(nodes) + " hold an admission"
}

func itStandsOrOneStands(nodes []string) string {
	if len(nodes) == 1 {
		return "it stands"
	}
	return "one stands"
}

// ackAttempt is the ATTEMPT column of one acknowledgement against the window
// on the page.
//
// A NUMBER ONLY FOR THIS WINDOW'S OWN. A node that restarted while no window
// was open records its restart bound to none, which the baseline reads and the
// seal never takes, and one bound to an earlier window carries that window's
// attempt: printed as numbers, the first read as attempt 0 and the second as
// an attempt of this window it never was.
func ackAttempt(ackOperation string, attempt int, window string) string {
	switch ackOperation {
	case "":
		return "no window"
	case window:
		return strconv.Itoa(attempt)
	}
	return "another window"
}

// maintenanceAbandon changes what the operation is trying to reach.
//
// IT NEVER CHANGES THE BARRIER IT MUST CROSS: from anywhere past `opened` it
// enters the seal, because a paused coordinator's request is outstanding
// whether or not a person has read a status page.
func maintenanceAbandon(environ config.Source, args []string, stdout, stderr io.Writer) error {
	var stream, confirm *string
	client, err := nodeClientFor(environ, args, "retention maintenance abandon", stderr,
		func(fs *flag.FlagSet) {
			stream = fs.String("stream", "", "which log; required")
			confirm = fs.String("confirm", "",
				"repeat the operation id, from `maintenance status`")
		})
	if err != nil {
		return err
	}
	if strings.TrimSpace(*stream) == "" || strings.TrimSpace(*confirm) == "" {
		fmt.Fprintln(stderr, "usage: crewlet retention maintenance abandon "+
			"-stream <name> -confirm <operation-id>")
		return errors.New("name the log and repeat the operation id")
	}
	var answer struct {
		Operation *capacityOperation `json:"operation"`
		windowsLeft
	}
	// THE CONFIRMATION TRAVELS WITH THE REQUEST, and the node refuses one
	// naming another operation before it abandons anything. Compared here
	// after the answer, a mismatch was reported for an abandonment that had
	// already run, and an abandonment that cleared the window was never
	// compared at all.
	if err := client.post(context.Background(),
		"/worklet/retention/maintenance/abandon?stream="+url.QueryEscape(*stream)+
			"&confirm="+url.QueryEscape(*confirm),
		&answer); err != nil {
		return err
	}
	// NO OPERATION IS A WINDOW CLEARED OUTRIGHT, which an abandonment from
	// `opened` is. The node answered the window it had cleared, so this
	// branch never ran and the operator was told to run set-capacity again
	// for the window they had just abandoned.
	if answer.Operation == nil {
		fmt.Fprintf(stdout, "The window on %s is closed, and nothing was sent. %s\n",
			*stream, returnToService(answer.windowsLeft))
		return nil
	}
	printOperation(stdout, "", *answer.Operation, pageGesture, windowsLeft{})
	return nil
}

// maintenanceExclude records an operator's assertion that a participant is
// stopped and holds no outstanding request.
//
// THE ONLY THING THAT WAIVES AN ACKNOWLEDGEMENT. An eviction does not: that is
// about whose records apply, and this is about whose process is running.
func maintenanceExclude(environ config.Source, args []string, stdout, stderr io.Writer) error {
	var stream, node, confirm *string
	client, err := nodeClientFor(environ, args, "retention maintenance exclude", stderr,
		func(fs *flag.FlagSet) {
			stream = fs.String("stream", "", "which log; required")
			node = fs.String("node", "", "which node; required")
			confirm = fs.String("confirm", "", "repeat the node id")
		})
	if err != nil {
		return err
	}
	if strings.TrimSpace(*stream) == "" || strings.TrimSpace(*node) == "" ||
		*confirm != *node {
		fmt.Fprintln(stderr, "usage: crewlet retention maintenance exclude "+
			"-stream <name> -node <node-id> -confirm <node-id>")
		return errors.New("excluding a participant asserts that its process is " +
			"stopped and holds no outstanding request. Repeat the node id")
	}
	var answer struct {
		Operation *capacityOperation `json:"operation"`
	}
	path := fmt.Sprintf("/worklet/retention/maintenance/exclude?stream=%s&node=%s&confirm=%s",
		url.QueryEscape(*stream), url.QueryEscape(*node), url.QueryEscape(*node))
	if err := client.post(context.Background(), path, &answer); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s is excluded: it no longer has to acknowledge, and "+
		"an admission it holds no longer stops the window.\n", *node)
	if answer.Operation != nil {
		printOperation(stdout, "", *answer.Operation, pageGesture, windowsLeft{})
	}
	return nil
}

// reanchorRequestTimeout is how long `crewlet retention reanchor` waits for the
// node's answer to the transition itself.
//
// SIX MINUTES. Once its generation record is appended, a reanchor finishes on
// the node's own budget — the broker's bring-up budget, five minutes on a
// clustered one — because it rebuilds this node's consumer on the log, a delete
// and a create of a replicated object each of which is a raft round trip there;
// the minute on top covers what it reads before the append: the stream, the
// positions register and the log's tail. At the ten seconds every other call
// gets, a transition on a fleet whose metadata group was slow was reported to
// the operator as failed while the node went on to finish it — and the node
// ran it on the request's own context, so the cancellation stopped it halfway,
// with its generation open on the log and nothing committed.
const reanchorRequestTimeout = 6 * time.Minute

// retentionReanchor is `crewlet retention reanchor`.
func retentionReanchor(environ config.Source, args []string, stdout, stderr io.Writer) error {
	var stream, confirm *string
	var force, discard *bool
	client, err := nodeClientFor(environ, args, "retention reanchor", stderr,
		func(fs *flag.FlagSet) {
			stream = fs.String("stream", "", "which log was recreated; required")
			confirm = fs.String("confirm", "",
				"the live stream's own created_at, which this verb prints when "+
					"it is omitted")
			force = fs.Bool("force", false,
				"re-anchor although this node may not be the most caught-up "+
					"on the stream its rows came from, or the positions "+
					"register cannot be read. It never overrides a peer that "+
					"has already re-anchored the stream: that peer's rows are "+
					"the fleet's history in the new generation, and this node "+
					"adopts its snapshot instead")
			discard = fs.Bool("discard", false,
				"re-anchor a RESTORED log although it holds records this "+
					"node's rows do not (written after the restore, by a node "+
					"whose rows were the copy's age), accepting that they are "+
					"applied on no node. Without it the verb refuses and names "+
					"the newest of them")
		})
	if err != nil {
		return err
	}
	if strings.TrimSpace(*stream) == "" {
		fmt.Fprintln(stderr, "usage: crewlet retention reanchor -stream <name> "+
			"-confirm <the live stream's created_at>")
		return errors.New("name the log")
	}
	return reanchor(context.Background(), client, reanchorAsk{
		stream: *stream, confirm: *confirm, force: *force, discard: *discard,
	}, stdout)
}

// reanchorAsk is what an operator asked `crewlet retention reanchor` for.
type reanchorAsk struct {
	stream, confirm string
	force, discard  bool
}

// reanchor asks client's node about the log ask names, and — once the operator
// has confirmed its created_at — for the transition itself, waiting
// [reanchorRequestTimeout] for that answer rather than the client's own
// timeout. The client is a parameter so the suite can prove that wait against
// an ordinary timeout it can afford to outlast, rather than sitting out the
// real ten seconds.
func reanchor(ctx context.Context, client *nodeClient, ask reanchorAsk, stdout io.Writer) error {
	var status struct {
		Stream     string                `json:"stream"`
		CreatedAt  time.Time             `json:"created_at"`
		Generation uint32                `json:"generation"`
		Case       statelog.ReanchorCase `json:"case"`
		Cursor     uint64                `json:"cursor"`
		Nothing    string                `json:"nothing_to_reanchor"`
		Discards   *statelog.TailRecord  `json:"discards"`
		Discarding string                `json:"discarding"`
	}
	if err := client.get(ctx,
		"/worklet/retention/reanchor?stream="+url.QueryEscape(ask.stream), &status); err != nil {
		return err
	}
	if strings.TrimSpace(ask.confirm) == "" {
		// PRINTED RATHER THAN ACCEPTED. The confirmation says "I looked
		// at the thing I am re-anchoring", and a verb that read the
		// value and fed it straight back would be confirming against
		// its own output.
		//
		// IN THE ONE SPELLING THE NODE CHECKS ([statelog.ConfirmationOf]):
		// this printed nanoseconds while the check compared whole
		// seconds, so pasting back what it printed was refused.
		instant := statelog.ConfirmationOf(status.CreatedAt)
		fmt.Fprintf(stdout, "%s is at generation %d and was created at %s.\n\n",
			status.Stream, status.Generation, instant)
		if !status.Case.Valid() {
			// NOTHING TO CONFIRM: the node would refuse the transition, and
			// printing a command it would refuse invites running it.
			fmt.Fprintf(stdout, "There is nothing to re-anchor: %s\n", status.Nothing)
			return errors.New("nothing to re-anchor")
		}
		flags := ""
		if status.Discards != nil {
			// WHAT IT WOULD DISCARD, before the command that would: the
			// operator's word is the only thing that lets it, and a command
			// printed without it would be refused.
			fmt.Fprintf(stdout, "%s\n\n", status.Discarding)
			flags = " -discard"
		}
		fmt.Fprintf(stdout, "%s\n\nRe-run with:\n  crewlet retention reanchor "+
			"-stream %s -confirm %s%s\n", reanchorCaseText(status.Case, status.Cursor),
			status.Stream, instant, flags)
		return errors.New("confirm the stream's own created_at")
	}

	path := fmt.Sprintf("/worklet/retention/reanchor?stream=%s&confirm=%s",
		url.QueryEscape(ask.stream), url.QueryEscape(ask.confirm))
	if ask.force {
		path += "&force=true"
	}
	if ask.discard {
		path += "&discard=true"
	}
	var answer struct {
		Stream     string                `json:"stream"`
		Generation uint32                `json:"generation"`
		Case       statelog.ReanchorCase `json:"case"`
		Cursor     uint64                `json:"cursor"`
		Discarded  *statelog.TailRecord  `json:"discarded"`
	}
	if err := client.patiently(reanchorRequestTimeout).post(ctx,
		path, &answer); err != nil {
		return err
	}
	from := fmt.Sprintf("from its first surviving record, after sequence %d",
		answer.Cursor)
	switch answer.Case {
	case statelog.ReanchorRestored:
		from = fmt.Sprintf("from its end, after sequence %d, replaying none of "+
			"the records the restored copy kept", answer.Cursor)
	case statelog.ReanchorAbandoned:
		from = fmt.Sprintf("from this node's own checkpoint, after sequence %d, "+
			"with every record of the generation the evicted node abandoned void",
			answer.Cursor)
	}
	fmt.Fprintf(stdout, "%s is re-anchored at generation %d (%s): it is followed "+
		"%s. Every position below the generation is now comparable and safely "+
		"stale, and its applier has resumed on this node; no other log moved.\n",
		answer.Stream, answer.Generation, answer.Case, from)
	if answer.Discarded != nil {
		fmt.Fprintf(stdout, "The records written to it after the restore that this "+
			"node's rows did not hold are applied on no node, as you accepted; the "+
			"newest was %s.\n", *answer.Discarded)
	}
	return nil
}

// reanchorCaseText is what a reanchor of this case would do, in the words an
// operator confirms against.
//
// ONE PARAGRAPH PER CASE, because where the log is followed from is the whole
// difference between them and the one fact the operator has to agree with: a
// recreated log holds none of what the rows came from, a restored one holds a
// prefix the rows already have, and an abandoned one holds everything the rows
// are missing in a generation nobody left can vouch for.
func reanchorCaseText(c statelog.ReanchorCase, cursor uint64) string {
	common := "A reanchor moves THIS log alone to its NEXT generation, declaring " +
		"every position below it stale; its applier resumes on this node with no " +
		"restart, and no other log moves."
	if c == statelog.ReanchorAbandoned {
		return fmt.Sprintf("This log CONTINUES IN A GENERATION ONLY AN EVICTED NODE "+
			"HELD: it is the stream this node's rows are keyed to and it holds every "+
			"record they are missing, but that generation's history was on the "+
			"evicted node's disk and nowhere else. It is followed from THIS NODE'S "+
			"CHECKPOINT, after sequence %d, in the generation after the evicted "+
			"node's; every record written in the generation it abandoned is void "+
			"here. %s Every other node adopts a snapshot from this one.", cursor, common)
	}
	if c == statelog.ReanchorRestored {
		return fmt.Sprintf("This log was RESTORED from an older copy: it is the "+
			"stream this node's rows are keyed to, and past the copy it is not "+
			"their history: it ends below their checkpoint, or another record was "+
			"written at their checkpoint's sequence after the restore. The "+
			"rows already hold every record the copy kept, so it is followed from "+
			"its END, after sequence %d, and none of them is replayed. %s What the "+
			"rows hold past the copy is on no log, so every other node adopts a "+
			"snapshot from this one.", cursor, common)
	}
	return fmt.Sprintf("This log was RECREATED: it is not the stream this node's "+
		"rows are keyed to, so it is followed from its first surviving record, "+
		"after sequence %d. %s It does NOT recover records that were on the old "+
		"stream and were never applied here.", cursor, common)
}

// retentionVerify is `crewlet retention verify --restore`.
//
// # Why it is LOCAL where every other verb talks to a node
//
// It restores a backup ARTEFACT and opens the copy. Nothing about that needs a
// running engine — the point is precisely to establish that the artefact
// alone is enough — and running it through a node would be asking the thing
// under test to test itself.
//
// It writes nothing to the live store and takes no lock on it.
func retentionVerify(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("retention verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	restore := fs.Bool("restore", false,
		"restore the newest artefact into a temporary directory and open it")
	dir := fs.String("dir", "",
		"the backup root to verify; defaults to the newest artefact under it")
	cadence := fs.Duration("cadence", defaultRestoreCadence,
		"how stale a verified restore may be before this exits non-zero")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*restore {
		fmt.Fprintln(stderr, "usage: crewlet retention verify --restore "+
			"[-dir <backup root>] [-cadence 720h]")
		return errors.New("--restore is the only verification this verb does")
	}
	if strings.TrimSpace(*dir) == "" {
		return errors.New("name the backup root with -dir: this reads an " +
			"artefact from disk rather than asking a node, so there is no " +
			"configured path for it to fall back on")
	}
	return verifyRestore(*dir, *cadence, time.Now().UTC(), stdout)
}

// defaultRestoreCadence is how often a restore has to be proved.
//
// MONTHLY, and it is derived rather than chosen: the trim's own age floor is
// seven days, so a restore path broken for longer than one replay window means
// the log can no longer bridge the gap between an artefact and the present.
// Monthly gives four of those windows of margin.
const defaultRestoreCadence = 30 * 24 * time.Hour

// verifyRestore opens the newest artefact under root and reports what it found.
func verifyRestore(root string, cadence time.Duration, now time.Time,
	stdout io.Writer) error {

	newest, finished, err := newestArtefact(root)
	if err != nil {
		return err
	}
	if newest == "" {
		return fmt.Errorf("no backup with a manifest was found under %s: a "+
			"directory without one is the debris of a run that did not finish "+
			"rather than a partial backup", root)
	}
	manifest, err := readManifest(newest)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ARTEFACT\t%s\t\n", newest)
	fmt.Fprintf(w, "TAKEN\t%s\tby %s\n", stampOrDash(manifest.TakenAt), manifest.NodeID)
	fmt.Fprintf(w, "ENGINE\t%s\t\n", manifest.EngineVersion)
	for _, store := range manifest.Stores {
		fmt.Fprintf(w, "STORE %s\t%s\t%d migration(s)\n",
			store.Estate, humanBytes(store.Bytes), len(store.Migrations))
	}
	for stream, at := range manifest.Domains {
		fmt.Fprintf(w, "DOMAIN\t%s\tgeneration %d sequence %d\n",
			stream, at.Generation, at.Seq)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	age := now.Sub(finished)
	fmt.Fprintf(stdout, "\nThe newest artefact is %s old.\n", age.Round(time.Minute))
	if len(manifest.Domains) == 0 {
		return errors.New("the manifest names no domain position, so a restore " +
			"has no sequence to replay from, so this artefact cannot be verified " +
			"as restorable")
	}
	if age > cadence {
		// NON-ZERO PAST THE CADENCE, which is what turns a lapsed
		// restore test into a failing check somebody's cron notices
		// rather than a paragraph in a runbook nobody read.
		return fmt.Errorf("the newest verified restore is %s old and the cadence "+
			"is %s: a restore path broken for longer than one replay window "+
			"means the log can no longer bridge the gap",
			age.Round(time.Hour), cadence)
	}
	return nil
}

// restoreManifest is the manifest as this command reads it.
type restoreManifest struct {
	TakenAt       time.Time `json:"taken_at"`
	FinishedAt    time.Time `json:"finished_at"`
	NodeID        string    `json:"node_id"`
	EngineVersion string    `json:"engine_version"`
	Stores        []struct {
		Estate     string   `json:"estate"`
		Bytes      int64    `json:"bytes"`
		Migrations []string `json:"migrations"`
	} `json:"stores"`
	Domains map[string]struct {
		Stream     string `json:"stream"`
		Generation uint32 `json:"generation"`
		Seq        uint64 `json:"seq"`
	} `json:"domains"`
}

// newestArtefact is the most recently finished backup under root.
//
// THE MANIFEST IS THE CLAIM: a directory without one is debris, so it is not a
// candidate however new it is.
func newestArtefact(root string) (string, time.Time, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read the backup root %s: %w", root, err)
	}
	var newest string
	var finished time.Time
	candidates := []string{root}
	for _, entry := range entries {
		if entry.IsDir() {
			candidates = append(candidates, filepath.Join(root, entry.Name()))
		}
	}
	for _, dir := range candidates {
		manifest, err := readManifest(dir)
		if err != nil {
			continue
		}
		if newest == "" || manifest.FinishedAt.After(finished) {
			newest, finished = dir, manifest.FinishedAt
		}
	}
	return newest, finished, nil
}

// readManifest reads one artefact's manifest.
func readManifest(dir string) (restoreManifest, error) {
	body, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return restoreManifest{}, err
	}
	var manifest restoreManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return restoreManifest{}, fmt.Errorf("the manifest in %s does not "+
			"decode: %w", dir, err)
	}
	return manifest, nil
}

func containsString(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
