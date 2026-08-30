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
// throughout — so the pieces are separated by the time the copy took. The
// skew is bounded and its direction is the safe one: the store is copied
// FIRST, so what a restored node knows about its own past is never newer than
// the fleet state it is restored beside. The reverse — a coordination ledger
// that has written off work whose local record is missing — is the one that
// loses work rather than repeating it. See docs/guides/backup.md.
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

// storeFileName is the store copy inside a backup directory.
const storeFileName = "store.db"

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
	Store *StoreArtifact `json:"store,omitempty"`

	// Streams is every stream captured, coordination buckets included.
	// Absent on a node with no broker reachable.
	Streams []StreamArtifact `json:"streams,omitempty"`
}

// StoreArtifact describes the database copy inside a backup.
type StoreArtifact struct {
	// File is the copy, relative to the backup directory.
	File string `json:"file"`

	// Source is the path it was copied from, for an operator matching a
	// backup to the node that produced it.
	Source string `json:"source"`

	// Bytes is its size.
	Bytes int64 `json:"bytes"`

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
	if s.store != nil {
		info, err := s.store.Backup(ctx, filepath.Join(dir, storeFileName))
		if err != nil {
			// The store's own destination refusals join this package's,
			// so one question answers for the whole subsystem.
			if errors.Is(err, store.ErrBackupExists) || errors.Is(err, store.ErrBadBackupPath) {
				return Manifest{}, fmt.Errorf("%w: %w", ErrBadDestination, err)
			}
			return Manifest{}, err
		}
		manifest.Store = &StoreArtifact{
			File:       storeFileName,
			Source:     s.store.Path(),
			Bytes:      info.Bytes,
			Migrations: info.Migrations,
		}
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

// emptyDir makes dir if it is absent and refuses it if it holds anything.
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

// storeBytes reports the store copy's size, or 0 when there is none.
func storeBytes(m Manifest) int64 {
	if m.Store == nil {
		return 0
	}
	return m.Store.Bytes
}
