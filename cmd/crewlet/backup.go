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

	"github.com/crewlet/crewlet/internal/config"
)

// runBackup is `crewlet backup`.
//
// It talks to a NODE, not to files: the store is two files locked exclusively
// by the engine (the driver supports no second process, and copying one alone
// loses its -wal), and the stream estate (mailboxes, leases, ledgers, sealed
// credentials) runs on a broker embedded in the engine that binds NO SOCKET on
// the default topology. So the process that reaches both takes the copy. The
// destination is a path on the ENGINE'S host: nothing is downloaded.
func runBackup(environ config.Source, args []string, stdout, stderr io.Writer) error {
	var dir *string
	var wait *time.Duration
	client, err := nodeClientFor(environ, args, "backup", stderr, func(fs *flag.FlagSet) {
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
			"host rather than in this shell. Give an absolute path", *dir)
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
		Stores     []struct {
			Estate     string   `json:"estate"`
			File       string   `json:"file"`
			Source     string   `json:"source"`
			Bytes      int64    `json:"bytes"`
			SHA256     string   `json:"sha256"`
			Migrations []string `json:"migrations"`
		} `json:"stores"`
		Streams []streamRow `json:"streams"`
		Objects *struct {
			Dir      string `json:"dir"`
			Stream   string `json:"stream"`
			Objects  int    `json:"objects"`
			Replayed int    `json:"replayed"`
			Bytes    int64  `json:"bytes"`
			Lost     []struct {
				Object  string `json:"object"`
				NamedBy string `json:"named_by"`
			} `json:"lost"`
		} `json:"objects"`
	}
	if err := client.patiently(*wait).post(context.Background(),
		"/backup?dir="+url.QueryEscape(*dir), &manifest); err != nil {
		return err
	}

	// The report NAMES WHAT IT GOT, per estate: a backup an operator
	// cannot inspect is one they have to trust, and the failure it hides — an
	// estate silently absent because this node does not run one — looks
	// exactly like success.
	fmt.Fprintf(stdout, "Backup written to %s on %s in %s\n",
		*dir, manifest.NodeID, manifest.FinishedAt.Sub(manifest.TakenAt).Round(time.Millisecond))
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\nWHAT\tFILE\tSIZE\tCONTENTS")
	if len(manifest.Stores) == 0 {
		fmt.Fprintln(w, "store\t—\t—\tnot on this node")
	}
	for _, st := range manifest.Stores {
		fmt.Fprintf(w, "store (%s)\t%s\t%s\t%d migrations\n",
			st.Estate, st.File, humanBytes(st.Bytes), len(st.Migrations))
	}
	// THE OBJECTS ARE THE COMPANY'S: absent only where no row a restore
	// brings back names a file at all. On the broker's own bucket they ride
	// that stream's snapshot, which is what the row names rather than a
	// directory — and their bytes are that snapshot's, so they are not
	// counted twice. The ones only a replayed record names — files written
	// while the backup ran — are said, since they are the ones a backup
	// carrying only its copy's would have missed.
	if o := manifest.Objects; o != nil {
		count := fmt.Sprintf("%d objects", o.Objects)
		if o.Replayed > 0 {
			count += fmt.Sprintf(" (%d written during the copy)", o.Replayed)
		}
		switch {
		case o.Stream != "":
			fmt.Fprintf(w, "objects\tin %s\t—\t%s\n", o.Stream, count)
		default:
			fmt.Fprintf(w, "objects\t%s/\t%s\t%s\n", o.Dir, humanBytes(o.Bytes), count)
		}
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
	if len(manifest.Streams) == 0 {
		// Said plainly: a backup of the store alone is not restorable on
		// its own, and an operator would otherwise file the output as a
		// backup of their company.
		//
		// Only a node that dialled an external NATS cluster produces this:
		// the queue owns that connection, so the engine has none to snapshot
		// the streams over, and they are backed up at the cluster. Every node
		// holds its own store, so the store is never the missing half.
		fmt.Fprintln(stdout, "\nThis node holds only part of the deployment's state: "+
			"its stream estate lives on the external NATS cluster it dialled. "+
			"Back that up there, from the same moment as this copy "+
			"(`nats account backup`).")
	}
	// LOST FILES ARE SAID, not left in the manifest: the backup completed
	// and restores everything else, and the one thing an operator can do
	// about these — restore them from an earlier backup, or upload them
	// again — needs to know which.
	if o := manifest.Objects; o != nil && len(o.Lost) > 0 {
		fmt.Fprintf(stdout, "\n%d file(s) could not be carried: the object store does not hold "+
			"their content, or holds other bytes than they record. Restore them from an "+
			"earlier backup or upload them again:\n", len(o.Lost))
		for _, l := range o.Lost {
			fmt.Fprintf(stdout, "  %s (object %s)\n", l.NamedBy, l.Object)
		}
	}
	return nil
}

// backupWait is how long the CLI waits for a copy: a ceiling, not an
// expectation. A backup is bounded by the store, the stream estate and, on
// `s3`, the company's files; a FIRST copy of a large company's objects is the
// long one (the design's large model holds ~1.15 TB, ~2.6 hours at 1 Gbit/s),
// and twelve hours is over four times that. Too short is the confusing
// failure: the engine finishes the copy whatever this command does, so an
// expired wait calls a good backup a failure.
const backupWait = 12 * time.Hour

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
