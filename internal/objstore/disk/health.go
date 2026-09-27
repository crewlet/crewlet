package disk

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// health is what the store knows about its own ability to hold chunks.
type health struct {
	mu sync.Mutex

	// errors counts the chunk operations that failed on I/O since the last
	// one that did not.
	errors int

	// failure is why the store is failed, empty while it is not.
	failure string

	// vol is the last measurement of the volume.
	vol volume
}

// volume is one measurement of the volume the store is on.
type volume struct {
	used float64 // the fraction in use, as df counts it
	free uint64  // the bytes this process may still write
}

// HealthState is how able the store is to hold chunks.
type HealthState string

const (
	// HealthOK is a store taking every chunk.
	HealthOK HealthState = "ok"

	// HealthNearFull is a store past [NearFullRatio]: it still takes
	// every chunk, and an operator should be adding space.
	HealthNearFull HealthState = "nearfull"

	// HealthFull is a store past [FullRatio]: it takes no new chunk, and
	// writers put their copies on other members.
	HealthFull HealthState = "full"

	// HealthFailed is a store whose probe failed or whose operations keep
	// failing: it takes no chunk, and the map takes it out after its grace.
	HealthFailed HealthState = "failed"
)

// Valid reports whether s is a state this build knows.
func (s HealthState) Valid() bool {
	switch s {
	case HealthOK, HealthNearFull, HealthFull, HealthFailed:
		return true
	}
	return false
}

// Health is the store's own account of whether it can hold chunks.
type Health struct {
	State HealthState `json:"state"`

	// Detail says why, when the state is anything but ok.
	Detail string `json:"detail,omitempty"`

	// UsedPercent is how much of the volume is in use, 0..100, and
	// FreeBytes how much this process may still write — both as last
	// measured.
	UsedPercent float64 `json:"used_percent"`
	FreeBytes   uint64  `json:"free_bytes"`
}

// FullRatio and NearFullRatio are the fractions of the volume in use past
// which the store is full and nearly full.
//
// CEPH'S OWN, mon_osd_full_ratio 0.95 and mon_osd_nearfull_ratio 0.85, for
// Ceph's reasons. Full is declared short of the last byte because a disk that
// fills mid-write fails that write and every one after it, the repair that
// would have copied a chunk somewhere it fits included — the last five percent
// is the margin in which chunks already held are still touched and counted.
// Nearfull is the alarm ten points before that, because adding a node or a
// disk takes an operator hours, and at the rate a company uploads, a tenth of
// a volume is days.
//
// "IN USE" IS df's COUNT — used over used plus what an unprivileged writer may
// still take — so the blocks a filesystem reserves for root are not space this
// store pretends it has.
const (
	FullRatio     = 0.95
	NearFullRatio = 0.85
)

// FailedAfter is how many chunk operations in a row must fail on I/O before
// the store calls itself failed without waiting for a probe.
//
// THREE: one error is a file racing a delete or a transient the next call does
// not see, and two in a row can be one bad sector read twice; three with no
// success between them, across whatever chunks were asked for, is a disk not
// answering — and the probe on the next heartbeat clears it if the disk has
// recovered.
const FailedAfter = 3

// Health is the store's current account of itself.
func (s *Store) Health() Health {
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	return s.health.report()
}

// report is the health the fields describe. The caller holds the lock.
func (h *health) report() Health {
	out := Health{UsedPercent: h.vol.used * 100, FreeBytes: h.vol.free}
	switch {
	case h.failure != "":
		out.State, out.Detail = HealthFailed, h.failure
	case h.vol.used >= FullRatio:
		out.State = HealthFull
		out.Detail = fmt.Sprintf("the volume is %.1f%% used, past the %.0f%% at which "+
			"no new chunk is taken", h.vol.used*100, FullRatio*100)
	case h.vol.used >= NearFullRatio:
		out.State = HealthNearFull
		out.Detail = fmt.Sprintf("the volume is %.1f%% used, past the %.0f%% mark; "+
			"new chunks are refused at %.0f%%", h.vol.used*100, NearFullRatio*100,
			FullRatio*100)
	default:
		out.State = HealthOK
	}
	return out
}

// failure is why the store is failed, empty while it is not.
func (s *Store) failure() string {
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	return s.health.failure
}

// fault counts an I/O error from a chunk operation and answers it, so a call
// site reads `return s.fault(err)`.
func (s *Store) fault(err error) error {
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	s.health.errors++
	if s.health.errors < FailedAfter {
		return err
	}
	if s.health.failure == "" {
		log.Warn("objects_store_failed", "dir", s.root, "error", err.Error(),
			"detail", fmt.Sprintf("%d operations failed in a row; the store takes no "+
				"chunks until a probe succeeds", s.health.errors))
	}
	s.health.failure = fmt.Sprintf("%d operations in a row failed, the last with: %v",
		s.health.errors, err)
	return err
}

// succeeded ends a run of I/O errors. It does not clear a failure: only a
// probe, which exercises every step a write takes, may say the disk is back.
func (s *Store) succeeded() {
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	s.health.errors = 0
}

// probeName is the file a probe writes.
const probeName = ".probe"

// Probe checks the store can still hold chunks — writes, syncs, reads back and
// removes a small file, and measures the volume ([Options.Space]) — and
// answers its health. A store that was closed answers failed.
func (s *Store) Probe() Health {
	done, err := s.hold()
	if err != nil {
		return Health{State: HealthFailed, Detail: err.Error()}
	}
	defer done()
	s.probing.Lock()
	defer s.probing.Unlock()
	vol, volErr := s.measure()
	probeErr := s.probe()

	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	if volErr == nil {
		s.health.vol = vol
	}
	was := s.health.failure
	switch {
	case probeErr != nil:
		s.health.failure = probeErr.Error()
	case volErr != nil:
		s.health.failure = volErr.Error()
	default:
		s.health.failure = ""
		s.health.errors = 0
	}
	switch {
	case was == "" && s.health.failure != "":
		log.Warn("objects_store_failed", "dir", s.root, "error", s.health.failure,
			"detail", "the store takes no chunks until a probe succeeds")
	case was != "" && s.health.failure == "":
		log.Info("objects_store_recovered", "dir", s.root, "was", was)
	}
	return s.health.report()
}

// probe writes, syncs, reads back and removes the probe's file.
func (s *Store) probe() error {
	path := filepath.Join(s.root, probeName)
	want := "crewlet object store probe " + strconv.FormatInt(time.Now().UnixNano(), 10) + "\n"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("objstore/disk: the probe could not create %s: %w", path, err)
	}
	if _, err := f.WriteString(want); err != nil {
		_ = f.Close()
		return fmt.Errorf("objstore/disk: the probe could not write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("objstore/disk: the probe could not sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("objstore/disk: the probe could not close %s: %w", path, err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		return fmt.Errorf("objstore/disk: the probe could not read back %s: %w", path, readErr)
	}
	if string(got) != want {
		return fmt.Errorf("objstore/disk: the probe read back %d bytes from %s that are "+
			"not the %d it wrote", len(got), path, len(want))
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("objstore/disk: the probe could not remove %s: %w", path, err)
	}
	return nil
}

// measure reads the volume under the store's directory and judges how full
// it is.
func (s *Store) measure() (volume, error) {
	capacity, free, err := s.space(s.root)
	if err != nil {
		return volume{}, fmt.Errorf("objstore/disk: measure the volume under %s: %w", s.root, err)
	}
	if free > capacity {
		// NOT A READING: statfs cannot produce one, since its capacity
		// is what is in use PLUS what is free, so this is a supplied
		// reading that is wrong — and a store that guessed a fullness
		// from it would tell the fleet something nobody measured.
		return volume{}, fmt.Errorf("objstore/disk: the volume under %s measured %d bytes "+
			"free of a capacity of %d", s.root, free, capacity)
	}
	if capacity == 0 {
		// A VOLUME REPORTING NO CAPACITY — some network and FUSE
		// filesystems do — is one whose fullness cannot be judged, so it
		// is never called full: its writes fail on their own if it is.
		return volume{}, nil
	}
	return volume{used: float64(capacity-free) / float64(capacity), free: free}, nil
}

// statfs is the filesystem's own measurement of the volume a directory is on,
// as df counts it: capacity is the blocks in use plus those an unprivileged
// writer may still take, and free is the latter — so the blocks a filesystem
// reserves for root, which this process can never write, are in neither.
func statfs(dir string) (capacity, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	// A CONVERSION ON EVERY PLATFORM: the block size is an int64 on linux
	// and a uint32 on darwin.
	bsize := uint64(st.Bsize)
	var used uint64
	if st.Blocks > st.Bfree {
		used = (st.Blocks - st.Bfree) * bsize
	}
	free = st.Bavail * bsize
	return used + free, free, nil
}
