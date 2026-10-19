package artifactupload

import (
	"bytes"
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// SourceText is up to limit bytes of a version's text file as its author wrote
// it, and whether that is all of it: what a conflict answer inlines, a tool
// reads a page of, and an edit is written against. A page is answered in its
// source form, the nonce placeholders the linter wrote removed, so an old_str
// quoted from it matches what [Service.Publish] applies edits to.
//
// The whole file is read through the store's verifying reader; a file longer
// than limit is read as a page of it, checked for its length, and cut at a
// character boundary.
func (s *Service) SourceText(ctx context.Context, e artifacts.Entry, limit int64) ([]byte, bool, error) {
	typ, err := typeOf(e.Path, e.ContentType)
	if err != nil {
		return nil, false, err
	}
	if !typ.Text {
		return nil, false, fmt.Errorf("%w: %q is a %s file, not text", ErrInvalid, e.Path, typ.Essence())
	}
	if limit <= 0 {
		return nil, false, fmt.Errorf("%w: a read of %d bytes", ErrInvalid, limit)
	}
	var (
		text     []byte
		complete = e.Object.Size <= limit
	)
	if complete {
		text, err = readObject(ctx, s.objects, e.Object, true)
	} else {
		// A PAGE IS READ BEYOND THE LIMIT by the placeholder's length, so
		// one the cut would split is removed whole before the cut.
		text, err = s.objects.ReadAt(ctx, e.Object, 0, limit+int64(len(lint.NonceAttr)))
	}
	if err != nil {
		return nil, false, fmt.Errorf("artifactupload: read %q: %w", e.Path, err)
	}
	if typ.Essence() == "text/html" {
		text = lint.Source(text)
	}
	if int64(len(text)) > limit {
		text, complete = text[:limit], false
		// CUT AT A CHARACTER: a last rune the cut split is dropped whole.
		for len(text) > 0 {
			if r, size := utf8.DecodeLastRune(text); r != utf8.RuneError || size > 1 {
				break
			}
			text = text[:len(text)-1]
		}
	}
	return bytes.Clone(text), complete, nil
}
