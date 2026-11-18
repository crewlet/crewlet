package assistantdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A CONVERSATION'S FILES (package doc, "Files"). A file's bytes, up to
// wire.FileMaxBytes, go to the object store (ADR-0026) because the estate is
// copied whole onto every data node, sealed ([AAD], [ColumnContent]) because
// the bucket may be somebody else's (ADR-0053); moved under another row, an
// object does not open. The row records the content's key, so a rotation
// counts it without a fetch ([Reader.SealedKeys]). The object is uploaded
// before its record, so no reader learns of a file whose bytes are nowhere;
// one whose record never lands is named by no row and collected a day later.
// Reads are keyed on owner and conversation ([ErrNotFound] otherwise), except
// [FileStore.AuditOpen].

// FileObjectReferences is the table whose rows keep a conversation's files'
// objects alive: the declaration internal/objstore/references collects.
//
// REQUIRED (ADR-0081): a kept file's bytes are nothing builds again, so its
// objects are audited and every backup carries them until a purge or an
// erasure removes the row; hence the declaration names the log a restore
// replays and how one record names the table's rows (ADR-0082).
var FileObjectReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "assistant_files",
	Key: "object", Hash: "hash", Size: "object_size",
	Owner:    []string{"conversation_id", "id"},
	Standing: objstore.Required,
	Log:      Domain{}.Stream().Name,
	Deferred: Domain{}.DeferredTable(),
	Records:  fileRecordReferences,
}

// fileRecordReferences is [objstore.ReferenceTable.Records] for
// [FileObjectReferences]: the row one record writes, and the object it names.
//
// Only [OpFileStored] writes the table ([applyFile]); its file is answered
// whether or not this node's apply admits it (the bound is decided again
// there): a backup reads a superset of what a restore's rows name, never a
// subset. A purge or an erasure removes rows, so it answers none, costing at
// most an object the collector deletes. An unreadable record is
// [objstore.ErrRecordUnreadable] when [mayWriteFiles]. The owner is spelled
// conversation then file ([objstore.JoinOwner]), as [FileObjectReferences]
// lists it, without reading the declaration (an initialisation cycle).
func fileRecordReferences(payload []byte) ([]objstore.RecordReference, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return nil, fmt.Errorf("assistantdomain: a record no build reads: %w", err)
	}
	if env.V > RecordVersion || !env.Subject.Kind.Valid() || !env.Op.Valid() {
		if !mayWriteFiles(env) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: the record on %s is version %d (this build reads %d) "+
			"of a kind or op this build may not know, and the scope its writer stated "+
			"reaches a conversation", objstore.ErrRecordUnreadable, env.Subject, env.V,
			RecordVersion)
	}
	if env.Subject.Kind != KindConversation || env.Op != OpFileStored {
		return nil, nil
	}
	rec, err := Decode(payload)
	if err != nil {
		return nil, err
	}
	p, err := decodePayload[FileRecorded](rec)
	if err != nil {
		return nil, err
	}
	if !present(env.Subject.ID) || !present(p.File) || !present(p.Object) {
		// THE APPLY WRITES NO ROW for a file or an object it cannot name.
		return nil, nil
	}
	o := objstore.Object{Size: p.ObjectSize}
	if o.Key, err = objstore.ParseKey(p.Object); err != nil {
		return nil, fmt.Errorf("assistantdomain: the file record on %s: %w", env.Subject, err)
	}
	if o.Hash, err = objstore.ParseHash(p.Hash); err != nil {
		return nil, fmt.Errorf("assistantdomain: the file record on %s: %w", env.Subject, err)
	}
	return []objstore.RecordReference{{
		NamedBy: objstore.JoinOwner(env.Subject.ID, p.File), Object: o, Names: true,
	}}, nil
}

// mayWriteFiles reports whether a record this build cannot read may write an
// assistant_files row, from its kind and the scope its writer stated. An
// unknown kind may, since this build cannot place what it names; a known kind
// may unless every path of its scope is the framework's own (a file row lives
// under its conversation, so only the barrier and trimmed records name none).
func mayWriteFiles(env RecordEnvelope) bool {
	if !env.Subject.Kind.Valid() {
		return true
	}
	for _, p := range env.Scope.Resolve(env.Subject).Paths {
		if p != statelog.FrameworkScope {
			return true
		}
	}
	return false
}

// NewFile is one file to keep: its bytes, the type sniffed from them, and the
// name it is kept under.
type NewFile struct {
	// ID is the file's id, minted by the caller: derived from the upload's
	// operation, so a retried upload is one file.
	ID           uuid.UUID
	Conversation uuid.UUID
	// Turn is the turn that delivered the file, uuid.Nil for an attachment
	// uploaded before the question it is sent with.
	Turn      uuid.UUID
	Origin    wire.FileOrigin
	Name      string
	MediaType string
	Content   []byte
}

// File is one kept file's record.
type File struct {
	ID           uuid.UUID
	Conversation uuid.UUID
	// Turn is uuid.Nil for an attachment uploaded before its question.
	Turn      uuid.UUID
	Owner     uuid.UUID
	Origin    wire.FileOrigin
	Name      string
	MediaType string
	// Size is the file's own size, never its sealed object's.
	Size      int64
	CreatedAt time.Time

	// object is the sealed object, as every read checks it.
	object objstore.Object
}

// Ref is the file as a message or a card names it, with its owner's download
// route.
func (f File) Ref() wire.FileRef {
	return wire.FileRef{FileID: f.ID, Name: f.Name, MediaType: f.MediaType, Size: f.Size,
		Download: wire.FilePath(f.Conversation, f.ID)}
}

// FileStore keeps a conversation's files: the sealed object, then the row.
type FileStore struct {
	writer  *Writer
	reader  *Reader
	objects *objstore.Store
}

// NewFileStore builds the file store over the domain's writer and reader and
// the fleet's object store, refusing a missing one by name.
func NewFileStore(w *Writer, r *Reader, objects *objstore.Store) (*FileStore, error) {
	switch {
	case w == nil:
		return nil, errors.New("assistantdomain: the file store needs the domain's writer")
	case r == nil:
		return nil, errors.New("assistantdomain: the file store needs the domain's reader")
	case objects == nil:
		return nil, errors.New("assistantdomain: the file store needs the fleet's object " +
			"store, where a file's sealed bytes are kept")
	}
	return &FileStore{writer: w, reader: r, objects: objects}, nil
}

// Store keeps one file for the call's owner and answers its record.
//
// Refused before any upload: an invalid file ([ErrInvalid]), another owner's
// conversation ([ErrNotFound]), and one already holding
// [FilesPerConversation] of the file's origin ([ErrTooManyFiles]). An id
// already recorded (a retried upload) answers that record with a zero result
// and uploads nothing. An `applied` result is read back, so a record the apply
// refused at the bound is [ErrTooManyFiles] rather than a file nobody can read.
func (s *FileStore) Store(ctx context.Context, c Call, f NewFile) (File, statelog.Result, error) {
	if err := c.validate(true); err != nil {
		return File{}, statelog.Result{}, err
	}
	if err := checkNewFile(f); err != nil {
		return File{}, statelog.Result{}, err
	}
	existing, held, err := s.reader.fileAdmission(ctx, c.Owner, f.Conversation, f.ID, f.Origin)
	switch {
	case err != nil:
		return File{}, statelog.Result{}, err
	case existing != nil:
		return *existing, statelog.Result{}, nil
	case held >= FilesPerConversation(f.Origin):
		return File{}, statelog.Result{}, fmt.Errorf("%w: conversation %s keeps %d %s files, "+
			"the most it keeps", ErrTooManyFiles, f.Conversation, held, f.Origin)
	}
	sealed, err := s.writer.sealer.Seal(f.Content, AAD(f.Conversation, RecordFile, f.ID, ColumnContent))
	if err != nil {
		return File{}, statelog.Result{}, err
	}
	key, ok := secrets.EnvelopeKeyID(sealed)
	if !ok {
		return File{}, statelog.Result{}, errors.New("assistantdomain: the keyring sealed a " +
			"file's content into something that is not an envelope")
	}
	object, err := s.objects.Put(ctx, strings.NewReader(sealed), int64(len(sealed)),
		objstore.PutMeta{ContentType: "application/octet-stream"})
	if err != nil {
		return File{}, statelog.Result{}, fmt.Errorf("assistantdomain: upload file %s: %w", f.ID, err)
	}
	name, err := s.writer.sealer.sealString(f.Name, AAD(f.Conversation, RecordFile, f.ID, ColumnName))
	if err != nil {
		return File{}, statelog.Result{}, err
	}
	res, err := s.writer.recordFile(ctx, c, fileRecord{file: f, nameSealed: name,
		object: object, contentKey: key})
	if err != nil {
		return File{}, res, err
	}
	out := File{ID: f.ID, Conversation: f.Conversation, Turn: f.Turn, Owner: c.Owner,
		Origin: f.Origin, Name: f.Name, MediaType: f.MediaType, Size: int64(len(f.Content)),
		object: object}
	if res.Outcome != statelog.OutcomeApplied {
		return out, res, nil
	}
	// READ BACK: the apply decides the bound again in log order.
	stored, err := s.reader.File(ctx, c.Owner, f.Conversation, f.ID)
	if errors.Is(err, ErrNotFound) {
		if _, convErr := s.reader.Conversation(ctx, c.Owner, f.Conversation); convErr != nil {
			return File{}, res, convErr
		}
		return File{}, res, fmt.Errorf("%w: conversation %s reached its bound for %s files "+
			"before this one applied", ErrTooManyFiles, f.Conversation, f.Origin)
	}
	return stored, res, err
}

// Stat is one of the owner's files in one of their conversations, its bytes
// neither fetched nor opened: another person's, or one of another
// conversation, is [ErrNotFound].
func (s *FileStore) Stat(ctx context.Context, owner, conversation, file uuid.UUID) (File, error) {
	return s.reader.File(ctx, owner, conversation, file)
}

// Open is one of the owner's files and its bytes, opened and checked against
// its record: another person's, or one of another conversation, is
// [ErrNotFound].
func (s *FileStore) Open(ctx context.Context, owner, conversation, file uuid.UUID) (File, []byte, error) {
	f, err := s.reader.File(ctx, owner, conversation, file)
	if err != nil {
		return File{}, nil, err
	}
	content, err := s.content(ctx, f)
	return f, content, err
}

// AuditOpen is one file of a conversation read for compliance (decision D06),
// for the receipt of the compliance read [Writer.RecordAuditRead] recorded
// beside it: a receipt whose record has not applied on this node opens
// nothing, so no file is read before its owner can see who read it and why.
func (s *FileStore) AuditOpen(ctx context.Context, receipt AuditReceipt, file uuid.UUID) (File, []byte, error) {
	f, err := s.reader.auditFile(ctx, receipt, file)
	if err != nil {
		return File{}, nil, err
	}
	content, err := s.content(ctx, f)
	return f, content, err
}

// content fetches a file's sealed object, checked against its row by the
// object store, and opens it under the file's own associated data. Called after
// the estate read closes, so none is held open across a fetch of up to 7 MiB.
func (s *FileStore) content(ctx context.Context, f File) ([]byte, error) {
	body, err := s.objects.Open(ctx, f.object)
	if err != nil {
		return nil, fmt.Errorf("assistantdomain: fetch file %s: %w", f.ID, err)
	}
	sealed, readErr := io.ReadAll(io.LimitReader(body, f.object.Size+1))
	if closeErr := errors.Join(readErr, body.Close()); closeErr != nil {
		return nil, fmt.Errorf("assistantdomain: fetch file %s: %w", f.ID, closeErr)
	}
	plain, err := s.reader.sealer.Open(string(sealed),
		AAD(f.Conversation, RecordFile, f.ID, ColumnContent))
	if err != nil {
		return nil, fmt.Errorf("file %s: %w", f.ID, err)
	}
	if int64(len(plain)) != f.Size {
		return nil, fmt.Errorf("%w: file %s opens to %d bytes and its record says %d",
			ErrUnreadable, f.ID, len(plain), f.Size)
	}
	return plain, nil
}

// ---- reads -------------------------------------------------------------- //

// fileColumns is every column a File is read from, in [Reader.scanFile]'s order.
const fileColumns = `id, conversation_id, turn_id, owner_id, origin, name_sealed,
	media_type, size, object, hash, object_size, created_at`

// File is one of the owner's files in one of their conversations.
func (r *Reader) File(ctx context.Context, owner, conversation, file uuid.UUID) (File, error) {
	var f File
	err := r.read(ctx, ConversationScope(conversation.String()), false, func(tx *sql.Tx) error {
		var err error
		f, err = r.fileIn(ctx, tx, &owner, conversation, file)
		return err
	})
	return f, err
}

// fileAdmission is what a [FileStore.Store] decides before it uploads: the
// file already recorded under id, or how many files of origin the owner's
// conversation keeps, in one read.
func (r *Reader) fileAdmission(ctx context.Context, owner, conversation, id uuid.UUID,
	origin wire.FileOrigin) (*File, int, error) {

	var (
		existing *File
		held     int
	)
	err := r.read(ctx, ConversationScope(conversation.String()), false, func(tx *sql.Tx) error {
		if _, err := r.conversationIn(ctx, tx, conversation, &owner); err != nil {
			return err
		}
		f, err := r.fileIn(ctx, tx, &owner, conversation, id)
		switch {
		case err == nil:
			existing = &f
			return nil
		case !errors.Is(err, ErrNotFound):
			return err
		}
		held, err = filesHeld(ctx, tx, conversation.String(), string(origin))
		return err
	})
	return existing, held, err
}

// auditFile is one file of the receipt's conversation, for a compliance read
// whose record has applied on this node.
func (r *Reader) auditFile(ctx context.Context, receipt AuditReceipt, file uuid.UUID) (File, error) {
	if receipt.conversation == uuid.Nil || receipt.opID == "" {
		return File{}, invalid("a compliance read opens a file only for the receipt its " +
			"recorded read answered")
	}
	var f File
	err := r.read(ctx, ConversationScope(receipt.conversation.String()), false,
		func(tx *sql.Tx) error {
			if err := auditRecorded(ctx, tx, receipt); err != nil {
				return err
			}
			var err error
			f, err = r.fileIn(ctx, tx, nil, receipt.conversation, file)
			return err
		})
	return f, err
}

// fileIn reads one file of conversation in tx, keyed on owner unless owner is
// nil (the audited read alone passes nil).
func (r *Reader) fileIn(ctx context.Context, tx *sql.Tx, owner *uuid.UUID, conversation,
	file uuid.UUID) (File, error) {

	args := []any{file.String(), conversation.String()}
	where := `id = ? AND conversation_id = ?`
	if owner != nil {
		where += ` AND owner_id = ?`
		args = append(args, owner.String())
	}
	f, err := r.scanFile(tx.QueryRowContext(ctx,
		`SELECT `+fileColumns+` FROM assistant_files WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, fmt.Errorf("%w: file %s", ErrNotFound, file)
	}
	return f, err
}

// scanFile reads one file's row, its name opened.
func (r *Reader) scanFile(row rowScanner) (File, error) {
	var (
		f                                     File
		id, conversation, turn, owner, origin string
		nameSealed, object, hash              string
		created                               int64
	)
	if err := row.Scan(&id, &conversation, &turn, &owner, &origin, &nameSealed,
		&f.MediaType, &f.Size, &object, &hash, &f.object.Size, &created); err != nil {
		return File{}, err
	}
	var err error
	if f.ID, f.Conversation, f.Owner, err = parseThree(id, conversation, owner); err != nil {
		return File{}, err
	}
	if turn != "" {
		if f.Turn, err = uuid.Parse(turn); err != nil {
			return File{}, err
		}
	}
	if f.object.Key, err = objstore.ParseKey(object); err != nil {
		return File{}, fmt.Errorf("assistantdomain: file %s's object: %w", id, err)
	}
	if f.object.Hash, err = objstore.ParseHash(hash); err != nil {
		return File{}, fmt.Errorf("assistantdomain: file %s's object: %w", id, err)
	}
	f.Origin, f.CreatedAt = wire.FileOrigin(origin), store.DecodeTime(created)
	if f.Name, err = r.sealer.openString(nameSealed,
		AAD(f.Conversation, RecordFile, f.ID, ColumnName)); err != nil {
		return File{}, fmt.Errorf("file %s: %w", id, err)
	}
	return f, nil
}

// ---- the collector's estate -------------------------------------------- //

// ObjectEstate is this domain as the object store's passes read it: a barrier
// on the assistant log and a read of its rows (internal/objstore/collect's
// Estate), through which the collector keeps every object a file's row names.
type ObjectEstate struct {
	Reader *Reader
}

// Name is the domain, as [FileObjectReferences] spells it.
func (ObjectEstate) Name() string { return Domain{}.Name() }

// Barrier waits until this node has applied everything the log had committed
// when it was called, and answers where that is: a linearizable read of the
// whole domain, which appends the read index's barrier.
func (s ObjectEstate) Barrier(ctx context.Context) (statelog.Position, error) {
	if s.Reader == nil {
		return statelog.Position{}, errNoReader
	}
	served, err := s.Reader.log.Read(ctx, statelog.Query{
		Level: statelog.ReadLinearizable, Scope: DomainScope(), Set: true,
	}, func(*sql.Tx) error { return nil })
	if err != nil {
		return statelog.Position{}, err
	}
	return served.Position, nil
}

// Read runs fn over this node's rows, no earlier than at, and reports whether
// they cover every record of the domain: false while this node holds a record
// it could not apply, which the collector must read as incomplete.
func (s ObjectEstate) Read(ctx context.Context, at statelog.Position, fn func(*sql.Tx) error) (bool, error) {
	if s.Reader == nil {
		return false, errNoReader
	}
	served, err := s.Reader.log.Read(ctx, statelog.Query{
		Level: statelog.ReadStale, Scope: DomainScope(), Set: true, Floors: statelog.FloorAt(statelog.ReadStale, at).Floors,
	}, fn)
	if err != nil {
		return false, err
	}
	return served.Complete, nil
}

// errNoReader is an object estate built without the domain's reader.
var errNoReader = errors.New("assistantdomain: the object estate has no reader")
