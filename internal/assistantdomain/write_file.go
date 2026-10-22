package assistantdomain

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// fileRecord is one file's record as the [FileStore] forms it: what the person
// or the run handed in, its name sealed, and the object its sealed content was
// uploaded into.
type fileRecord struct {
	file       NewFile
	nameSealed string
	object     objstore.Object
	contentKey string
}

// recordFile publishes one file's record on its conversation's subject, decided
// in a snapshot that finds the conversation the call's owner's, the object's key
// young enough to be named, and the conversation below its bound for the file's
// origin. ADDITIVE: the id is the caller's, so a duplicate publish writes one
// row, and two uploads never contend; the bound is decided again at the apply
// ([applyFile]).
func (w *Writer) recordFile(ctx context.Context, c Call, r fileRecord) (statelog.Result, error) {
	if err := c.validate(true); err != nil {
		return statelog.Result{}, err
	}
	if err := r.object.Validate(); err != nil {
		return statelog.Result{}, fmt.Errorf("%w: the file's object: %w", ErrInvalid, err)
	}
	f := r.file
	turn := ""
	if f.Turn != uuid.Nil {
		turn = f.Turn.String()
	}
	at := w.now()
	subject, scope := ConversationSubject(f.Conversation), ScopeSet{Subject: true}
	return w.publish(ctx, request(subject, scope, c.OpID, statelog.PatternAdditive,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, err := owned(ctx, tx, c.Owner, f.Conversation.String()); err != nil {
				return statelog.Decision{}, err
			}
			// THE KEY'S AGE IS JUDGED HERE, on the deciding node's clock,
			// for the reason objstore.RecordWithin gives: a record naming an
			// older key could land after a collection read it as nobody's.
			if age := w.now().Sub(r.object.Key.Minted()); age > objstore.RecordWithin {
				return statelog.Decision{}, invalid("the upload of %s is %s old, past "+
					"the %s a write may name an object within: upload it again", f.ID,
					age.Truncate(time.Second), objstore.RecordWithin)
			}
			// AN ID NAMES ONE FILE FOR EVER: one another conversation's
			// file already holds would apply as nothing and read back as
			// a file that is not there.
			var elsewhere int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assistant_files
				WHERE id = ?`, f.ID.String()).Scan(&elsewhere); err != nil {
				return statelog.Decision{}, fmt.Errorf("assistantdomain: read file %s: %w",
					f.ID, err)
			}
			if elsewhere > 0 {
				return statelog.Decision{}, invalid("file id %s already names another "+
					"file: mint a new id", f.ID)
			}
			held, err := filesHeld(ctx, tx, f.Conversation.String(), string(f.Origin))
			if err != nil {
				return statelog.Decision{}, err
			}
			if held >= FilesPerConversation(f.Origin) {
				return statelog.Decision{}, fmt.Errorf("%w: conversation %s keeps %d %s "+
					"files, the most it keeps", ErrTooManyFiles, f.Conversation, held, f.Origin)
			}
			return decision(stamp, c.By, subject, OpFileStored, scope, c.OpID, FileRecorded{
				V: baseRecordVersion, File: f.ID.String(), Turn: turn,
				Owner: c.Owner.String(), Origin: f.Origin, NameSealed: r.nameSealed,
				MediaType: f.MediaType, Size: int64(len(f.Content)),
				Object: r.object.Key.String(), Hash: string(r.object.Hash),
				ObjectSize: r.object.Size, ContentKey: r.contentKey,
			}, at)
		}))
}

// checkNewFile refuses a file the domain could not keep, naming the field.
func checkNewFile(f NewFile) error {
	switch {
	case f.ID == uuid.Nil:
		return invalid("the file id is required")
	case f.Conversation == uuid.Nil:
		return invalid("the file's conversation is required")
	case !f.Origin.Valid():
		return invalid("origin %q is not one of %s", f.Origin, words.Or(wire.FileOrigins))
	case len(f.Content) == 0:
		return invalid("file %s is empty", f.ID)
	case len(f.Content) > wire.FileMaxBytes:
		return invalid("file %s is %d bytes and the maximum is %d", f.ID,
			len(f.Content), wire.FileMaxBytes)
	}
	if allowed := fileMediaTypes(f.Origin); !slices.Contains(allowed, f.MediaType) {
		return invalid("a %s file of type %q is not kept: only %s are",
			f.Origin, f.MediaType, words.And(allowed))
	}
	return checkFileName(f.Name)
}

// checkFileName refuses a name a download could not carry: empty, past
// [FileNameMaxBytes], not UTF-8, a path, or holding a control character, a
// quote or a semicolon, which a Content-Disposition header would read as its
// own syntax. internal/assistant's SniffFile keeps names to the same rule
// before they reach here; this is the domain's own floor.
func checkFileName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return invalid("a file needs a name")
	case !utf8.ValidString(name) || len(name) > FileNameMaxBytes:
		return invalid("a file's name must be UTF-8 of at most %d bytes", FileNameMaxBytes)
	case strings.ContainsAny(name, `/\`) || name == "." || name == "..":
		return invalid("a file's name %q is a path", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '"' || r == ';' {
			return invalid("a file's name %q holds a character a download cannot name", name)
		}
	}
	return nil
}
