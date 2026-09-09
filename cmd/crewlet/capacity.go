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
    5. restart every node with no -mode

  crewlet retention maintenance status -stream <stream>  says where it stands
  and what is holding it. The procedure itself is documented once, in
  docs/guides/retention.md.`

// retentionSetCapacity is `crewlet retention set-capacity`.
func retentionSetCapacity(args []string, stdout, stderr io.Writer) error {
	stream, rest := splitSubject(args)
	target, rest := splitSubject(rest)
	var confirm *string
	var assert *bool
	client, err := nodeClientFor(rest, "retention set-capacity", stderr,
		func(fs *flag.FlagSet) {
			confirm = fs.String("confirm", "",
				"repeat the byte count — this restarts the whole fleet three times")
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

	path := fmt.Sprintf("/work/retention/capacity?stream=%s&bytes=%d&confirm=%d",
		url.QueryEscape(stream), bytes, bytes)
	if *assert {
		path += "&assert_excluded=true"
	}
	var answer struct {
		Mode      string             `json:"mode"`
		Operation *capacityOperation `json:"operation"`
	}
	if err := client.post(context.Background(), path, &answer); err != nil {
		return err
	}
	if answer.Operation == nil {
		fmt.Fprintln(stdout, "no capacity operation is open")
		return nil
	}
	printOperation(stdout, answer.Mode, *answer.Operation)
	return nil
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
	Blocked   string    `json:"blocked"`
	EnteredAt time.Time `json:"entered_at"`
	By        string    `json:"by"`
}

// printOperation renders one operation, and what an operator does next.
func printOperation(stdout io.Writer, mode string, op capacityOperation) {
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "OPERATION\t%s\ton %s\n", op.OperationID, op.Stream)
	fmt.Fprintf(w, "PHASE\t%s\tattempt %d\n", op.Phase, op.Attempt)
	fmt.Fprintf(w, "TARGET\t%s\twas %s\n",
		humanBytes(int64(op.TargetMaxBytes)), humanBytes(int64(op.OriginalMaxBytes)))
	if op.ObservedMaxBytes > 0 {
		fmt.Fprintf(w, "OBSERVED\t%s\t\n", humanBytes(int64(op.ObservedMaxBytes)))
	}
	fmt.Fprintf(w, "OPENED\t%s\tby %s\n", stampOrDash(op.EnteredAt), op.By)
	fmt.Fprintf(w, "THIS NODE\tmode %s\t\n", mode)
	if op.Blocked != "" {
		fmt.Fprintf(w, "BLOCKED\t%s\t\n", op.Blocked)
	}
	_ = w.Flush()

	// WHAT TO DO NEXT, derived from the phase and NOT a restatement of
	// the transitions: an operator needs the next gesture, and a list of
	// evidence rules is the thing that forks.
	fmt.Fprintln(stdout)
	switch op.Phase {
	case "opened", "baselined":
		fmt.Fprintln(stdout, "Next: restart every node with `crewlet run -mode seal`, "+
			"then run this verb again.")
	case "applied":
		fmt.Fprintln(stdout, "The ceiling is applied and NOT yet sealed — another "+
			"request may still be in flight.\n"+
			"Next: restart every node with `crewlet run -mode seal`, then run "+
			"this verb again.")
	case "sealing":
		fmt.Fprintln(stdout, "Waiting for the barrier. `crewlet retention "+
			"maintenance status` names who has not acknowledged.")
	case "sealed":
		fmt.Fprintln(stdout, "Sealed. Run this verb again to verify and confirm.")
	}
}

// retentionMaintenance is `crewlet retention maintenance`.
func retentionMaintenance(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "status":
		return maintenanceStatus(rest, stdout, stderr)
	case "abandon":
		return maintenanceAbandon(rest, stdout, stderr)
	case "exclude":
		return maintenanceExclude(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, "usage: crewlet retention maintenance "+
			"status|abandon|exclude -stream <name>")
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown maintenance command %q", sub)
	}
}

// maintenanceStatus prints the durable state, and what is holding it.
//
// It is the ONE place an operator can see why a fleet is still excluded: the
// operation, its phase and attempt, every participant's incarnation against
// the current one, which acknowledgements are missing, and any admission
// blocking activation.
func maintenanceStatus(args []string, stdout, stderr io.Writer) error {
	var stream *string
	client, err := nodeClientFor(args, "retention maintenance status", stderr,
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
		Stream             string             `json:"stream"`
		Open               bool               `json:"open"`
		Mode               string             `json:"mode"`
		Sealed             bool               `json:"sealed"`
		Blocking           string             `json:"blocking"`
		AdmissionsBlocking []string           `json:"admissions_blocking"`
		Operation          *capacityOperation `json:"operation"`
		Acks               []struct {
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
		"/work/retention/maintenance?stream="+url.QueryEscape(*stream), &answer); err != nil {
		return err
	}
	if !answer.Open {
		fmt.Fprintf(stdout, "No capacity operation is open on %s. "+
			"This node's mode is %s.\n", answer.Stream, answer.Mode)
		if len(answer.Admissions) > 0 {
			fmt.Fprintf(stdout, "%d node(s) hold an admission, which is normal "+
				"for a fleet in service.\n", len(answer.Admissions))
		}
		return nil
	}
	printOperation(stdout, answer.Mode, *answer.Operation)

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\nPARTICIPANT\tBASELINED AS\tACKNOWLEDGED AS\tATTEMPT\tMODE")
	acked := map[string]int{}
	for i, ack := range answer.Acks {
		acked[ack.NodeID] = i
	}
	for _, node := range answer.Operation.Participants {
		baseline := dashIfEmpty(answer.Operation.WriteIncarnations[node])
		if containsString(answer.Operation.Excluded, node) {
			fmt.Fprintf(w, "%s\t%s\texcluded by an operator\t\t\n", node, baseline)
			continue
		}
		i, ok := acked[node]
		if !ok {
			fmt.Fprintf(w, "%s\t%s\t—\t\t\n", node, baseline)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", node, baseline,
			answer.Acks[i].Incarnation, answer.Acks[i].Attempt, answer.Acks[i].Mode)
	}
	if len(answer.Operation.Journal) > 0 {
		fmt.Fprintln(w, "\nWRITE ATTEMPT\tSTATE\tBY\tEVIDENCE")
		for _, record := range answer.Operation.Journal {
			fmt.Fprintf(w, "attempt %d\t%s\t%s\t%s\n",
				record.Attempt, record.State, record.By, dashIfEmpty(record.Evidence))
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(stdout)
	if answer.Sealed {
		fmt.Fprintln(stdout, "The barrier holds.")
	} else if answer.Blocking != "" {
		fmt.Fprintf(stdout, "Not sealed: %s\n", answer.Blocking)
	}
	if len(answer.AdmissionsBlocking) > 0 {
		// AN `admitted` KEY IS A NODE THAT MAY BE PUBLISHING, which is
		// the one thing that stops activation and the one an operator
		// cannot deduce from anything else on this page.
		fmt.Fprintf(stdout, "Admissions still held by %v — those nodes may be "+
			"publishing. Restart them with `-mode maintenance`, or record that "+
			"one is gone with `crewlet retention maintenance exclude`.\n",
			answer.AdmissionsBlocking)
	}
	return nil
}

// maintenanceAbandon changes what the operation is trying to reach.
//
// IT NEVER CHANGES THE BARRIER IT MUST CROSS: from anywhere past `opened` it
// enters the seal, because a paused coordinator's request is outstanding
// whether or not a person has read a status page.
func maintenanceAbandon(args []string, stdout, stderr io.Writer) error {
	var stream, confirm *string
	client, err := nodeClientFor(args, "retention maintenance abandon", stderr,
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
	}
	if err := client.post(context.Background(),
		"/work/retention/maintenance/abandon?stream="+url.QueryEscape(*stream),
		&answer); err != nil {
		return err
	}
	if answer.Operation == nil {
		fmt.Fprintln(stdout, "The operation is cleared and the exclusion released. "+
			"Restart every node with no -mode to return the fleet to service.")
		return nil
	}
	if answer.Operation.OperationID != *confirm {
		return fmt.Errorf("the open operation is %s and -confirm named %s: read "+
			"`crewlet retention maintenance status` before abandoning",
			answer.Operation.OperationID, *confirm)
	}
	printOperation(stdout, "", *answer.Operation)
	return nil
}

// maintenanceExclude records an operator's assertion that a participant is
// stopped and holds no outstanding request.
//
// THE ONLY THING THAT WAIVES AN ACKNOWLEDGEMENT. An eviction does not: that is
// about whose records apply, and this is about whose process is running.
func maintenanceExclude(args []string, stdout, stderr io.Writer) error {
	var stream, node, confirm *string
	client, err := nodeClientFor(args, "retention maintenance exclude", stderr,
		func(fs *flag.FlagSet) {
			stream = fs.String("stream", "", "which log; required")
			node = fs.String("node", "", "which participant; required")
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
			"stopped and holds no outstanding request — repeat the node id")
	}
	var answer struct {
		Operation *capacityOperation `json:"operation"`
	}
	path := fmt.Sprintf("/work/retention/maintenance/exclude?stream=%s&node=%s&confirm=%s",
		url.QueryEscape(*stream), url.QueryEscape(*node), url.QueryEscape(*node))
	if err := client.post(context.Background(), path, &answer); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s is excluded; it no longer has to acknowledge.\n", *node)
	if answer.Operation != nil {
		printOperation(stdout, "", *answer.Operation)
	}
	return nil
}

// retentionReanchor is `crewlet retention reanchor`.
func retentionReanchor(args []string, stdout, stderr io.Writer) error {
	var stream, confirm *string
	var force *bool
	client, err := nodeClientFor(args, "retention reanchor", stderr,
		func(fs *flag.FlagSet) {
			stream = fs.String("stream", "", "which log was recreated; required")
			confirm = fs.String("confirm", "",
				"the live stream's own created_at, which this verb prints when "+
					"it is omitted")
			force = fs.Bool("force", false,
				"re-anchor even though a peer is hydrated on the live stream. "+
					"Adopting that peer's snapshot is strictly better, so this "+
					"is for the case where it cannot be reached")
		})
	if err != nil {
		return err
	}
	if strings.TrimSpace(*stream) == "" {
		fmt.Fprintln(stderr, "usage: crewlet retention reanchor -stream <name> "+
			"-confirm <the live stream's created_at>")
		return errors.New("name the log")
	}

	var status struct {
		Stream     string    `json:"stream"`
		CreatedAt  time.Time `json:"created_at"`
		Generation uint32    `json:"generation"`
	}
	if err := client.get(context.Background(),
		"/work/retention/reanchor?stream="+url.QueryEscape(*stream), &status); err != nil {
		return err
	}
	if strings.TrimSpace(*confirm) == "" {
		// PRINTED RATHER THAN ACCEPTED. The confirmation says "I looked
		// at the thing I am re-anchoring", and a verb that read the
		// value and fed it straight back would be confirming against
		// its own output.
		fmt.Fprintf(stdout, "%s is at generation %d and was created at %s.\n\n"+
			"A reanchor declares every position below the NEXT generation stale, "+
			"and does NOT recover records that were on the old stream and were "+
			"never applied here.\n\nRe-run with:\n  "+
			"crewlet retention reanchor -stream %s -confirm %s\n",
			status.Stream, status.Generation,
			status.CreatedAt.UTC().Format(time.RFC3339Nano),
			status.Stream, status.CreatedAt.UTC().Format(time.RFC3339Nano))
		return errors.New("confirm the stream's own created_at")
	}

	path := fmt.Sprintf("/work/retention/reanchor?stream=%s&confirm=%s",
		url.QueryEscape(*stream), url.QueryEscape(*confirm))
	if *force {
		path += "&force=true"
	}
	var answer struct {
		Stream     string `json:"stream"`
		Generation uint32 `json:"generation"`
	}
	if err := client.post(context.Background(), path, &answer); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s is re-anchored at generation %d. Every position "+
		"below it is now comparable and safely stale.\n",
		answer.Stream, answer.Generation)
	return nil
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
			"has no sequence to replay from — this artefact cannot be verified " +
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
