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

// A SHARED LOGIN IS A SET OF FILES, AND IT IS WRITTEN AS ONE.
//
// A coding CLI may keep its login in more than one file — gemini-cli's OAuth
// token beside the account it names, kimi-code's token beside the config.toml
// that declares its provider, cursor-agent's sign-in beside its settings — and
// those files are one login only while they come from the same sign-in. Every
// writer of the shared credential directory used to replace them one rename at
// a time, so a write that failed or was killed between two renames left half of
// one: a directory that every presence check read as a login, every seed handed
// a seat, every bundle exported, and that the automatic restore of a configured
// bundle declined to repair because the directory was not empty.
//
// So a write is JOURNALED, under the directory's lock ([LockCredentials]) that
// every writer already takes:
//
//  1. the whole new login — every file it holds, and every name it does not —
//     is written to the directory's JOURNAL ([CredentialJournal]) through a
//     temporary file and a rename, and the journal's directory is synced. THAT
//     RENAME IS THE COMMIT: from here on the new login is the login of record;
//  2. each file is replaced or removed, and the credential directory is
//     synced;
//  3. the journal is removed.
//
// A JOURNAL THAT STANDS is a write that stopped between 1 and 3 — a process
// killed, a disk that refused a file — and every READ under the lock finishes it
// first ([ReadCredentials]), so no reader that holds the lock ever sees a
// directory part way through a write: it reads the login before the write or
// the login after it, whichever the journal says. A write that finds one
// standing REPLACES it rather than finishing it ([WriteCredentials]): the new
// write states every name it knows, so the old one's decision about each of
// them is superseded unapplied — which is what lets a logout discard a login
// that was never put in place, rather than writing it to disk to delete it.
//
// THE OBVIOUS ALTERNATIVE, an immutable directory per login behind a symbolic
// link swapped by one rename, is no less locked: a reader that resolved the link
// without the lock reads a directory a later writer may already be collecting,
// and an absent file there reads as half a login again — so the readers take
// the lock either way, and the link adds a collector, a directory every
// consumer's path now goes through, and a layout every document describing the
// directory has to change for. The journal leaves the directory a directory of
// files, where every path a consumer was handed still names the file it did.

// CredentialJournal is the journal of the shared credential directory dir
// ([WriteCredentials]): dir + ".pending", BESIDE the directory for the reason
// its lock is ([CredentialLock]) — nothing that copies the directory, a login
// bundle or a box's seed, ever carries a write in progress along with the
// login it is part way through replacing.
func CredentialJournal(dir string) string { return filepath.Clean(dir) + ".pending" }

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

// ReadCredentials reads the login the shared credential directory dir holds,
// among names: the content of every one of them the directory holds a regular
// file under, by name, and none of the others — ONE committed login, never
// part of one write and part of another.
//
// The caller holds dir's lock ([LockCredentials]). A write that stopped part
// way is finished first, so what is read is the login that write committed; one
// that cannot be finished is an error rather than the half it left, because a
// login read as whole from half of one is what this exists to end.
//
// A name that is there but is not a regular file, or is larger than
// [MaxCredentialBytes], is an error rather than an absence: read as nothing it
// would hand a seat a login with a file missing, which is the half login again.
func ReadCredentials(dir string, names []string) (map[string][]byte, error) {
	if err := checkNames(names); err != nil {
		return nil, err
	}
	if err := settle(dir); err != nil {
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
// EXACTLY login, over names: every name login holds is written with its
// content, every other name in names is removed, and nothing else in the
// directory is touched — as ONE committed step ([CredentialJournal]).
//
// names is every file the login may hold, and the write decides each of them:
// a caller that writes only what changed passes the rest as it read them under
// the same hold of the lock. login may name nothing outside names.
//
// The caller holds dir's lock ([LockCredentials]). A write left standing by an
// earlier one is REPLACED rather than finished: its decision about every name in
// names is superseded, and only its decisions about names outside them are
// carried into this write's journal, so nothing it committed is lost and
// nothing this write states is overridden.
//
// A failure before the journal lands writes nothing; one after it is an
// [*UnappliedError] — see [Committed].
func WriteCredentials(dir string, names []string, login map[string][]byte) error {
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
	if err = ReplaceFile(CredentialJournal(dir), bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("hostbox: recording the login write in %s: %w", CredentialJournal(dir), err)
	}
	if err = settle(dir); err != nil {
		return &UnappliedError{Journal: CredentialJournal(dir), Err: err}
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
func settle(dir string) error {
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
	if err := apply(dir, journal); err != nil {
		return fmt.Errorf("hostbox: finishing the login write recorded in %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("hostbox: closing the login write recorded in %s: %w", path, err)
	}
	return nil
}

// apply puts a journal's login in place: its files written, in name order,
// then its removals, then the directory synced. A file already holding what
// the journal says is left alone, so finishing a write a second time — after a
// power loss undid the journal's removal — writes nothing.
func apply(dir string, journal credentialJournal) error {
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
	}
	removals := slices.Clone(journal.Remove)
	slices.Sort(removals)
	for _, name := range removals {
		target := filepath.Join(dir, name)
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %q: %w", target, err)
		}
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
// Exported for a write that has to reach the disk BEFORE a login write does,
// which [ReplaceFile] alone does not promise: it syncs the file it renames into
// place but not the directory holding the rename — the shared login's
// operator epoch, which a login write must never outlive.
//
// A FILESYSTEM THAT CANNOT SYNC A DIRECTORY is not refused: some network and
// FUSE mounts answer EINVAL or ENOTSUP, and such a filesystem never promised
// an order to wait for. The journal still makes a write all or nothing against
// a killed process there, which is the failure a host actually has; refusing
// every login write instead would leave the entry unable to sign in at all.
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
