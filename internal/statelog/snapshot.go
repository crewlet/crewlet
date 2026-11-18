package statelog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// The snapshot repository's own constants.
const (
	// SnapshotsKept is how many snapshots a node retains: one, because a
	// second copy on the same disk protects against nothing the first does
	// not. Every data node taking its own is what survives losing a donor,
	// which is why the trim's snapshot term is the k-th highest.
	SnapshotsKept = 1

	// SnapshotLagSlack is how far behind the log a node may be and still
	// donate: a copy taken further behind saves a joiner less replay than
	// it costs to transfer.
	SnapshotLagSlack = 1_000

	// SnapshotDonorsRequired is how many verified snapshots the fleet must
	// hold before the trim may pass them: two, so losing any one donor
	// leaves a usable artefact. The minimum over the counted set would
	// block on any node not yet snapshotted; the maximum makes one disk the
	// fleet's recovery plan. A fleet whose log counts one node is satisfied
	// by construction: its recovery artefact is a backup.
	SnapshotDonorsRequired = 2

	// SnapshotFreeSpaceFactor is the free space the snapshot volume must
	// hold for a take, as a multiple of the replicated estate's file. By
	// default the copy shares the live database's volume, so filling it
	// would stop the applier committing; the margin makes the loop refuse.
	//
	// The write-ahead log a copy holds back ([store.DB.CopyHoldsBack], as
	// the last copy measured it) is added on top, and the live file's own
	// volume must hold that log alone: a copy pins every checkpoint for its
	// run. The tenth covers it only while the node writes under a tenth of
	// the copy's read rate (~10 MB/s at the ~104 MB/s measured on the
	// pinned driver), and stands for it until a copy has measured it.
	SnapshotFreeSpaceFactor = 1.1

	// SnapshotChunkBytes and SnapshotTransferWindow size the transfer for a
	// network, unlike the backup transfer's in-process stop-and-wait at
	// 128 KiB, which across two broker hops is 128 KiB per round trip. A
	// 32 MiB credit window keeps a 100 MB/s pipe full at any round trip
	// under 335 ms, at 32 MiB of buffer per side.
	SnapshotChunkBytes     = 1 << 20
	SnapshotTransferWindow = 32
)

// SkipReason is why a snapshot was not taken. Every one is PUBLISHED, not
// just logged: a fleet that silently stopped snapshotting is invisible until
// a node needs one, and `deferred` (a rolling upgrade), `insufficient_space`
// and `sole_node` (a two-node fleet after an eviction) are routine.
type SkipReason string

const (
	// SkipLagging — this node is further behind the log than
	// [SnapshotLagSlack].
	SkipLagging SkipReason = "lagging"

	// SkipUnhydrated — this node has not drained the log since its applier
	// started, or has stalled since ([Health.Drained]), so it holds a prefix
	// rather than a state. A drained node's later distance is `lagging`.
	SkipUnhydrated SkipReason = "unhydrated"

	// SkipSoleNode — the fleet counts no other node on the estate's logs,
	// so there is nobody to donate to. A single node's recovery artefact is
	// a backup, which the trim's backup term gates, so the snapshot term
	// does not deadlock a solo fleet.
	SkipSoleNode SkipReason = "sole_node"

	// SkipInsufficientSpace — the snapshot volume has less than the
	// margin free.
	SkipInsufficientSpace SkipReason = "insufficient_space"

	// SkipDeferred — this node holds a record it cannot decode.
	//
	// The precondition that makes the scrub safe: the deferred table is
	// scrubbed out of every artefact, so a checkpoint above a RETAINED
	// record would hand an adopter a resume point above bytes it never
	// received, a silent hole. Invariant: inside a verified snapshot, every
	// domain's applied-through equals its checkpoint.
	SkipDeferred SkipReason = "deferred"

	// SkipRecent — the newest local snapshot is younger than the interval
	// AND names every domain this node runs at the generation it stands
	// at. One that does not is refused wholesale by every joiner, whatever
	// its age, so the node takes a complete one and logs
	// `statelog_snapshot_recent_but_short`.
	SkipRecent SkipReason = "recent"

	// SkipAheadOfLog — this node's checkpoint sits PAST the log's last
	// sequence, so it holds another stream's history (a deleted and
	// recreated stream), and an artefact would name a position nobody can
	// replay from. Its own reason because lag is clamped at zero, so such a
	// node reads as caught up and passes the lag terms. Reads and readiness
	// refuse the same state through [Health.AheadOfLog], writes through
	// [Runner.ObserveEnd] and [ZeroFence].
	SkipAheadOfLog SkipReason = "ahead_of_log"

	// SkipLogDiverged — the log holds, at this node's checkpoint, another
	// record than the one it consumed there ([ErrLogDiverged]), so its rows
	// are a history the log does not continue. Its own reason beside
	// [SkipAheadOfLog] because once a restored log is written past the
	// checkpoint the end looks ordinary and the caught-up term is true.
	SkipLogDiverged SkipReason = "log_diverged"

	// SkipFailed — the attempt ran and errored, the one reason that is not
	// a declined precondition. Without it a node whose copy fails every
	// interval (a read-only disk, a scrub that cannot open the file) would
	// publish an empty reason, which reads as a loop that has not run yet.
	// The error is logged; the fleet learns only that there was one.
	SkipFailed SkipReason = "failed"
)

// SkipReasons are every reason, so an operator surface can enumerate them.
var SkipReasons = []SkipReason{
	SkipLagging, SkipUnhydrated, SkipSoleNode,
	SkipInsufficientSpace, SkipDeferred, SkipRecent, SkipAheadOfLog, SkipLogDiverged,
	SkipFailed,
}

// Valid reports whether a skip reason off the wire is one this build knows.
func (s SkipReason) Valid() bool { return slices.Contains(SkipReasons, s) }

// ManifestVersion is the artefact format this build writes and the only one it
// reads: a manifest of any other version is nobody's artefact here. Fields are
// added within a version, so a later build reads what this one took.
const ManifestVersion = 2

// DomainPosition is what a manifest says about one registered domain. Every
// field is a refusal a recipient can make: an artefact is adopted wholesale,
// so what it cannot check before installing, it cannot check at all.
type DomainPosition struct {
	// Stream is the domain's stream name.
	Stream string `json:"stream"`

	// Generation and StreamCreatedAt detect a recreated stream: the instant
	// notices and the generation responds. A generation other than the live
	// stream's is refused, since its sequences name a different history.
	Generation      uint32    `json:"generation"`
	StreamCreatedAt time.Time `json:"stream_created_at"`

	// Seq is the position committed IN THE FILE, read from the verified copy
	// (package doc, "The checkpoint commits with the rows"). A claim the
	// file does not keep is a corrupt snapshot.
	Seq uint64 `json:"seq"`

	// RecordVersion is the highest record version the rows were APPLIED
	// from, as the copy's checkpoint row records it ([FileCursor]). A
	// recipient whose build reads less refuses: its rows would sit past a
	// record it could never apply, defer or reprocess once trimmed. Never
	// the donor build's readable version, which would refuse every upgraded
	// donor through a rolling upgrade over records that do not exist.
	RecordVersion int `json:"record_version"`

	// Replay is the protocol the donor's build declares. A recipient
	// declaring another refuses: a compacted position adopted into a strict
	// loop is a permanent stall.
	Replay ReplayProtocol `json:"replay"`

	// FirstSeqAtTake and LastSeqAtTake are what the stream looked like when
	// the copy was made, for the operator reading why an offer was refused.
	FirstSeqAtTake uint64 `json:"first_seq_at_take,omitempty"`
	LastSeqAtTake  uint64 `json:"last_seq_at_take,omitempty"`
}

// Manifest is the artefact's claim about itself, written LAST and atomically
// through a rename, so its presence IS the claim: a copy without one is debris
// of an unfinished run.
type Manifest struct {
	V             int                       `json:"v"`
	TakenAt       time.Time                 `json:"taken_at"`
	NodeID        string                    `json:"node_id"`
	EngineVersion string                    `json:"engine_version"`
	Migrations    []string                  `json:"migrations"`
	Scrubbed      []string                  `json:"scrubbed"`
	Domains       map[string]DomainPosition `json:"domains"`
	Bytes         int64                     `json:"bytes"`
	SHA256        string                    `json:"sha256"`

	// Artifact is the name of the database file this manifest describes,
	// beside it. Carried, not derived from the position: the position need
	// not move between takes, and a name derived from it lets a take on a
	// quiet company publish over the previous pair, so a failed manifest
	// write leaves no artefact at all. A carried name is unique per take,
	// so the new pair lands beside the old, which is rotated away only once
	// the new manifest is durable.
	Artifact string `json:"artifact"`
}

// Registered is one domain's log as the framework holds it, for the surfaces
// that walk every log rather than serving one.
//
// Domain and Spec must agree ([Registered.Check]): a manifest names a position
// by the domain's name and reads it out of the copy by the spec's stream, so a
// registration whose two named different logs would stamp one log's
// checkpoint under another's key.
type Registered struct {
	Domain Domain

	// Spec is the domain's stream as this node runs it.
	Spec StreamSpec

	// Health is this node's readiness for that domain, which the snapshot
	// gate reads.
	Health func() Health

	// No creation instant: a manifest names the one the copied FILE's
	// checkpoint was committed under ([CursorsInFile]); the donor's live
	// handle describes another stream once one is recreated.
}

// Check refuses a registration whose domain and stream are not one log.
func (r Registered) Check() error {
	if r.Domain == nil {
		return fmt.Errorf("statelog: a registered log has no domain")
	}
	return r.Spec.Instantiates(r.Domain)
}

// checkRegistered refuses a set of registrations any one of which fails
// [Registered.Check].
func checkRegistered[K comparable](regs map[K]Registered) error {
	for _, r := range regs {
		if err := r.Check(); err != nil {
			return err
		}
	}
	return nil
}

// SnapshotDeps is everything the snapshot loop needs that it does not own.
type SnapshotDeps struct {
	// Domains are every log this build registers. A recipient refuses an
	// artefact that does not name all of them.
	Domains []Registered

	// File is the replicated estate's file, which the artefact copies
	// alone: the node's own estate holds the audit log and the secret
	// bootstrap, which a peer must not inherit.
	File store.ReplicatedHandle

	// Dir is where this node keeps its snapshots.
	Dir string

	// NodeID and EngineVersion identify the donor in its own manifest.
	NodeID        string
	EngineVersion string

	// Recipients is how many nodes OTHER THAN THIS ONE the fleet counts on
	// the estate's logs (the live data nodes and every node whose row names
	// one), which decides whether anybody can be donated to. Others, not a
	// head count, because this node need not be counted: an evicted machine
	// back with its files stops being counted once the fence window passes,
	// yet its copy may be the only one a joiner beside it can adopt.
	Recipients func(ctx context.Context) (int, error)

	// Interval is how stale the newest local snapshot may be.
	Interval time.Duration

	// Logger is where this writes. Nil is the package's own component
	// logger, never silence (see loggerOr).
	Logger *slog.Logger
	Now    func() time.Time
}

// Snapshotter takes this node's own snapshots.
type Snapshotter struct {
	deps SnapshotDeps
	now  func() time.Time
	log  *slog.Logger

	// volumeFree measures what a volume has free: [store.VolumeFree], which
	// only a case replaces (export_test.go), since what the machine running
	// it has free is no condition a case can state.
	volumeFree func(dir string) (int64, error)
}

// NewSnapshotter builds the loop, refusing a dependency set that could not
// produce a usable artefact rather than producing one nobody can adopt.
func NewSnapshotter(d SnapshotDeps) (*Snapshotter, error) {
	switch {
	case len(d.Domains) == 0:
		return nil, fmt.Errorf("statelog: a snapshot with no registered domain " +
			"names no position, and a recipient refuses an artefact that does " +
			"not name every domain its own build registers")
	case d.File.IsZero():
		return nil, fmt.Errorf("statelog: the snapshot loop names no estate file to copy")
	case d.Dir == "":
		return nil, fmt.Errorf("statelog: the snapshot loop has nowhere to write")
	case d.NodeID == "":
		return nil, fmt.Errorf("statelog: a snapshot names no donor")
	case d.Recipients == nil:
		return nil, fmt.Errorf("statelog: the snapshot loop cannot count the fleet")
	case d.Interval <= 0:
		return nil, fmt.Errorf("statelog: the snapshot loop has no interval")
	}
	if err := checkRegistered(maps.Collect(slices.All(d.Domains))); err != nil {
		return nil, err
	}
	logger := loggerOr(d.Logger)
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Snapshotter{deps: d, now: now, log: logger, volumeFree: store.VolumeFree}, nil
}

// ErrSkipped reports a tick that took no snapshot, carrying the reason.
type ErrSkipped struct {
	Reason SkipReason
	Detail string
}

func (e *ErrSkipped) Error() string {
	return fmt.Sprintf("statelog: no snapshot taken (%s): %s", e.Reason, e.Detail)
}

// Skipped extracts a skip reason from an error, reporting false for anything
// else.
func Skipped(err error) (SkipReason, bool) {
	var skip *ErrSkipped
	if errors.As(err, &skip) {
		return skip.Reason, true
	}
	return "", false
}

// Take makes one snapshot, or reports why it did not with an [ErrSkipped]
// whose reason is published (see [Snapshotter.gate] and [SkipReason]).
func (s *Snapshotter) Take(ctx context.Context) (Manifest, error) {
	if err := s.gate(ctx); err != nil {
		// Returned, not logged: the caller retries while the reason holds
		// and reports only a change, so a line here would repeat every
		// retry on `sole_node`, a healthy single node's steady state. One
		// event, one reporting path.
		return Manifest{}, err
	}

	taken := s.now().UTC()
	if err := os.MkdirAll(s.deps.Dir, 0o700); err != nil {
		return Manifest{}, fmt.Errorf("statelog: create %s: %w", s.deps.Dir, err)
	}
	// Written under a part name, since its final name carries the position
	// inside the file, unknown until the copy exists. A crashed attempt's
	// part file is debris, sidecars included: a stale -wal beside a fresh
	// copy is applied to it on the next open.
	part := filepath.Join(s.deps.Dir, snapshotPartName)
	if err := store.RemoveCopy(part); err != nil {
		return Manifest{}, fmt.Errorf("statelog: clear the previous attempt: %w", err)
	}
	discard := func() { _ = store.RemoveCopy(part) }

	live, err := s.deps.File.DB()
	if err != nil {
		return Manifest{}, fmt.Errorf("statelog: copy the replicated estate: %w", err)
	}
	info, err := live.Backup(ctx, part)
	if err != nil {
		return Manifest{}, fmt.Errorf("statelog: copy the replicated estate: %w", err)
	}

	// The donor scrubs, on the list the domains declare ([Snapshotter.scrubList]).
	scrubbed, err := store.ScrubFile(ctx, part, s.scrubList())
	if err != nil {
		discard()
		return Manifest{}, err
	}

	// Positions come from the copy, never the live node: a recipient refuses
	// a manifest the file's checkpoint disagrees with, and the applier
	// commits while the copy is taken. The backup manifest reads the same.
	positions, err := s.positionsIn(ctx, part)
	if err != nil {
		discard()
		return Manifest{}, err
	}
	// Reading the copy grew a -wal and -shm beside it; what is digested and
	// offered is one file.
	if err = store.QuiesceCopy(ctx, part); err != nil {
		discard()
		return Manifest{}, err
	}

	// The checksum covers the scrubbed bytes, the file actually sent.
	digest, err := store.FileDigest(ctx, part)
	if err != nil {
		discard()
		return Manifest{}, err
	}
	size, err := os.Stat(part)
	if err != nil {
		discard()
		return Manifest{}, fmt.Errorf("statelog: measure %s: %w", part, err)
	}

	m := Manifest{
		V:             ManifestVersion,
		TakenAt:       taken,
		NodeID:        s.deps.NodeID,
		EngineVersion: s.deps.EngineVersion,
		Migrations:    info.Migrations,
		Scrubbed:      scrubbed,
		Domains:       positions,
		Bytes:         size.Size(),
		SHA256:        digest,
	}
	// A name of its own, so this pair never lands on the last one's (see
	// [Manifest.Artifact]): the take's instant disambiguates, and the
	// sequence is there for an operator reading the directory.
	base := filepath.Join(s.deps.Dir, fmt.Sprintf("snapshot-%d-%d",
		newestSeq(positions), taken.UnixNano()))
	m.Artifact = filepath.Base(base) + ".db"
	copyPath := base + ".db"
	manifestPath := base + ".json"
	if err := os.Rename(part, copyPath); err != nil {
		discard()
		return Manifest{}, fmt.Errorf("statelog: place %s: %w", copyPath, err)
	}
	if err := writeManifest(manifestPath, m); err != nil {
		// Only this attempt's file: the previous pair stays complete.
		_ = os.Remove(copyPath)
		return Manifest{}, err
	}

	// The previous snapshot goes last, transiently costing twice the store:
	// deleting it first leaves a window with nothing to donate, and before
	// the new manifest is durable, possibly for good.
	s.rotate(ctx, base)

	s.log.InfoContext(ctx, "statelog_snapshot_taken",
		"node", s.deps.NodeID, "path", copyPath, "bytes", m.Bytes,
		"domains", len(m.Domains), "scrubbed", len(m.Scrubbed))
	return m, nil
}

// snapshotPartName is what a copy is called until its position is known.
//
// It does not start with `snapshot-`, so the rotation never mistakes it for a
// finished pair and the newest-manifest walk never finds a manifest beside it.
const snapshotPartName = "snapshot.part.db"

// positionsIn is every registered domain's position AS THE COPY KEEPS IT.
//
// A domain with no checkpoint row in the file is at the ZERO position, as the
// recipient reads it too, so a domain whose log is still empty does not leave
// every snapshot unadoptable until its first record.
func (s *Snapshotter) positionsIn(ctx context.Context, path string) (map[string]DomainPosition, error) {
	cursors, err := CursorsInFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("statelog: read the positions the copy keeps: %w", err)
	}
	positions := make(map[string]DomainPosition, len(s.deps.Domains))
	for _, reg := range s.deps.Domains {
		h := reg.Health()
		spec := reg.Spec
		// Every checkpoint field comes out of the file, the identity and
		// applied version included: an identity from the live registration
		// would stamp old rows with a recreated stream's identity, and the
		// artefact would pass every check a recipient can make.
		at := cursors[spec.Name]
		pos := DomainPosition{
			Stream:          spec.Name,
			Generation:      at.Position.Generation,
			StreamCreatedAt: at.StreamCreatedAt,
			Seq:             at.Position.Seq,
			RecordVersion:   at.AppliedVersion,
			Replay:          spec.Replay,
		}
		// The stream's bounds are advisory, from live health because the
		// file cannot know them.
		if h.FirstSeq != nil {
			pos.FirstSeqAtTake = *h.FirstSeq
		}
		if h.Lag != nil {
			pos.LastSeqAtTake = h.Position.Seq + *h.Lag
		}
		positions[reg.Domain.Name()] = pos
	}
	return positions, nil
}

// gate is the preconditions of a take, cheapest first.
func (s *Snapshotter) gate(ctx context.Context) error {
	recipients, err := s.deps.Recipients(ctx)
	if err != nil {
		return fmt.Errorf("statelog: count the fleet: %w", err)
	}
	if recipients < 1 {
		return &ErrSkipped{Reason: SkipSoleNode, Detail: "the fleet counts no node " +
			"on the estate's logs but this one, so there is nobody to donate to: a " +
			"single node is recovered from a backup, which the trim's own " +
			"backup term gates"}
	}

	for _, reg := range s.deps.Domains {
		h := reg.Health()
		name := reg.Domain.Name()
		if h.Deferred > 0 {
			return &ErrSkipped{Reason: SkipDeferred, Detail: fmt.Sprintf(
				"this node holds %d record(s) of %s it cannot decode, from "+
					"sequence %d — the deferred table is scrubbed out of every "+
					"artefact, so a checkpoint above a retained record would hand "+
					"an adopter a resume point above bytes it never received",
				h.Deferred, name, h.DeferredFrom)}
		}
		// Before the caught-up terms, which a checkpoint past the log's
		// end passes: lag is clamped at zero.
		if h.AheadOfLog() {
			return &ErrSkipped{Reason: SkipAheadOfLog, Detail: fmt.Sprintf(
				"this node's %s checkpoint is at %d and the log ends at %d, so "+
					"its rows are keyed to a sequence space this stream no "+
					"longer has — a copy would name a position no recipient "+
					"could replay from", name, h.Position.Seq, *h.LastSeq)}
		}
		// Likewise a diverged log, whose end looks ordinary.
		if h.LogDiverged {
			return &ErrSkipped{Reason: SkipLogDiverged, Detail: fmt.Sprintf(
				"the log's record at this node's %s checkpoint %d is not the one "+
					"it consumed there, so its rows are a history the log does not "+
					"continue — a copy would hand a recipient rows that disagree "+
					"with the log at the position it resumes from",
				name, h.Position.Seq)}
		}
		// The history; the term below is the distance. Kept apart: a
		// drained flag read from the instantaneous lag would fire at a lag
		// of one and leave [SnapshotLagSlack] unreachable.
		if !h.Drained {
			return &ErrSkipped{Reason: SkipUnhydrated, Detail: fmt.Sprintf(
				"this node has not drained %s since its applier started, or has "+
					"stalled since it did, so what it holds is a prefix rather "+
					"than a state", name)}
		}
		if h.Lag != nil && *h.Lag > SnapshotLagSlack {
			return &ErrSkipped{Reason: SkipLagging, Detail: fmt.Sprintf(
				"this node is %d record(s) behind %s, past the %d a copy can be "+
					"and still save a joiner more than it costs to transfer",
				*h.Lag, name, SnapshotLagSlack)}
		}
	}

	newest, found, err := s.newest()
	if err != nil {
		return err
	}
	// Recent means adoptable, not merely young ([Snapshotter.current]): once
	// a reanchor or adoption moved a domain's generation, or the taking build
	// ran a domain fewer, no joiner can use the artefact, and the interval
	// would keep it the node's only one.
	if found && s.now().Sub(newest.TakenAt) < s.deps.Interval {
		if s.current(newest) {
			return &ErrSkipped{Reason: SkipRecent, Detail: fmt.Sprintf(
				"the newest local snapshot is %s old, inside the %s interval, and "+
					"names every domain at the generation this node stands at",
				s.now().Sub(newest.TakenAt).Round(time.Second), s.deps.Interval)}
		}
		covers := slices.Sorted(maps.Keys(newest.Domains))
		s.log.Info("statelog_snapshot_recent_but_short",
			"taken_at", newest.TakenAt, "covers", covers,
			"missing", missingFrom(covers, s.domainNames()),
			"detail", "taking a complete one now rather than waiting out the "+
				"interval: an artefact that names no position for a domain this "+
				"node runs, or names one at a generation it has since left, is "+
				"refused by every joiner")
	}

	room, err := s.space()
	if err != nil {
		return err
	}
	// The held-back log counts on the snapshot volume as well as the live
	// file's: nothing tells two volumes of one pool apart for certain.
	if want := int64(float64(room.size)*SnapshotFreeSpaceFactor) + room.held; room.free < want {
		held := ""
		if room.held > 0 {
			held = fmt.Sprintf(", %d of it the write-ahead log the last copy measured a "+
				"copy holding back while it runs", room.held)
		}
		return &ErrSkipped{Reason: SkipInsufficientSpace, Detail: fmt.Sprintf(
			"%s has %d bytes free and a copy of a %d-byte store needs %d%s — the "+
				"default puts the copy on the same volume as the live database, "+
				"so filling it would stop the applier committing",
			s.deps.Dir, room.free, room.size, want, held)}
	}
	if room.liveFree < room.held {
		return &ErrSkipped{Reason: SkipInsufficientSpace, Detail: fmt.Sprintf(
			"the volume holding the replicated estate (%s) has %d bytes free, and a "+
				"copy holds back %d bytes of its write-ahead log there while it runs, as "+
				"the last copy measured — the applier commits to that volume, and would "+
				"stop once the log filled it", room.liveDir, room.liveFree, room.held)}
	}
	return nil
}

// scrubList is every table the artefact must not carry, derived from the
// domains' [Local] declarations rather than hardcoded: the same declaration
// serves the identity claim's membership and the local sweep, so the three
// cannot drift apart.
func (s *Snapshotter) scrubList() []string {
	var out []string
	for _, reg := range s.deps.Domains {
		for table, class := range reg.Domain.Tables() {
			if class == Local {
				out = append(out, table)
			}
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// newest is this node's newest complete snapshot's manifest. Only a manifest
// counts, so a crashed attempt's debris suppresses no later take; it comes
// back whole because "recent" also asks [Snapshotter.current].
func (s *Snapshotter) newest() (Manifest, bool, error) {
	entries, err := os.ReadDir(s.deps.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, false, nil
	}
	if err != nil {
		return Manifest{}, false, fmt.Errorf("statelog: read %s: %w", s.deps.Dir, err)
	}
	var newest Manifest
	var found bool
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		m, err := ReadManifest(filepath.Join(s.deps.Dir, e.Name()))
		if err != nil {
			continue
		}
		if !found || m.TakenAt.After(newest.TakenAt) {
			newest, found = m, true
		}
	}
	return newest, found, nil
}

// domainNames is every domain this node runs, which is the set an artefact has
// to name to be adoptable anywhere.
func (s *Snapshotter) domainNames() []string {
	out := make([]string, 0, len(s.deps.Domains))
	for _, d := range s.deps.Domains {
		out = append(out, d.Domain.Name())
	}
	return out
}

// missingFrom is the domains this node runs that an artefact does not name,
// which is exactly what makes it unadoptable.
func missingFrom(covers []string, running []string) []string {
	var missing []string
	for _, name := range running {
		if !slices.Contains(covers, name) {
			missing = append(missing, name)
		}
	}
	return missing
}

// current reports whether a manifest names every registered log at the
// generation this node's own checkpoint stands at — which is the only
// generation a joiner asking this node would accept it at.
func (s *Snapshotter) current(m Manifest) bool {
	for _, reg := range s.deps.Domains {
		at, named := m.Domains[reg.Domain.Name()]
		if !named || at.Generation != reg.Health().Position.Generation {
			return false
		}
	}
	return true
}

// rotate removes every snapshot but the newest SnapshotsKept.
func (s *Snapshotter) rotate(ctx context.Context, keep string) {
	entries, err := os.ReadDir(s.deps.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := filepath.Join(s.deps.Dir, e.Name())
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".db")
		if base == keep || !strings.HasPrefix(filepath.Base(base), "snapshot-") {
			continue
		}
		if err := os.Remove(name); err != nil {
			s.log.WarnContext(ctx, "statelog_snapshot_rotate_failed",
				"path", name, "error", err.Error())
		}
	}
}

// snapshotRoom is what the gate weighs a take against: the snapshot volume's
// free bytes, the replicated estate's file size (the most a copy writes), the
// write-ahead log a copy holds back on the file's own volume
// ([store.DB.CopyHoldsBack]), and that volume's free bytes.
type snapshotRoom struct {
	free, size, held int64
	liveFree         int64
	liveDir          string
}

// space is what the gate weighs a take against, measured now.
func (s *Snapshotter) space() (snapshotRoom, error) {
	dir := s.deps.Dir
	if _, statErr := os.Stat(dir); errors.Is(statErr, os.ErrNotExist) {
		// Created on the first take, so measure the parent's volume.
		dir = filepath.Dir(dir)
	}
	// Free to an unprivileged process ([store.VolumeFree]), which this is.
	free, err := s.volumeFree(dir)
	if err != nil {
		return snapshotRoom{}, fmt.Errorf("statelog: %w", err)
	}
	live, err := s.deps.File.DB()
	if err != nil {
		return snapshotRoom{}, fmt.Errorf("statelog: measure the replicated estate: %w", err)
	}
	info, err := os.Stat(live.Path())
	if err != nil {
		return snapshotRoom{}, fmt.Errorf("statelog: measure the replicated estate: %w", err)
	}
	liveDir := filepath.Dir(live.Path())
	liveFree, err := s.volumeFree(liveDir)
	if err != nil {
		return snapshotRoom{}, fmt.Errorf("statelog: %w", err)
	}
	return snapshotRoom{free: free, size: info.Size(), held: live.CopyHoldsBack(info.Size()),
		liveFree: liveFree, liveDir: liveDir}, nil
}

// newestSeq is the highest position any domain reached, which names the
// artefact.
func newestSeq(positions map[string]DomainPosition) uint64 {
	var out uint64
	for _, p := range positions {
		if p.Seq > out {
			out = p.Seq
		}
	}
	return out
}

// writeManifest writes the manifest LAST and atomically, so its presence is
// the claim that everything beside it is complete.
func writeManifest(path string, m Manifest) error {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("statelog: encode the manifest: %w", err)
	}
	part := path + ".part"
	if err := os.WriteFile(part, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("statelog: write %s: %w", part, err)
	}
	if err := os.Rename(part, path); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("statelog: publish %s: %w", path, err)
	}
	return nil
}

// ReadManifest reads one artefact's claim about itself.
func ReadManifest(path string) (Manifest, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("statelog: read %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return Manifest{}, fmt.Errorf("statelog: decode %s: %w", path, err)
	}
	if m.V != ManifestVersion {
		return Manifest{}, fmt.Errorf("statelog: %s is a version %d manifest and "+
			"this build reads %d", path, m.V, ManifestVersion)
	}
	return m, nil
}
