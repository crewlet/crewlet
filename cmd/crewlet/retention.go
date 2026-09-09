package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
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
type retentionReport struct {
	NodeID           string    `json:"node_id"`
	At               time.Time `json:"at"`
	BackupOwner      string    `json:"backup_owner"`
	RegisterReadable bool      `json:"register_readable"`

	Domains   []retentionDomain   `json:"domains"`
	Nodes     []retentionNode     `json:"nodes"`
	Snapshots []retentionSnapshot `json:"snapshots"`
	Replica   retentionReplicaRow `json:"replica"`
	Alarms    []retentionAlarm    `json:"alarms"`
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
	HeadroomFraction  *float64        `json:"headroom_fraction"`
	TrimFloor         uint64          `json:"trim_floor"`
	TrimTo            uint64          `json:"trim_to"`
	BlockedBy         string          `json:"blocked_by"`
	BlockedSince      time.Time       `json:"blocked_since"`
	Prose             string          `json:"prose"`
	SnapshotBlockedBy string          `json:"snapshot_blocked_by"`
	Terms             []retentionTerm `json:"terms"`
}

// retentionTerm is one of the six, with the third value it can take.
type retentionTerm struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Seq    uint64 `json:"seq"`
	Detail string `json:"detail"`
	Remedy string `json:"remedy"`
}

// retentionNode is one row of the fleet.
type retentionNode struct {
	NodeID   string              `json:"node_id"`
	Counted  bool                `json:"counted"`
	Live     bool                `json:"live"`
	At       time.Time           `json:"at"`
	Note     string              `json:"note"`
	Eviction *retentionEviction  `json:"eviction"`
	Domains  []retentionNodeSeat `json:"domains"`
}

type retentionEviction struct {
	By          string    `json:"by"`
	At          time.Time `json:"at"`
	EffectiveAt time.Time `json:"effective_at"`
}

type retentionNodeSeat struct {
	Domain         string `json:"domain"`
	Seq            uint64 `json:"seq"`
	AppliedThrough uint64 `json:"applied_through"`
	Lag            uint64 `json:"lag"`
	Deferred       int    `json:"deferred"`
}

// retentionSnapshot is one node's artefact, or its absence with the reason.
type retentionSnapshot struct {
	NodeID  string                `json:"node_id"`
	At      time.Time             `json:"at"`
	Bytes   int64                 `json:"bytes"`
	Skip    string                `json:"skip"`
	Domains []retentionSnapshotAt `json:"domains"`
}

type retentionSnapshotAt struct {
	Domain string `json:"domain"`
	Seq    uint64 `json:"seq"`
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
	return retentionAlarms(stdout, stderr, report)
}

// retentionDomains prints one row per registered domain.
//
// ONE ROW PER DOMAIN rather than a second command, so a fleet that registers a
// third domain grows a line rather than an argument. A term that does not
// apply to a domain prints `n/a` rather than `0`, because an absent term and a
// term that permits nothing are different facts.
func retentionDomains(w io.Writer, report retentionReport, only string) {
	fmt.Fprintln(w, "\nDOMAIN\tSTREAM\tGEN\tREPLAY\tFIRST\tLAST\tBYTES\tCEILING\tHEADROOM\tTRIM FLOOR")
	for _, d := range report.Domains {
		if only != "" && d.Domain != only {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d\t%d\t%s\t%s\t%s\t%d\n",
			d.Domain, d.Stream, d.Generation, d.Replay, d.FirstSeq, d.LastSeq,
			humanBytes(int64(d.Bytes)), ceilingOrDash(d.MaxBytes),
			headroomOrDash(d.HeadroomFraction), d.TrimFloor)
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
			at := strconv.FormatUint(t.Seq, 10)
			if t.State != "known" {
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
		for i, d := range n.Domains {
			name := n.NodeID
			if i > 0 {
				name = ""
			}
			fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n",
				name, d.Domain, d.Seq, d.AppliedThrough, d.Lag, d.Deferred,
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
func retentionAlarms(stdout, stderr io.Writer, report retentionReport) error {
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
		for _, d := range s.Domains {
			positions = append(positions, fmt.Sprintf("%s %d", d.Domain, d.Seq))
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
func retentionGate(args []string, stdout, stderr io.Writer, evict bool) error {
	verb := "readmit"
	if evict {
		verb = "evict"
	}
	node, rest := splitSubject(args)
	var confirm *string
	client, err := nodeClientFor(rest, "retention "+verb, stderr, func(fs *flag.FlagSet) {
		confirm = fs.String("confirm", "",
			"repeat the node id — this changes whether that machine's records apply")
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

	// THE WATERMARK BEFORE AND AFTER, so an operator sees what the gesture
	// did rather than being told it succeeded. The trim floor is what an
	// eviction is FOR — it is how a floor an absent node is pinning gets
	// to move — and a readmission can be refused by exactly that number.
	before := retentionFloors(client)
	var answer struct {
		Node     string `json:"node"`
		Evicted  bool   `json:"evicted"`
		Outcome  string `json:"outcome"`
		Position struct {
			Stream     string `json:"stream"`
			Generation uint32 `json:"generation"`
			Seq        uint64 `json:"seq"`
		} `json:"position"`
	}
	path := fmt.Sprintf("/work/retention/%s/%s?confirm=%s", verb,
		url.PathEscape(node), url.QueryEscape(node))
	if err := client.post(context.Background(), path, &answer); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s %s: %s at %s %d\n", verb, node, answer.Outcome,
		answer.Position.Stream, answer.Position.Seq)
	if answer.Outcome == "pending" {
		// THE THREE-VALUED OUTCOME, said plainly. A gate the operator
		// believes has landed and which is only durable is the
		// difference between a node that has stopped writing and one
		// that is about to.
		fmt.Fprintln(stdout, "  The record is durable and this node has not "+
			"applied it yet: the gate takes effect as each node reaches it.")
	}
	if evict {
		fmt.Fprintf(stdout, "  %s stays COUNTED for about %s, so a live node is "+
			"certain to have read its own tombstone before the trim passes it.\n",
			node, evictionFenceWindow)
	}
	after := retentionFloors(client)
	for domain, was := range before {
		fmt.Fprintf(stdout, "  %s trim floor %d → %d\n", domain, was, after[domain])
	}
	return nil
}

// evictionFenceWindow is how long an evicted node stays counted, as this
// command says it.
//
// A STRING rather than the constant, because the CLI is a client: importing
// the engine's own value would make an older binary print a window a newer
// node is not using, which is worse than printing an approximation and saying
// "about".
const evictionFenceWindow = "a minute"

// retentionFloors reads each domain's published floor, for the before-and-after
// print. A failure yields nothing rather than an error: the gesture has already
// run, and failing here would report a failure that did not happen.
func retentionFloors(client *nodeClient) map[string]uint64 {
	var report retentionReport
	if err := client.get(context.Background(), "/query/retention", &report); err != nil {
		return nil
	}
	out := map[string]uint64{}
	for _, d := range report.Domains {
		out[d.Domain] = d.TrimFloor
	}
	return out
}

func ceilingOrDash(maxBytes uint64) string {
	if maxBytes == 0 {
		// A LOG WITH NO CEILING is a real setting, and printing `0`
		// would read as the opposite — a log that may hold nothing.
		return "none"
	}
	return humanBytes(int64(maxBytes))
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

func noteOrStamp(n retentionNode) string {
	if n.Eviction != nil {
		return fmt.Sprintf("evicted by %s, effective %s", n.Eviction.By,
			stampOrDash(n.Eviction.EffectiveAt))
	}
	if n.Note != "" {
		return n.Note
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
