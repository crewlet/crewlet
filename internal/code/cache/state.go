package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE STATE FILE: what an incarnation's directory reflects, as a handful of
// scalars. CONSTANT-SIZE on purpose — it carries no list of packs, because
// the installed pack set is the directory itself (a pack is present once its
// .idx is, which is git's own rule), and a map of every pack rewritten and
// made durable on every sync would be write amplification proportional to
// the pack count on every push's path on every node: 10 000 packs is about
// 2 MB a sync. It is renamed in LAST, after everything it describes is
// durable, so a state is never ahead of the directory it describes.

// stateFormat is the layout this build writes and reads. A reshape takes a
// new number and its own reader; nothing migrates a cache, which is
// disposable, so a state of another format is a reason to start the
// incarnation cold rather than a document to convert.
const stateFormat = 1

// stateName is the state file, in the incarnation's root.
const stateName = "crewlet-state.json"

// maxStateBytes bounds the read of a state file. A state at the domain's
// bounds is about 25 KB — 64 protection rules of a 255-byte pattern each
// (code.MaxProtectionsPerRepo), a 1 KB description, and a few hundred bytes
// of scalars — so a mebibyte is forty times anything this cache writes, and a
// file past it is not one this cache wrote, whatever is in it.
const maxStateBytes = 1 << 20

// state is the state file.
type state struct {
	Format int    `json:"format"`
	RepoID string `json:"repo_id"`
	// StreamCreated is the snapshot's StreamCreated: which incarnation of
	// the code log Position is on.
	StreamCreated time.Time `json:"stream_created"`
	// Position: every row up to here is reflected in the directory.
	Position statelog.Position `json:"position"`
	// Repo is the repository row the round read (status, default branch,
	// protections, sizes).
	Repo code.RepoRef `json:"repo"`
	// RefsDigest is the hex SHA-256 of packed-refs as it was written.
	RefsDigest string `json:"refs_digest"`
	// Refs is how many refs packed-refs holds.
	Refs int `json:"refs"`
	// Packs is how many packs are installed.
	Packs int `json:"packs"`
	// DiskBytes is the bytes of the installed pack files and packed-refs —
	// what the rounds wrote. The maintainer's files are Midx.Bytes.
	DiskBytes int64     `json:"disk_bytes"`
	Midx      midxState `json:"midx"`
}

// midxState is what the maintainer last left on disk.
type midxState struct {
	// Packs is how many packs the multi-pack-index on disk covers; zero is
	// none on disk.
	Packs int `json:"packs"`
	// Bitmap reports that the index on disk has its bitmap — the
	// maintainer's — rather than being one without: a full round's, or the
	// maintainer's where the node's memory cannot hold the bitmap.
	Bitmap bool `json:"bitmap"`
	// Graph reports that the maintainer's commit-graph is on disk. Recorded
	// apart from the index, because the two are written by two processes and
	// the second can be refused a slot the first had: a repository whose
	// index is current and whose graph is missing is due for the graph
	// alone, where read only from the index it would never be due again
	// until MidxPacks more packs had landed.
	Graph bool `json:"graph"`
	// Bytes is the size of the maintainer's files: the multi-pack-index,
	// its bitmap and reverse index, and the commit-graph. Kept apart from
	// DiskBytes so a boot restores both halves of what the incarnation
	// holds without measuring it.
	Bytes int64 `json:"bytes"`
	// LastRun is when the last maintenance run ENDED, whatever its outcome.
	LastRun time.Time `json:"last_run"`
	// Took is how long that run took.
	Took time.Duration `json:"took"`
}

// errStateUnreadable is a state file that is present and says nothing this
// build can use: undecodable, of format zero, too large, unreadable, or
// another repository's. The incarnation is KEPT, and its first round reads
// the whole answer and converges it.
var errStateUnreadable = errors.New("cache: unreadable state")

// formatError is a state of a format this build does not read — another
// build's layout. The incarnation is trashed: what its files mean is that
// build's to say.
type formatError struct{ format int }

func (e *formatError) Error() string {
	return fmt.Sprintf("cache: a state of format %d, which this build (format %d) does not read", e.format, stateFormat)
}

// readState reads the state of repository id's incarnation at dir: the
// state, or nil and no error when there is none, an error wrapping
// [errStateUnreadable], or a *[formatError].
func readState(dir, id string) (*state, error) {
	f, err := os.Open(filepath.Join(dir, stateName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errStateUnreadable, err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	_ = f.Close() // read-only: what the read returned is the answer
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: %w", errStateUnreadable, err)
	case len(raw) > maxStateBytes:
		return nil, fmt.Errorf("%w: larger than the %d bytes any state this cache writes", errStateUnreadable, maxStateBytes)
	}
	var st state
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err = dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("%w: %w", errStateUnreadable, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: more than one document", errStateUnreadable)
	}
	switch {
	case st.Format <= 0:
		return nil, fmt.Errorf("%w: format %d", errStateUnreadable, st.Format)
	case st.Format != stateFormat:
		return nil, &formatError{format: st.Format}
	case st.RepoID != id:
		return nil, fmt.Errorf("%w: it names repository %q, and the directory is %s's", errStateUnreadable, st.RepoID, id)
	}
	return &st, nil
}

// persist writes inc's state from its in-memory fields: the whole value,
// under the entry's stateMu, so whichever of two writers — a round and the
// maintainer — writes last writes the latest values. A round passes round,
// which hits the sync.state fault point between the state's staging and its
// rename (codefault.SyncState).
func (c *Cache) persist(ctx context.Context, e *entry, inc *incarnation, round bool) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	e.mu.Lock()
	st := inc.stateValue(e.id)
	e.mu.Unlock()

	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("cache: encoding %s's state: %w", e.id, err)
	}
	staged, err := c.stage(hostbox.FileMode, func(f *os.File) error {
		_, werr := f.Write(raw)
		return werr
	})
	if err != nil {
		return err
	}
	if round && c.faults != nil {
		if err = c.faults.Hit(ctx, codefault.SyncState, 0); err != nil {
			return removeStaged(staged, fmt.Errorf("cache: %s's state: %w", e.id, err))
		}
	}
	if err = c.place(staged, filepath.Join(inc.dir, stateName)); err != nil {
		return err
	}
	if err = syncDir(inc.dir); err != nil {
		return err
	}
	e.mu.Lock()
	inc.state = &st
	e.mu.Unlock()
	return nil
}
