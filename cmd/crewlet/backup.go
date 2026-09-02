package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"path"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

// runBackup is `crewlet backup`.
//
// # Why this talks to a NODE and not to files
//
// The same reason `budgets` does, twice over. A node's durable state is two
// estates and neither is reachable from outside the process that holds them:
//
//   - THE STORE is one file the engine locks exclusively for the life of the
//     handle, and the driver does not support a second process on a database
//     file. So there is no safe way for this command to open it while the
//     engine runs — and copying it anyway is worse than useless, because
//     committed data lives in the file and its -wal together.
//   - THE STREAM ESTATE — the agent mailboxes and every coordination bucket,
//     which is where the fleet's leases, ledgers, counters and sealed
//     credentials live — runs on a broker embedded in the engine that binds
//     NO SOCKET on the default topology. There is no address to give the
//     `nats` CLI.
//
// So the copy is taken by the one process that can reach both, and this
// command asks it to. The destination is a path on the ENGINE'S host, which
// is the surprise worth stating plainly: this does not download anything.
func runBackup(args []string, stdout, stderr io.Writer) error {
	var dir *string
	var wait *time.Duration
	client, err := nodeClientFor(args, "backup", stderr, func(fs *flag.FlagSet) {
		dir = fs.String("dir", "",
			"absolute directory ON THE ENGINE'S HOST to write the backup into; required")
		wait = fs.Duration("wait", backupWait,
			"how long to wait for the copy before giving up on the answer")
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(*dir) == "" {
		fmt.Fprintln(stderr, "usage: crewlet backup [<config.yaml>] -dir <absolute path>")
		return fmt.Errorf("name where the backup should go with -dir")
	}
	// POSIX semantics deliberately, via path rather than filepath: what is
	// being judged is a path on the ENGINE's host, and the engine ships for
	// linux and darwin only. Using this machine's rules to judge another
	// machine's path is the kind of thing that is right until somebody runs
	// the CLI from somewhere unexpected.
	if !path.IsAbs(*dir) {
		// Caught here as well as by the node, because the node's refusal
		// travels as an HTTP error and this one can say the thing that
		// actually matters: the path is resolved somewhere else.
		return fmt.Errorf("-dir %s is relative, and it is resolved on the engine's "+
			"host rather than in this shell — give an absolute path", *dir)
	}

	// DECODED INTO A SHAPE OF THIS COMMAND'S OWN rather than into
	// backup.Manifest: the CLI reads a node's answer over HTTP, and a
	// struct shared with the writer would make an older `crewlet backup`
	// refuse a newer node's manifest over a field it does not print.
	type streamRow struct {
		Name     string `json:"name"`
		File     string `json:"file"`
		Bytes    int64  `json:"bytes"`
		Messages uint64 `json:"messages"`
	}
	var manifest struct {
		TakenAt    time.Time `json:"taken_at"`
		FinishedAt time.Time `json:"finished_at"`
		NodeID     string    `json:"node_id"`
		Store      *struct {
			File       string   `json:"file"`
			Source     string   `json:"source"`
			Bytes      int64    `json:"bytes"`
			Migrations []string `json:"migrations"`
		} `json:"store"`
		Streams []streamRow `json:"streams"`
	}
	if err := client.patiently(*wait).post(context.Background(),
		"/backup?dir="+url.QueryEscape(*dir), &manifest); err != nil {
		return err
	}

	// The report NAMES WHAT IT GOT, per estate, for the same reason the
	// budget reset names what it cleared: a backup an operator cannot
	// inspect is one they have to trust, and the failure it hides — an
	// estate silently absent because this node does not run one — looks
	// exactly like success.
	fmt.Fprintf(stdout, "Backup written to %s on %s in %s\n",
		*dir, manifest.NodeID, manifest.FinishedAt.Sub(manifest.TakenAt).Round(time.Millisecond))
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\nWHAT\tFILE\tSIZE\tCONTENTS")
	if manifest.Store != nil {
		fmt.Fprintf(w, "store\t%s\t%s\t%d migrations\n",
			manifest.Store.File, humanBytes(manifest.Store.Bytes), len(manifest.Store.Migrations))
	} else {
		fmt.Fprintln(w, "store\t—\t—\tnot on this node")
	}
	streams := manifest.Streams
	slices.SortFunc(streams, func(a, b streamRow) int { return cmp.Compare(a.Name, b.Name) })
	for _, s := range streams {
		what := "stream"
		if bucket, found := strings.CutPrefix(s.Name, "KV_"); found {
			what = "bucket"
			s.Name = bucket
		}
		fmt.Fprintf(w, "%s %s\t%s\t%s\t%d messages\n",
			what, s.Name, s.File, humanBytes(s.Bytes), s.Messages)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if manifest.Store == nil || len(manifest.Streams) == 0 {
		// Said plainly rather than left to be inferred from a dash: a
		// backup missing an estate is not restorable on its own, and an
		// operator running this against an ingress node would otherwise
		// file the output as a backup of their company.
		fmt.Fprintln(stdout, "\nThis node holds only part of the deployment's state. "+
			"A restorable backup needs both estates — take this against a node "+
			"running seats or workers.")
	}
	return nil
}

// backupWait is how long the CLI waits for a copy.
//
// Thirty minutes, and the number is a ceiling rather than an expectation: a
// backup is bounded by the size of the store and the stream estate, which is
// tens of milliseconds on a company that has just started and minutes on a
// large one with a full event-retention window. The cost of being too short
// is the confusing one — the engine finishes and writes a perfectly good
// backup that this command has already called a failure — so this is set far
// past any plausible real duration rather than close to it.
const backupWait = 30 * time.Minute

// humanBytes renders a size an operator is going to eyeball against a disk.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
