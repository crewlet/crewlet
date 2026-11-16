package gitproc

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/textcut"
)

// THE RECORDS GIT PRINTS THAT MORE THAN ONE PACKAGE READS, each read here once
// (ADR-0008): a batch-check answer and an ls-tree -l entry.

// Object is one `cat-file --batch-check` answer: an object's id, type and
// size, or a submodule's commit id with Type "submodule" and Size 0.
type Object struct {
	ID, Type string
	Size     int64
}

// ParseCheck reads one --batch-check record: "<id> <type> <size>", or the
// "<id> submodule" git answers for a gitlink whose commit the repository
// lacks. A "<input> missing" record is the caller's to match to its input.
func ParseCheck(rec []byte) (Object, bool) {
	fields := strings.Fields(string(rec))
	if len(fields) < 2 || !gitref.ValidOID(fields[0]) {
		return Object{}, false
	}
	if len(fields) == 2 {
		if fields[1] != "submodule" {
			return Object{}, false
		}
		return Object{ID: fields[0], Type: fields[1]}, true
	}
	if len(fields) != 3 {
		return Object{}, false
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return Object{}, false
	}
	switch fields[1] {
	case "commit", "tree", "blob", "tag":
		return Object{ID: fields[0], Type: fields[1], Size: size}, true
	}
	return Object{}, false
}

// TreeEntry is one `ls-tree -l` record: "<mode> <type> <id> <size>\t<path>",
// the size "-" (Size -1) for a tree or a submodule.
type TreeEntry struct {
	Mode, Type, ID string
	Size           int64
	Path           []byte
}

// ParseTreeEntry reads one ls-tree -l record. A blob git could not read is
// printed "BAD" in place of its size: that is ErrMissingObject, never a size.
// Path aliases rec.
func ParseTreeEntry(rec []byte) (TreeEntry, error) {
	meta, path, ok := bytes.Cut(rec, []byte{'\t'})
	fields := strings.Fields(string(meta))
	if !ok || len(fields) != 4 || !gitref.ValidOID(fields[2]) {
		return TreeEntry{}, fmt.Errorf("gitproc: ls-tree answered %q, which is no entry", cut(rec))
	}
	e := TreeEntry{Mode: fields[0], Type: fields[1], ID: fields[2], Size: -1, Path: path}
	switch fields[3] {
	case "-":
	case "BAD":
		return TreeEntry{}, fmt.Errorf("%w: ls-tree could not read blob %s (BAD)", ErrMissingObject, e.ID)
	default:
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size < 0 {
			return TreeEntry{}, fmt.Errorf("gitproc: ls-tree answered %q, whose size is none", cut(rec))
		}
		e.Size = size
	}
	return e, nil
}

// cut is a record as an error quotes it: its opening.
func cut(rec []byte) string { return textcut.Ellipsis(string(rec), 200) }
