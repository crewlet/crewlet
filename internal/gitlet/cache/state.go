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
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE STATE FILE: what an incarnation's directory reflects, CONSTANT-SIZE on
// purpose. It lists no packs, because the installed pack set is the directory
// itself (a pack is present once its .idx is, git's own rule), and a list
// rewritten durably at every sync would cost every push on every node in
// proportion to the pack count. Its one map is keyed by DECLARATION
// (derived.go), never by file (ADR-0086), so it grows with the kinds a build
// declares, never with the repository. It is renamed in LAST, after
// everything it describes is durable, so a state is never ahead of its
// directory. A boot restores from it both halves of what an incarnation holds
// (the packs' bytes and each derived file's) and what the maintainer needs to
// judge it due, without measuring a file.

// stateFormat is the layout this build writes and reads. A reshape takes a
// new number and its own reader; nothing migrates a cache, which is
// disposable, so a state of another format is a reason to start the
// incarnation cold rather than a document to convert.
const stateFormat = 2

// stateName is the state file, in the incarnation's root.
const stateName = "crewlet-state.json"

// maxStateBytes bounds the read of a state file. A state at the domain's
// bounds is about 120 KB, nearly all of it the repository row's 64 rules
// (gitlet.MaxProtectionsPerRepo), each a 255-byte pattern JSON may spell at
// six bytes a byte; the row carries no rule's merge gate (gitlet.PushRule),
// which would make it five times wider at every push. A mebibyte is eight
// times anything this cache writes (TestTheWidestStateFitsItsRead holds it to
// four), so a file past it is not one this cache wrote.
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
	Repo gitlet.RepoRef `json:"repo"`
	// RefsDigest is the hex SHA-256 of packed-refs as it was written.
	RefsDigest string `json:"refs_digest"`
	// Refs is how many refs packed-refs holds.
	Refs int `json:"refs"`
	// Packs is how many packs are installed in objects/pack: what a
	// multi-pack-index covers, which never covers the dropping view.
	Packs int `json:"packs"`
	// DiskBytes is the bytes of the installed pack files — the dropping
	// view's among them — and packed-refs: what the rounds wrote. The
	// derived files' are each Derived entry's.
	DiskBytes int64 `json:"disk_bytes"`
	// DroppingBytes is, of DiskBytes, the dropping view's pack files'
	// (objects-dropping/pack): what a boot restores of the node's dropping
	// total (crewlet.code.cache.dropping.bytes) without listing a directory.
	// Absent while the copy holds none.
	DroppingBytes int64 `json:"dropping_bytes,omitempty"`
	// Derived is every declared derived file with a file of it on disk, by
	// its declaration's name: a declaration with none on disk has no entry.
	// One entry a declaration, never one field a fact (ADR-0086).
	Derived map[DerivedName]derivedState `json:"derived,omitempty"`
	// Maintained is the repository's last maintenance run: the duty cycle's
	// clock (MidxMinInterval, MidxDutyCycle). The REPOSITORY'S, never one
	// file's, because the duty cycle bounds a run of up to two steps — an
	// index and a graph — over one repository.
	Maintained maintainedState `json:"maintained"`
	// LeavingSince is, while the directory still holds packs a compaction
	// retired that the swap has not taken out (incarnation.leaving: a crash or
	// a failed swap between a round and its swap), the position all their
	// retirements are above. A boot's first round asks retirements from it
	// (StateQuery.RetiredSince) in place of Position, so it reads them as the
	// retirements they are and keeps what a retirement keeps (the
	// commit-graph, X04), not as packs no row accounts for. Zero where it
	// holds none: one position, so the state stays constant-size.
	LeavingSince statelog.Position `json:"leaving_since,omitzero"`

	// memoVersion is the version of the memo's record this value holds
	// (incarnation.memoVersion), never written: what the incarnation records
	// as stated once the value is on the disk (writeState).
	memoVersion uint64
	// lastUsed is the incarnation's last use as the value was taken, never
	// written as a field: the file's modification time, which is where a boot
	// reads the last use from (lastUsedOn, writeState).
	lastUsed time.Time
}

// derivedState is one declared derived file as the cache last left it on
// disk.
type derivedState struct {
	// Bytes is its owned files' bytes, whatever their kind: what Layout.Held
	// and the budget count. Kept beside DiskBytes so a boot restores both
	// halves of what the incarnation holds without measuring it.
	Bytes int64 `json:"bytes"`
	// Covers is how many packs the write that made it covered: an index
	// covering fewer than the incarnation holds has packs beside it.
	Covers int `json:"covers"`
	// Step is the write that made it (derived.go's Writes) — the
	// multi-pack-index's index or bitmap, the commit-graph's graph — and
	// empty where files of it are on disk that no write this cache recorded
	// made: a state a previous process could not leave readable, or a part
	// of it beside no main file (a bitmap a killed writer left with no
	// index). Such an entry is counted and never read as made — it covers
	// nothing (covers), and the maintainer writes it again.
	Step Step `json:"step,omitempty"`
	// Provenance is the provenance index's record — its coverage and its runs
	// (provenance.go) — which only that declaration's entry carries, and only
	// with its Step: a set of runs has no main file, so its record is what
	// says it is there. At most MaxProvenanceRuns runs, so a state stays
	// constant-size whatever the index holds. IMMUTABLE once stored: every
	// change stores a new value (copy on write), so a state encoded outside
	// the entry's lock shares it safely.
	Provenance *provenanceState `json:"provenance,omitempty"`
	// Cohorts is the memo's record (memo.go) — each cohort of its tree and
	// what the cohort holds, ordered by cohort, adding up to Bytes — which only
	// that declaration's entry carries: the tree holds no file whose presence
	// is a cohort's but its directory, and a boot counts and trims each from
	// this. At most maxStateCohorts, so a state stays constant-size whatever
	// the memo holds. IMMUTABLE once stored, as Provenance is.
	Cohorts []memoCohort `json:"cohorts,omitempty"`
	// CodeIndex is the code index's record (codeindex.go) — its tip, its tip
	// map and its segments — which only that declaration's entry carries, and
	// only with its Step: a set of files with no main one, so its record is
	// what says it is there. At most codeindex.MaxSegments segments, so a
	// state stays constant-size. IMMUTABLE once stored, as Provenance is.
	CodeIndex *codeIndexState `json:"codeindex,omitempty"`
	// Layers is an installed published chain's record (layers.go), which only
	// the two chains' entries carry, and only with their Step: the ids their
	// chain file lists, root first, which a boot's first round holds that file
	// to before any round has read the rows' chain (chainWants), and reads what
	// the copy holds by (bootInstalled). None reads as nothing installed. At
	// most gitlet.MaxPublishedLayers ids, so a state stays constant-size.
	// IMMUTABLE once stored, as Provenance is.
	Layers []string `json:"layers,omitempty"`
	// Tip is the node's tip commit-graph layer (maintain.go), which only the
	// commit-graph chain's entry carries: its id, the line its chain file lists
	// after Layers, which a boot's first round holds that file to and keeps the
	// tip by where the copy keeps every layer it was written over (planChain).
	// Its bytes are counted in Bytes. None reads as no tip.
	Tip string `json:"tip,omitempty"`
	// TipOver is the digest of the TAIL the last tip write that succeeded
	// covered (incarnation.tipOver, thirty-two hex digits), which only the
	// commit-graph chain's entry carries, with its tip or with none where that
	// write found the tail held no commit the layers lack. A boot that keeps
	// the tip and the layers beneath it writes the tip again only once the
	// tail moves; without it a quiet copy's tip is rewritten every restart.
	TipOver string `json:"tip_over,omitempty"`
}

// covers is how many packs d covers: the packs the write that made it
// covered, and none where no write this cache recorded made it, whatever
// Covers says — so no entry, in memory or read from a state, makes a file
// nothing made cover a pack (state.check refuses such a state besides).
func (d derivedState) covers() int {
	if d.Step == "" {
		return 0
	}
	return d.Covers
}

// maintainedState is the repository's last maintenance run.
type maintainedState struct {
	// LastRun is when the last run the duty cycle counts ENDED, whatever its
	// outcome.
	LastRun time.Time `json:"last_run"`
	// Took is how long that run took.
	Took time.Duration `json:"took"`
}

// derivedBytes is what the state's derived files hold together: what a boot
// counts beside DiskBytes, and what the incarnation's metaBytes starts from.
func (st *state) derivedBytes() int64 {
	var n int64
	for _, d := range st.Derived {
		n += d.Bytes
	}
	return n
}

// check refuses a state whose derived files are not ones this build
// declares, or are recorded as no write of theirs could leave them: a key
// none of DerivedFiles names, a negative size or count, packs covered by
// files no write made (only a write sets Covers), a step none of its
// declaration's writes takes. Such an entry's meaning is the writing build's,
// and its bytes, counted by a boot, would be a total no declaration makes up.
func (st *state) check() error {
	steps := declaredSteps()
	for name, d := range st.Derived {
		takes, declared := steps[name]
		switch {
		case !declared:
			return fmt.Errorf("it records a derived file %q, which this build does not declare", name)
		case d.Bytes < 0 || d.Covers < 0:
			return fmt.Errorf("it records derived file %q at %d bytes over %d packs", name, d.Bytes, d.Covers)
		case d.Step == "" && d.Covers != 0:
			return fmt.Errorf("it records derived file %q over %d packs, made by no write", name, d.Covers)
		case d.Step != "" && !slices.Contains(takes, d.Step):
			return fmt.Errorf("it records derived file %q as made by step %q, which no write of it takes", name,
				d.Step)
		case d.Provenance != nil && name != DerivedProvenance:
			return fmt.Errorf("it records an index of pushes for derived file %q, which keeps none", name)
		case name == DerivedProvenance && (d.Provenance != nil) != (d.Step != ""):
			return fmt.Errorf("it records derived file %q made by step %q with an index %t: an index is recorded "+
				"by the write that made it", name, d.Step, d.Provenance != nil)
		case d.Provenance != nil:
			if err := d.Provenance.check(d.Bytes); err != nil {
				return fmt.Errorf("it records derived file %q as %w", name, err)
			}
		case d.CodeIndex != nil && name != DerivedCodeIndex:
			return fmt.Errorf("it records a code index for derived file %q, which keeps none", name)
		case name == DerivedCodeIndex && (d.CodeIndex != nil) != (d.Step != ""):
			return fmt.Errorf("it records derived file %q made by step %q with an index %t: an index is recorded "+
				"by the write that made it", name, d.Step, d.CodeIndex != nil)
		case d.CodeIndex != nil:
			if err := d.CodeIndex.check(d.Bytes); err != nil {
				return fmt.Errorf("it records derived file %q as %w", name, err)
			}
		case d.Cohorts != nil && name != DerivedMemo:
			return fmt.Errorf("it records cohorts of a memo for derived file %q, which keeps none", name)
		case name == DerivedMemo:
			if err := checkCohorts(d); err != nil {
				return fmt.Errorf("it records derived file %q as %w", name, err)
			}
		case d.Layers != nil && name != DerivedLayers && name != DerivedGraphLayers:
			return fmt.Errorf("it records published layers for derived file %q, which installs none", name)
		case (d.Tip != "" || d.TipOver != "") && name != DerivedGraphLayers:
			return fmt.Errorf("it records a tip commit-graph layer for derived file %q, which keeps none", name)
		case d.Layers != nil || name == DerivedLayers || name == DerivedGraphLayers:
			if err := checkLayers(d); err != nil {
				return fmt.Errorf("it records derived file %q as %w", name, err)
			}
		}
	}
	return nil
}

// checkLayers refuses an installed chain's record no write of this build
// leaves: its chain file's lines recorded by another write than the one that
// made its first — the install's (StepLayer) where it lists a published layer,
// the tip's (StepTipGraph) where it lists the node's tip alone, and none where
// it lists nothing, since the record is what says the chain was made — more
// layers than a published chain holds, a line that is no layer's checksum, or
// one twice; and a tail's digest that is none, or recorded beside a chain file
// that lists nothing (recordChains keeps no record of one).
func checkLayers(d derivedState) error {
	var made Step
	switch {
	case len(d.Layers) > 0:
		made = StepLayer
	case d.Tip != "":
		made = StepTipGraph
	}
	switch {
	case d.Step != made:
		return fmt.Errorf("a chain of %d layers and a tip %q made by step %q: a chain is recorded by the write "+
			"of its first line, %q", len(d.Layers), d.Tip, d.Step, made)
	case len(d.Layers) > gitlet.MaxPublishedLayers:
		return fmt.Errorf("a chain of %d layers, past the %d a published chain holds (gitlet.MaxPublishedLayers)",
			len(d.Layers), gitlet.MaxPublishedLayers)
	case d.TipOver != "" && made == "":
		return fmt.Errorf("a tail's digest %q beside a chain file that lists nothing", d.TipOver)
	case d.TipOver != "":
		if _, err := parseTailDigest(d.TipOver); err != nil {
			return fmt.Errorf("a tip written over %w", err)
		}
	}
	lines := d.Layers
	if d.Tip != "" {
		lines = append(slices.Clone(d.Layers), d.Tip)
	}
	for i, id := range lines {
		switch {
		case !gitref.ValidOID(id) || id == gitref.ZeroOID:
			return fmt.Errorf("a chain naming %q, which is no layer's checksum", id)
		case slices.Contains(lines[:i], id):
			return fmt.Errorf("a chain naming %s twice", id)
		}
	}
	return nil
}

// maxStateCohorts bounds the cohorts a state records of a memo: twice what the
// node keeps (maxMemoCohorts), since cohorts a crash left unrecorded are
// counted when a listing finds them and the tick that drops the oldest past
// the bound follows within a minute.
const maxStateCohorts = 2 * maxMemoCohorts

// checkCohorts refuses a memo's record no write of this build leaves: cohorts
// out of order or repeated, of no number, holding a negative size, more of
// them than any state this cache writes, or adding up to other than the
// record's bytes.
func checkCohorts(d derivedState) error {
	if len(d.Cohorts) > maxStateCohorts {
		return fmt.Errorf("a memo of %d cohorts, past the %d any state records", len(d.Cohorts), maxStateCohorts)
	}
	var total int64
	for i, c := range d.Cohorts {
		switch {
		case c.N == 0:
			return fmt.Errorf("a memo holding a cohort of no number")
		case c.Bytes < 0:
			return fmt.Errorf("a memo whose cohort %d holds %d bytes", c.N, c.Bytes)
		case i > 0 && c.N <= d.Cohorts[i-1].N:
			return fmt.Errorf("a memo whose cohorts are out of order at %d", c.N)
		}
		total += c.Bytes
	}
	if total != d.Bytes {
		return fmt.Errorf("a memo of %d bytes whose cohorts hold %d", d.Bytes, total)
	}
	return nil
}

// declaredSteps is the build's declarations (DerivedFiles) as state.check
// holds a state to them: each declaration's name, and the steps its writes
// take. Built ONCE A PROCESS, never once a state: a boot reads a state a
// repository, and DerivedFiles builds every declaration anew on each call
// (package doc, "Measured").
var declaredSteps = sync.OnceValue(func() map[DerivedName][]Step {
	steps := map[DerivedName][]Step{}
	for _, d := range DerivedFiles() {
		takes := make([]Step, 0, len(d.Writes))
		for _, w := range d.Writes {
			if !slices.Contains(takes, w.Step) {
				takes = append(takes, w.Step)
			}
		}
		steps[d.Name] = takes
	}
	return steps
})

// errStateUnreadable is a state file that is present and says nothing this
// build can use: undecodable, of format zero, too large, unreadable, another
// repository's, or recording a derived file as no declaration of this build's
// could leave it (state.check). The incarnation is KEPT, and its first round
// reads the whole answer and converges it.
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
	if err = st.check(); err != nil {
		return nil, fmt.Errorf("%w: %w", errStateUnreadable, err)
	}
	return &st, nil
}

// persist writes inc's state from its in-memory fields: the whole value,
// under the entry's stateMu, so whichever of two writers (a round, the
// maintainer) writes last writes the latest values. A round passes round,
// which hits the sync.state fault point between the state's staging and its
// rename (gitletfault.SyncState).
//
// The index of pushes' runs retired before the value is taken
// (incarnation.retiredRuns) are trashed only ONCE IT IS ON THE DISK: until
// then the state there still names them, so a crash boots on a state whose
// runs are all present. A failed write leaves them for the next writer.
func (c *Cache) persist(ctx context.Context, e *entry, inc *incarnation, round bool) error {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()

	e.mu.Lock()
	st := inc.stateValue(e.id)
	retired := inc.retiredRuns
	inc.retiredRuns = nil
	e.mu.Unlock()
	if err := c.writeState(ctx, e, inc, st, round); err != nil {
		e.mu.Lock()
		inc.retiredRuns = append(retired, inc.retiredRuns...)
		e.mu.Unlock()
		return err
	}
	c.trashRuns(e, inc, retired)
	return nil
}

// writeState writes st as inc's state, durable before it answers.
func (c *Cache) writeState(ctx context.Context, e *entry, inc *incarnation, st state, round bool) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("cache: encoding %s's state: %w", e.id, err)
	}
	st.Derived = nil // encoded; the incarnation's derived is the record (incarnation.state)
	if c.knobs != nil && c.knobs.stateWrite != nil {
		if err = c.knobs.stateWrite(e.id, round); err != nil {
			return fmt.Errorf("cache: %s's state: %w", e.id, err)
		}
	}
	staged, err := c.stage(hostbox.FileMode, func(f *os.File) error {
		_, werr := f.Write(raw)
		return werr
	})
	if err != nil {
		return err
	}
	if round && c.faults != nil {
		if err = c.faults.Hit(ctx, gitletfault.SyncState, 0); err != nil {
			return removeStaged(staged, fmt.Errorf("cache: %s's state: %w", e.id, err))
		}
	}
	if err = c.place(staged, filepath.Join(inc.dir, stateName)); err != nil {
		return err
	}
	// A STATE CARRIES THE COPY'S LAST USE, never the moment it was written: a
	// boot reads the last use from its modification time (lastUsedOn), and a
	// state is also written for work that is no use (the maintainer, the
	// backfill, the memo's trims), which would bring an unread copy back from a
	// restart as the last read and have the budget evict copies seats do read.
	// Stamped as the janitor stamps it (stampUse), before the directory's sync
	// makes the rename durable.
	if !st.lastUsed.IsZero() {
		if err = stampUse(inc.dir, st.lastUsed); err != nil {
			return fmt.Errorf("cache: stamping %s's state with its last use: %w", e.id, err)
		}
	}
	if err = c.durableDir(inc.dir); err != nil {
		return err
	}
	e.mu.Lock()
	inc.state = &st
	inc.memoStated = st.memoVersion
	if st.lastUsed.After(inc.stamped) {
		inc.stamped = st.lastUsed
	}
	e.mu.Unlock()
	return nil
}
