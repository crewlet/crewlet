// Package backup takes a restorable copy of everything a node holds.
//
// # Why this is in the engine at all
//
// A node's durable state is split across two estates that an outside tool
// cannot reach, for two different reasons.
//
// The STORE is one file this process owns exclusively for the life of the
// handle, and the driver does not support a second process on a database file
// — so every SQLite-ecosystem backup tool, all of which work by being a
// second opener, is unavailable. Copying the file underneath a running engine
// is worse than unavailable: committed data lives in the file and its -wal,
// and a copy of either alone is torn.
//
// The STREAM ESTATE — the agent mailboxes and every coordination bucket, which
// is where the fleet's leases, ledgers, counters and sealed credentials live —
// runs on a broker EMBEDDED in this process that binds no socket. There is no
// address to point the `nats` CLI at.
//
// Both are reachable from exactly one place: inside the running engine. That
// is what this package is, and it is why `crewlet backup` is a client of a
// route rather than a command that opens files.
//
// # What a backup is
//
// A directory holding the store copy, one snapshot per stream, and a
// manifest. The manifest is written LAST and its presence is the claim: a
// directory with one is a complete backup, a directory without one is the
// debris of an attempt that did not finish. Nothing else in the directory
// says so, and an operator restoring from a half-written backup is the
// failure this ordering exists to prevent.
//
// # What it is a copy OF
//
// A moment, not an instant. The engine is not stopped, the store copy and
// each stream snapshot are taken one after another, and work continues
// throughout — so the pieces are separated by however long the copy took.
//
// THE STORE IS COPIED FIRST, and the order is the decision. It leaves the
// store OLDER than the stream estate, and the reason that is the safe
// direction is that nothing in the store decides whether work runs again:
// the completion ledger, the delivery dedupe and the fire claims all moved
// to coordination (migrations 0010–0013), and they travel with the streams.
// So an older store costs a bounded gap in one seat's own memory and audit —
// a few episodes and conversation rows that the ledger already counts as
// done — and changes nothing about what the fleet will do next.
//
// The reverse order costs more. A stream estate older than the store is a
// ledger that has NOT recorded work whose episode the store already holds:
// the trigger is still unacked in its mailbox, so it is redelivered and run
// again, and the duplicate reaches whoever the seat was talking to. Between
// a small gap in a seat's memory and a repeated post to somebody's issue
// tracker, the gap is the cheaper failure and the one that stays inside the
// company. See docs/guides/backup.md.
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/version"
)

var log = logging.Get("backup")

// ManifestName is the file whose presence marks a directory as a finished
// backup. Exported because it is the thing an operator, a restore procedure
// or a shipping script has to look for — see docs/guides/backup.md.
const ManifestName = "manifest.json"

// storeFileNames are the database copies inside a backup directory, one per
// estate. Named rather than derived, because these strings are what a restore
// procedure and every shipping script look for — see docs/guides/backup.md.
var storeFileNames = map[store.Estate]string{
	store.EstateNode:       "store.db",
	store.EstateReplicated: "store-replicated.db",
}

// streamDirName holds the stream snapshots.
const streamDirName = "streams"

// ErrBadDestination marks every refusal that is about the destination the
// CALLER named rather than about this node.
//
// One sentinel over the whole class, because its consumers ask exactly one
// question — "is this the operator's mistake or ours?" — and answering it by
// listing every specific refusal is how a new refusal silently starts
// reporting as an engine failure.
var ErrBadDestination = errors.New("backup: unusable destination")

// ErrNotEmpty reports a destination that already holds something.
var ErrNotEmpty = fmt.Errorf("%w: not empty", ErrBadDestination)

// Manifest describes one backup: what was captured, from where, and when.
//
// It is the artifact a restore reads, so every field here is something a
// restore or an operator checking one actually needs — not a record of the
// run for its own sake.
type Manifest struct {
	// TakenAt is when the backup started. The start rather than the end,
	// because it is the bound that matters: nothing in this backup is
	// older than the state at this instant.
	TakenAt time.Time `json:"taken_at"`

	// FinishedAt is when the manifest was written.
	FinishedAt time.Time `json:"finished_at"`

	// NodeID is the node this was taken from. Load-bearing on a clustered
	// embedded stream, where replicas are placed by server name: a node
	// restored under a different name is a new peer, and its old replicas
	// are orphaned.
	NodeID string `json:"node_id"`

	// EngineVersion is the binary that took it. A restore into an older
	// binary than the schema in the copy is not supported, and this is
	// what lets an operator see that before trying.
	EngineVersion string `json:"engine_version"`

	// Store describes the database copy, absent on a node running without
	// one.
	Stores []StoreArtifact `json:"stores,omitempty"`

	// Streams is every stream captured, coordination buckets included.
	// Absent on a node with no broker reachable.
	Streams []StreamArtifact `json:"streams,omitempty"`
}

// StoreArtifact describes one database copy inside a backup.
//
// ONE PER ESTATE, and a backup carries every estate or it carries none: a node
// is two databases, and a restore holding one of them has a company whose
// tracker and whose audit log are from different moments — which is not a
// partial restore, it is an inconsistent one.
type StoreArtifact struct {
	// Estate is which of the node's databases this copy is.
	Estate store.Estate `json:"estate"`

	// File is the copy, relative to the backup directory.
	File string `json:"file"`

	// Source is the path it was copied from, for an operator matching a
	// backup to the node that produced it.
	Source string `json:"source"`

	// Bytes is its size.
	Bytes int64 `json:"bytes"`

	// SHA256 is the hex digest of the copy as it was written, taken by the
	// engine at the moment the copy passed its integrity check. What it
	// answers is the one question a shipped artifact raises: whether the
	// file that arrived is the file that was verified.
	SHA256 string `json:"sha256"`

	// Migrations is the schema the copy carries. What a restore brings
	// back, and the thing to compare against a binary before restoring
	// into it.
	Migrations []string `json:"migrations"`
}

// Options configures a [Service].
type Options struct {
	// Store is this node's database. Nil on a node running without one,
	// which is a real deployment — the backup then covers the stream
	// estate alone and says so in the manifest.
	Store *store.DB

	// Conn is the broker connection the streams are snapshotted over. Nil
	// when this process has none, which is the standalone API case.
	Conn *nats.Conn

	// NodeID names the node in the manifest.
	NodeID string

	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// Service takes backups. Nil when nothing on this node can be backed up.
type Service struct {
	store  *store.DB
	conn   *nats.Conn
	nodeID string
	now    func() time.Time
}

// New builds the service, or returns nil when there is nothing to back up.
//
// NIL RATHER THAN AN EMPTY SERVICE, matching the other optional surfaces in
// this tree: a process with neither a store nor a broker cannot produce a
// backup of anything, and a service that cheerfully wrote a manifest
// describing nothing would be the worst possible answer — an operator would
// have a file that says "backup" and no data.
func New(opts Options) *Service {
	if opts.Store == nil && opts.Conn == nil {
		return nil
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{store: opts.Store, conn: opts.Conn, nodeID: opts.NodeID, now: now}
}

// Take writes a complete backup into dir and returns its manifest.
//
// dir must not exist, or must be empty. A backup is a SET of files whose
// meaning depends on being one set — a store copy from today beside stream
// snapshots from last week is not a backup of anything — so writing into an
// occupied directory is refused rather than merged.
//
// EVERY PART OR NONE. A backup missing an estate is not a partial backup, it
// is an unrestorable one: the store alone loses every lease, ledger and
// credential, and the streams alone lose every seat's memory. So a failure
// anywhere leaves the directory WITHOUT a manifest, which is exactly how a
// reader tells debris from a backup.
func (s *Service) Take(ctx context.Context, dir string) (Manifest, error) {
	if s == nil {
		return Manifest{}, errors.New("backup: this node has neither a store nor a broker to back up")
	}
	if dir == "" {
		return Manifest{}, errors.New("backup: no destination directory")
	}
	if !filepath.IsAbs(dir) {
		// An engine's working directory is not something an operator
		// driving it over HTTP can see, so a relative path would land
		// somewhere they did not choose.
		return Manifest{}, fmt.Errorf("%w: %s is relative; name an absolute directory, "+
			"because it is resolved on the engine's host rather than yours",
			ErrBadDestination, dir)
	}
	if err := emptyDir(dir); err != nil {
		return Manifest{}, err
	}

	started := s.now()
	manifest := Manifest{
		TakenAt:       started,
		NodeID:        s.nodeID,
		EngineVersion: version.String(),
	}

	// THE STORE FIRST, and the order is a decision rather than a
	// convenience — see the package doc. A store copy older than the
	// stream estate makes a restore repeat work; the other way round makes
	// it lose work.
	// BOTH ESTATES, and neither is optional. A backup with the node
	// estate alone has every credential and no tracker; with the
	// replicated estate alone it has the tracker and no audit log, no
	// memory and no secret bootstrap. Either one restores into a company
	// that is missing half of itself while looking like a backup.
	for _, db := range estates(s.store) {
		name := storeFileNames[db.Estate()]
		info, err := db.Backup(ctx, filepath.Join(dir, name))
		if err != nil {
			// The store's own destination refusals join this package's,
			// so one question answers for the whole subsystem.
			if errors.Is(err, store.ErrBackupExists) || errors.Is(err, store.ErrBadBackupPath) {
				return Manifest{}, fmt.Errorf("%w: %w", ErrBadDestination, err)
			}
			return Manifest{}, err
		}
		manifest.Stores = append(manifest.Stores, StoreArtifact{
			Estate:     db.Estate(),
			File:       name,
			Source:     db.Path(),
			Bytes:      info.Bytes,
			SHA256:     info.SHA256,
			Migrations: info.Migrations,
		})
	}

	if s.conn != nil {
		streams, err := snapshotStreams(ctx, s.conn, dir)
		if err != nil {
			return Manifest{}, err
		}
		manifest.Streams = streams
	}

	manifest.FinishedAt = s.now()
	if err := writeManifest(dir, manifest); err != nil {
		return Manifest{}, err
	}
	log.InfoContext(ctx, "backup_taken",
		"dir", dir,
		"store_bytes", storeBytes(manifest),
		"streams", len(manifest.Streams),
		"stream_bytes", snapshotSize(manifest.Streams),
		"took", manifest.FinishedAt.Sub(started).String())
	return manifest, nil
}

// estates is every database handle a node holds, in copy order, and empty on
// a node running without a store.
func estates(db *store.DB) []*store.DB {
	if db == nil {
		return nil
	}
	out := []*store.DB{db}
	if peer := db.Replicated(); peer != nil {
		out = append(out, peer)
	}
	return out
}

// emptyDir makes dir if it is absent, refuses it if it holds anything, and
// makes sure it is the caller's alone to read.
func emptyDir(dir string) error {
	// 0700 because of what lands in here: the store copy carries every
	// sealed credential the secret store bootstrapped and every seat's
	// memory, and the coordination snapshot carries the company's
	// credentials outright.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("backup: create %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("backup: read %s: %w", dir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %s holds %d entries — name a directory of its own, "+
			"so one backup cannot be read as part of another",
			ErrNotEmpty, dir, len(entries))
	}
	// AND 0700 EVEN IF IT WAS ALREADY THERE. MkdirAll's mode applies only
	// to a directory it creates, so an operator's `mkdir -p` under the
	// usual umask leaves a directory anyone can list — and every file
	// written below is 0600, so what leaks is not the credentials
	// themselves but the shape of the estate: that this company keeps a
	// secrets bucket, how large it is, when it was last copied.
	//
	// Tightened rather than refused, because refusing would reject
	// `mkdir -p /backups/today && crewlet backup -dir /backups/today`,
	// which is how anyone would drive this. The directory is ours by then:
	// it was required to be empty two lines above.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("backup: make %s private: %w", dir, err)
	}
	return nil
}

// writeManifest writes the manifest last, through a temporary name.
//
// LAST AND ATOMICALLY, because its presence is the only thing that says the
// backup is complete. Written in place, a crash mid-write would leave a
// truncated manifest — a file that exists, making the claim, and cannot be
// parsed to act on it.
func writeManifest(dir string, manifest Manifest) error {
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: encode the manifest: %w", err)
	}
	body = append(body, '\n')
	final := filepath.Join(dir, ManifestName)
	part := final + ".part"
	if err := os.WriteFile(part, body, 0o600); err != nil {
		return fmt.Errorf("backup: write %s: %w", part, err)
	}
	if err := os.Rename(part, final); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("backup: place %s: %w", final, err)
	}
	return nil
}

// storeBytes reports the size of every database copy, or 0 when there is none.
func storeBytes(m Manifest) int64 {
	var total int64
	for _, s := range m.Stores {
		total += s.Bytes
	}
	return total
}
