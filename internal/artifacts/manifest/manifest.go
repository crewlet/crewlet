package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/objstore"
)

// NewEntry is one file of a version: its path cleaned, its served type the
// media table's (the client's declaration only disambiguates), its object
// checked, and its size held to the bound for its class.
//
// The entry bound ([artifacts.MaxEntryBytes]) is checked by [Validate], which
// knows which path is the entry; this checks the per-file bounds every file is
// held to wherever it sits.
func NewEntry(path, declaredType string, obj objstore.Object, carried bool) (artifacts.Entry, error) {
	p, err := CleanPath(path)
	if err != nil {
		return artifacts.Entry{}, err
	}
	typ, err := mediatype.Resolve(p, declaredType)
	if err != nil {
		return artifacts.Entry{}, err
	}
	if err := obj.Validate(); err != nil {
		return artifacts.Entry{}, fmt.Errorf("%w: %q: %w", ErrInvalid, p, err)
	}
	e := artifacts.Entry{Path: p, ContentType: typ.Served, Object: obj, Carried: carried}
	if err := checkFile(e, typ); err != nil {
		return artifacts.Entry{}, err
	}
	return e, nil
}

// checkFile holds one entry to the bounds its path and type set.
func checkFile(e artifacts.Entry, typ mediatype.Type) error {
	limit := artifacts.MaxBinaryFileBytes
	if typ.Text {
		limit = artifacts.MaxTextFileBytes
	}
	if e.Object.Size > limit {
		return fmt.Errorf("%w: %q is %d bytes and a %s file may be at most %d",
			ErrInvalid, e.Path, e.Object.Size, sizeClass(typ), limit)
	}
	if e.Path == PreflightPath {
		if typ.Class != mediatype.ClassScript {
			return fmt.Errorf("%w: %q at the root is the preflight script and must be "+
				"JavaScript", ErrReserved, e.Path)
		}
		if e.Object.Size > artifacts.MaxPreflightBytes {
			return fmt.Errorf("%w: %q is %d bytes and the preflight script may be at "+
				"most %d", ErrReserved, e.Path, e.Object.Size, artifacts.MaxPreflightBytes)
		}
	}
	return nil
}

// sizeClass names the bound a refusal cites.
func sizeClass(typ mediatype.Type) string {
	if typ.Text {
		return "text"
	}
	return "binary"
}

// Validate refuses a manifest a version of kind may not hold: files out of
// bytewise path order or repeated, more than [artifacts.MaxFilesPerVersion] of
// them, more than [artifacts.MaxVersionBytes] in all, a file the table would
// serve as another type, an entry that is not one of the files or not a page
// the kind renders, or an entry past [artifacts.MaxEntryBytes].
//
// A NATIVE SHEET MAY HOLD NO FILE AT ALL: its content is its datastore, and a
// version of one with an empty manifest is the ordinary case (D03).
func Validate(kind artifacts.Kind, m artifacts.Manifest) error {
	if !kind.Valid() {
		return fmt.Errorf("%w: kind %q is not one this build publishes", ErrInvalid, kind)
	}
	if len(m.Files) > artifacts.MaxFilesPerVersion {
		return fmt.Errorf("%w: the version names %d files and the maximum is %d",
			ErrInvalid, len(m.Files), artifacts.MaxFilesPerVersion)
	}
	var total int64
	for i, e := range m.Files {
		if i > 0 && m.Files[i-1].Path >= e.Path {
			return fmt.Errorf("%w: the files are not in strictly ascending path order "+
				"at %q (after %q): a manifest is sorted bytewise by path and names "+
				"each path once", ErrInvalid, e.Path, m.Files[i-1].Path)
		}
		p, err := CleanPath(e.Path)
		if err != nil {
			return err
		}
		if p != e.Path {
			return fmt.Errorf("%w: %q is not in its stored form %q", ErrInvalid, e.Path, p)
		}
		typ, err := mediatype.Resolve(e.Path, e.ContentType)
		if err != nil {
			return err
		}
		if typ.Served != e.ContentType {
			return fmt.Errorf("%w: %q records type %q and the table serves it as %q",
				ErrInvalid, e.Path, e.ContentType, typ.Served)
		}
		if err := e.Object.Validate(); err != nil {
			return fmt.Errorf("%w: %q: %w", ErrInvalid, e.Path, err)
		}
		if err := checkFile(e, typ); err != nil {
			return err
		}
		if e.Script != nil && !typ.Scriptable {
			return fmt.Errorf("%w: %q carries a script policy and its type %q runs no "+
				"script", ErrInvalid, e.Path, e.ContentType)
		}
		total += e.Object.Size
	}
	if total > artifacts.MaxVersionBytes {
		return fmt.Errorf("%w: the version's files total %d bytes and the maximum is %d",
			ErrInvalid, total, artifacts.MaxVersionBytes)
	}
	return checkEntry(kind, m)
}

// checkEntry holds the manifest's entry to its kind's entry rule.
func checkEntry(kind artifacts.Kind, m artifacts.Manifest) error {
	spec, _ := kind.Spec()
	if m.Entry == "" {
		if spec.Entry.Optional {
			return nil
		}
		return fmt.Errorf("%w: a %s version names an entry: the file it opens on",
			ErrInvalid, kind)
	}
	e, ok := EntryAt(m, m.Entry)
	if !ok {
		return fmt.Errorf("%w: the entry %q is not one of the version's files",
			ErrInvalid, m.Entry)
	}
	if e.Object.Size > artifacts.MaxEntryBytes {
		return fmt.Errorf("%w: the entry %q is %d bytes and the maximum is %d",
			ErrInvalid, e.Path, e.Object.Size, artifacts.MaxEntryBytes)
	}
	// AN OPTIONAL ENTRY admits any of the version's files: a sheet that does
	// name one names a file shown beside the grid.
	if len(spec.Entry.Essences) == 0 && len(spec.Entry.Indexes) == 0 {
		return nil
	}
	typ, _ := mediatype.Resolve(e.Path, e.ContentType)
	if !spec.Entry.AdmitsAt(e.Path, typ.Essence()) {
		return fmt.Errorf("%w: a %s artifact's entry is %s, and %q is %s", ErrInvalid, kind,
			spec.Entry.Names(), e.Path, typ.Essence())
	}
	return nil
}

// EntryAt is the file at path, and false when the manifest holds none.
func EntryAt(m artifacts.Manifest, path string) (artifacts.Entry, bool) {
	i, found := slices.BinarySearchFunc(m.Files, path, func(e artifacts.Entry, p string) int {
		return strings.Compare(e.Path, p)
	})
	if !found {
		return artifacts.Entry{}, false
	}
	return m.Files[i], true
}

// Bytes is the sum of the manifest's file sizes: what a version holds, whether
// or not its objects are shared with another version.
func Bytes(m artifacts.Manifest) int64 {
	var total int64
	for _, e := range m.Files {
		total += e.Object.Size
	}
	return total
}

// Canonical is m in its one form: files sorted bytewise by path, and each
// script policy's handler hashes sorted and unique.
//
// ONE FORM, because the manifest is stored as bytes in every data node's row
// and compared across nodes by the identity claim: two writers that listed the
// same files in another order, or the same hashes twice, must produce the same
// row.
func Canonical(m artifacts.Manifest) artifacts.Manifest {
	out := artifacts.Manifest{Entry: m.Entry, Files: make([]artifacts.Entry, len(m.Files))}
	for i, e := range m.Files {
		if e.Script != nil {
			script := *e.Script
			script.HandlerHashes = slices.Compact(slices.Sorted(slices.Values(e.Script.HandlerHashes)))
			if len(script.HandlerHashes) == 0 {
				script.HandlerHashes = nil
			}
			e.Script = &script
		}
		out.Files[i] = e
	}
	slices.SortStableFunc(out.Files, func(a, b artifacts.Entry) int {
		return strings.Compare(a.Path, b.Path)
	})
	return out
}

// Encode is the canonical manifest's bytes: what a record carries and a row
// stores.
func Encode(m artifacts.Manifest) ([]byte, error) {
	c := Canonical(m)
	if c.Files == nil {
		c.Files = []artifacts.Entry{}
	}
	out, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("manifest: encode: %w", err)
	}
	return out, nil
}

// Decode reads a stored manifest back, refusing one that is not canonical:
// a row that held another form would be a row two nodes could disagree about.
func Decode(raw []byte) (artifacts.Manifest, error) {
	var m artifacts.Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil {
		return artifacts.Manifest{}, fmt.Errorf("manifest: decode: %w", err)
	}
	again, err := Encode(m)
	if err != nil {
		return artifacts.Manifest{}, err
	}
	if !bytes.Equal(again, bytes.TrimSpace(raw)) {
		return artifacts.Manifest{}, fmt.Errorf("%w: the stored manifest is not in "+
			"canonical form", ErrInvalid)
	}
	return m, nil
}
