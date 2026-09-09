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
// THE STORE IS COPIED FIRST, and the order is now MANDATORY rather than
// preferable. The reason it used to have — "nothing in the store decides
// whether work runs again" — died the moment the store became the durable home
// of the company's own history: the tracker's rows are derived from an ordered
// log by an applier that keeps its checkpoint in the same transaction as the
// rows, so the store copy carries a POSITION, and a position is a claim about
// what has already been applied.
//
// Copy the store first and the artefact holds a store at position P beside a
// log that has since moved past it. A restore replays the difference; the gap
// is bounded and replayable, and it costs a few minutes of work being applied
// twice — which is free, because the applier's guard is monotone in the
// position.
//
// Copy the store LAST and the artefact holds a store at position P beside a
// log whose newest record is BELOW P. Every subsequent record then lands at a
// sequence the store has already marked applied, and the version-guarded
// upsert drops it SILENTLY. That is not a gap, it is a permanent hole that
// nothing reports — a restored company quietly missing whatever was written
// during the copy, for ever.
//
// So the order trades a bounded, replayable gap against a permanent, silent
// one. See docs/guides/backup.md.
//
// # The hold and the assertion
//
// The gap is only replayable while the log still HAS the records. The trim
// deletes a record once every counted node has committed past it, and a backup
// is not a counted node — so between the store copy and the stream snapshot the
// trim can delete exactly the records the artefact needs. Two things close it,
// and they are different kinds of thing:
//
//   - THE HOLD is a heartbeated pin in the fleet's own register, taken at the
//     pre-copy position and released when the manifest is written. It is what
//     makes the race not happen. Heartbeated, because a pin that outlived its
//     owner would stop the trim for ever and the log would grow to its ceiling.
//   - THE ASSERTION is `first_seq <= position + 1`, per domain, checked after
//     the stream snapshots from the bounds those snapshots already captured. It
//     is what makes "restorable" a CHECKABLE inequality rather than a hope, and
//     it is why the hold failing is a refused backup instead of a corrupt one.
//
// A backup that cannot assert it writes NO manifest, which is exactly how a
// reader tells debris from a backup.
package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
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

	// Domains is where each state-log domain's applier stood IN THE COPY,
	// keyed by domain stream.
	//
	// READ FROM THE VERIFIED COPY, never from the live database. The
	// checkpoint commits in the same transaction as the rows, so the
	// position inside a file is the only position that describes that file
	// — and the copy is taken while the applier is running, so the live
	// cursor names where the node was when the copy STARTED.
	//
	// What a restore does with it is the whole reason it is here: it is the
	// sequence the log has to replay from, and the number the assertion
	// below compares the log's own first sequence against.
	Domains map[string]statelog.Position `json:"domains,omitempty"`
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

	// Holds is the fleet's trim-hold register. Nil on a process with no
	// coordination, which is the standalone API case.
	//
	// WITHOUT IT THE BACKUP STILL RUNS, and the assertion is what makes
	// that safe: a trim that raced the copy is caught and the manifest is
	// refused, so the outcome is a failed backup rather than an
	// unrestorable one. The hold is what makes the race not happen; the
	// assertion is what makes its absence loud.
	Holds coord.HoldRegister

	// Backups is where a finished copy is announced to the fleet. Nil on a
	// process with no coordination, exactly as Holds is.
	//
	// WITHOUT IT THE TRIM CANNOT ADVANCE AT ALL, which is why it is here
	// rather than left to a caller: the trim's backup term refuses to
	// delete anything the newest backup does not hold, the node evaluating
	// that term is not necessarily this one, and a copy nobody announced
	// is a copy the fleet cannot see. A backup that ran and said nothing
	// leaves a log that grows for ever with a healthy backup schedule
	// behind it — the most confusing shape this gate has.
	Backups coord.BackupRegister

	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// Service takes backups. Nil when nothing on this node can be backed up.
type Service struct {
	store   *store.DB
	conn    *nats.Conn
	holds   coord.HoldRegister
	backups coord.BackupRegister
	nodeID  string
	now     func() time.Time
}

// HoldPurpose names this subsystem in the trim-hold register, so an operator
// reading a stalled trim sees what is pinning it rather than which node is.
const HoldPurpose = "backup"

// HoldHeartbeat is how often the hold is renewed while a copy runs.
//
// A QUARTER OF THE STALE BOUND, which is the derivation every other
// heartbeated fact in this estate uses: a holder may miss three beats and
// still be believed. Deriving it rather than picking a number is what keeps
// the two in step — a heartbeat slower than the bound would have the trim
// ignore a live backup's pin, which is the bug the pin exists to prevent.
const HoldHeartbeat = statelog.TrimHoldStale / 4

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
	return &Service{store: opts.Store, conn: opts.Conn, holds: opts.Holds,
		backups: opts.Backups, nodeID: opts.NodeID, now: now}
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

	// THE HOLD IS TAKEN BEFORE THE FIRST BYTE IS COPIED, at the position
	// this node's appliers stand at NOW — the live cursor, deliberately,
	// because the pin has to cover everything the copy is about to include
	// and the copy has not happened yet. A pin at the copy's own position
	// would be taken after the window it is meant to protect.
	release, err := s.hold(ctx)
	if err != nil {
		return Manifest{}, err
	}
	defer release()

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

	// THE POSITIONS COME OUT OF THE COPY, not out of the live database.
	// The checkpoint commits with the rows, so the position inside the file
	// is the only one that describes the file — and the applier ran
	// throughout the copy, so the live cursor names where this node was
	// when the copy started.
	if replicated := copyOf(manifest, store.EstateReplicated); replicated != "" {
		path := filepath.Join(dir, replicated)
		cursors, err := statelog.CursorsInFile(ctx, path)
		if err != nil {
			return Manifest{}, fmt.Errorf("backup: read the copy's own "+
				"checkpoints: %w", err)
		}
		manifest.Domains = cursors
		// READING A DATABASE CREATES SIDECARS, even for a read, so the
		// copy is folded back into one file — a -wal left inside the
		// artefact is debris carrying the reader's own umask rather
		// than this directory's deliberate 0700, and a restore script
		// looking for a set of named files finds one it does not know.
		if err := store.QuiesceCopy(ctx, path); err != nil {
			return Manifest{}, err
		}
		// AND THE DIGEST IS RETAKEN, because it now describes a file
		// this package has opened since the copy was verified. A
		// checksum that named the bytes before that open would answer
		// the ONE question a shipped artefact raises — is the file that
		// arrived the file that was verified — with a number that never
		// matched what was written.
		digest, err := store.FileDigest(path)
		if err != nil {
			return Manifest{}, err
		}
		size, err := os.Stat(path)
		if err != nil {
			return Manifest{}, fmt.Errorf("backup: measure the copy: %w", err)
		}
		for i := range manifest.Stores {
			if manifest.Stores[i].Estate == store.EstateReplicated {
				manifest.Stores[i].SHA256 = digest
				manifest.Stores[i].Bytes = size.Size()
			}
		}
	}

	if s.conn != nil {
		streams, err := snapshotStreams(ctx, s.conn, dir)
		if err != nil {
			return Manifest{}, err
		}
		manifest.Streams = streams
		// THE ASSERTION, and it is what makes "restorable" checkable.
		// The store is older than the streams by however long the copy
		// took, and that gap is only replayable while the log still
		// HOLDS the records — so a trim that ran inside the window is
		// caught here, from bounds the snapshots already captured, and
		// the manifest is refused rather than written over a hole.
		if err := assertReplayable(manifest); err != nil {
			return Manifest{}, err
		}
	}

	manifest.FinishedAt = s.now()
	if err := writeManifest(dir, manifest); err != nil {
		return Manifest{}, err
	}
	// AFTER THE MANIFEST, because the manifest is the claim: a point
	// announced before it would name an artefact a crash could leave as
	// debris, and the trim would delete the log against a backup that does
	// not exist. See [Service.announce] for why a failure here does not
	// fail the backup.
	s.announce(ctx, dir, manifest)
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

// hold pins the trim at this node's current positions and returns the release.
//
// A GOROUTINE RENEWS IT while the copy runs, because the trim ignores a hold
// older than its stale bound — a pin that stopped being renewed would be
// treated as a crashed holder, which is exactly right for a crashed holder and
// exactly wrong for a 40-minute copy of a large store.
//
// A node with no coordination gets a no-op release and no pin, which is
// honest: the standalone API case has no register to write to, and the
// assertion is what makes the missing pin loud rather than silent.
func (s *Service) hold(ctx context.Context) (func(), error) {
	if s.holds == nil || s.store == nil || s.store.Replicated() == nil {
		return func() {}, nil
	}
	live, err := s.livePositions(ctx)
	if err != nil {
		return nil, err
	}
	if len(live) == 0 {
		// A node with no registered domain has no log to pin. Not a
		// failure: it is every deployment before the first domain's
		// stream exists.
		return func() {}, nil
	}
	owner := coord.HoldOwner(s.nodeID, HoldPurpose)
	put := func(ctx context.Context) error {
		return s.holds.PutHold(ctx, coord.TrimHold{
			Owner: owner, At: s.now(), Streams: live,
			Reason: "a backup is copying this node's store",
		})
	}
	if err := put(ctx); err != nil {
		// REFUSED RATHER THAN CARRIED ON WITHOUT. A backup that cannot
		// pin the log is a backup whose own gap may be trimmed away
		// while it runs, and it would report success either way.
		return nil, fmt.Errorf("backup: pin the log's tail before copying: %w", err)
	}

	beat, stop := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(HoldHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-beat.Done():
				return
			case <-ticker.C:
				if err := put(beat); err != nil {
					// LOGGED AND CARRIED. A missed beat is
					// survivable — the stale bound allows
					// three — and the assertion is what
					// catches the case where it was not.
					log.WarnContext(beat, "backup_hold_not_renewed",
						"owner", owner, "error", err.Error())
				}
			}
		}
	}()
	return func() {
		stop()
		<-done
		// RELEASED WITH A CONTEXT THAT OUTLIVES THE FAILURE. The
		// failure being undone here is often the cancellation itself,
		// and a release that inherited a dead context would leave the
		// pin standing until the stale bound expired it.
		if err := s.holds.ReleaseHold(context.WithoutCancel(ctx), owner); err != nil {
			log.WarnContext(ctx, "backup_hold_not_released",
				"owner", owner, "error", err.Error(),
				"detail", "the trim ignores it after the stale bound, so this "+
					"delays trimming rather than stopping it")
		}
	}, nil
}

// livePositions is where this node's appliers stand right now.
func (s *Service) livePositions(ctx context.Context) (map[string]coord.Position, error) {
	replicated := s.store.Replicated()
	out := map[string]coord.Position{}
	if err := replicated.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT stream, generation, seq FROM statelog_cursor`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var stream string
			var generation, seq int64
			if err := rows.Scan(&stream, &generation, &seq); err != nil {
				return err
			}
			out[stream] = coord.Position{
				Stream: stream, Generation: uint32(generation), Seq: uint64(seq),
			}
		}
		return rows.Err()
	}); err != nil {
		return nil, fmt.Errorf("backup: read this node's log positions: %w", err)
	}
	return out, nil
}

// copyOf is the file name one estate's copy was written under.
func copyOf(m Manifest, estate store.Estate) string {
	for _, artifact := range m.Stores {
		if artifact.Estate == estate {
			return artifact.File
		}
	}
	return ""
}

// assertReplayable refuses a backup whose store copy names a position the
// captured log can no longer reach.
//
// # Why this is an inequality and not a warning
//
// A restore replays from the store's position to the log's head. That is only
// possible while the log still holds `position + 1`; below that the records
// are gone, and applying what remains writes state derived from a prefix that
// has a hole in it — silently, because every remaining record applies cleanly.
// There is nothing a restore could do about it afterwards, so the check is at
// the only moment anything can still be done: before the manifest is written.
//
// It needs NO NEW READ. The stream snapshot already captured the server's own
// state verbatim, and its first sequence is the bound.
func assertReplayable(m Manifest) error {
	if len(m.Domains) == 0 {
		return nil
	}
	for _, artifact := range m.Streams {
		at, tracked := m.Domains[artifact.Name]
		if !tracked {
			// Not a state-log domain: a mailbox, a namespace stream,
			// a coordination bucket. Nothing in the store names a
			// position on it.
			continue
		}
		var state struct {
			FirstSeq uint64 `json:"first_seq"`
		}
		if len(artifact.State) == 0 {
			return fmt.Errorf("backup: the snapshot of %s carries no state, so "+
				"whether the copy at position %d is still replayable cannot be "+
				"established", artifact.Name, at.Seq)
		}
		if err := json.Unmarshal(artifact.State, &state); err != nil {
			return fmt.Errorf("backup: read %s's captured stream state: %w",
				artifact.Name, err)
		}
		if state.FirstSeq > at.Seq+1 {
			return fmt.Errorf("backup: the store copy stands at %s sequence %d "+
				"and the captured log starts at %d — the records between them "+
				"were trimmed while the copy ran, so a restore would apply a "+
				"prefix with a hole in it and report nothing. Check that the "+
				"trim hold was taken (a node with no coordination cannot take "+
				"one) and take the backup again",
				artifact.Name, at.Seq, state.FirstSeq)
		}
	}
	return nil
}

// announce publishes what this backup covers, so the fleet's trim can see it.
//
// # Why a failure here does not fail the backup
//
// The artefact on disk is complete and restorable the moment its manifest is
// written; this call is what lets the log be trimmed against it. Those are
// different goods, and failing the backup because the announcement did not
// land would throw away the first to report the second — while an operator's
// cron reports a failed backup that in fact succeeded.
//
// The cost of a lost announcement is bounded and self-correcting: the trim's
// backup term goes on refusing, the log keeps a longer window than it needed
// to, and the next backup announces again. That is the safe direction, and it
// is why this is a WARN rather than an error.
func (s *Service) announce(ctx context.Context, dir string, manifest Manifest) {
	if s.backups == nil || len(manifest.Domains) == 0 {
		// NO DOMAINS IS NOT AN EMPTY ANNOUNCEMENT. A node with no state
		// log has nothing to say about how far a log is covered, and a
		// point claiming to reach no domain would be refused anyway —
		// see [coord.BackupPoint.Validate].
		return
	}
	point := coord.BackupPoint{
		Owner: s.nodeID,
		// THE INSTANT THE COPY STARTED, not the one it finished. Nothing
		// in the artefact is older than that, and the trim's age
		// comparison has to be against the state the copy describes
		// rather than against how long writing it took.
		At:       manifest.TakenAt,
		Dir:      dir,
		Streams:  map[string]coord.Position{},
		Verified: true,
	}
	for stream, at := range manifest.Domains {
		point.Streams[stream] = coord.Position{
			Stream: stream, Generation: at.Generation, Seq: at.Seq,
		}
	}
	if err := s.backups.PutBackupPoint(ctx, point); err != nil {
		log.WarnContext(ctx, "backup_unannounced", "dir", dir, "err", err)
	}
}
