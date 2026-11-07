package issuereport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/store"
)

// NextStartDir is where a node keeps, beside its own database, what one process
// leaves for the next: the setting it last applied and the output of a crash.
const NextStartDir = "issue-reports"

const (
	// settingFile is the copy of the setting this node last applied.
	settingFile = "setting"
	// crashFile is where the runtime writes a crash while the copy says on, and
	// leftCrashFile where a start moves the last process's, until a revision
	// says whether it may be read.
	crashFile     = "crash"
	leftCrashFile = "crash.left"
	// migrationFile is a failed migration, kept for the next successful start.
	migrationFile = "migration"
	settingOn     = "on\n"
	settingOff    = "off\n"
)

// NextStart is what a node keeps on its own disk between one process and the
// next, for the defects a process cannot report itself: its crash, and a
// migration that stopped it starting.
//
// OFF MEANS OFF, LITERALLY, ACROSS A RESTART. Crash output is armed only while
// the node's copy of the setting says on, so a node that applied off writes
// nothing; and what the last process left is read only once a revision this
// process applied says on. One that says off deletes it unread. The copy is
// what lets a crash in early boot, before any revision is read, be covered at
// all: it is the last word this node had.
type NextStart struct {
	dir string
	// mayReport is the origin's word: an inert build arms nothing. tagged is a
	// release's, the one build a failed migration is reported from.
	mayReport bool
	tagged    bool

	mu    sync.Mutex
	on    bool
	armed bool
}

// OpenNextStart reads the copy under storeDir and arms crash output if it says
// on, after moving the last process's crash aside for the first revision to
// judge. A copy that says off, or none, deletes what is there unread.
func OpenNextStart(storeDir string, origin Origin) (*NextStart, error) {
	n := &NextStart{dir: filepath.Join(storeDir, NextStartDir), mayReport: origin.InertReason() == "",
		tagged: origin.Tagged()}
	if err := os.MkdirAll(n.dir, 0o700); err != nil {
		return nil, fmt.Errorf("issuereport: make %s: %w", n.dir, err)
	}
	copied, err := os.ReadFile(n.path(settingFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("issuereport: read the setting's copy: %w", err)
	}
	n.on = string(copied) == settingOn && n.mayReport
	if !n.on {
		return n, n.forget()
	}
	if err := os.Rename(n.path(crashFile), n.path(leftCrashFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("issuereport: keep the last crash: %w", err)
	}
	return n, n.arm()
}

// Apply holds the copy and the crash output to the setting this node applied,
// and is the last process's crash as a signal where that setting is on and it
// left one. Off disarms and deletes what is kept, unread.
func (n *NextStart) Apply(setting bool) ([]Signal, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	on := setting && n.mayReport
	if err := n.writeCopy(on); err != nil {
		return nil, err
	}
	n.on = on
	if !on {
		return nil, n.forget()
	}
	if err := n.arm(); err != nil {
		return nil, err
	}
	return n.leftovers()
}

// leftovers reads what the last processes left and deletes it.
func (n *NextStart) leftovers() ([]Signal, error) {
	var out []Signal
	crashed, err := n.leftCrash()
	if crashed != nil {
		out = append(out, *crashed)
	}
	failed, markErr := n.leftMigration()
	if failed != nil {
		out = append(out, *failed)
	}
	return out, errors.Join(err, markErr)
}

func (n *NextStart) leftCrash() (*Signal, error) {
	f, err := os.Open(n.path(leftCrashFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("issuereport: open the last crash: %w", err)
	}
	read, ok := readCrash(f)
	_ = f.Close()
	if err := removeIfThere(n.path(leftCrashFile)); err != nil || !ok {
		return nil, err
	}
	signal := crashSignal(read)
	return &signal, nil
}

// migrationMark is what a failed migration leaves: its closed values alone.
type migrationMark struct {
	Estate  store.Estate           `json:"estate"`
	Version string                 `json:"version"`
	From    int                    `json:"from"`
	Failure store.MigrationFailure `json:"failure"`
}

// MigrationFailed keeps a migration that stopped this process starting, for
// the next start that succeeds: on a release tag's build whose copy says on, for
// a failure of the engine's own. Upgrades between unreleased builds are not
// supported, so a migration between them is nobody's defect to report.
func (n *NextStart) MigrationFailed(err error) error {
	var failed *store.MigrationError
	if n == nil || !errors.As(err, &failed) {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.on || !n.tagged || !slices.Contains(store.MigrationFailures(), failed.Failure) {
		return nil
	}
	body, err := json.Marshal(migrationMark{Estate: failed.Estate, Version: failed.Version, From: failed.From,
		Failure: failed.Failure})
	if err != nil {
		return fmt.Errorf("issuereport: keep the failed migration: %w", err)
	}
	return n.replace(migrationFile, body)
}

// leftMigration is the failed migration a process left, held to its shape:
// the file is this node's, and still nothing in it reaches a report unchecked.
func (n *NextStart) leftMigration() (*Signal, error) {
	raw, err := os.ReadFile(n.path(migrationFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("issuereport: read the failed migration: %w", err)
	}
	if err := removeIfThere(n.path(migrationFile)); err != nil {
		return nil, err
	}
	var mark migrationMark
	if json.Unmarshal(raw, &mark) != nil || !mark.Estate.Valid() || mark.From < 0 || mark.From > maxMigrationNumber ||
		!slices.Contains(store.MigrationFailures(), mark.Failure) {
		return nil, nil
	}
	signal := migrationSignal(mark)
	return &signal, nil
}

// migrationSignal is a failed migration as a signal: its file as the site, and
// the class's own site where the file's name is outside the shape.
func migrationSignal(mark migrationMark) Signal {
	at := MigrationLocationOf(string(mark.Estate), mark.Version)
	return Signal{Class: ClassMigrationFailed, Subject: string(at.Site), Location: at, Facts: map[FactKey]FactValue{
		FactEstate:        enumFact(string(mark.Estate)),
		FactFromMigration: intFact(int64(mark.From)),
		FactSQLErrorClass: enumFact(string(mark.Failure)),
	}}
}

// arm points the runtime's crash output at a fresh file of this node's alone.
func (n *NextStart) arm() error {
	if n.armed {
		return nil
	}
	f, err := os.OpenFile(n.path(crashFile), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("issuereport: open the crash file: %w", err)
	}
	// THE RUNTIME HOLDS ITS OWN DUPLICATE, so this handle closes here.
	defer func() { _ = f.Close() }()
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		return fmt.Errorf("issuereport: arm crash output: %w", err)
	}
	n.armed = true
	return nil
}

// forget disarms crash output and deletes everything kept, unread.
func (n *NextStart) forget() error {
	if n.armed {
		if err := debug.SetCrashOutput(nil, debug.CrashOptions{}); err != nil {
			return fmt.Errorf("issuereport: disarm crash output: %w", err)
		}
		n.armed = false
	}
	return errors.Join(removeIfThere(n.path(crashFile)), removeIfThere(n.path(leftCrashFile)),
		removeIfThere(n.path(migrationFile)))
}

// writeCopy holds the copy to the setting.
func (n *NextStart) writeCopy(on bool) error {
	word := settingOff
	if on {
		word = settingOn
	}
	if copied, err := os.ReadFile(n.path(settingFile)); err == nil && string(copied) == word {
		return nil
	}
	return n.replace(settingFile, []byte(word))
}

// replace writes a file whole, so a crash mid-write leaves the old one.
func (n *NextStart) replace(name string, body []byte) error {
	temp := n.path(name + ".new")
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		return fmt.Errorf("issuereport: write %s: %w", name, err)
	}
	if err := os.Rename(temp, n.path(name)); err != nil {
		return fmt.Errorf("issuereport: write %s: %w", name, err)
	}
	return nil
}

func (n *NextStart) path(name string) string { return filepath.Join(n.dir, name) }

func removeIfThere(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("issuereport: remove %s: %w", filepath.Base(path), err)
	}
	return nil
}

// crashSignal is a crash as a signal: its top module frame as the site, the
// frames under it, and its kinds. A crash any of whose module frames is in code
// that reads outside input or runs a tool call on a model's arguments is held,
// never sent: whoever can raise it can stop every node.
func crashSignal(c crash) Signal {
	var at Location
	if len(c.frames) > 0 {
		at = LocationOf(c.frames[0].Function, c.frames[0].File)
	}
	frames := make([]Frame, 0, maxFrames)
	for _, f := range c.frames {
		if len(frames) == maxFrames {
			break
		}
		if frame, ok := FrameOf(f.Function, f.File, f.Line); ok {
			frames = append(frames, frame)
		}
	}
	facts := map[FactKey]FactValue{FactFatalKind: enumFact(string(c.kind))}
	if c.kind == FatalPanic {
		facts[FactPanicKind] = enumFact(string(c.panicKind))
	}
	return Signal{Class: ClassProcessCrashed, Subject: string(at.Site), Kind: string(c.kind), Location: at,
		Frames: frames, Facts: facts, Held: exposedCrash(c.frames)}
}

// exposedCrash says a crash is held: any of the crashing goroutine's module
// frames, not only the few a report carries, is in code an outside party or a
// model's arguments can reach.
func exposedCrash(frames []panics.Frame) bool {
	for _, f := range frames {
		site := SiteOf(f.Function)
		if site == "" {
			continue
		}
		if pkg := site.Package(); BeforeAuthentication(pkg) || ExecutesToolCalls(pkg) {
			return true
		}
	}
	return false
}
