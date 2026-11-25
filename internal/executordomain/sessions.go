package executordomain

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
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

// SessionMediaReferences is the table whose rows keep sessions' media objects
// alive, REQUIRED for [SessionObjectReferences]' reason: a media member is
// bytes of the session nothing builds again, which its restore splices in.
var SessionMediaReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "executor_session_media",
	Key: "object", Hash: "object_hash", Size: "object_size",
	Owner:    []string{"agent_handle", "turn_id", "digest"},
	Standing: objstore.Required,
	Log:      Domain{}.Stream().Name,
	Deferred: Domain{}.DeferredTable(),
	Records:  sessionMediaRecordReferences,
}

// sessionRecordReferences is [SessionObjectReferences]' Records: the
// executor_sessions row one record writes and the object it names. Only a
// capture names one ([applyCapture]); a record taking objects off rows answers
// none, and a capture is answered whether or not the apply would admit it, so
// a backup carries a superset of what a restore names (the collector deletes
// the extra), never a subset. The owner is spelled seat then turn
// ([objstore.JoinOwner]), as the table's walk spells it, rather than read off
// the declaration, which would be an initialisation cycle.
func sessionRecordReferences(payload []byte) ([]objstore.RecordReference, error) {
	p, placed, err := placedCapture(payload)
	if err != nil || !placed {
		return nil, err
	}
	o, err := parsedObject(p.Object, p.ObjectHash, p.ObjectSize)
	if err != nil {
		return nil, fmt.Errorf("executordomain: the capture of %s's turn %s: %w", p.Handle, p.Turn, err)
	}
	return []objstore.RecordReference{{
		NamedBy: objstore.JoinOwner(p.Handle, p.Turn), Object: o, Names: true,
	}}, nil
}

// sessionMediaRecordReferences is [SessionMediaReferences]' Records: the
// executor_session_media rows one capture writes, its whole media set, carried
// ones included (a superset, as [sessionRecordReferences] answers), owned by
// seat, turn and digest.
func sessionMediaRecordReferences(payload []byte) ([]objstore.RecordReference, error) {
	p, placed, err := placedCapture(payload)
	if err != nil || !placed {
		return nil, err
	}
	var out []objstore.RecordReference
	for _, m := range p.Media {
		o, parseErr := parsedObject(m.Object, m.ObjectHash, m.ObjectSize)
		if parseErr != nil {
			return nil, fmt.Errorf("executordomain: the capture of %s's turn %s, media %s: %w", p.Handle,
				p.Turn, m.Digest, parseErr)
		}
		out = append(out, objstore.RecordReference{
			NamedBy: objstore.JoinOwner(p.Handle, p.Turn, m.Digest), Object: o, Names: true,
		})
	}
	return out, nil
}

// placedCapture is the capture one record of the log carries, and whether the
// apply places it: false for any other record, and for a capture the apply
// refuses on the record alone (a mismatched subject, an object or media set no
// reader parses: [nameable], [mediaWellFormed]). A record this build cannot
// read is [objstore.ErrRecordUnreadable] where [mayWriteSessions] says it may
// write a row.
func placedCapture(payload []byte) (Capture, bool, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return Capture{}, false, fmt.Errorf("executordomain: a record no build reads: %w", err)
	}
	if env.V > RecordVersion || !env.Subject.Kind.Valid() || !env.Op.Valid() {
		if !mayWriteSessions(env) {
			return Capture{}, false, nil
		}
		return Capture{}, false, fmt.Errorf("%w: the record on %s is version %d (this build reads %d) "+
			"of a kind or op this build may not know, and the scope its writer stated "+
			"reaches a seat's sessions", objstore.ErrRecordUnreadable, env.Subject, env.V,
			RecordVersion)
	}
	if env.Subject.Kind != KindConversation || env.Op != OpCapture {
		return Capture{}, false, nil
	}
	rec, err := Decode(payload)
	if err != nil {
		return Capture{}, false, err
	}
	payloadValue, err := DecodeMutation(rec)
	if err != nil {
		return Capture{}, false, err
	}
	p, ok := payloadValue.(Capture)
	if !ok {
		return Capture{}, false, fmt.Errorf("executordomain: the capture on %s carries a %T",
			env.Subject, payloadValue)
	}
	placed := present(p.Handle) && present(p.Turn) && nameable(p.Object, p.ObjectHash) &&
		mediaWellFormed(p) && rec.Subject == ConversationSubject(p.Handle, p.ConversationKey, p.Turn)
	return p, placed, nil
}

// parsedObject is an object a record names, as the store reads one.
func parsedObject(key, digest string, size int64) (objstore.Object, error) {
	var (
		o   = objstore.Object{Size: size}
		err error
	)
	if o.Key, err = objstore.ParseKey(key); err != nil {
		return objstore.Object{}, err
	}
	o.Hash, err = objstore.ParseHash(digest)
	return o, err
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

// MediaAAD is the associated data a session's media object is sealed under:
// the seat and the object's key, NOT the turn, so a later turn of the seat
// that carries it opens it, and no other seat's row does.
func MediaAAD(handle string, key objstore.Key) []byte {
	return []byte("executor-media/" + handle + "/" + key.String())
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
	contentKey, err := s.seal(counted, archive, AAD(handle, turn, key), executorkeep.SessionMaxBytes)
	if err != nil {
		return executorkeep.SealedSpool{}, err
	}
	return executorkeep.SealedSpool{Key: key, ContentKey: contentKey, Size: counted.n}, nil
}

// SealMedia streams one media member of a session sealed onto dst, under an
// object key it mints now and [MediaAAD] names, and answers the spool
// [SessionStore.UploadMedia] names it by, with the member's digest. A member
// past [executorkeep.SessionKeptMaxBytes] is [executorkeep.ErrSessionTooLarge].
// Apart from its upload, as [SessionStore.Seal] is.
func (s *SessionStore) SealMedia(handle string, member io.Reader, dst io.Writer) (executorkeep.MediaSpool, error) {
	if err := checkSeat(handle, ""); err != nil {
		return executorkeep.MediaSpool{}, err
	}
	key := s.objects.Mint()
	digest := sha256.New()
	counted := &countingWriter{w: dst}
	contentKey, err := s.seal(counted, io.TeeReader(member, digest), MediaAAD(handle, key),
		executorkeep.SessionKeptMaxBytes)
	if err != nil {
		return executorkeep.MediaSpool{}, err
	}
	return executorkeep.MediaSpool{
		SealedSpool: executorkeep.SealedSpool{Key: key, ContentKey: contentKey, Size: counted.n},
		Digest:      hex.EncodeToString(digest.Sum(nil)),
	}, nil
}

// UploadMedia puts what [SessionStore.SealMedia] sealed into the object it
// minted the spool for, reading no further than
// [executorkeep.KeptSealedMaxBytes], and answers what a capture names it by.
func (s *SessionStore) UploadMedia(ctx context.Context, spool executorkeep.MediaSpool,
	sealed io.Reader) (executorkeep.MediaObject, error) {

	object, err := s.objects.PutMinted(ctx, spool.Key, sealed, executorkeep.KeptSealedMaxBytes,
		objstore.PutMeta{ContentType: "application/octet-stream"})
	if err != nil {
		return executorkeep.MediaObject{}, fmt.Errorf("executordomain: upload the media %s sealed for %s: %w",
			spool.Digest, spool.Key, err)
	}
	return executorkeep.MediaObject{Digest: spool.Digest, Object: object, ContentKey: spool.ContentKey}, nil
}

// seal streams at most limit bytes of src sealed onto dst, and answers the key
// it sealed under.
func (s *SessionStore) seal(dst io.Writer, src io.Reader, aad []byte, limit int64) (string, error) {
	w, keyID, err := s.sealer.Seal(dst, aad)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(w, io.LimitReader(src, limit+1))
	if err != nil {
		return "", err
	}
	if n > limit {
		return "", fmt.Errorf("%w: the archive is past %d bytes", executorkeep.ErrSessionTooLarge, limit)
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
	return s.openSealed(ctx, summary.Object, SummaryAAD(session.Handle, session.Turn, summary.Object.Key),
		func(err error) error { return unreadable(session, err) })
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
// against [AAD] before a byte of it is handed out. The caller closes it. A row
// naming media names a text object of frames ([executorkeep.FrameText]), each
// media member spliced back where its frame stands, verbatim.
//
// WHAT IT HANDS OUT IS PROVISIONAL UNTIL io.EOF: a cut stream is known cut
// only when the read past its last whole chunk fails, so a restorer unpacking
// as it reads stages what it unpacks and keeps none unless it reached EOF.
//
// A row naming no object, an object or media gone, and bytes not sealed for
// this row, not its digest, or that no keyring key opens are [ErrUnreadable].
func (s *SessionStore) Open(ctx context.Context, session Session) (io.ReadCloser, error) {
	if !session.HasObject() {
		return nil, fmt.Errorf("%w: %s's turn %s names no session", ErrUnreadable,
			session.Handle, session.Turn)
	}
	fail := func(err error) error { return unreadable(session, err) }
	text, err := s.openSealed(ctx, session.Object, AAD(session.Handle, session.Turn, session.Object.Key), fail)
	if err != nil || len(session.Media) == 0 {
		return text, err
	}
	named := make(map[[executorkeep.MediaDigestBytes]byte]executorkeep.MediaObject, len(session.Media))
	for _, m := range session.Media {
		var digest [executorkeep.MediaDigestBytes]byte
		if !executorkeep.ValidMediaDigest(m.Digest) {
			_ = text.Close()
			return nil, fmt.Errorf("%w: %s's turn %s names media %q, which is no digest", ErrUnreadable,
				session.Handle, session.Turn, m.Digest)
		}
		_, _ = hex.Decode(digest[:], []byte(m.Digest))
		named[digest] = m
	}
	open := func(m executorkeep.MediaObject) (io.ReadCloser, error) {
		return s.openSealed(ctx, m.Object, MediaAAD(session.Handle, m.Object.Key), func(err error) error {
			return unreadable(session, fmt.Errorf("media %s: %w", m.Digest, err))
		})
	}
	return &splicedSession{session: session, text: text, frames: bufio.NewReader(text), named: named,
		open: open}, nil
}

// openSealed opens one sealed object of a session under aad, each failure
// read through fail.
func (s *SessionStore) openSealed(ctx context.Context, object objstore.Object, aad []byte,
	fail func(error) error) (io.ReadCloser, error) {

	body, err := s.objects.Open(ctx, object)
	if err != nil {
		return nil, fail(err)
	}
	plain, err := s.sealer.Open(body, aad)
	if err != nil {
		_ = body.Close()
		return nil, fail(err)
	}
	return &openSession{fail: fail, plain: plain, body: body}, nil
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

// openSession is one sealed object of a session, opened.
type openSession struct {
	fail  func(error) error
	plain io.Reader
	body  io.ReadCloser
}

func (o *openSession) Read(p []byte) (int, error) {
	n, err := o.plain.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, o.fail(err)
	}
	return n, err
}

func (o *openSession) Close() error { return o.body.Close() }

// splicedSession is [SessionStore.Open]'s reader of a session whose row names
// media: its text object's frames, each media frame read as its member,
// opened when the frame is reached and held to its digest at its end.
type splicedSession struct {
	session Session
	text    io.Closer
	frames  *bufio.Reader
	named   map[[executorkeep.MediaDigestBytes]byte]executorkeep.MediaObject
	open    func(executorkeep.MediaObject) (io.ReadCloser, error)

	textLeft uint64
	member   io.ReadCloser
	sum      hash.Hash
	want     [executorkeep.MediaDigestBytes]byte
}

func (s *splicedSession) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		switch {
		case s.textLeft > 0:
			n, err := s.frames.Read(p[:min(uint64(len(p)), s.textLeft)])
			s.textLeft -= uint64(n)
			if errors.Is(err, io.EOF) {
				err = s.malformed("ends inside a text frame")
			}
			return n, err
		case s.member != nil:
			n, err := s.member.Read(p)
			_, _ = s.sum.Write(p[:n])
			if !errors.Is(err, io.EOF) {
				return n, err
			}
			if err = s.endMember(); err != nil || n > 0 {
				return n, err
			}
			continue
		}
		if err := s.next(); err != nil {
			return 0, err
		}
	}
}

// next reads the next frame's head: io.EOF where the frames end whole.
func (s *splicedSession) next() error {
	kind, err := s.frames.ReadByte()
	if err != nil {
		return err
	}
	switch kind {
	case executorkeep.FrameText:
		s.textLeft, err = binary.ReadUvarint(s.frames)
		return s.cut(err, "ends inside a text frame's length")
	case executorkeep.FrameMedia:
		var digest [executorkeep.MediaDigestBytes]byte
		if _, err = io.ReadFull(s.frames, digest[:]); err != nil {
			return s.cut(err, "ends inside a media frame")
		}
		m, ok := s.named[digest]
		if !ok {
			return fmt.Errorf("%w: %s's turn %s frames media %x, which its row does not name", ErrUnreadable,
				s.session.Handle, s.session.Turn, digest)
		}
		if s.member, err = s.open(m); err != nil {
			return err
		}
		s.sum, s.want = sha256.New(), digest
		return nil
	}
	return s.malformed(fmt.Sprintf("holds a frame of kind %q no build writes", kind))
}

// endMember closes the member read to its end, holding it to its frame's digest.
func (s *splicedSession) endMember() error {
	err := s.member.Close()
	s.member = nil
	var got [executorkeep.MediaDigestBytes]byte
	if copy(got[:], s.sum.Sum(nil)); got != s.want {
		return fmt.Errorf("%w: %s's turn %s: media %x holds bytes of another digest", ErrUnreadable,
			s.session.Handle, s.session.Turn, s.want)
	}
	return err
}

// cut is err, a read of a frame's head, as the frames' fault where they ended
// inside it.
func (s *splicedSession) cut(err error, why string) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return s.malformed(why)
	}
	return err
}

// malformed is a text object whose frames do not parse.
func (s *splicedSession) malformed(why string) error {
	return fmt.Errorf("%w: %s's turn %s: its session's frames %s", ErrUnreadable, s.session.Handle,
		s.session.Turn, why)
}

func (s *splicedSession) Close() error {
	var memberErr error
	if s.member != nil {
		memberErr = s.member.Close()
	}
	return errors.Join(memberErr, s.text.Close())
}
