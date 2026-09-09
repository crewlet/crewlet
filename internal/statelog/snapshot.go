package statelog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"golang.org/x/sys/unix"
)

// The snapshot repository's own constants.
const (
	// SnapshotsKept is how many snapshots a node retains.
	//
	// ONE, because the fleet is the redundancy: a snapshot is a recovery
	// artefact for a peer, and a second copy on the same disk protects
	// against nothing the first does not. What DOES protect against losing
	// a donor is that every node takes its own — which is why the trim's
	// snapshot term is the k-th highest rather than the newest.
	SnapshotsKept = 1

	// SnapshotLagSlack is how far behind the log a node may be and still
	// donate. A snapshot's whole value is the replay it saves a joiner, and
	// one taken far behind saves less than it costs to transfer.
	SnapshotLagSlack = 1_000

	// SnapshotDonorsRequired is how many verified snapshots the fleet must
	// hold before the trim may pass them.
	//
	// TWO. The minimum over the counted set blocks for ever on any node
	// that has not snapshotted yet; the maximum makes one donor's disk the
	// whole fleet's recovery plan. Two means losing any single donor still
	// leaves a usable artefact.
	SnapshotDonorsRequired = 2

	// SnapshotFreeSpaceFactor is how much free space the snapshot volume
	// must hold, as a multiple of the store's own size.
	//
	// The default puts a full copy on the SAME VOLUME as the live
	// database, so a snapshot write that filled the disk would become a
	// disk-full condition on the volume the applier is committing to —
	// where the rows and the checkpoint commit together. The margin is
	// what makes the loop refuse instead.
	SnapshotFreeSpaceFactor = 1.1

	// SnapshotChunkBytes and SnapshotTransferWindow size the transfer.
	//
	// TUNED FOR A NETWORK, and deliberately not the values this tree's
	// backup transfer uses: that one is strict stop-and-wait at 128 KiB
	// because it runs between two halves of one process over an in-memory
	// connection, and its own doc says so. Across two broker hops that is
	// 128 KiB per round trip. A 32 MiB credit window keeps a 100 MB/s pipe
	// full at any round-trip time under 335 ms, which is every realistic
	// deployment, at 32 MiB of buffer per side.
	SnapshotChunkBytes     = 1 << 20
	SnapshotTransferWindow = 32
)

// SkipReason is why a snapshot was not taken, and every one is PUBLISHED
// rather than logged and forgotten.
//
// A fleet that has silently stopped snapshotting is invisible until a node
// needs one — and three of these are routine rather than exotic: a rolling
// upgrade produces `deferred`, a full snapshot volume produces
// `insufficient_space`, and a two-node fleet after one eviction produces
// `sole_node`, where the loop does not run at all.
type SkipReason string

const (
	// SkipLagging — this node is too far behind the log for its copy to
	// save a joiner the replay it would cost to transfer.
	SkipLagging SkipReason = "lagging"

	// SkipUnhydrated — this node has not drained the log even once, so
	// what it holds is a prefix rather than a state.
	SkipUnhydrated SkipReason = "unhydrated"

	// SkipSoleNode — there is nobody to donate to. The recovery artefact
	// for a single node is a backup, which the trim's backup term already
	// gates, and saying so here stops a reader concluding the trim's
	// snapshot term deadlocks a solo fleet.
	SkipSoleNode SkipReason = "sole_node"

	// SkipInsufficientSpace — the snapshot volume has less than the
	// margin free.
	SkipInsufficientSpace SkipReason = "insufficient_space"

	// SkipDeferred — this node holds a record it cannot decode.
	//
	// THE PRECONDITION THAT MAKES THE SCRUB SAFE. The deferred table is
	// this node's own and is scrubbed out of every artefact, so a donor
	// whose checkpoint sits above a record it RETAINED would hand an
	// adopter a resume point above bytes the adopter never received — a
	// silent hole with no repairer. Stated as an invariant: inside a
	// verified snapshot, what this node has applied through equals its
	// checkpoint, for every domain.
	SkipDeferred SkipReason = "deferred"

	// SkipRecent — the newest local snapshot is younger than the
	// interval.
	SkipRecent SkipReason = "recent"
)

// SkipReasons are every reason, so an operator surface can enumerate them
// rather than discovering them one production incident at a time.
var SkipReasons = []SkipReason{
	SkipLagging, SkipUnhydrated, SkipSoleNode,
	SkipInsufficientSpace, SkipDeferred, SkipRecent,
}

// Valid reports whether a skip reason off the wire is one this build knows.
func (s SkipReason) Valid() bool { return slices.Contains(SkipReasons, s) }

// ManifestVersion is the artefact format this build writes.
const ManifestVersion = 1

// DomainPosition is what a manifest says about one registered domain.
//
// EVERY FIELD IS A REFUSAL A RECIPIENT CAN MAKE, which is why they are here
// rather than derived: an artefact is adopted wholesale, so anything a
// recipient cannot check before installing it, it cannot check at all.
type DomainPosition struct {
	// Stream is the domain's stream name.
	Stream string `json:"stream"`

	// Generation and StreamCreatedAt are the pair that detects a recreated
	// stream: the instant NOTICES and the generation is the RESPONSE. An
	// artefact whose generation differs from the live stream's is refused
	// rather than adopted, because its sequences name a different history.
	Generation      uint32    `json:"generation"`
	StreamCreatedAt time.Time `json:"stream_created_at"`

	// Seq is the position committed IN THE FILE, read from the verified
	// copy rather than from the live database — the checkpoint commits
	// with the rows, so the position inside the file is the only one that
	// describes the file. A metadata claim the file does not keep is a
	// corrupt snapshot.
	Seq uint64 `json:"seq"`

	// RecordVersion is the donor's own highest decodable version. A donor
	// running a NEWER build has applied records an older recipient cannot
	// read and its checkpoint sits above them — so the recipient would
	// arrive past records it can never defer or reprocess, and once those
	// sequences are trimmed it never can.
	RecordVersion int `json:"record_version"`

	// Replay is the protocol the donor's build declares. A recipient whose
	// build declares a different one refuses: adopting a compacted
	// position into a strict loop is a permanent stall arriving through a
	// file instead of a configuration.
	Replay ReplayProtocol `json:"replay"`

	// FirstSeqAtTake and LastSeqAtTake are what the stream looked like when
	// the copy was made, for the operator reading why an offer was refused.
	FirstSeqAtTake uint64 `json:"first_seq_at_take,omitempty"`
	LastSeqAtTake  uint64 `json:"last_seq_at_take,omitempty"`
}

// Manifest is the artefact's claim about itself, written LAST and atomically
// through a rename — so its presence IS the claim, and a directory without
// one holds the debris of a run that did not finish rather than a partial
// snapshot.
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
}

// Registered is one domain as the framework holds it, for the surfaces that
// walk every domain rather than serving one.
type Registered struct {
	Domain Domain

	// Health is this node's readiness for that domain, which is what the
	// snapshot gate reads: how far behind, whether it has ever drained,
	// and whether it holds anything it cannot decode.
	Health func() Health

	// StreamCreatedAt is the broker's own creation instant for the
	// domain's stream, which is what detects a recreated one.
	StreamCreatedAt time.Time
}

// SnapshotDeps is everything the snapshot loop needs that it does not own.
type SnapshotDeps struct {
	// Domains are every domain this build registers. An artefact names
	// ALL of them or a recipient refuses it, so a snapshot taken with one
	// missing is one nobody can use.
	Domains []Registered

	// DB is the node's store. The artefact is a copy of the REPLICATED
	// estate alone — the node's own estate holds the audit log and the
	// secret bootstrap, which is exactly what a peer must not inherit and
	// what would otherwise dominate the transfer.
	DB *store.DB

	// Dir is where this node keeps its snapshots.
	Dir string

	// NodeID and EngineVersion identify the donor in its own manifest.
	NodeID        string
	EngineVersion string

	// Counted is how many nodes the fleet counts, which decides whether
	// there is anybody to donate to at all.
	Counted func(ctx context.Context) (int, error)

	// Interval is how stale the newest local snapshot may be.
	Interval time.Duration

	Logger *slog.Logger
	Now    func() time.Time
}

// Snapshotter takes this node's own snapshots.
type Snapshotter struct {
	deps SnapshotDeps
	now  func() time.Time
	log  *slog.Logger
}

// NewSnapshotter builds the loop, refusing a dependency set that could not
// produce a usable artefact rather than producing one nobody can adopt.
func NewSnapshotter(d SnapshotDeps) (*Snapshotter, error) {
	switch {
	case len(d.Domains) == 0:
		return nil, fmt.Errorf("statelog: a snapshot with no registered domain " +
			"names no position, and a recipient refuses an artefact that does " +
			"not name every domain its own build registers")
	case d.DB == nil:
		return nil, fmt.Errorf("statelog: the snapshot loop has no store")
	case d.Dir == "":
		return nil, fmt.Errorf("statelog: the snapshot loop has nowhere to write")
	case d.NodeID == "":
		return nil, fmt.Errorf("statelog: a snapshot names no donor")
	case d.Counted == nil:
		return nil, fmt.Errorf("statelog: the snapshot loop cannot count the fleet")
	case d.Interval <= 0:
		return nil, fmt.Errorf("statelog: the snapshot loop has no interval")
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Snapshotter{deps: d, now: now, log: logger}, nil
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

// Take makes one snapshot, or reports why it did not.
//
// # Five preconditions, each published
//
// Caught up on every domain, not too far behind, more than one node in the
// fleet, room on the volume, and NOTHING DEFERRED anywhere. The last is the
// one that looks like tidiness and is not: the deferred table is scrubbed out
// of the artefact, so a donor whose checkpoint sits above a record it retained
// hands an adopter a resume point above bytes the adopter never received.
func (s *Snapshotter) Take(ctx context.Context) (Manifest, error) {
	if err := s.gate(ctx); err != nil {
		if reason, ok := Skipped(err); ok {
			s.log.InfoContext(ctx, "statelog_snapshot_skipped",
				"node", s.deps.NodeID, "reason", string(reason))
		}
		return Manifest{}, err
	}

	positions := make(map[string]DomainPosition, len(s.deps.Domains))
	for _, reg := range s.deps.Domains {
		h := reg.Health()
		spec := reg.Domain.Stream()
		pos := DomainPosition{
			Stream:          spec.Name,
			Generation:      h.Position.Generation,
			StreamCreatedAt: reg.StreamCreatedAt,
			Seq:             h.Position.Seq,
			RecordVersion:   reg.Domain.RecordVersion(),
			Replay:          spec.Replay,
		}
		if h.FirstSeq != nil {
			pos.FirstSeqAtTake = *h.FirstSeq
		}
		if h.Lag != nil {
			pos.LastSeqAtTake = h.Position.Seq + *h.Lag
		}
		positions[reg.Domain.Name()] = pos
	}

	taken := s.now().UTC()
	base := filepath.Join(s.deps.Dir, fmt.Sprintf("snapshot-%d", newestSeq(positions)))
	copyPath := base + ".db"
	manifestPath := base + ".json"

	if err := os.MkdirAll(s.deps.Dir, 0o700); err != nil {
		return Manifest{}, fmt.Errorf("statelog: create %s: %w", s.deps.Dir, err)
	}
	// A PART FILE FROM A CRASHED ATTEMPT IS DEBRIS, not data: only this
	// function writes these names, and the manifest is what makes a pair a
	// snapshot.
	_ = os.Remove(copyPath)
	_ = os.Remove(manifestPath)

	info, err := s.deps.DB.Replicated().Backup(ctx, copyPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("statelog: copy the replicated estate: %w", err)
	}

	// THE DONOR SCRUBS, on a list DERIVED from what every domain declares
	// about its own tables rather than written out here. A hardcoded list
	// would silently omit whatever a deployment actually has.
	scrubbed, err := store.ScrubFile(ctx, copyPath, s.scrubList())
	if err != nil {
		_ = os.Remove(copyPath)
		return Manifest{}, err
	}

	// THE CHECKSUM COVERS THE SCRUBBED BYTES, which is the second reason
	// the scrub is the donor's: a checksum taken before it would certify
	// a file nobody sends.
	digest, err := store.FileDigest(copyPath)
	if err != nil {
		_ = os.Remove(copyPath)
		return Manifest{}, err
	}
	size, err := os.Stat(copyPath)
	if err != nil {
		_ = os.Remove(copyPath)
		return Manifest{}, fmt.Errorf("statelog: measure %s: %w", copyPath, err)
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
	if err := writeManifest(manifestPath, m); err != nil {
		_ = os.Remove(copyPath)
		return Manifest{}, err
	}

	// THE PREVIOUS SNAPSHOT GOES LAST, transiently costing twice the
	// store: deleting first leaves a window in which this node can donate
	// nothing at all.
	s.rotate(ctx, base)

	s.log.InfoContext(ctx, "statelog_snapshot_taken",
		"node", s.deps.NodeID, "path", copyPath, "bytes", m.Bytes,
		"domains", len(m.Domains), "scrubbed", len(m.Scrubbed))
	return m, nil
}

// gate is the five preconditions, in the order that answers cheapest first.
func (s *Snapshotter) gate(ctx context.Context) error {
	counted, err := s.deps.Counted(ctx)
	if err != nil {
		return fmt.Errorf("statelog: count the fleet: %w", err)
	}
	if counted < 2 {
		return &ErrSkipped{Reason: SkipSoleNode, Detail: fmt.Sprintf(
			"the fleet counts %d node(s), so there is nobody to donate to — a "+
				"single node's recovery artefact is a backup, which the trim's "+
				"own backup term gates", counted)}
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
		if !h.CaughtUp {
			return &ErrSkipped{Reason: SkipUnhydrated, Detail: fmt.Sprintf(
				"this node has never drained %s, so what it holds is a prefix "+
					"rather than a state", name)}
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
	if found && s.now().Sub(newest) < s.deps.Interval {
		return &ErrSkipped{Reason: SkipRecent, Detail: fmt.Sprintf(
			"the newest local snapshot is %s old, inside the %s interval",
			s.now().Sub(newest).Round(time.Second), s.deps.Interval)}
	}

	free, storeSize, err := s.space()
	if err != nil {
		return err
	}
	if want := int64(float64(storeSize) * SnapshotFreeSpaceFactor); free < want {
		return &ErrSkipped{Reason: SkipInsufficientSpace, Detail: fmt.Sprintf(
			"%s has %d bytes free and a copy of a %d-byte store needs %d — the "+
				"default puts the copy on the same volume as the live database, "+
				"so filling it would stop the applier committing",
			s.deps.Dir, free, storeSize, want)}
	}
	return nil
}

// scrubList is every table the artefact must not carry, DERIVED from what the
// domains declare.
//
// It is the same declaration doing a third job — after the identity claim's
// membership and the local sweep — which is what keeps the three from drifting
// apart.
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

// newest is when this node's newest complete snapshot was taken.
//
// COMPLETE, which is what the manifest means: a copy with no manifest beside
// it is the debris of a run that did not finish, and treating it as a snapshot
// would let one crashed attempt suppress every later one.
func (s *Snapshotter) newest() (time.Time, bool, error) {
	entries, err := os.ReadDir(s.deps.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("statelog: read %s: %w", s.deps.Dir, err)
	}
	var newest time.Time
	var found bool
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		m, err := ReadManifest(filepath.Join(s.deps.Dir, e.Name()))
		if err != nil {
			continue
		}
		if !found || m.TakenAt.After(newest) {
			newest, found = m.TakenAt, true
		}
	}
	return newest, found, nil
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

// space is the snapshot volume's free bytes and the replicated estate's size.
func (s *Snapshotter) space() (free, size int64, err error) {
	dir := s.deps.Dir
	if _, statErr := os.Stat(dir); errors.Is(statErr, os.ErrNotExist) {
		// The directory is created on the first take, so the volume to
		// measure is its parent's.
		dir = filepath.Dir(dir)
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		return 0, 0, fmt.Errorf("statelog: measure the free space on %s: %w", dir, err)
	}
	info, err := os.Stat(s.deps.DB.ReplicatedPath())
	if err != nil {
		return 0, 0, fmt.Errorf("statelog: measure the replicated estate: %w", err)
	}
	// Bavail is what an unprivileged process may actually use, which is
	// what this loop is: Bfree includes the reserve only root can reach.
	return int64(fs.Bavail) * int64(fs.Bsize), info.Size(), nil
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
