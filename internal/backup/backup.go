// Package backup takes a restorable copy of everything a node holds.
//
// # Why this is in the engine at all
//
// A node's durable state is split across two estates that an outside tool
// cannot reach, for two different reasons.
//
// The STORE is two files on a data node — its own and the replicated estate
// beside it — each of which this process owns exclusively for the life of its
// handle, and the driver does not support a second process on a database file
// — so every SQLite-ecosystem backup tool, all of which work by being a
// second opener, is unavailable. Copying a file underneath a running engine is
// worse than unavailable: committed data lives in the file and its -wal, and a
// copy of either alone is torn.
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
//
// # The objects follow the log
//
// ADR-0075. A restore brings the copy back at position P on each declaring log
// and replays the log to the last sequence L its snapshot captured — the
// snapshot is point-in-time at the state its answer states, however much is
// appended while it transfers — and a build that can read a record the copy's
// applier deferred applies that too. So, for each declaring log,
//
//	carried ⊇ named_required(copy at P) ∪ { o : a record r, deferred or with
//	          P < seq(r) ≤ L, has Records(r) naming o }
//
// and the right-hand side holds every object the restored rows name: a row
// naming o after the replay either was touched by no record the restore applied
// (then it named o at P, in the copy) or the last applied record touching it
// set it to o (then that record names o). Every record is read, applied or not,
// so whichever records a restore's applier drops — an evicted writer's, a
// redelivery its guard skips — the union still holds what the restore names; a
// dropped record only adds an object nobody restores. No gate is repeated here,
// which is the point: the backup never decides which records apply.
//
// THE COPY'S OBJECTS ARE READ BEFORE THE STREAMS ARE SNAPSHOTTED (`s3`), which
// is what makes the loss rule exact. The collector deletes an object only once
// a complete read of its estate finds no declared table naming it — and past a
// grace measured from the object's UPLOAD, never from when it stopped being
// named, so a weeks-old object goes within the hour of the record that
// un-names it. An object the copy names that the store no longer holds was
// therefore un-named by a record the collector's estate had applied before the
// deletion, and so a record already on the log; the deletion came before this
// backup read the object, which came before the snapshot began, so the record
// lies in (P, L] — the window holds it, and it says whether the restore still
// names the object. THE WINDOW'S OBJECTS ARE READ
// AFTER, once L is fixed; they are young, and an upload precedes the record
// naming it, so the store holds them. On `nats` the bucket is a stream the
// backup snapshots AFTER every log, so every object a captured record names was
// in the bucket before the bucket's snapshot began, and a stat after the
// snapshots says whether the snapshot holds it.
//
// THE OBVIOUS ALTERNATIVE — wait for this node's applier to reach L and read
// the live rows — lands at some R ≥ L the applier's runs cannot pin, so a file
// written during the copy and rewritten before R is named by the restore and by
// neither read; and it needs an applier, which a node in maintenance or seal
// mode does not run. A node that dialled an external cluster takes no snapshot
// and has no L, so it carries what its copy names alone (docs/guides/backup.md).
//
// One residual is stated rather than hidden: a record a restore's applier reads
// past that rewrote a file is read here as having rewritten it, so an object
// the store lost whose file such a record touched is counted as replaced rather
// than lost. It needs an eviction and a store loss on one file inside one
// backup; the collector's daily audit reports the object either way.
package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
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
//
// AND IT IS RETURNED ONLY BEFORE A BYTE IS COPIED. A refusal wearing it wrote
// nothing into the directory (at most created it, empty), which is what lets
// POST /backup leave it out of the audit: a request refused before the copy
// began is not a backup that failed. A refusal that arrives once an estate is
// already in the directory is NOT one — the directory holds a partial copy
// somebody has to clear — so [Service.Take] returns it bare, and it is
// audited as the failed backup it is.
var ErrBadDestination = errors.New("backup: unusable destination")

// ErrNotEmpty reports a destination that already holds something.
var ErrNotEmpty = fmt.Errorf("%w: not empty", ErrBadDestination)

// ErrBackupRunning refuses a backup asked for while another runs on this node.
//
// TWO AT ONCE WOULD SHARE ONE TRIM HOLD. The hold is keyed on the node and the
// purpose ([HoldPurpose]) — which is what lets an operator read a stalled trim
// as "node-0's backup" — so a second backup's pin is the first's, and the first
// to finish releases the pin the other's gap still needs: the trim may then
// delete exactly the records the slower copy has to replay, and that backup is
// refused at its assertion after however long it spent copying. One at a time
// also keeps one copy's I/O, not two, competing with the applier's own commits.
//
// NOT A DESTINATION REFUSAL (the directory is fine) and returned before a
// byte is written, the directory included.
var ErrBackupRunning = errors.New("backup: a backup is already running on this node — " +
	"take this one once it finishes; two at once would share the node's one trim " +
	"hold, and the first to finish would release the other's pin")

// ErrLogGap refuses a backup whose store copy a restore could not replay to
// the snapshot: a record the log no longer holds lies between the copy's
// position and the snapshot's last sequence — the trim ran past the copy
// (the assertion), or the window a restore replays has a hole in it.
//
// ONE NAME FOR BOTH, because they are one fact to whoever reads the refusal —
// the log a restore needs is missing records — with one remedy: take the
// backup again, whose copy stands past them, and look at the trim hold if it
// is refused the same way. Nothing is wrong with the request, and a 500 would
// send an operator to a node log for a backup the next attempt takes.
var ErrLogGap = errors.New("backup: the log a restore replays from the copy is missing records")

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

	// Stores describes the database copies, one per estate: the node's own
	// store and the replicated estate beside it, which every node opens
	// together.
	Stores []StoreArtifact `json:"stores,omitempty"`

	// Streams is every stream captured, coordination buckets included.
	// Absent on a node that dialled an external NATS cluster, whose streams
	// are backed up at that cluster (see [Options.Conn]).
	Streams []StreamArtifact `json:"streams,omitempty"`

	// Objects describes the objects the backup owes — the store copy's
	// required tables' and the replayed records' — absent when it owes none.
	Objects *ObjectArtifact `json:"objects,omitempty"`

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
// ONE PER ESTATE THE NODE HOLDS, and a backup carries every one of them or it
// carries none: a data node is two databases, and a restore holding one of
// them has a company whose tracker and whose audit log are from different
// moments — which is not a partial restore, it is an inconsistent one.
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
	// Store is this node's database. Required: every node opens one, and
	// [New] refuses to build without it.
	Store *store.DB

	// Estate is which of the node's databases it HOLDS — the engine's
	// answer for its configuration ([HoldingFor] over
	// engine.HoldsEstate), never whether the replicated estate happens to
	// be open. Required: the zero value is refused.
	//
	// WHY NOT WHAT IS OPEN: a replicated estate an adoption holds closed
	// between its rename and its reopen, or one whose reopen failed, is
	// still one the node holds, with rows no other artefact of this backup
	// carries — and read off what is open it simply was not there, so the
	// backup left it out and wrote a manifest claiming to be complete.
	// Asked of the holding, it is an estate that must be copied and cannot
	// be right now, which is a refusal ([store.ErrNoEstate]) rather than a
	// smaller backup.
	Estate EstateHolding

	// Conn is the broker connection the streams are snapshotted over.
	//
	// Nil on a node that DIALLED an external NATS cluster, which is a real
	// deployment: the stream estate then belongs to that cluster and is
	// backed up there, with `nats account backup`, so the manifest holds
	// the store copies and no streams. The CLI says so when it prints one.
	Conn *nats.Conn

	// API is the JetStream API the streams are addressed in over Conn —
	// the embedded fleet's domain (see internal/jsapi). Required with a
	// Conn: a snapshot is a raw request past the client library, and one
	// sent to the wrong API is answered by nothing.
	API jsapi.API

	// NodeID is this node's RESOLVED id (config.ResolveNodeID), never the
	// raw `node.id` field. Required, and [New] refuses a blank one.
	//
	// It is more than a label in the manifest: it keys this node's trim
	// hold and the backup point announced to the fleet. The raw field is
	// empty on the node a container orchestrator runs, which names itself
	// through CREWLET_NODE_ID, and a blank id is a hold every such node
	// shares (one node's copy releasing another's pin) and a point the
	// register refuses, so the trim waits on a backup nobody can announce.
	NodeID string

	// Holds is the fleet's trim-hold register. Required: every node opens
	// the fleet store, and [New] refuses to build without it.
	//
	// The hold is what makes the race with the trim not happen; the
	// assertion (see the package doc) is what makes a hold that failed
	// loud, as a refused backup rather than an unrestorable one.
	Holds coord.HoldRegister

	// Backups is where a finished copy is announced to the fleet. Required,
	// exactly as Holds is.
	//
	// WITHOUT IT THE TRIM CANNOT ADVANCE AT ALL, which is why it is here
	// rather than left to a caller: the trim's backup term refuses to
	// delete anything the newest backup does not hold, the node evaluating
	// that term is not necessarily this one, and a copy nobody announced
	// is a copy the fleet cannot see. A backup that ran and said nothing
	// leaves a log that grows for ever with a healthy backup schedule
	// behind it — the most confusing shape this gate has.
	Backups coord.BackupRegister

	// Objects is how the objects the backup owes are reached. Nil is a node
	// that runs no object store, which is refused only when it owes an
	// object — see [ErrObjectsUnreachable].
	Objects *Objects

	// Keyring is the fleet keyring the state logs' records are signed under
	// (ADR-0031) — the engine's own (engine.Engine.RecordKeyring), never a
	// second reading of Tier A. Required where the node holds the replicated
	// estate, and refused at [New] when it cannot verify a record.
	//
	// WHAT THE WINDOW IS READ THROUGH: every record a restore replays past
	// the copy — on its log, and among the records the copy deferred — is a
	// signed frame, and the domain's reader of one record decodes the body
	// inside it. So each frame is opened and its signature checked first,
	// exactly as the applier a restore replays it through does, and a record
	// this node cannot verify refuses the backup rather than being guessed
	// at (see [objectPass.noteRecord]). A node holding no replicated estate
	// reads no log and needs none.
	Keyring statelog.Keyring

	// Metrics is where the copy's duration is recorded. Nil records
	// nothing, which is a legal deployment and leaves the `backup_taken`
	// log line as the only account of how long it took.
	Metrics *metrics.Recorder

	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// EstateHolding is which of a node's two databases it holds, and therefore
// which a backup of it copies.
type EstateHolding string

const (
	// HoldsNodeOnly is a node without the `data` role: its own file, and no
	// replicated estate at all.
	HoldsNodeOnly EstateHolding = "node_only"

	// HoldsReplicated is a data node: its own file and the replicated
	// estate beside it.
	HoldsReplicated EstateHolding = "replicated"
)

// Valid reports whether h is one of the two. The zero value is not: a holding
// nobody declared is not quietly the node's own file alone, which would be a
// data node's backup without its tracker.
func (h EstateHolding) Valid() bool { return h == HoldsNodeOnly || h == HoldsReplicated }

// HoldingFor is the holding of a node that does, or does not, hold the
// replicated estate — engine.HoldsEstate's answer for its configuration.
func HoldingFor(replicated bool) EstateHolding {
	if replicated {
		return HoldsReplicated
	}
	return HoldsNodeOnly
}

// Service takes backups.
type Service struct {
	store   *store.DB
	holding EstateHolding
	conn    *nats.Conn
	api     jsapi.API
	holds   coord.HoldRegister
	backups coord.BackupRegister
	nodeID  string
	objects *Objects
	// tables is the declared tables a copy is read against —
	// references.All, which only a test replaces (export_test.go), since a
	// table no migration creates can never be in the one list.
	tables []objstore.ReferenceTable
	// ring is the keyring each declaring log's records are opened with
	// ([Options.Keyring]).
	ring    statelog.Keyring
	metrics *metrics.Recorder
	now     func() time.Time
	// pageRead is called after each read of the copy's references — nil but
	// in a test (export_test.go), which counts the reads to see that the
	// copy is walked a page at a time rather than gathered first.
	pageRead func()
	// deferredRead is called after each read of the records a copy
	// deferred, with how many the read answered — nil but in a test, which
	// sees that the table is walked a page at a time.
	deferredRead func(rows int)
	// running is set while a backup runs, so a second is refused
	// ([ErrBackupRunning]).
	running atomic.Bool
	// volumeFree measures what a volume has free — [store.VolumeFree], which
	// only a case replaces (export_test.go), since what the machine running
	// it has free is no condition a case can state.
	volumeFree func(dir string) (int64, error)
}

// HoldPurpose names this subsystem in the trim-hold register, so an operator
// reading a stalled trim sees what is pinning it rather than which node is.
const HoldPurpose = "backup"

// New builds the service.
//
// A MISSING STORE, HOLD REGISTER OR BACKUP REGISTER IS REFUSED. `crewlet run`
// builds this beside an engine holding all three, so a nil is a wiring mistake,
// and a backup that quietly did less around it (the streams without the store,
// a copy with no trim pin or no announcement) would be an artefact the trim
// cannot see or one missing the node's own estate. A blank node id is refused
// for the same reason: see [Options.NodeID].
func New(opts Options) (*Service, error) {
	switch {
	case strings.TrimSpace(opts.NodeID) == "":
		return nil, errors.New("backup: Options.NodeID is required: it keys this " +
			"node's trim hold and its announced backup point, so pass the id " +
			"config.ResolveNodeID answers rather than the raw node.id field")
	case opts.Store == nil:
		return nil, errors.New("backup: Options.Store is required: a node's " +
			"backup starts with its own store, which the engine opens")
	case !opts.Estate.Valid():
		return nil, fmt.Errorf("backup: Options.Estate is %q, and it is required: "+
			"a backup copies every database the node holds, and only the engine "+
			"can say whether that includes the replicated estate — pass "+
			"backup.HoldingFor(engine.HoldsEstate(boot))", opts.Estate)
	case opts.Holds == nil:
		return nil, errors.New("backup: Options.Holds is required: without the " +
			"fleet's trim-hold register the trim can delete what the copy needs")
	case opts.Backups == nil:
		return nil, errors.New("backup: Options.Backups is required: a copy the " +
			"fleet is never told about is one the trim can never advance against")
	case opts.Conn != nil && !opts.API.Named():
		return nil, errors.New("backup: Options.API is required with a Conn: the " +
			"stream snapshots are raw requests, and only the API names where " +
			"they are answered")
	}
	if opts.Estate == HoldsReplicated {
		// ASKED OF EVERY DECLARING LOG'S DOMAIN NOW, rather than when its
		// first record turns up: a keyring that cannot verify one is a
		// wiring mistake, and found at the first record past a copy it
		// would be a backup that works until a file is written while one
		// runs.
		if _, err := verifiers(references.All, opts.Keyring); err != nil {
			return nil, fmt.Errorf("backup: Options.Keyring is required on a node holding "+
				"the replicated estate — the records a restore replays past the copy are "+
				"signed, and are read for the files they name only once verified; pass "+
				"the engine's own: %w", err)
		}
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{store: opts.Store, holding: opts.Estate, conn: opts.Conn, api: opts.API, holds: opts.Holds,
		backups: opts.Backups, nodeID: opts.NodeID, objects: opts.Objects,
		tables: references.All, ring: opts.Keyring, metrics: opts.Metrics, now: now,
		volumeFree: store.VolumeFree}, nil
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
	// ONE AT A TIME, refused before anything is written — the directory
	// included — and after the two refusals above, which are the caller's
	// own mistakes and answered the same whatever is running.
	if !s.running.CompareAndSwap(false, true) {
		return Manifest{}, ErrBackupRunning
	}
	defer s.running.Store(false)
	if err := emptyDir(dir); err != nil {
		return Manifest{}, err
	}

	started := s.now()
	manifest := Manifest{
		TakenAt:       started,
		NodeID:        s.nodeID,
		EngineVersion: version.String(),
	}

	// EVERY ESTATE THE NODE HOLDS, and none of them is optional. A data
	// node's backup with the node estate alone has every credential and no
	// tracker; with the replicated estate alone it has the tracker and no
	// audit log, no memory and no secret bootstrap. Either one restores
	// into a company that is missing half of itself while looking like a
	// backup.
	files, err := s.estates()
	if err != nil {
		return Manifest{}, err
	}
	// ROOM FOR THE LOG EACH COPY HOLDS BACK, before anything is pinned or
	// copied: see [Service.room].
	if err := s.room(files); err != nil {
		return Manifest{}, err
	}

	// THE HOLD IS TAKEN BEFORE THE FIRST BYTE IS COPIED, at the position
	// this node's appliers stand at NOW — the live cursor, deliberately,
	// because the pin has to cover everything the copy is about to include
	// and the copy has not happened yet. A pin at the copy's own position
	// would be taken after the window it is meant to protect.
	release, err := s.hold(ctx, files.replicated)
	if err != nil {
		return Manifest{}, err
	}
	defer release()

	// THE STORE FIRST, and the order is a decision rather than a
	// convenience — see the package doc. A store copy older than the
	// stream estate makes a restore repeat work; the other way round makes
	// it lose work.
	for _, db := range files.all() {
		name := storeFileNames[db.Estate()]
		info, err := db.Backup(ctx, filepath.Join(dir, name))
		if err != nil {
			return Manifest{}, storeRefusal(err, len(manifest.Stores))
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
	var (
		pass    *objectPass
		path    string
		cursors map[string]statelog.FileCursor
	)
	if replicated := copyOf(manifest, store.EstateReplicated); replicated != "" {
		path = filepath.Join(dir, replicated)
		var err error
		cursors, err = statelog.CursorsInFile(ctx, path)
		if err != nil {
			return Manifest{}, fmt.Errorf("backup: read the copy's own "+
				"checkpoints: %w", err)
		}
		// THE POSITIONS ONLY. A backup names where the copy got to so a
		// restore knows what the log still has to replay; which stream
		// instance it was is a snapshot's concern, because only a
		// snapshot is adopted into a running fleet.
		manifest.Domains = make(map[string]statelog.Position, len(cursors))
		for stream, cursor := range cursors {
			manifest.Domains[stream] = cursor.Position
		}
		// THE OBJECTS THE COPY'S REQUIRED TABLES NAME, read from the
		// copy for the reason the positions are: the rows a restore
		// brings back are the ones in this file. Copied now, before the
		// streams, where the backup copies them (`s3`); asked after once
		// the streams are snapshotted where they ride one (`nats`).
		if pass, err = s.newObjectPass(ctx, dir, path); err != nil {
			return Manifest{}, err
		}
		if err := pass.carryCopy(ctx); err != nil {
			return Manifest{}, err
		}
	}

	if s.conn != nil {
		// THE OBJECT STREAM LAST (`nats`), after every log: see
		// [snapshotStreams].
		var objectStream string
		if s.objects != nil {
			objectStream = s.objects.Stream
		}
		streams, err := snapshotStreams(ctx, s.conn, s.api, dir, objectStream)
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
	if pass != nil {
		// THE OBJECTS THE LOG A RESTORE REPLAYS NAMES, read once the
		// snapshots fix where that replay ends — see "The objects follow
		// the log" in the package doc.
		if err := pass.readWindow(ctx, s.conn, s.api, manifest, cursors); err != nil {
			return Manifest{}, err
		}
		// THE OBJECTS A STREAM CARRIES are asked about once its snapshot
		// is taken, and the window's are carried — see [objectPass.finish]
		// for why after and not before.
		var err error
		if manifest.Objects, err = pass.finish(ctx); err != nil {
			return Manifest{}, err
		}
		if retried := pass.retried.Load(); retried > 0 {
			log.WarnContext(ctx, "backup_objects_retried",
				"dir", dir, "retried", retried,
				"detail", "the object store failed to answer for these objects at first and "+
					"answered when asked again; the backup carries them, and a store that "+
					"needs a second try this often is one to look at before it stops answering")
		}
		if pass.removed > 0 {
			log.InfoContext(ctx, "backup_objects_removed_since_copy",
				"dir", dir, "removed", pass.removed,
				"detail", "the copy names files a record after it replaced or removed, whose "+
					"objects the store no longer holds; a restore replays those records and "+
					"names none of them")
		}
		// READING A DATABASE CREATES SIDECARS, even for a read, so the
		// copy is folded back into one file — a -wal left inside the
		// artefact is debris carrying the reader's own umask rather than
		// this directory's deliberate 0700, and a restore script looking
		// for a set of named files finds one it does not know. HERE,
		// after every read of the copy: the stream backup's question
		// about each object it names re-opens it once the streams are
		// snapshotted.
		if err = store.QuiesceCopy(ctx, path); err != nil {
			return Manifest{}, err
		}
		// AND THE DIGEST IS RETAKEN, because it now describes a file this
		// package has opened since the copy was verified. A checksum that
		// named the bytes before that open would answer the ONE question a
		// shipped artefact raises — is the file that arrived the file that
		// was verified — with a number that never matched what was written.
		digest, err := store.FileDigest(ctx, path)
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
	if lost := manifest.Objects; lost != nil && len(lost.Lost) > 0 {
		log.WarnContext(ctx, "backup_objects_lost",
			"dir", dir, "lost", len(lost.Lost), "objects", lost.Objects+len(lost.Lost),
			"detail", "a restore of this backup names files whose objects the store does "+
				"not hold, or holds as other bytes; the backup carries everything else and "+
				"lists them in its manifest, and the objects_missing alarm counts them")
	}

	manifest.FinishedAt = s.now()
	if err := writeManifest(dir, manifest); err != nil {
		return Manifest{}, err
	}
	// AFTER THE MANIFEST AND ONLY ON SUCCESS. What this measures is how
	// long the trim hold covered and how much I/O the copy spent competing
	// with the applier's own commits — both of which are properties of a
	// backup that FINISHED. A failed run's partial duration would be
	// mixed into the same distribution and would make the p95 an operator
	// sizes a maintenance window against shorter than any real copy.
	if s.metrics != nil {
		s.metrics.Observe(metrics.BackupDuration,
			manifest.FinishedAt.Sub(started), nil)
	}
	// AFTER THE MANIFEST, because the manifest is the claim: a point
	// announced before it would name an artefact a crash could leave as
	// debris, and the trim would delete the log against a backup that does
	// not exist. See [Service.announce] for why a failure here does not
	// fail the backup.
	s.announce(ctx, dir, manifest)
	var objects, reused, replayed int
	if manifest.Objects != nil {
		objects, reused, replayed = manifest.Objects.Objects, manifest.Objects.Reused,
			manifest.Objects.Replayed
	}
	log.InfoContext(ctx, "backup_taken",
		"dir", dir,
		"store_bytes", storeBytes(manifest),
		"streams", len(manifest.Streams),
		"stream_bytes", snapshotSize(manifest.Streams),
		"objects", objects,
		"objects_reused", reused,
		"objects_replayed", replayed,
		"object_bytes", objectBytes(manifest),
		"took", manifest.FinishedAt.Sub(started).String())
	return manifest, nil
}

// held is the database handles a backup copies: the node's own store, and the
// replicated estate beside it on a node that holds one.
type held struct {
	node       *store.DB
	replicated *store.DB
}

// all is every handle, in copy order: the node's own first.
func (h held) all() []*store.DB {
	if h.replicated == nil {
		return []*store.DB{h.node}
	}
	return []*store.DB{h.node, h.replicated}
}

// estates is every database a backup copies: the node's own store, and the
// replicated estate where the node HOLDS one.
//
// WHAT THE NODE HOLDS, NOT WHAT HAPPENS TO BE OPEN (see [Options.Estate]). A
// held estate that is not open is REFUSED with [store.ErrNoEstate]: the backup
// would otherwise be missing every replicated row and say nothing. And an open
// estate on a node that holds none is refused too, because a backup that copied
// what the engine says and skipped what is actually open would be as silent the
// other way.
func (s *Service) estates() (held, error) {
	replicated, err := s.store.ReplicatedDB()
	switch {
	case s.holding == HoldsNodeOnly && err == nil:
		return held{}, fmt.Errorf("backup: the replicated estate is open on this "+
			"node, which is configured to hold none — a backup copies what the "+
			"node holds, and this one would leave an open file (%s) out",
			replicated.Path())
	case s.holding == HoldsNodeOnly:
		return held{node: s.store}, nil
	case err != nil:
		return held{}, fmt.Errorf("backup: this node holds the replicated estate "+
			"and it is not open — an adoption is replacing its file, or reopening "+
			"it failed; take the backup again once the node reports its estate "+
			"serving: %w", err)
	}
	return held{node: s.store, replicated: replicated}, nil
}

// room refuses a backup a database's own volume has no room for: the
// write-ahead log its copy will hold back there.
//
// A copy holds its file's log for its whole run (store.DB.Copies), so the log
// grows by what the node writes while the copy runs, on the volume the node
// commits to — and once that volume is full the node stops committing for the
// rest of the copy, since nothing ends the hold before the copy does. So each
// file's own volume must have room for what the last copy of it measured a copy
// holding back, scaled to the file as it stands ([store.DB.CopyHoldsBack]), and
// the backup is refused before a byte is pinned or copied otherwise. The copies
// run one after another, each holding its own file's log alone, so each file
// is weighed on its own.
//
// THE COPIES THEMSELVES ARE NOT COUNTED HERE. They land in the directory the
// operator named, and a copy that fills it fails there and removes what it
// wrote, while the log it held grows on the live volume until the copy ends
// whichever volume the copy lands on. Counted on every live volume, since
// nothing tells two volumes of one pool apart for certain, the copies would
// refuse every backup to a disk of its own from a node short of a second
// copy's room beside its store, which such a backup does not need.
//
// The figure is the last copy's: a node that has copied nothing since it
// started has no measure, and a write burst past the rate the last copy saw is
// not foreseen. `wal_large` and `volume_low` name both while they happen.
func (s *Service) room(files held) error {
	for _, db := range files.all() {
		info, err := os.Stat(db.Path())
		if err != nil {
			return fmt.Errorf("backup: measure %s for the log its copy holds back: %w", db.Path(), err)
		}
		holds := db.CopyHoldsBack(info.Size())
		if holds == 0 {
			continue
		}
		dir := filepath.Dir(db.Path())
		free, err := s.volumeFree(dir)
		if err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		if free < holds {
			return fmt.Errorf("backup: the volume holding %s (%s) has %d bytes free, and its "+
				"copy holds back about %d bytes of its write-ahead log there while it runs, as "+
				"the last copy of it measured — the node commits to that volume and would stop "+
				"once the log filled it; make room on it, or take the backup while the node "+
				"writes less", db.Path(), dir, free, holds)
		}
	}
	return nil
}

// storeRefusal classifies a store copy that failed, copied estates into.
//
// The store's own destination refusals join this package's, so one question
// answers for the whole subsystem — but only while the directory is still
// empty. Once an estate has been copied the directory holds part of a backup,
// and a refusal then is a failed backup with debris rather than a request
// refused before it began: [ErrBadDestination] promises nothing was copied.
func storeRefusal(err error, copied int) error {
	destination := errors.Is(err, store.ErrBackupExists) || errors.Is(err, store.ErrBadBackupPath)
	if !destination {
		return err
	}
	if copied > 0 {
		return fmt.Errorf("backup: refused after %d estate(s) were already copied, "+
			"so the directory holds a partial copy with no manifest — clear it "+
			"before naming it again: %w", copied, err)
	}
	return fmt.Errorf("%w: %w", ErrBadDestination, err)
}

// destinationFault reports whether preparing a directory failed because of
// the path the caller named — one through a regular file, a parent that does
// not exist, one this user may not write, a read-only mount, a name too long
// or looping — rather than because the host itself failed.
//
// Deliberately a list of the caller's errors rather than of the host's: an
// I/O error or a full disk is not fixed by naming another directory, and an
// errno nobody listed is the engine's until someone decides otherwise, which
// errs toward an operator looking at the node rather than at a typo.
func destinationFault(err error) bool {
	for _, target := range []error{
		fs.ErrNotExist, fs.ErrExist, fs.ErrPermission,
		syscall.ENOTDIR, syscall.EROFS, syscall.ENAMETOOLONG, syscall.ELOOP,
		syscall.EINVAL,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// prepareFailure wraps a failure to prepare dir: a destination refusal when
// it is the caller's path at fault, the engine's failure otherwise.
func prepareFailure(what, dir, remedy string, err error) error {
	if destinationFault(err) {
		return fmt.Errorf("%w: cannot %s %s: %w — %s", ErrBadDestination, what, dir, err, remedy)
	}
	return fmt.Errorf("backup: %s %s: %w", what, dir, err)
}

// emptyDir makes dir if it is absent, refuses it if it holds anything, and
// makes sure it is the caller's alone to read.
func emptyDir(dir string) error {
	// 0700 because of what lands in here: the store copy carries every
	// sealed credential the secret store bootstrapped and every seat's
	// memory, and the coordination snapshot carries the company's
	// credentials outright.
	//
	// A FAILURE THAT IS ABOUT THE PATH says so with ErrBadDestination
	// (destinationFault): a path through a regular file, a parent that does
	// not exist or cannot be written, a read-only mount — each is a fact
	// about the directory the caller named, and the fix is to name another.
	// Returned bare, they read as the engine failing, which sent an operator
	// who typed /etc/passwd to the engine's log for a mistake in their own
	// request. An I/O error or a full disk stays the engine's failure.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return prepareFailure("create", dir,
			"name a directory the engine's host can create and write", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return prepareFailure("read", dir,
			"name a directory the engine's host can read and write", err)
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
		return prepareFailure("chmod 0700", dir,
			"name a directory the engine's user owns", err)
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
// RENEWED WHILE THE COPY RUNS, because the trim ignores a hold older than its
// stale bound — a pin that stopped being renewed would be treated as a crashed
// holder, which is exactly right for a crashed holder and exactly wrong for a
// 40-minute copy of a large store. The pin is [statelog.HoldTail]'s, the one
// implementation every holder of the tail shares, renewed on
// [statelog.TrimHoldHeartbeat].
//
// A node holding no replicated estate — replicated is nil — gets a no-op
// release and no pin: there is no applier cursor to read a position from, so
// there is nothing to pin at.
func (s *Service) hold(ctx context.Context, replicated *store.DB) (func(), error) {
	if replicated == nil {
		return func() {}, nil
	}
	live, err := s.livePositions(ctx, replicated)
	if err != nil {
		return nil, err
	}
	if len(live) == 0 {
		// A node with no registered domain has no log to pin. Not a
		// failure: it is every deployment before the first domain's
		// stream exists.
		return func() {}, nil
	}
	pin, err := statelog.HoldTail(ctx, statelog.TailHold{
		Register: s.holds,
		Owner:    coord.HoldOwner(s.nodeID, HoldPurpose),
		Streams:  live,
		Reason:   "a backup is copying this node's store",
		Every:    statelog.TrimHoldHeartbeat,
		Now:      s.now,
		Logger:   log,
	})
	if err != nil {
		// REFUSED RATHER THAN CARRIED ON WITHOUT. A backup that cannot
		// pin the log is a backup whose own gap may be trimmed away
		// while it runs, and it would report success either way.
		return nil, fmt.Errorf("backup: pin the log's tail before copying: %w", err)
	}
	return pin.Release, nil
}

// livePositions is where the appliers of the replicated estate a backup copies
// stand right now.
func (s *Service) livePositions(ctx context.Context, replicated *store.DB) (map[string]coord.Position, error) {
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
// state verbatim, and its first sequence is the bound — asked through
// [statelog.Replayable], the predicate every reader of that boundary shares,
// so a backup and the node that restores it cannot disagree about it by one.
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
		first, _, err := capturedBounds(artifact)
		if err != nil {
			return err
		}
		if !statelog.Replayable(at.Seq, first) {
			return fmt.Errorf("%w: the store copy stands at %s sequence %d "+
				"and the captured log starts at %d: the records between them "+
				"were trimmed while the copy ran, so a restore would apply a "+
				"prefix with a hole in it and report nothing. The trim hold did "+
				"not cover the copy; look for statelog_hold_not_renewed in this "+
				"node's log (a hold that misses its renewals is read as "+
				"abandoned), then take the backup again",
				ErrLogGap, artifact.Name, at.Seq, first)
		}
	}
	return nil
}

// previousObjects is the object directory of this node's previous backup,
// when it is still on this host and is not dir itself — the source a backup
// takes the objects it already holds from — or empty.
//
// THIS NODE'S OWN ROW of the backup register, whose Dir is a path on this
// host: another owner's names a directory on another machine. A register that
// cannot be read costs the reuse and nothing else, since every object can
// still be read from the store.
func (s *Service) previousObjects(ctx context.Context, dir string) string {
	points, err := s.backups.BackupPoints(ctx)
	if err != nil {
		log.WarnContext(ctx, "backup_previous_unknown", "error", err,
			"detail", "every object is read from the store rather than reused")
		return ""
	}
	for _, p := range points {
		if p.Owner != s.nodeID || p.Dir == "" || filepath.Clean(p.Dir) == filepath.Clean(dir) {
			continue
		}
		prev := filepath.Join(p.Dir, objectsDirName)
		if info, err := os.Stat(prev); err == nil && info.IsDir() {
			return prev
		}
	}
	return ""
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
	if len(manifest.Domains) == 0 {
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
		// THE WHOLE ARTEFACT, the same three sums the `backup_taken` line
		// logs: what an operator weighs against the disk it went to and
		// the link it is about to be shipped over. The objects were left
		// out, which for a company with files is most of it.
		Bytes: storeBytes(manifest) + snapshotSize(manifest.Streams) + objectBytes(manifest),
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
