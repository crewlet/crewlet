package hostbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// A SHARED LOGIN IS A SET OF FILES, AND IT IS WRITTEN AS ONE: a harness may
// keep its login in several (gemini-cli's token beside its account, kimi-code's
// beside config.toml), one login only while they come from the same sign-in.
// Replacing them one rename at a time left half a login after a failure or
// kill, which every presence check, seed and export read as a login and the
// restore declined to repair because the directory was not empty.
//
// So a write is JOURNALED under the directory's lock ([LockCredentials]):
// (1) the whole new login, every file it holds and every name it removes, is
// renamed into the JOURNAL ([CredentialJournal]) and the directory synced: THAT
// RENAME IS THE COMMIT; (2) each file is replaced or removed, the directory
// synced; (3) the journal is removed.

// CredentialJournal is the journal of the shared credential directory dir
// ([WriteCredentials]): dir + ".pending", BESIDE the directory for the reason
// its lock is ([CredentialLock]): nothing that copies the directory carries a
// write in progress along.
//
// A JOURNAL THAT STANDS is a write stopped between steps 1 and 3. Every READ
// under the lock finishes it first ([ReadCredentials]), so a reader sees the
// login before or after the write, never part way. A write that finds one
// REPLACES it ([WriteCredentials]): the new write states every name it knows,
// so the old decisions are superseded unapplied, which lets a logout discard a
// login never put in place.
func CredentialJournal(dir string) string { return filepath.Clean(dir) + ".pending" }

// stagedJournal is where a write of dir's login stages its journal before the
// rename that commits it: beside the journal, under a name of its own rather
// than ReplaceFile's, because the directory beside it is where other writers
// stage their own files under other locks, and the next holder of this lock
// removes what a killed write left here ([sweep]).
func stagedJournal(dir string) string { return CredentialJournal(dir) + ".tmp" }

// MaxCredentialBytes bounds one file of a shared login, on its way in and out.
//
// The largest real login file is a few kilobytes (a Claude credential file is
// under 2 KB, an OpenCode export of several providers under 10), and a write
// holds the whole login in memory and in its journal, so a file that size is
// not a login: a mebibyte is two orders of magnitude of headroom and small
// enough that the worst case is nothing — the same bound a credential bundle is
// held to, decompressed.
const MaxCredentialBytes = 1 << 20

// journalVersion is the one journal shape this build writes and finishes. A
// journal of another version is refused rather than read, because a build that
// finished a write it only half understood would put a login nobody wrote in
// front of every seat.
const journalVersion = 1

// credentialJournal is one committed write of a shared login: the content of
// every file it puts in place, and every name it removes.
//
// THE OBVIOUS ALTERNATIVE, an immutable directory per login behind a symlink
// swapped by one rename, is no less locked: a reader resolving the link
// without the lock may read a directory a writer is collecting, an absent file
// reading as half a login again. It adds a collector, a directory every
// consumer's path goes through and a layout every document must change for;
// the journal leaves the directory a directory of files.
type credentialJournal struct {
	Version int               `json:"version"`
	Write   map[string][]byte `json:"write"`
	Remove  []string          `json:"remove"`
}

// UnappliedError is a login write that COMMITTED — its journal stands, so the
// new login is the login of record and every reader of the directory puts it in
// place before it reads — and could not be put in place itself: a file the disk
// refused, a directory that would not sync.
//
// Its own type because it is the one failure after which the write HAS
// happened, and the caller's steps that follow a write (an operator epoch, a
// logout's record) must follow this one too: [Committed] is how a caller asks.
type UnappliedError struct {
	// Journal is where the committed login is recorded.
	Journal string
	// Err is why it could not be put in place now.
	Err error
}

func (e *UnappliedError) Error() string {
	return fmt.Sprintf("hostbox: the new login is recorded in %s and is the login of record — "+
		"every reader of the directory puts it in place before it reads — but putting it in "+
		"place now failed: %v", e.Journal, e.Err)
}

func (e *UnappliedError) Unwrap() error { return e.Err }

// Committed reports whether a [WriteCredentials] that answered err replaced the
// login of record: it succeeded, or it failed only after its journal landed
// ([*UnappliedError]).
func Committed(err error) bool {
	var unapplied *UnappliedError
	return err == nil || errors.As(err, &unapplied)
}

// CredentialStep is one step of a journaled login write ([WriteCredentials]),
// as the write reaches it: a point at which a writer killed there leaves a
// directory no other instant leaves.
//
// Between two steps nothing a reader can see changes (a staged file is
// invisible until its rename, which is the step; a directory sync changes only
// what a power loss keeps), so a writer killed at every step has been killed at
// every instant that matters.
type CredentialStep string

const (
	// StepCommitted is the write's journal landing: the write is the login
	// of record, and nothing it writes or removes has moved yet.
	StepCommitted CredentialStep = "committed"
	// StepWritten is one file of the login, the one named, put in place: the
	// files after it in name order, and every removal, have not moved yet.
	StepWritten CredentialStep = "written"
	// StepRemoved is one file the login does not hold, the one named,
	// removed: every file of the login is in place, and the removals after it
	// in name order have not moved yet.
	StepRemoved CredentialStep = "removed"
	// StepClosed is the journal's removal: the write is finished.
	StepClosed CredentialStep = "closed"
)

// Valid reports whether s is one of the four steps.
func (s CredentialStep) Valid() bool {
	switch s {
	case StepCommitted, StepWritten, StepRemoved, StepClosed:
		return true
	}
	return false
}

// CredentialSteps observes a login write's steps as it reaches them: the step,
// and the file it is about ("" for a step about the whole login). It is called
// on the writing goroutine, under the caller's hold of the lock, after the
// step happened, so an observer that does not return holds the write there:
// how the kill matrix (internal/hostbox/hostboxtest) parks a writer's process
// to kill it.
//
// NIL OBSERVES NOTHING, and nil is every write the engine makes. What changes
// nothing a reader can see (a file already holding its content, a removal of
// an absent file, a directory sync, a staged file) reports nothing, and so does
// a READER FINISHING a write: it puts in place what the write would, in order,
// so a reader killed part way leaves what a write killed at a step leaves.
type CredentialSteps func(step CredentialStep, name string)

// reach reports step to s, where there is an s.
func (s CredentialSteps) reach(step CredentialStep, name string) {
	if s != nil {
		s(step, name)
	}
}

// ReadCredentials reads the login the shared credential directory dir holds,
// among names: the content of every one of them the directory holds a regular
// file under, by name, and none of the others — ONE committed login, never
// part of one write and part of another.
//
// The caller holds dir's lock ([LockCredentials]). A write that stopped part
// way is finished first; one that cannot be finished is an error rather than
// the half it left, which is what this exists to end.
//
// A name that is there but is no regular file, or exceeds [MaxCredentialBytes],
// is an error rather than an absence: read as nothing it would hand a seat a
// login with a file missing, the half login again.
func ReadCredentials(dir string, names []string) (map[string][]byte, error) {
	if err := checkNames(names); err != nil {
		return nil, err
	}
	if err := sweep(dir); err != nil {
		return nil, err
	}
	if err := settle(dir, nil); err != nil {
		return nil, err
	}
	login := map[string][]byte{}
	for _, name := range names {
		content, present, err := readCredential(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if present {
			login[name] = content
		}
	}
	return login, nil
}

// WriteCredentials makes the login the shared credential directory dir holds
// EXACTLY login, over names: every name login holds is written, every other
// name in names removed, nothing else touched, as ONE committed step
// ([CredentialJournal]). names is every file the login may hold, and the write
// decides each: a caller writing only what changed passes the rest as it read
// them under the same lock. login may name nothing outside names.
//
// The caller holds dir's lock ([LockCredentials]). A write left standing is
// REPLACED: its decisions about names in names are superseded, others carried
// over, so nothing committed is lost. A failure before the journal lands
// writes nothing; after it, an [*UnappliedError] ([Committed]).
func WriteCredentials(dir string, names []string, login map[string][]byte, steps CredentialSteps) error {
	if err := checkNames(names); err != nil {
		return err
	}
	journal := credentialJournal{Version: journalVersion, Write: map[string][]byte{}, Remove: []string{}}
	decided := map[string]bool{}
	for _, name := range names {
		decided[name] = true
		if content, ok := login[name]; ok {
			if len(content) > MaxCredentialBytes {
				return fmt.Errorf("hostbox: the login file %q is larger than %d bytes, so it was not "+
					"written — a file that size is not a login", name, MaxCredentialBytes)
			}
			journal.Write[name] = content
		} else {
			journal.Remove = append(journal.Remove, name)
		}
	}
	for name := range login {
		if !decided[name] {
			return fmt.Errorf("hostbox: the login names %q, which is none of the files it may "+
				"hold (%s)", name, strings.Join(names, ", "))
		}
	}
	if err := sweep(dir); err != nil {
		return err
	}
	standing, ok, err := readJournal(dir)
	if err != nil {
		return err
	}
	if ok {
		for name, content := range standing.Write {
			if !decided[name] {
				journal.Write[name] = content
			}
		}
		for _, name := range standing.Remove {
			if !decided[name] {
				journal.Remove = append(journal.Remove, name)
			}
		}
	}
	if err = os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("hostbox: preparing %q: %w", dir, err)
	}
	raw, err := json.Marshal(journal)
	if err != nil {
		return fmt.Errorf("hostbox: encoding the login write: %w", err)
	}
	if err = writeJournal(dir, raw); err != nil {
		return fmt.Errorf("hostbox: recording the login write in %s: %w", CredentialJournal(dir), err)
	}
	steps.reach(StepCommitted, "")
	if err = settle(dir, steps); err != nil {
		return &UnappliedError{Journal: CredentialJournal(dir), Err: err}
	}
	return nil
}

// writeJournal puts raw at dir's journal through its staged copy
// ([stagedJournal]) and a rename, under dir's lock the caller holds.
func writeJournal(dir string, raw []byte) error {
	journal := CredentialJournal(dir)
	parent, err := os.OpenRoot(filepath.Dir(journal))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	return replaceStaged(parent, filepath.Base(journal), filepath.Base(stagedJournal(dir)), bytes.NewReader(raw))
}

// sweep removes what a write of dir's login killed between staging a file and
// its rename left behind: the journal's staged copy beside dir, and every file
// staged inside it. The caller holds dir's lock, which every writer that
// stages either holds, so nothing it removes is a write in progress. Only
// regular files go: anything else at such a name is no copy a write made.
//
// A SIGKILL, OOM kill or power loss runs no deferred removal, leaving
// credential bytes under a name no reader reads and no logout removes (a
// logout removes the login's own names), so every read and write sweeps first.
// The directory's PARENT is never swept: other writers stage there under
// other locks.
func sweep(dir string) error {
	staged := []string{stagedJournal(dir)}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("hostbox: reading %q for what a killed login write staged: %w", dir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), stagedPrefix) {
			staged = append(staged, filepath.Join(dir, entry.Name()))
		}
	}
	for _, path := range staged {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) || (err == nil && !info.Mode().IsRegular()) {
			continue
		}
		if err == nil {
			err = os.Remove(path)
		}
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("hostbox: removing %s, a copy a killed login write staged: %w", path, err)
		}
	}
	return nil
}

// settle finishes the write a standing journal records, if one does: the
// journal made durable, every file it names put in place, the directory synced,
// and the journal removed — in that order, so that across a power loss as well
// as a kill no state exists in which the journal is gone and its files are not
// all in place.
//
// The journal's removal is NOT synced: a removal a power loss undoes leaves the
// journal to be finished again, which writes what is already there.
//
// steps observes what it puts in place ([CredentialSteps]): the writer's own
// observer when a write settles itself, nil when a reader finishes a write.
func settle(dir string, steps CredentialSteps) error {
	journal, ok, err := readJournal(dir)
	if err != nil || !ok {
		return err
	}
	path := CredentialJournal(dir)
	// THE JOURNAL DURABLE BEFORE ANY FILE MOVES: a rename of a credential file
	// that reached the disk ahead of the journal's own is a half login with
	// nothing left to finish it.
	if err := SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("hostbox: syncing the directory of %s: %w", path, err)
	}
	if err := apply(dir, journal, steps); err != nil {
		return fmt.Errorf("hostbox: finishing the login write recorded in %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("hostbox: closing the login write recorded in %s: %w", path, err)
	}
	steps.reach(StepClosed, "")
	return nil
}

// apply puts a journal's login in place: its files written, in name order,
// then its removals, then the directory synced. A file already holding what
// the journal says is left alone, so finishing a write a second time — after a
// power loss undid the journal's removal — writes nothing. Each file it puts
// in place and each it removes is a step ([CredentialSteps]); one it leaves
// alone, or finds already gone, is none.
func apply(dir string, journal credentialJournal, steps CredentialSteps) error {
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("preparing %q: %w", dir, err)
	}
	for _, name := range slices.Sorted(maps.Keys(journal.Write)) {
		target := filepath.Join(dir, name)
		content := journal.Write[name]
		if current, present, err := readCredential(target); err == nil && present && bytes.Equal(current, content) {
			continue
		}
		// REPLACED rather than rewritten in place: a temporary file and a
		// rename, so a file is never seen half written either.
		if err := ReplaceFile(target, bytes.NewReader(content)); err != nil {
			return fmt.Errorf("writing %q: %w", target, err)
		}
		steps.reach(StepWritten, name)
	}
	removals := slices.Clone(journal.Remove)
	slices.Sort(removals)
	for _, name := range removals {
		target := filepath.Join(dir, name)
		err := os.Remove(target)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("removing %q: %w", target, err)
		}
		steps.reach(StepRemoved, name)
	}
	if err := SyncDir(dir); err != nil {
		return fmt.Errorf("syncing %q: %w", dir, err)
	}
	return nil
}

// readJournal reads dir's standing journal, and reports false where none
// stands.
func readJournal(dir string) (credentialJournal, bool, error) {
	path := CredentialJournal(dir)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return credentialJournal{}, false, nil
	}
	if err != nil {
		return credentialJournal{}, false, fmt.Errorf("hostbox: reading %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return credentialJournal{}, false, unreadableJournal(path, errors.New("it is not a regular file"))
	}
	raw, err := os.ReadFile(path) //nolint:gosec // beside the caller's own credential directory
	if err != nil {
		return credentialJournal{}, false, fmt.Errorf("hostbox: reading %s: %w", path, err)
	}
	var journal credentialJournal
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return credentialJournal{}, false, unreadableJournal(path, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return credentialJournal{}, false, unreadableJournal(path, errors.New("it holds more than one record"))
	}
	if journal.Version != journalVersion {
		return credentialJournal{}, false, unreadableJournal(path,
			fmt.Errorf("it is version %d, and this build finishes version %d", journal.Version, journalVersion))
	}
	if err := checkNames(append(slices.Collect(maps.Keys(journal.Write)), journal.Remove...)); err != nil {
		return credentialJournal{}, false, unreadableJournal(path, err)
	}
	return journal, true, nil
}

// unreadableJournal is a journal this build cannot finish, which NAMES THE WAY
// OUT: refused, every reader and writer of the login stops at it, and what
// discards it is the one thing the person reading this needs.
func unreadableJournal(path string, err error) error {
	return fmt.Errorf("hostbox: %s does not record a login write this build can finish (%v) — it is "+
		"a write of the shared login that stopped part way, so the directory beside it may hold part "+
		"of one login and part of another; remove it to discard that write, then sign the entry in "+
		"again", path, err)
}

// readCredential reads one file of a shared login, reporting false where there
// is none.
func readCredential(path string) ([]byte, bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("hostbox: reading the login file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("hostbox: the login file %q is not a regular file", path)
	}
	if info.Size() > MaxCredentialBytes {
		return nil, false, fmt.Errorf("hostbox: the login file %q is larger than %d bytes, which is "+
			"not a login", path, MaxCredentialBytes)
	}
	content, err := os.ReadFile(path) //nolint:gosec // inside the caller's own credential directory
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("hostbox: reading the login file %q: %w", path, err)
	}
	return content, true, nil
}

// checkNames holds every name a login is read or written under to ONE FILE
// NAME: no separator, and not "", "." or "..". A login file is kept in the
// shared directory under its base name, so a name that is a path is one that
// would read or write somewhere else.
func checkNames(names []string) error {
	for _, name := range names {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
			return fmt.Errorf("hostbox: %q is not the name of a file in a login", name)
		}
	}
	return nil
}

// SyncDir makes the entries of dir — a rename into it, a removal from it —
// durable.
//
// Exported for a write that must reach the disk BEFORE a login write does
// (the shared login's operator epoch): [ReplaceFile] syncs the file it renames
// but not the directory holding the rename.
//
// A FILESYSTEM THAT CANNOT SYNC A DIRECTORY is not refused: some network and
// FUSE mounts answer EINVAL or ENOTSUP and never promised an order. The
// journal still makes a write all or nothing against a killed process, the
// failure a host has; refusing would leave the entry unable to sign in.
func SyncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // the caller's own directory
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
