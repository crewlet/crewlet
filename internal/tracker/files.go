package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A project's files: the record, the rules a path follows, and the write.
//
// # What a file is here, and what it is not
//
// A file is a ROW — its project, its path, its content type, its size, the
// hash of its content and the OBJECT its bytes were uploaded into. The bytes
// themselves are in the object store (internal/objstore, ADR-0026) — one
// store the whole fleet shares, the broker's own bucket or an S3 bucket,
// rather than a copy in every data node's database — and nothing in this
// package ever reads one. What this package owns is the fact that the file
// exists and which object holds it: the object's key on the row IS the
// reference that keeps that object alive, so a file written here is a file
// whose bytes the collector will never delete, and a file removed or
// rewritten here is one whose old object it may.
//
// # The bytes go first, and the write that names them is bounded
//
// A writer uploads the object BEFORE it writes the record naming it, and the
// object store keeps an unnamed object for a day for exactly that window. The
// other order would publish a file whose content is not anywhere yet, and a
// reader in the gap would be told a file exists and then fail to read it.
//
// AND THE WRITE IS REFUSED once the key it names was minted more than
// [objstore.RecordWithin] ago, by the clock of the node deciding it
// ([ErrUploadStale]): the collector judges an object by its key's minting
// instant as well as by its age in the store, so a write naming an older key
// could land after a collection read it as nobody's — and name bytes that
// collection had just deleted. ADR-0027.
//
// # A removal is a stamp, and it drops the object
//
// The row stays, with who removed it and when, for the reason a task's does:
// every upsert here is guarded by the record's version and SKIPS an older one,
// and a guard needs a row to compare against — a deleted row would let a
// redelivered put bring the file back. What a removal does take away is the
// object, which is the whole point of removing a file from a store whose
// bytes are the expensive part.

// ErrNoFile reports a file this node has no row for, or holds removed.
//
// ITS OWN SENTINEL for [ErrNoTask]'s reason: a tool says "no such file" and an
// API says 404, and neither should when what actually happened is a mistyped
// project key — which is [ErrNoProject].
var ErrNoFile = errors.New("tracker: no such file")

// ErrUploadStale reports a write naming an object whose key was minted more
// than [objstore.RecordWithin] before the write was decided: the upload is
// too old to be named safely, and the file has to be uploaded again.
var ErrUploadStale = errors.New("tracker: the upload is too old to be recorded")

// The file caps. Refused at WRITE naming the value, never cut.
const (
	// MaxFileBytes is the largest file a project holds.
	//
	// A GIBIBYTE: the largest upload one request is expected to carry to
	// completion — a gibibyte at 100 Mbit/s is under a minute and a half —
	// and what one download streams back through one node. Larger artefacts
	// belong in a store built for them, with a link in the project.
	MaxFileBytes = 1 << 30

	// MaxFilePath is a path's length in bytes.
	//
	// 1 024, the PATH_MAX-shaped bound every filesystem an agent will
	// export to agrees with, and what keeps a listing's row a row.
	MaxFilePath = 1024

	// MaxContentType is a content type's length, which is RFC 6838's own
	// bound on a registered type and subtype together.
	MaxContentType = 255
)

// File is one file in a project, as its record carries it and its row holds
// it.
type File struct {
	V int `json:"v"`

	// Version is the row's version as it was READ — what `if_match`
	// compares against — and nothing a record states.
	Version uint64 `json:"version"`

	Project     string `json:"project"`
	Path        string `json:"path"`
	ContentType string `json:"content_type,omitempty"`

	// Hash and Size are the whole content's, and Object the key of the
	// object it was uploaded into. All three are empty on a removed file,
	// and only there: a live file always names its object.
	Hash   objstore.Hash `json:"hash,omitempty"`
	Size   int64         `json:"size"`
	Object objstore.Key  `json:"object,omitzero"`

	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`

	// RemovedAt is set on a removed file, with who removed it.
	RemovedAt *time.Time `json:"removed_at,omitempty"`
	RemovedBy string     `json:"removed_by,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// Removed reports whether the file has been taken away.
func (f File) Removed() bool { return f.RemovedAt != nil }

// Content is the object holding the file's bytes, as the object store reads
// it back — and false for a file whose row names none, which is a removed
// file and nothing else.
func (f File) Content() (objstore.Object, bool) {
	if f.Object.IsZero() {
		return objstore.Object{}, false
	}
	return objstore.Object{Key: f.Object, Hash: f.Hash, Size: f.Size}, true
}

// FileObjectReferences is the table whose rows keep a file's object alive —
// the declaration internal/objstore/references collects. See
// [objstore.ReferenceTable] for why a table missing from that list is the
// mistake that deletes files.
var FileObjectReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "tracker_files",
	Key: "object", Hash: "hash", Size: "size",
	Owner: []string{"project_key", "path"},
}

// NormalizeFilePath is a path as it is stored and addressed: slash-separated
// segments, no leading slash, and nothing a filesystem an agent exports to
// would read differently.
//
// REFUSED RATHER THAN REPAIRED where a repair would be a guess: an empty
// segment, `.` and `..` are three spellings a caller meant something by, and
// `a/../b` stored as `b` is a file somebody will look for under the name they
// typed. A leading slash and surrounding space are the one repair, because
// every caller means the same thing by them.
func NormalizeFilePath(raw string) (string, error) {
	p := strings.TrimLeft(strings.TrimSpace(raw), "/")
	switch {
	case p == "":
		return "", invalid("a file needs a path")
	case len(p) > MaxFilePath:
		return "", invalid("the path is %d bytes and the maximum is %d",
			len(p), MaxFilePath)
	case !utf8.ValidString(p):
		return "", invalid("the path %q is not UTF-8", p)
	case strings.HasSuffix(p, "/"):
		return "", invalid("the path %q ends in a slash, which names a "+
			"folder — a file needs a name after it", p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", invalid("the path %q carries a control character "+
				"or a backslash — use / between folders", p)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return "", invalid("the path %q has an empty folder in it", p)
		case ".", "..":
			return "", invalid("the path %q has a %q in it, which a path "+
				"stored here never resolves — name the file where it lives", p, seg)
		}
	}
	return p, nil
}

// checkFileProject refuses a project key that cannot lead a file's subject:
// the key is the half of the id before the dot, so a key holding a dot, a
// slash, a space or a wildcard would address some other file or none.
func checkFileProject(key string) error {
	if key == "" || strings.ContainsAny(key, "./ \t\n*>") {
		return invalid("%q is not a project key a file can live in", key)
	}
	return nil
}

// FilePut is one file written: where, what, and the object already uploaded.
type FilePut struct {
	Project     string
	Path        string
	ContentType string

	// Object is the content as the object store holds it — uploaded
	// BEFORE this write, under a key minted for that upload alone (see the
	// file's head).
	Object objstore.Object

	// IfMatch conditions the write on the version the caller read, or is
	// [NoIfMatch]. A file that does not exist, or was removed, is at
	// version zero only to NoIfMatch: conditioned on a version, a put
	// onto nothing is refused, because the caller read something that is
	// no longer there.
	IfMatch uint64
}

// checkFilePut normalises a put and refuses one that could not be stored —
// which is what keeps every refusal the applier could make
// ([fileMatches]) unreachable.
func checkFilePut(put *FilePut) error {
	put.Project = ProjectKey(put.Project)
	if err := checkFileProject(put.Project); err != nil {
		return err
	}
	path, err := NormalizeFilePath(put.Path)
	if err != nil {
		return err
	}
	put.Path = path
	put.ContentType = strings.TrimSpace(put.ContentType)
	switch {
	case len(put.ContentType) > MaxContentType:
		return invalid("the content type is %d bytes and the maximum is %d",
			len(put.ContentType), MaxContentType)
	case strings.ContainsAny(put.ContentType, "\r\n"):
		return invalid("the content type %q spans lines", put.ContentType)
	case put.Object.Size > MaxFileBytes:
		return invalid("%s is %d bytes and a project's files are at most %d",
			path, put.Object.Size, MaxFileBytes)
	}
	if err := put.Object.Validate(); err != nil {
		return invalid("the content of %s: %w", path, err)
	}
	return nil
}

// checkUploadAge refuses a put whose object's key was minted more than
// [objstore.RecordWithin] before now — the instant the write is DECIDED, on
// the node deciding it.
//
// NOTHING IS REFUSED FOR BEING MINTED IN THE FUTURE. A key that a fast clock
// minted ahead of this one protects itself: the collector deletes nothing
// whose minting instant is inside its grace, so the only effect of refusing it
// would be uploads failing from a node whose clock runs ahead.
func checkUploadAge(put FilePut, now time.Time) error {
	if age := now.Sub(put.Object.Key.Minted()); age > objstore.RecordWithin {
		return fmt.Errorf("%w: %s's content was uploaded under a key minted %s ago, "+
			"and a write may name one minted at most %s before it — upload it again",
			ErrUploadStale, put.Path, age.Truncate(time.Second), objstore.RecordWithin)
	}
	return nil
}

// PutFile writes a file at its path, creating it or replacing its content.
//
// WHOLE POST-STATE, like every document here, and the ADDRESS IS THE SUBJECT:
// two writers putting one path contend at the broker and exactly one wins,
// which is the whole of what makes a path one file.
func (w *Writer) PutFile(ctx context.Context, opID string, put FilePut) (WriteResult, error) {
	if err := checkFilePut(&put); err != nil {
		return WriteResult{}, err
	}
	subject := FileSubject(put.Project, put.Path)
	scope := ScopeSet{Subject: true, Container: put.Project}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// THE DECIDE'S OWN CLOCK, read here rather than before the
			// publish: a write retried after its key aged past the bound
			// is judged at the instant it is decided again.
			if err := checkUploadAge(put, w.Now()); err != nil {
				return statelog.Decision{}, err
			}
			if err := w.refuseFileProject(ctx, tx, put.Project); err != nil {
				return statelog.Decision{}, err
			}
			current, held, err := readFile(ctx, tx, subject)
			if err != nil {
				return statelog.Decision{}, err
			}
			live := held && !current.Removed()
			if put.IfMatch != NoIfMatch && (!live || current.Version != put.IfMatch) {
				return statelog.Decision{}, fmt.Errorf("%w: file %s in %s is at "+
					"version %d and this write was conditioned on %d — read it "+
					"again and decide", ErrStaleVersion, put.Path, put.Project,
					liveVersion(current, live), put.IfMatch)
			}
			post := File{
				V: DocumentVersion, Project: put.Project, Path: put.Path,
				ContentType: put.ContentType, Hash: put.Object.Hash,
				Size: put.Object.Size, Object: put.Object.Key,
				CreatedBy: w.Actor, CreatedAt: at, UpdatedBy: w.Actor, UpdatedAt: at,
			}
			if live {
				// THE CREATION FACTS ARE THE STORED ROW'S, for the view's
				// reason: a put that carried them would let a second
				// writer re-attribute a file somebody else made.
				post.CreatedBy, post.CreatedAt = current.CreatedBy, current.CreatedAt
			}
			return w.decide(stamp, subject, OpPatch, ChangeFileWritten, scope, opID,
				post, nil, at)
		},
	})
}

// RemoveFile takes a file away. Its object stops being named, and the object
// store's collector deletes it once it is past the grace.
//
// A FILE THAT IS NOT THERE IS [ErrNoFile], never a quiet success: a caller
// removing the wrong path should hear that the one it named did not exist.
func (w *Writer) RemoveFile(ctx context.Context, opID, project, path string,
	ifMatch uint64) (WriteResult, error) {

	project = ProjectKey(project)
	if err := checkFileProject(project); err != nil {
		return WriteResult{}, err
	}
	path, err := NormalizeFilePath(path)
	if err != nil {
		return WriteResult{}, err
	}
	subject := FileSubject(project, path)
	scope := ScopeSet{Subject: true, Container: project}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			current, held, err := readFile(ctx, tx, subject)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !held || current.Removed() {
				return statelog.Decision{}, fmt.Errorf("%w: %s in %s", ErrNoFile, path, project)
			}
			if ifMatch != NoIfMatch && current.Version != ifMatch {
				return statelog.Decision{}, fmt.Errorf("%w: file %s in %s is at "+
					"version %d and this removal was conditioned on %d",
					ErrStaleVersion, path, project, current.Version, ifMatch)
			}
			post := current
			post.V, post.Version = DocumentVersion, 0
			post.Hash, post.Size, post.Object = "", 0, objstore.Key{}
			post.UpdatedBy, post.UpdatedAt = w.Actor, at
			post.RemovedAt, post.RemovedBy = &at, w.Actor
			return w.decide(stamp, subject, OpPatch, ChangeFileRemoved, scope, opID,
				post, nil, at)
		},
	})
}

// liveVersion is the version a caller could have read: zero for a file that
// is not there.
func liveVersion(f File, live bool) uint64 {
	if !live {
		return 0
	}
	return f.Version
}

// refuseFileProject refuses a file in a project this node does not hold, or
// one that is archived — the same two refusals a task create meets, for the
// same reason: an archived project takes no new work.
func (w *Writer) refuseFileProject(ctx context.Context, tx *sql.Tx, key string) error {
	project, held, err := readProject(ctx, tx, key)
	switch {
	case err != nil:
		return err
	case !held:
		return fmt.Errorf("%w: %s (or it is not on this node yet: %w)",
			ErrNoProject, key, statelog.ErrUnavailable)
	}
	return AcceptFiles(key, project.Archived)
}

// AcceptFiles refuses a new file into project when it is archived, marked
// [ErrInvalid]: the same request can never land, and unarchiving the project
// is the change that lets it.
//
// EXPORTED FOR THE UPLOAD ROUTE, which asks before it streams a body that
// could be a gibibyte into the store only for the write naming it to be
// refused here — one sentence for both, so the two refusals cannot drift.
func AcceptFiles(project string, archived bool) error {
	if archived {
		return invalid("project %s is archived, so it takes no new files; "+
			"unarchive it first", project)
	}
	return nil
}

// readFile reads one file inside a write's own snapshot.
func readFile(ctx context.Context, tx *sql.Tx, s Subject) (File, bool, error) {
	return readDocument(ctx, tx, s, func(f *File, v uint64) { f.Version = v })
}
