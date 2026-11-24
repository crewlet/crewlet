package executordomain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE SESSION'S BYTES: one object per capture, streamed a chunk at a time and
// sealed before a byte reaches the store, which is not encrypted at rest
// (package doc, "Sealed, bounded, and gone with the conversation"). The seal's
// associated data names the seat, the turn and the object's key ([AAD]), so an
// object copied under another turn's row, or a row edited to name another's,
// does not open; hence the key is minted BEFORE the upload
// ([objstore.Store.Mint]) and the upload goes under exactly it
// ([objstore.Store.PutMinted]). A missing or unreadable object is
// [ErrUnreadable], read as no session, never a failed launch: a release, an
// absorption or a retention delete can unname it between a row's read and the
// open, and the collector deletes unnamed objects without a lock (ADR-0027).

// SessionObjectReferences is the table whose rows keep sessions' objects
// alive: the declaration internal/objstore/references collects.
//
// REQUIRED ([objstore.Required]): a session is bytes nothing builds again, so
// its objects are audited and every backup carries them while a row names
// them. Not Derived (no node can rebuild one), not Retired (a live row is the
// next turn's continuity, not a grace copy); a restorer's fresh start on a
// missing object tolerates a loss, never excuses not accounting for one. So it
// names the log a restore replays and how a record names the table's rows.
var SessionObjectReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "executor_sessions",
	Key: "object", Hash: "object_hash", Size: "object_size",
	Owner:    []string{"agent_handle", "turn_id"},
	Standing: objstore.Required,
	Log:      Domain{}.Stream().Name,
	Deferred: Domain{}.DeferredTable(),
	Records:  sessionRecordReferences,
}

// sessionRecordReferences is [SessionObjectReferences]' Records: the
// executor_sessions row one record writes and the object it names. Only a
// capture names one ([applyCapture]); a record taking objects off rows answers
// none, and a capture is answered whether or not the apply would admit it, so
// a backup carries a superset of what a restore names (the collector deletes
// the extra), never a subset. What the apply refuses on the record alone (a
// mismatched subject, [nameable]) answers none. A record this build cannot
// read is [objstore.ErrRecordUnreadable] where [mayWriteSessions] says it may
// write a row. The owner is spelled seat then turn ([objstore.JoinOwner]), as
// the table's walk spells it, rather than read off the declaration, which
// would be an initialisation cycle.
func sessionRecordReferences(payload []byte) ([]objstore.RecordReference, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return nil, fmt.Errorf("executordomain: a record no build reads: %w", err)
	}
	if env.V > RecordVersion || !env.Subject.Kind.Valid() || !env.Op.Valid() {
		if !mayWriteSessions(env) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: the record on %s is version %d (this build reads %d) "+
			"of a kind or op this build may not know, and the scope its writer stated "+
			"reaches a seat's sessions", objstore.ErrRecordUnreadable, env.Subject, env.V,
			RecordVersion)
	}
	if env.Subject.Kind != KindConversation || env.Op != OpCapture {
		return nil, nil
	}
	rec, err := Decode(payload)
	if err != nil {
		return nil, err
	}
	payloadValue, err := DecodeMutation(rec)
	if err != nil {
		return nil, err
	}
	p, ok := payloadValue.(Capture)
	if !ok {
		return nil, fmt.Errorf("executordomain: the capture on %s carries a %T",
			env.Subject, payloadValue)
	}
	if !present(p.Handle) || !present(p.Turn) || !nameable(p.Object, p.ObjectHash) ||
		rec.Subject != ConversationSubject(p.Handle, p.ConversationKey, p.Turn) {
		// THE APPLY WRITES NO ROW for a capture it cannot place or whose
		// object no reader can parse.
		return nil, nil
	}
	o := objstore.Object{Size: p.ObjectSize}
	if o.Key, err = objstore.ParseKey(p.Object); err != nil {
		return nil, fmt.Errorf("executordomain: the capture on %s: %w", env.Subject, err)
	}
	if o.Hash, err = objstore.ParseHash(p.ObjectHash); err != nil {
		return nil, fmt.Errorf("executordomain: the capture on %s: %w", env.Subject, err)
	}
	return []objstore.RecordReference{{
		NamedBy: objstore.JoinOwner(p.Handle, p.Turn), Object: o, Names: true,
	}}, nil
}

// SummaryObjectReferences is the table whose rows keep sessions' newest
// summaries alive (executorkeep's Summary): the declaration
// internal/objstore/references collects.
//
// REQUIRED, for [SessionObjectReferences]' reason: a summary is the harness's
// own account of a session, which nothing builds again once its box is gone,
// and the next turn's start where its session is not continued. Only an end
// names one ([applyEnd]); it goes with its turn's row.
var SummaryObjectReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "executor_session_summaries",
	Key: "object", Hash: "object_hash", Size: "object_size",
	Owner:    []string{"agent_handle", "turn_id"},
	Standing: objstore.Required,
	Log:      Domain{}.Stream().Name,
	Deferred: Domain{}.DeferredTable(),
	Records:  summaryRecordReferences,
}

// summaryRecordReferences is [SummaryObjectReferences]' Records: the
// executor_session_summaries row an end writes and the summary it names,
// answered whether or not the apply would admit it, as
// [sessionRecordReferences] answers a capture. A record this build cannot
// read is [objstore.ErrRecordUnreadable] where [mayWriteSessions] says it may
// write a row.
func summaryRecordReferences(payload []byte) ([]objstore.RecordReference, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return nil, fmt.Errorf("executordomain: a record no build reads: %w", err)
	}
	if env.V > RecordVersion || !env.Subject.Kind.Valid() || !env.Op.Valid() {
		if !mayWriteSessions(env) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: the record on %s is version %d (this build reads %d) "+
			"of a kind or op this build may not know, and the scope its writer stated "+
			"reaches a seat's sessions", objstore.ErrRecordUnreadable, env.Subject, env.V,
			RecordVersion)
	}
	if env.Subject.Kind != KindConversation || env.Op != OpEnd {
		return nil, nil
	}
	rec, err := Decode(payload)
	if err != nil {
		return nil, err
	}
	payloadValue, err := DecodeMutation(rec)
	if err != nil {
		return nil, err
	}
	p, ok := payloadValue.(End)
	if !ok {
		return nil, fmt.Errorf("executordomain: the end on %s carries a %T", env.Subject, payloadValue)
	}
	summary := p.Ending.Summary
	if summary == nil || !present(p.Handle) || !present(p.Turn) || !summary.Within() ||
		rec.Subject != ConversationSubject(p.Handle, p.ConversationKey, p.Turn) {
		return nil, nil
	}
	return []objstore.RecordReference{{
		NamedBy: objstore.JoinOwner(p.Handle, p.Turn), Object: summary.Object, Names: true,
	}}, nil
}

// mayWriteSessions reports whether a record this build cannot read may write
// an executor_sessions row, from what every build reads of it: its kind and
// the scope its WRITER stated. An unknown kind may, since this build cannot
// place what it names; a known kind may unless every path is the framework's
// own (only the barrier and the trimmed record), as a session row lives under
// its seat.
func mayWriteSessions(env RecordEnvelope) bool {
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

// AAD is the associated data a session's object is sealed under: the seat, the
// turn and the object's key, so the object opens only where its row names it.
func AAD(handle, turn string, key objstore.Key) []byte {
	return []byte("executor/" + handle + "/" + turn + "/" + key.String())
}

// SummaryAAD is the associated data a session's summary is sealed under: the
// seat, the turn and the object's key, apart from [AAD], so neither a session's
// object nor a summary's opens as the other.
func SummaryAAD(handle, turn string, key objstore.Key) []byte {
	return []byte("executor-summary/" + handle + "/" + turn + "/" + key.String())
}

// SessionStore streams sessions into the object store sealed, and back out.
type SessionStore struct {
	objects *objstore.Store
	sealer  *secrets.StreamSealer
}

// NewSessionStore builds the store over the fleet's object store and the
// keyring's stream sealer, refusing a missing one by name.
func NewSessionStore(objects *objstore.Store, sealer *secrets.StreamSealer) (*SessionStore, error) {
	switch {
	case objects == nil:
		return nil, errors.New("executordomain: a session store needs the fleet's " +
			"object store, where a session's sealed bytes are kept")
	case sealer == nil:
		return nil, errors.New("executordomain: a session store needs the keyring's " +
			"stream sealer: a session is never stored in the clear")
	}
	return &SessionStore{objects: objects, sealer: sealer}, nil
}

// Seal streams the archive archive yields sealed onto dst, under an object key
// it mints now and [AAD] names, and answers the spool [SessionStore.Upload]
// names it by. An archive past [executorkeep.SessionMaxBytes] is
// [executorkeep.ErrSessionTooLarge]; a failed read fails the seal with its own
// error.
//
// SEAL AND UPLOAD ARE APART so a capture can take a session out of a box about
// to be paused or reclaimed, holding at most a chunk in memory, and upload it
// after (internal/sandbox's capture); dst holds it sealed, never in the clear.
// The upload and its record follow within objstore.RecordWithin of the mint.
func (s *SessionStore) Seal(handle, turn string, archive io.Reader, dst io.Writer) (executorkeep.SealedSpool, error) {
	if err := checkTurn(handle, turn, "", ""); err != nil {
		return executorkeep.SealedSpool{}, err
	}
	return s.sealMinted(dst, handle, turn, archive, make(chan objstore.Key, 1))
}

// Upload puts what [SessionStore.Seal] sealed into the object it minted the
// spool for, reading sealed to its end and no further than [executorkeep.SealedMaxBytes],
// and answers what a capture names it by.
func (s *SessionStore) Upload(ctx context.Context, spool executorkeep.SealedSpool, sealed io.Reader) (executorkeep.SealedObject, error) {
	object, err := s.objects.PutMinted(ctx, spool.Key, sealed, executorkeep.SealedMaxBytes,
		objstore.PutMeta{ContentType: "application/octet-stream"})
	if err != nil {
		return executorkeep.SealedObject{}, fmt.Errorf("executordomain: upload the session sealed for %s: %w",
			spool.Key, err)
	}
	return executorkeep.SealedObject{Object: object, ContentKey: spool.ContentKey}, nil
}

// Put seals the archive archive yields into a new object as it streams, and
// answers what a capture names it by: [SessionStore.Seal] and
// [SessionStore.Upload] with no spool between. An archive past
// [executorkeep.SessionMaxBytes] is [executorkeep.ErrSessionTooLarge], leaving
// only what the store's failed put cleans up.
//
// PUT CLOSES THE ARCHIVE when ctx ends, when the upload fails and before it
// returns: only a close ends a Read blocked on a stalled box stream, so the
// archive's Close must be safe beside an in-flight Read (as a pipe's, a file's
// and a network stream's are). Its one goroutine seals into the pipe the
// upload reads, and Put waits for it whatever the upload answered.
func (s *SessionStore) Put(ctx context.Context, handle, turn string, archive io.ReadCloser) (executorkeep.SealedObject, error) {
	closeArchive := sync.OnceFunc(func() { _ = archive.Close() })
	defer closeArchive()
	if err := checkTurn(handle, turn, "", ""); err != nil {
		return executorkeep.SealedObject{}, err
	}
	// UNBLOCK A STALLED SOURCE the moment the caller gives up on it.
	stop := context.AfterFunc(ctx, closeArchive)
	defer stop()
	read, write := io.Pipe()
	var (
		wg      sync.WaitGroup
		spooled executorkeep.SealedSpool
		sealErr error
		minted  = make(chan objstore.Key, 1)
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		spooled, sealErr = s.sealMinted(write, handle, turn, archive, minted)
		_ = write.CloseWithError(sealErr)
	}()
	var (
		object objstore.Object
		putErr error
	)
	if key, ok := <-minted; ok {
		object, putErr = s.objects.PutMinted(ctx, key, read, executorkeep.SealedMaxBytes,
			objstore.PutMeta{ContentType: "application/octet-stream"})
	}
	// UNBLOCK THE SEALER: a failed put stopped reading, so a sealer writing
	// would wait on the pipe for ever, and one reading is ended by the close.
	_ = read.CloseWithError(putErr)
	if putErr != nil {
		closeArchive()
	}
	wg.Wait()
	switch {
	case errors.Is(sealErr, executorkeep.ErrSessionTooLarge):
		return executorkeep.SealedObject{}, sealErr
	case putErr != nil:
		return executorkeep.SealedObject{}, fmt.Errorf("executordomain: upload %s's session of turn "+
			"%s: %w", handle, turn, putErr)
	case sealErr != nil:
		return executorkeep.SealedObject{}, fmt.Errorf("executordomain: seal %s's session of turn "+
			"%s: %w", handle, turn, sealErr)
	}
	return executorkeep.SealedObject{Object: object, ContentKey: spooled.ContentKey}, nil
}

// sealMinted is [SessionStore.Seal] handing the key it minted to minted before
// a byte is sealed, for an upload that reads the seal as it is written.
func (s *SessionStore) sealMinted(dst io.Writer, handle, turn string, archive io.Reader,
	minted chan<- objstore.Key) (executorkeep.SealedSpool, error) {

	key := s.objects.Mint()
	minted <- key
	close(minted)
	counted := &countingWriter{w: dst}
	contentKey, err := s.seal(counted, archive, AAD(handle, turn, key))
	if err != nil {
		return executorkeep.SealedSpool{}, err
	}
	return executorkeep.SealedSpool{Key: key, ContentKey: contentKey, Size: counted.n}, nil
}

// seal streams at most [executorkeep.SessionMaxBytes] of archive sealed onto dst, and
// answers the key it sealed under.
func (s *SessionStore) seal(dst io.Writer, archive io.Reader, aad []byte) (string, error) {
	w, keyID, err := s.sealer.Seal(dst, aad)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(w, io.LimitReader(archive, executorkeep.SessionMaxBytes+1))
	if err != nil {
		return "", err
	}
	if n > executorkeep.SessionMaxBytes {
		return "", fmt.Errorf("%w: the archive is past %d bytes", executorkeep.ErrSessionTooLarge,
			executorkeep.SessionMaxBytes)
	}
	return keyID, w.Close()
}

// PutSummary seals a session's summary, the text text yields, into a new
// object under a key it mints now and [SummaryAAD] names, and uploads it,
// answering what an end names it by. A text past
// [executorkeep.SummaryMaxBytes] is refused before it is sealed.
func (s *SessionStore) PutSummary(ctx context.Context, handle, turn string, text io.Reader) (executorkeep.SealedObject, error) {
	if err := checkTurn(handle, turn, "", ""); err != nil {
		return executorkeep.SealedObject{}, err
	}
	plain, err := io.ReadAll(io.LimitReader(text, executorkeep.SummaryMaxBytes+1))
	switch {
	case err != nil:
		return executorkeep.SealedObject{}, fmt.Errorf("executordomain: read %s's summary of turn %s: %w",
			handle, turn, err)
	case len(plain) > executorkeep.SummaryMaxBytes:
		return executorkeep.SealedObject{}, invalid("the summary is past %d bytes", executorkeep.SummaryMaxBytes)
	}
	key := s.objects.Mint()
	var sealed bytes.Buffer
	w, keyID, err := s.sealer.Seal(&sealed, SummaryAAD(handle, turn, key))
	if err != nil {
		return executorkeep.SealedObject{}, err
	}
	if _, err = w.Write(plain); err == nil {
		err = w.Close()
	}
	if err != nil {
		return executorkeep.SealedObject{}, fmt.Errorf("executordomain: seal %s's summary of turn %s: %w",
			handle, turn, err)
	}
	object, err := s.objects.PutMinted(ctx, key, &sealed, executorkeep.SummarySealedMaxBytes,
		objstore.PutMeta{ContentType: "application/octet-stream"})
	if err != nil {
		return executorkeep.SealedObject{}, fmt.Errorf("executordomain: upload %s's summary of turn %s: %w",
			handle, turn, err)
	}
	return executorkeep.SealedObject{Object: object, ContentKey: keyID}, nil
}

// OpenSummary streams one session's summary back out of the store, as
// [SessionStore.Open] streams its session: checked against its row, every
// chunk authenticated against [SummaryAAD] before it is handed out. A row
// naming none, an object gone, and bytes that do not open are [ErrUnreadable].
func (s *SessionStore) OpenSummary(ctx context.Context, session Session) (io.ReadCloser, error) {
	summary := session.Summary
	if summary == nil {
		return nil, fmt.Errorf("%w: %s's turn %s names no summary", ErrUnreadable, session.Handle, session.Turn)
	}
	body, err := s.objects.Open(ctx, summary.Object)
	if err != nil {
		return nil, unreadable(session, err)
	}
	plain, err := s.sealer.Open(body, SummaryAAD(session.Handle, session.Turn, summary.Object.Key))
	if err != nil {
		_ = body.Close()
		return nil, unreadable(session, err)
	}
	return &openSession{session: session, plain: plain, body: body}, nil
}

// CopySummary copies the summary object holds, of handle's turn, into a new
// object under a fresh key ([SessionStore.PutSummary]): what a turn's end names
// where the one its run's record names was minted too long ago for the
// executor log to name. A summary gone or not opening is [ErrUnreadable].
func (s *SessionStore) CopySummary(ctx context.Context, handle, turn string, object objstore.Object) (executorkeep.SealedObject, error) {
	body, err := s.OpenSummary(ctx, Session{Handle: handle, Turn: turn, Summary: &executorkeep.Summary{Object: object}})
	if err != nil {
		return executorkeep.SealedObject{}, err
	}
	defer func() { _ = body.Close() }()
	return s.PutSummary(ctx, handle, turn, body)
}

// countingWriter counts what passes through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Open streams one session's archive back out of the store: the object
// checked against its row by the object store, and every chunk authenticated
// against [AAD] before a byte of it is handed out. The caller closes it.
//
// WHAT IT HANDS OUT IS PROVISIONAL UNTIL io.EOF: a cut stream is known cut
// only when the read past its last whole chunk fails, so a restorer unpacking
// as it reads stages what it unpacks and keeps none unless it reached EOF.
//
// A row naming no object, an object gone, and bytes not sealed for this turn
// or that no keyring key opens are [ErrUnreadable], at the call or from the
// Read that reaches the fault.
func (s *SessionStore) Open(ctx context.Context, session Session) (io.ReadCloser, error) {
	if !session.HasObject() {
		return nil, fmt.Errorf("%w: %s's turn %s names no session", ErrUnreadable,
			session.Handle, session.Turn)
	}
	body, err := s.objects.Open(ctx, session.Object)
	if err != nil {
		return nil, unreadable(session, err)
	}
	plain, err := s.sealer.Open(body, AAD(session.Handle, session.Turn, session.Object.Key))
	if err != nil {
		_ = body.Close()
		return nil, unreadable(session, err)
	}
	return &openSession{session: session, plain: plain, body: body}, nil
}

// unreadable is a failure to read a session's object, in this package's words
// where it is one a restorer reads as no session: the object gone, its bytes
// not the ones stored, or a seal this keyring cannot open. Any other error (a
// store that did not answer) is returned as it came, for the caller to retry.
func unreadable(session Session, err error) error {
	if errors.Is(err, objstore.ErrNotFound) || errors.Is(err, objstore.ErrCorrupt) ||
		errors.Is(err, secrets.ErrDecrypt) {
		return fmt.Errorf("%w: %s's turn %s: %w", ErrUnreadable, session.Handle,
			session.Turn, err)
	}
	return err
}

// openSession is [SessionStore.Open]'s reader.
type openSession struct {
	session Session
	plain   io.Reader
	body    io.ReadCloser
}

func (o *openSession) Read(p []byte) (int, error) {
	n, err := o.plain.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, unreadable(o.session, err)
	}
	return n, err
}

func (o *openSession) Close() error { return o.body.Close() }
