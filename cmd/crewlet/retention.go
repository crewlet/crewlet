package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// `crewlet retention` — what the log is holding, why, and the gestures that
// change it.
//
// # Why this is a GROUP and not a set of top-level verbs
//
// Two of the verbs stop a machine writing. A group is what keeps a destructive
// fleet gesture from sitting at the same level as `version`, and it is what
// makes the shape of the surface readable: everything under `retention`
// answers or changes one question, which is what the log may delete.
//
// # Why every verb talks to a NODE
//
// The same reason `backup` and `budgets` do. The register these verbs read and
// write is a coordination bucket on a broker embedded in the engine, which
// binds NO SOCKET on the default topology — so there is no address any other
// tool could be given, and the answer has to come from a process that already
// holds it.

// THE NINE VERBS, and seven of them are node-client commands for `backup`'s
// reason. TWO ARE NOT, and each is an exception with its own cause:
//
//   - `set-capacity` and `maintenance` need every node OUT of normal service,
//     and a node-client command needs one in it. They run against a node in a
//     maintenance mode, whose API is the mode's own control surface — see
//     capacity.go, and note that the procedure itself is stated once, in
//     internal/statelog.
//   - `verify --restore` reads a backup ARTEFACT off disk. The whole point is
//     that the artefact alone is enough, so asking a running node would be
//     asking the thing under test to test itself.

// runRetention dispatches the group.
func runRetention(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "status":
		return retentionStatus(rest, stdout, stderr)
	case "snapshots":
		return retentionSnapshots(rest, stdout, stderr)
	case "ack":
		return retentionAck(rest, stdout, stderr)
	case "evict":
		return retentionGate(rest, stdout, stderr, true)
	case "readmit":
		return retentionGate(rest, stdout, stderr, false)
	case "reanchor":
		return retentionReanchor(rest, stdout, stderr)
	case "verify":
		return retentionVerify(rest, stdout, stderr)
	case "set-capacity":
		return retentionSetCapacity(rest, stdout, stderr)
	case "maintenance":
		return retentionMaintenance(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, "usage: crewlet retention "+
			"status|snapshots|ack|evict|readmit|reanchor|verify|"+
			"set-capacity|maintenance [<config.yaml>] [-url] [-token]")
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown retention command %q", sub)
	}
}

// retentionReport is the answer's shape as this command reads it.
//
// DECODED INTO A SHAPE OF THIS COMMAND'S OWN rather than into
// [statelog.Report], for `crewlet backup`'s reason: the CLI reads a node's
// answer over HTTP, and a struct shared with the writer would make an older
// binary refuse a newer node's report over a field it does not print.
//
// WHICH IS WHY ITS TESTS SERVE THE WRITER'S OWN TYPE rather than JSON typed
// by hand. A shape of its own is a shape that can drift, and this one had: the
// node block's per-domain progress and each snapshot's positions are MAPS keyed
// by domain, the tombstone is `evicted`, and a readable term is `ok` — while
// this file declared lists, `eviction` and `known`, and its fixture spelled the
// same invention. So `status` and `snapshots` failed to decode the answer of
// every real node, whose own heartbeat puts a row in the node block at boot,
// and `evict`/`readmit` swallowed that failure and printed no watermark.
type retentionReport struct {
	NodeID           string    `json:"node_id"`
	At               time.Time `json:"at"`
	BackupOwner      string    `json:"backup_owner"`
	RegisterReadable bool      `json:"register_readable"`

	// ReadLevel is the level the node served this document at, and it is
	// PRINTED rather than merely decoded: the whole point of the field is
	// that a document which cannot claim an age looks identical to one
	// that can, and an operator reading the second as the first is the
	// silent downgrade the read-level contract exists to prevent.
	ReadLevel string `json:"read_level"`

	Domains   []retentionDomain   `json:"domains"`
	Nodes     []retentionNode     `json:"nodes"`
	Snapshots []retentionSnapshot `json:"snapshots"`
	Replica   retentionReplicaRow `json:"replica"`
	Alarms    []retentionAlarm    `json:"alarms"`

	// Maintenance is the capacity operation holding the fleet, absent
	// when there is none. A POINTER because absent and zeroed are
	// different answers: a zeroed block claims an operation in phase ""
	// with nobody outstanding.
	Maintenance *maintenanceRow `json:"maintenance"`
}

// maintenanceRow is one open capacity operation, as `retention status`
// renders it.
//
// NOT NAMED `retentionMaintenance`, which is the VERB `crewlet retention
// maintenance` in capacity.go — a type and a command with one name in one
// package is a compile error today and would have been a reader's error every
// day after.
type maintenanceRow struct {
	Stream              string    `json:"stream"`
	OperationID         string    `json:"operation_id"`
	Phase               string    `json:"phase"`
	Attempt             int       `json:"attempt"`
	TargetMaxBytes      uint64    `json:"target_max_bytes"`
	OriginalMaxBytes    uint64    `json:"original_max_bytes"`
	Since               time.Time `json:"since"`
	By                  string    `json:"by"`
	ParticipantsMissing []string  `json:"participants_missing"`
	Blocked             string    `json:"blocked"`
}

// retentionDomain is one registered domain's row.
type retentionDomain struct {
	Domain            string          `json:"domain"`
	Stream            string          `json:"stream"`
	Generation        uint32          `json:"generation"`
	Replay            string          `json:"replay"`
	FirstSeq          uint64          `json:"first_seq"`
	LastSeq           uint64          `json:"last_seq"`
	Bytes             uint64          `json:"bytes"`
	MaxBytes          uint64          `json:"max_bytes"`
	ReserveBytes      uint64          `json:"reserve_bytes"`
	HeadroomFraction  *float64        `json:"headroom_fraction"`
	TrimFloor         uint64          `json:"trim_floor"`
	TrimTo            uint64          `json:"trim_to"`
	BlockedBy         string          `json:"blocked_by"`
	BlockedSince      time.Time       `json:"blocked_since"`
	Prose             string          `json:"prose"`
	SnapshotBlockedBy string          `json:"snapshot_blocked_by"`
	Terms             []retentionTerm `json:"terms"`
}

// retentionTerm is one of the six, with the third value it can take — in the
// writer's vocabulary, [statelog.TermState].
type retentionTerm struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Seq    uint64 `json:"seq"`
	Detail string `json:"detail"`
	Remedy string `json:"remedy"`
}

// retentionNode is one row of the fleet.
type retentionNode struct {
	NodeID  string    `json:"node_id"`
	Counted bool      `json:"counted"`
	Live    bool      `json:"live"`
	At      time.Time `json:"at"`

	// Evicted is the node's tombstone, absent when it has none.
	Evicted *retentionEviction `json:"evicted"`

	// Domains is its progress KEYED BY DOMAIN, as the writer keys it; a
	// domain absent from the map is one the node has not reported on.
	Domains map[string]retentionNodeSeat `json:"domains"`
}

type retentionEviction struct {
	By          string    `json:"by"`
	At          time.Time `json:"at"`
	EffectiveAt time.Time `json:"effective_at"`
	Effective   bool      `json:"effective"`
}

type retentionNodeSeat struct {
	Generation     uint32 `json:"generation"`
	Seq            uint64 `json:"seq"`
	AppliedThrough uint64 `json:"applied_through"`

	// Lag is NIL when the stream could not be read, and printed as `-`:
	// an unknown lag rendered as zero is a node reported as caught up.
	Lag      *uint64 `json:"lag"`
	Deferred int     `json:"deferred"`
}

// retentionSnapshot is one node's artefact, or its absence with the reason.
type retentionSnapshot struct {
	NodeID string    `json:"node_id"`
	At     time.Time `json:"at"`
	Bytes  int64     `json:"bytes"`
	Skip   string    `json:"skip"`

	// Domains is the artefact's position KEYED BY DOMAIN.
	Domains map[string]uint64 `json:"domains"`
}

type retentionReplicaRow struct {
	StoreBytes           int64   `json:"store_bytes"`
	ProjectedJoinSeconds float64 `json:"projected_join_seconds"`
	RejoinWindowSeconds  float64 `json:"rejoin_window_seconds"`
}

type retentionAlarm struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
	Remedy string `json:"remedy"`
}

// retentionStatus prints what the log is holding and what is stopping it from
// shrinking.
//
// # The exit code
//
// NON-ZERO WHEN THIS NODE HAS AN ACTIVE ALARM, and the rule is the report's
// own — see [statelog.Report.ExitNonZero]. A second rule here would be a
// second definition of "is something wrong", and the cron watching this exit
// code would be watching the one nobody maintained.
func retentionStatus(args []string, stdout, stderr io.Writer) error {
	var domain *string
	client, err := nodeClientFor(args, "retention status", stderr, func(fs *flag.FlagSet) {
		domain = fs.String("domain", "",
			"limit the log and watermark blocks to one domain; empty prints every one")
	})
	if err != nil {
		return err
	}
	var report retentionReport
	if err := client.get(context.Background(), "/query/retention", &report); err != nil {
		return err
	}

	// MAINTENANCE ABOVE EVERYTHING, because while it is open every number
	// below it describes a fleet in which nothing is running: no seats, no
	// duties, no scheduler and no write routes. An operator reading a
	// blocked trim without knowing that goes looking for the wrong thing.
	if m := report.Maintenance; m != nil {
		fmt.Fprintf(stdout, "MAINTENANCE IS OPEN on %s — no publisher is running "+
			"anywhere in this fleet.\n", m.Stream)
		fmt.Fprintf(stdout, "  phase %s, attempt %d, open since %s",
			m.Phase, m.Attempt, m.Since.UTC().Format(time.RFC3339))
		if m.By != "" {
			fmt.Fprintf(stdout, ", run by %s", m.By)
		}
		fmt.Fprintln(stdout, ".")
		if m.Blocked != "" {
			fmt.Fprintf(stdout, "  BLOCKED: %s — this needs a person, not time.\n",
				m.Blocked)
		}
		if len(m.ParticipantsMissing) > 0 {
			fmt.Fprintf(stdout, "  waiting on %s.\n",
				strings.Join(m.ParticipantsMissing, ", "))
		} else {
			// NOBODY OUTSTANDING IS NOT PROGRESS: it is the
			// operation waiting on whoever ran the verb, and
			// printing nothing here reads as "nearly done".
			fmt.Fprintln(stdout, "  no acknowledgement is outstanding — this "+
				"operation is waiting on its operator.")
		}
		fmt.Fprintln(stdout)
	}

	// THE BLOCKING TERM FIRST AND IN PROSE, because it is the answer to
	// the only question anybody runs this for. A table an operator has to
	// read a `blocked_by` column out of is a table they read second.
	for _, d := range report.Domains {
		if *domain != "" && d.Domain != *domain {
			continue
		}
		if d.Prose != "" {
			fmt.Fprintln(stdout, d.Prose)
		}
		if d.SnapshotBlockedBy != "" {
			// ITS OWN LINE BENEATH, never folded into the one
			// above: a fleet that has stopped trimming and one that
			// has stopped snapshotting are two different problems,
			// and the second is silent until a node tries to join.
			fmt.Fprintf(stdout, "  This node is taking no snapshots: %s.\n",
				d.SnapshotBlockedBy)
		}
	}
	if !report.RegisterReadable {
		fmt.Fprintln(stdout, "The fleet register could not be listed, so the node "+
			"block below is what could be read rather than the fleet.")
	}
	// AND WHAT THIS DOCUMENT MAY CLAIM ABOUT ITS OWN AGE, on the one
	// answer that has to keep answering during the outage it describes.
	// `stale` is a claim about age; anything weaker is this node saying
	// it could not measure its distance from the log — which is the
	// ordinary signature of the broker or coordination being unreachable,
	// and the state an operator most needs named rather than inferred.
	if report.ReadLevel != "" && report.ReadLevel != string(statelog.ReadStale) {
		fmt.Fprintf(stdout, "This node could not measure its own distance from "+
			"the log, so the figures below are a coherent point in its order "+
			"with no statement about age (read_level %s).\n", report.ReadLevel)
	}

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	retentionDomains(w, report, *domain)
	retentionWatermarks(w, report, *domain)
	retentionNodes(w, report)
	retentionReplica(w, report)
	if err := w.Flush(); err != nil {
		return err
	}
	if report.BackupOwner != "" {
		fmt.Fprintf(stdout, "\nBACKUP OWNER  %s\n", report.BackupOwner)
	}
	return retentionAlarms(stderr, report)
}

// retentionDomains prints one row per registered domain.
//
// ONE ROW PER DOMAIN rather than a second command, so a fleet that registers a
// third domain grows a line rather than an argument. A term that does not
// apply to a domain prints `n/a` rather than `0`, because an absent term and a
// term that permits nothing are different facts.
//
// RESERVE is the top of CEILING kept for gate records on a log that claims
// identity, and HEADROOM is what is left of the rest — the ceiling ordinary
// writes are refused `log_full` at — so a log at 0% still takes an eviction.
func retentionDomains(w io.Writer, report retentionReport, only string) {
	fmt.Fprintln(w, "\nDOMAIN\tSTREAM\tGEN\tREPLAY\tFIRST\tLAST\tBYTES\tCEILING\tRESERVE\tHEADROOM\tTRIM FLOOR")
	for _, d := range report.Domains {
		if only != "" && d.Domain != only {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%d\n",
			d.Domain, d.Stream, d.Generation, d.Replay, d.FirstSeq, d.LastSeq,
			humanBytes(int64(d.Bytes)), ceilingOrDash(d.MaxBytes),
			reserveOrDash(d.ReserveBytes), headroomOrDash(d.HeadroomFraction),
			d.TrimFloor)
	}
}

// retentionWatermarks prints each domain's six terms.
func retentionWatermarks(w io.Writer, report retentionReport, only string) {
	for _, d := range report.Domains {
		if only != "" && d.Domain != only {
			continue
		}
		fmt.Fprintf(w, "\nWATERMARKS %s\tTERM\tSTATE\tAT\tDETAIL\n", d.Domain)
		for _, t := range d.Terms {
			// A SEQUENCE ONLY WHERE THE TERM WAS READ AND BINDS: an
			// unreadable, absent or unbounded term carries a number
			// that describes no position.
			at := strconv.FormatUint(t.Seq, 10)
			if t.State != string(statelog.TermKnown) {
				at = "-"
			}
			fmt.Fprintf(w, "\t%s\t%s\t%s\t%s\n", t.Name, t.State, at, t.Detail)
		}
		if d.BlockedBy != "" {
			fmt.Fprintf(w, "\tblocked_by\t%s\tsince %s\t\n",
				d.BlockedBy, stampOrDash(d.BlockedSince))
		}
	}
}

// retentionNodes prints the fleet's own rows.
func retentionNodes(w io.Writer, report retentionReport) {
	if len(report.Nodes) == 0 {
		return
	}
	fmt.Fprintln(w, "\nNODE\tDOMAIN\tSEQ\tAPPLIED\tLAG\tDEFERRED\tCOUNTED\tLIVE\tAT")
	for _, n := range report.Nodes {
		if len(n.Domains) == 0 {
			// A COUNTED NODE WITH NO POSITION STILL GETS A ROW.
			// It is exactly a node between boot and its first
			// heartbeat — a node adopting a snapshot — and it
			// blocks every term derived from the counted set, so
			// dropping the row would hide the block's cause.
			fmt.Fprintf(w, "%s\t-\t-\t-\t-\t-\t%s\t%s\t%s\n",
				n.NodeID, yesNo(n.Counted), yesNo(n.Live), noteOrStamp(n))
			continue
		}
		// SORTED, because the writer keys the block by domain and a map's
		// order would move a node's lines between two runs of the command.
		names := make([]string, 0, len(n.Domains))
		for name := range n.Domains {
			names = append(names, name)
		}
		sort.Strings(names)
		for i, domain := range names {
			d := n.Domains[domain]
			name := n.NodeID
			if i > 0 {
				name = ""
			}
			lag := "-"
			if d.Lag != nil {
				lag = strconv.FormatUint(*d.Lag, 10)
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%d\t%s\t%s\t%s\n",
				name, domain, d.Seq, d.AppliedThrough, lag, d.Deferred,
				yesNo(n.Counted), yesNo(n.Live), noteOrStamp(n))
		}
	}
}

// retentionReplica prints what THIS node costs to replace.
func retentionReplica(w io.Writer, report retentionReport) {
	fmt.Fprintf(w, "\nREPLICA (%s)\tstore %s\tprojected join %s\trejoin window %s\n",
		report.NodeID, humanBytes(report.Replica.StoreBytes),
		roundSeconds(report.Replica.ProjectedJoinSeconds),
		roundSeconds(report.Replica.RejoinWindowSeconds))
}

// retentionAlarms prints every active condition and decides the exit code.
//
// STDERR ONLY, so it takes no stdout: the report itself has already gone to
// stdout by the time this runs, and a second writer here would be a parameter
// whose only honest value is the one nothing writes to.
func retentionAlarms(stderr io.Writer, report retentionReport) error {
	if len(report.Alarms) == 0 {
		return nil
	}
	// ON STDERR, so a cron that captures stdout for a dashboard still puts
	// the reason in its own mail.
	fmt.Fprintln(stderr)
	for _, alarm := range report.Alarms {
		fmt.Fprintf(stderr, "%s: %s\n  %s\n", alarm.Kind, alarm.Detail, alarm.Remedy)
	}
	return fmt.Errorf("%d retention alarm(s) active on %s",
		len(report.Alarms), report.NodeID)
}

// retentionSnapshots prints the per-node snapshot inventory.
//
// # Why it is its own verb and not a block of `status`
//
// The repository is PER NODE: "which of my machines can donate, and how old is
// what they hold" is a disk question, and it is the one an operator asks when a
// join has failed. Folding it into `status` would put it behind the trim's own
// story, which is a different failure with a different remedy.
func retentionSnapshots(args []string, stdout, stderr io.Writer) error {
	client, err := nodeClientFor(args, "retention snapshots", stderr, nil)
	if err != nil {
		return err
	}
	var report retentionReport
	if err := client.get(context.Background(), "/query/retention", &report); err != nil {
		return err
	}
	if len(report.Snapshots) == 0 {
		fmt.Fprintln(stdout, "No node has reported a snapshot. A single-node "+
			"deployment takes none by design — its recovery artefact is "+
			"`crewlet backup`.")
		return nil
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tPOSITIONS\tAGE\tBYTES\tSTATE")
	for _, s := range report.Snapshots {
		positions := make([]string, 0, len(s.Domains))
		for domain, seq := range s.Domains {
			positions = append(positions, fmt.Sprintf("%s %d", domain, seq))
		}
		sort.Strings(positions)
		state, age := "holding", ageOrDash(s.At, report.At)
		if s.Skip != "" {
			// A NODE WITH NO ARTEFACT AND NO REASON STILL GETS A
			// ROW, and so does one whose reason is known: the
			// absence is the answer to "why did the join fail", and
			// dropping the row renders it as a node nobody asked.
			state = "none — " + s.Skip
		}
		if len(positions) == 0 {
			positions = []string{"-"}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.NodeID,
			strings.Join(positions, " / "), age, humanBytes(s.Bytes), state)
	}
	return w.Flush()
}

// retentionAck publishes an operator backup floor.
//
// It exists because the engine cannot see a copy that has left the host, and
// `backup_floor: operator` is the policy that says only such a copy counts.
func retentionAck(args []string, stdout, stderr io.Writer) error {
	var stream *string
	var position *uint64
	client, err := nodeClientFor(args, "retention ack", stderr, func(fs *flag.FlagSet) {
		stream = fs.String("stream", "",
			"the log this acknowledgement is about; required")
		position = fs.Uint64("position", 0,
			"the sequence your copy reaches; required")
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(*stream) == "" || *position == 0 {
		fmt.Fprintln(stderr,
			"usage: crewlet retention ack -stream <name> -position <n>")
		return fmt.Errorf("an acknowledgement moves the floor the trim deletes " +
			"against, so both the log and the sequence have to be named")
	}
	var answer struct {
		Stream     string `json:"stream"`
		Position   uint64 `json:"position"`
		Generation uint32 `json:"generation"`
	}
	path := "/work/retention/ack?stream=" + url.QueryEscape(*stream) +
		"&position=" + strconv.FormatUint(*position, 10)
	if err := client.post(context.Background(), path, &answer); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "acknowledged %s through %d at generation %d\n",
		answer.Stream, answer.Position, answer.Generation)
	return nil
}

// retentionGate evicts a node or takes it back.
//
// ONE FUNCTION FOR BOTH, because they are one gesture with a sign — and
// because the confirmation, the printing and the outcome vocabulary would
// otherwise be written twice and drift.
//
// # One gesture, every identity-claiming log, one line per log
//
// The node judges the gesture once and writes its record to every log the
// trim counts nodes on, so the answer is PER LOG: each with its own
// three-valued outcome, or the refusal that stopped that log. A gesture that
// did not reach every log exits non-zero and says how to finish it — the same
// command with the operation id it answered with, which every log that already
// holds the record answers from its own ledger rather than writing twice.
func retentionGate(args []string, stdout, stderr io.Writer, evict bool) error {
	verb := "readmit"
	if evict {
		verb = "evict"
	}
	node, rest := splitSubject(args)
	var confirm, opID *string
	var force *bool
	client, err := nodeClientFor(rest, "retention "+verb, stderr, func(fs *flag.FlagSet) {
		confirm = fs.String("confirm", "",
			"repeat the node id — this changes whether that machine's records apply")
		opID = fs.String("op-id", "",
			"the operation id an earlier run of this gesture answered with, to "+
				"finish it on the logs it did not reach; empty starts a new one")
		if evict {
			force = fs.Bool("force", false,
				"evict a node that still holds a live presence lease — only for "+
					"one wedged in a way that still renews it")
		}
	})
	if err != nil {
		return err
	}
	if node == "" || *confirm != node {
		fmt.Fprintf(stderr, "usage: crewlet retention %s <node-id> -confirm <node-id>\n", verb)
		return fmt.Errorf("an eviction stops a machine's records applying anywhere "+
			"in the fleet and a readmission lets them apply again — repeat the "+
			"node id in -confirm to run %s", verb)
	}

	// THE OPERATION ID IS MINTED HERE, BEFORE THE REQUEST, when the operator
	// brought none. A gesture writes one log after another and the node
	// finishes it whatever happens to this connection, so a request that
	// times out or drops has very likely done its work — and the id the
	// node would have answered with is the only handle on it. Minted by the
	// node, it was lost with the answer, and the only way on was a second
	// gesture under a fresh id.
	//
	// THROUGH THE STATE LOG'S OWN MINT ([statelog.NewOpID]), under the name
	// the route itself mints with, so the id carries its instant — the node
	// refuses one that does not, since no ledger could vouch for it — and
	// reads in the ledger exactly as a node-minted one would.
	gesture := *opID
	if gesture == "" {
		gesture = statelog.NewOpID(time.Now(), verb+"-"+node)
	}
	forced := force != nil && *force
	// THE COMMAND THAT FINISHES THIS GESTURE, with the flags that decide it:
	// -force carried, because a retry without it is judged again against
	// the very lease the operator overrode, and refused.
	again := "-op-id " + gesture
	if forced {
		again += " -force"
	}

	// THE WATERMARK BEFORE AND AFTER, so an operator sees what the gesture
	// did rather than being told it succeeded. The trim floor is what an
	// eviction is FOR — it is how a floor an absent node is pinning gets
	// to move — and a readmission can be refused by exactly that number.
	before, beforeErr := retentionFloors(client)
	var answer gateAnswer
	query := url.Values{"confirm": {node}, "op_id": {gesture}}
	if forced {
		query.Set("force", "true")
	}
	path := fmt.Sprintf("/work/retention/%s/%s?%s", verb, url.PathEscape(node),
		query.Encode())
	// PATIENTLY, for the reason [nodeClient.patiently] names: how long a
	// gesture takes is a property of what it waits on — a write per log,
	// each resolved against this node's applier — and not of the network.
	if err := client.patiently(gateRequestTimeout).post(context.Background(), path,
		&answer); err != nil {
		var lost noAnswer
		if errors.As(err, &lost) {
			fmt.Fprintf(stderr, "The node did not answer, so what the %s did is "+
				"unknown: it may have reached every log. Run the same command "+
				"again with %s — every log that already holds the record answers "+
				"from its own rows, and only a missing one is written.\n", verb, again)
		}
		return err
	}
	fmt.Fprintf(stdout, "%s %s (operation %s)\n", verb, node, answer.OpID)
	pending, retry := false, false
	for _, d := range answer.Domains {
		switch {
		case d.Outcome == string(statelog.OutcomeUnknown):
			// NO POSITION, which is the whole content of unknown: printed
			// as "at 0" it read as a record landed at the log's origin.
			fmt.Fprintf(stdout, "  %s: unknown — the record may or may not be "+
				"on the log\n", d.Domain)
		case d.Outcome != "":
			fmt.Fprintf(stdout, "  %s: %s at %s %d\n", d.Domain, d.Outcome,
				d.Position.Stream, d.Position.Seq)
			pending = pending || d.Outcome == string(statelog.OutcomePending)
		case d.Reason != "":
			fmt.Fprintf(stdout, "  %s: not written (%s) — %s\n", d.Domain, d.Reason, d.Error)
		default:
			fmt.Fprintf(stdout, "  %s: no outcome — %s\n", d.Domain, d.Error)
		}
		// WHAT TO DO ABOUT A LOG THE GESTURE DID NOT FINISH, in the node's
		// own words — and only where it has some: running the command
		// again is the remedy for some refusals and a loop for others.
		if d.Hint != "" {
			fmt.Fprintf(stdout, "    %s\n", d.Hint)
			retry = retry || d.Retry
		}
	}
	if pending {
		// THE THREE-VALUED OUTCOME, said plainly. A gate the operator
		// believes has landed and which is only durable is the
		// difference between a node that has stopped writing and one
		// that is about to.
		fmt.Fprintln(stdout, "  A pending log holds the record durably and this node "+
			"has not applied it yet: the gate takes effect as each node reaches it.")
	}
	if !answer.Complete {
		// NOT A FAILURE TO RETRY BLINDLY: the logs that answered hold
		// their record, a fresh operation id would be a second gesture
		// rather than this one finished, and a log whose refusal no
		// retry clears is not offered one — its own line above says
		// what does.
		if !retry {
			return fmt.Errorf("the %s of %s did not reach every log, and running "+
				"it again cannot finish it until what each log's line names is "+
				"done", verb, node)
		}
		fmt.Fprintf(stdout, "  The gesture has not reached every log. Run it again "+
			"with %s to finish it: a log that already holds the record answers "+
			"from its own rows and is not written twice.\n", again)
		return fmt.Errorf("the %s of %s did not reach every log — run it again "+
			"with %s", verb, node, again)
	}
	if evict {
		fmt.Fprintf(stdout, "  %s stays COUNTED for about %s, so a live node is "+
			"certain to have read its own tombstone before the trim passes it.\n",
			node, evictionFenceWindow)
	}
	after, afterErr := retentionFloors(client)
	if err := errors.Join(beforeErr, afterErr); err != nil {
		fmt.Fprintf(stdout, "  The trim floors could not be read, so no watermark "+
			"is printed: %v\n", err)
		return nil
	}
	now := make(map[string]uint64, len(after))
	for _, d := range after {
		now[d.domain] = d.floor
	}
	for _, d := range before {
		fmt.Fprintf(stdout, "  %s trim floor %d → %d\n", d.domain, d.floor, now[d.domain])
	}
	return nil
}

// gateAnswer is the gate routes' answer as this command reads it — a shape of
// its own, for [retentionReport]'s reason.
type gateAnswer struct {
	Node     string       `json:"node"`
	Evicted  bool         `json:"evicted"`
	OpID     string       `json:"op_id"`
	Complete bool         `json:"complete"`
	Domains  []gateDomain `json:"domains"`
}

// gateDomain is one log's answer: an outcome and its position, or the reason
// and the error that stopped it.
type gateDomain struct {
	Domain   string `json:"domain"`
	Stream   string `json:"stream"`
	OpID     string `json:"op_id"`
	Outcome  string `json:"outcome"`
	Position struct {
		Stream     string `json:"stream"`
		Generation uint32 `json:"generation"`
		Seq        uint64 `json:"seq"`
	} `json:"position"`
	Reason string `json:"reason"`
	Error  string `json:"error"`

	// Retry and Hint are the node's own judgement of a log the gesture did
	// not finish: whether running it again under the same operation id can
	// finish it, and what to do either way. Absent on a finished log.
	Retry bool   `json:"retry"`
	Hint  string `json:"hint"`
}

// gateRequestTimeout is how long `retention evict` and `readmit` wait for the
// node's answer.
//
// SEVENTY-FIVE SECONDS: the node bounds a gesture at a minute from its first
// record to its last answer (engine.GateBudget), and the judgement before it
// and the round trip around it are a coordination read and a request. Waiting
// past the node's own bound is what makes its answer — every log's outcome and
// what to do about the ones it could not finish — reach the operator rather
// than a client timeout that knows none of it. The ten seconds every other
// verb waits was two of the five-second resolutions a gesture legitimately
// makes, back to back.
const gateRequestTimeout = 75 * time.Second

// evictionFenceWindow is how long an evicted node stays counted, as this
// command says it.
//
// A STRING rather than the constant, because the CLI is a client: importing
// the engine's own value would make an older binary print a window a newer
// node is not using, which is worse than printing an approximation and saying
// "about".
const evictionFenceWindow = "a minute"

// retentionFloors reads each domain's published floor, for the before-and-after
// print, in the report's own domain order.
//
// A FAILURE NEVER FAILS THE GESTURE — it has already run, and failing here
// would report a failure that did not happen — but it is SAID rather than
// swallowed. Swallowed, it hid for as long as this file existed that these
// reads decoded nothing from any real node: the gesture promised a watermark
// and printed none, and nothing looked wrong.
func retentionFloors(client *nodeClient) ([]domainFloor, error) {
	var report retentionReport
	if err := client.get(context.Background(), "/query/retention", &report); err != nil {
		return nil, err
	}
	out := make([]domainFloor, 0, len(report.Domains))
	for _, d := range report.Domains {
		out = append(out, domainFloor{domain: d.Domain, floor: d.TrimFloor})
	}
	return out, nil
}

// domainFloor is one domain's published floor, as the watermark print reads it.
type domainFloor struct {
	domain string
	floor  uint64
}

func ceilingOrDash(maxBytes uint64) string {
	if maxBytes == 0 {
		// A LOG WITH NO CEILING is a real setting, and printing `0`
		// would read as the opposite — a log that may hold nothing.
		return "none"
	}
	return humanBytes(int64(maxBytes))
}

func reserveOrDash(reserve uint64) string {
	if reserve == 0 {
		// A LOG THAT KEEPS NO RESERVE — one claiming no identity, or
		// one with no ceiling to keep it under — rather than one whose
		// reserve is empty: nothing is ever written into it.
		return "-"
	}
	return humanBytes(int64(reserve))
}

func headroomOrDash(fraction *float64) string {
	if fraction == nil {
		// NOT ZERO. A fraction of an unknown ceiling is not zero
		// headroom, and zero is what the one alarm an operator cannot
		// ignore fires on.
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *fraction*100)
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// noteOrStamp is a node row's last column: its tombstone, the absence of any
// report, or when it last reported.
//
// A NODE THAT HAS NEVER REPORTED is said so rather than printed as `-`: it is
// counted at position zero and blocks every term derived from the counted
// set, and the row is how an operator finds the block's cause.
func noteOrStamp(n retentionNode) string {
	if n.Evicted != nil {
		if !n.Evicted.Effective {
			// INSIDE THE FENCE WINDOW the node is still counted, and
			// an operator reading an unchanged floor beside a bare
			// "evicted" runs the gesture again.
			return fmt.Sprintf("evicted by %s, still counted until %s",
				n.Evicted.By, stampOrDash(n.Evicted.EffectiveAt))
		}
		return fmt.Sprintf("evicted by %s, effective %s", n.Evicted.By,
			stampOrDash(n.Evicted.EffectiveAt))
	}
	if n.At.IsZero() {
		return "no position yet"
	}
	return stampOrDash(n.At)
}

func stampOrDash(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func ageOrDash(at, now time.Time) string {
	if at.IsZero() {
		return "-"
	}
	return roundSeconds(now.Sub(at).Seconds()) + " ago"
}

func roundSeconds(seconds float64) string {
	if seconds <= 0 {
		return "-"
	}
	return (time.Duration(seconds) * time.Second).Round(time.Second).String()
}
