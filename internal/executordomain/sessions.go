package executordomain

import (
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

// THE SESSION'S BYTES: one sealed object per capture in the fleet's object
// store, streamed in and out a chunk at a time — two chunk buffers a side
// (secrets.StreamChunkBytes), and below them whatever the object store's
// backend buffers per upload.
//
// # Sealed, and bound to where it belongs
//
// Neither the estate nor the object store is encrypted at rest, and a session
// is a harness's whole transcript: every file it read, every command's
// output. So the archive is sealed under the fleet keyring before a byte of it
// reaches the store ([secrets.StreamSealer]), with associated data naming the
// seat, the turn and the object's own key ([AAD]): an object copied under
// another turn's row, or a row edited to name another turn's object, does not
// open. The key is minted BEFORE the upload ([objstore.Store.Mint]) so the
// associated data can name it, and the upload goes under exactly that key
// ([objstore.Store.PutMinted]).
//
// # Missing or unreadable is "no session"
//
// Between a launch reading a row and opening its object, a retention delete,
// an absorption or a release can leave the object unnamed, and the collector
// deletes unnamed objects without a lock (ADR-0027); a keyring key dropped
// before every session sealed under it left leaves objects nobody opens. A
// restorer reads every such object as [ErrUnreadable] and starts the turn
// fresh, never fails the launch.

// SessionObjectReferences is the table whose rows keep sessions' objects
// alive: the declaration internal/objstore/references collects.
//
// REQUIRED ([objstore.Required]): a session is the harness's own transcript,
// bytes nothing builds again once the run that wrote it is gone, so its objects
// are audited and every backup carries them until a release, a retention
// delete or the seat's removal takes the row's naming away. Not Derived, since
// no node can rebuild one, and not Retired, since a live row is the next
// turn's continuity rather than a grace copy. A restorer reading a missing
// object starts the turn fresh, but that is the launch's tolerance of a loss,
// never a reason to stop accounting for one. The declaration therefore names
// the log a restore replays and how one record of it names the table's rows.
var SessionObjectReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "executor_sessions",
	Key: "object", Hash: "object_hash", Size: "object_size",
	Owner:    []string{"agent_handle", "turn_id"},
	Standing: objstore.Required,
	Log:      Domain{}.Stream().Name,
	Deferred: Domain{}.DeferredTable(),
	Records:  sessionRecordReferences,
}

// sessionRecordReferences answers the executor_sessions row one record of the
// executor log writes, and the object it names once the record applies:
// [objstore.ReferenceTable.Records] for [SessionObjectReferences].
//
// A RECORD THIS BUILD READS is answered by the applier's own decoders
// ([Decode], [DecodeMutation]): only a capture ([OpCapture] on a
// conversation) names an object, through [applyCapture], so every other
// record answers none. An end stamps a row and names nothing new; a release,
// an absorption, a retention delete and a seat's removal take objects OFF
// rows, which a record answers as none: a backup that carries an object such a
// record later unnamed carries one object more than a restore names, which the
// collector deletes, never one fewer. For the same reason a capture is
// answered whether or not this node's applier would admit it (the seat's
// budget, a tombstone, a newer capture already applied are decided again at
// the apply): a backup reads a superset of what a restore's rows name. What
// the apply refuses on the record alone, it is refused here too: a capture
// on another conversation's subject, or naming no seat, no turn or an object
// no reader can parse ([nameable]), writes no row.
//
// A RECORD THIS BUILD CANNOT READ is answered from what EVERY build reads of
// it ([mayWriteSessions]): one that may write a session row is
// [objstore.ErrRecordUnreadable].
//
// THE ROW IS SPELLED AS [SessionObjectReferences] LISTS ITS OWNER, the seat
// and then the turn, through [objstore.JoinOwner], as the walk of the table
// spells it; the declaration itself is not read here, which would be an
// initialisation cycle.
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

// mayWriteSessions reports whether a record this build cannot read may write
// an executor_sessions row, from what every build reads of it: its kind and the
// scope its WRITER stated as the complete set of objects its apply may write.
//
// AN UNKNOWN KIND MAY: this build cannot place what a kind it has never heard
// of names. A KNOWN KIND may when any path of its scope reaches a seat, a
// conversation, the domain or a path this build cannot interpret, and may not
// when every path is the framework's own: a session row lives under its seat,
// so only the barrier and the trimmed record name none.
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
// this store mints now and the seal's associated data names ([AAD]), and
// answers the spool an upload names it by ([SessionStore.Upload]). An archive
// past [executorkeep.SessionMaxBytes] is [executorkeep.ErrSessionTooLarge]; one that fails as it is read
// fails the seal with its own error.
//
// THE TWO HALVES OF A KEEP ARE APART so a capture can take its session out of a
// box that is about to be paused or reclaimed, holding no more than a chunk of
// it in memory, and upload it after (internal/sandbox's capture): what dst holds
// in between is sealed, so a session never rests in the clear on a node's disk
// either. The key's minting instant is what objstore.RecordWithin is counted
// from, so the upload and its record follow within it.
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

// Put seals the archive archive yields, as it streams, into a new object, and
// answers what a capture names it by: [SessionStore.Seal] and
// [SessionStore.Upload] at once, the seal streamed straight into the upload
// with no spool between. An archive past [executorkeep.SessionMaxBytes] is
// [executorkeep.ErrSessionTooLarge] and leaves nothing behind but what the store's own
// failed put cleans up.
//
// PUT CLOSES THE ARCHIVE, and the close is what bounds it: when ctx ends, when
// the upload fails, and before it returns. A box's stream that stalls blocks a
// read for as long as it stalls, which no check of ctx between reads can
// interrupt; closing the stream is what ends a read blocked on it, so the
// archive's Close must be safe to call while a Read is in flight, as it is for
// a pipe, a file and a network stream.
//
// ONE GOROUTINE, owned here: it seals the archive into the pipe the upload
// reads, and Put waits for it to end before it returns, whatever the upload
// answered, so nothing outlives the call.
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
	// UNBLOCK THE SEALER whatever the upload answered: a put that failed
	// stopped reading, and a sealer still writing would wait on the pipe
	// for ever; and one still reading the archive is ended by its close.
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

// Open streams one session's archive back out of the store, opened as it
// streams: the object checked byte for byte against its row by the object
// store, and every chunk authenticated against the session's own associated
// data before a byte of it is handed out. The caller closes what it answers.
//
// WHAT IT HANDS OUT IS PROVISIONAL UNTIL io.EOF. Every byte is from a chunk
// that authenticated, but a stream cut short is known to be cut only when the
// read past its last whole chunk fails, so a restorer that unpacks as it reads
// stages what it unpacks and keeps none of it unless the reader ended at EOF.
//
// A row that names no object, an object the store no longer holds, and bytes
// that are not the ones sealed for this turn or that no key of this keyring
// opens are all [ErrUnreadable], at the call or from the Read that reaches the
// fault.
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

// Fetch copies the sealed object a row names into dst as the store holds it,
// every byte checked against the row by the object store before it is handed
// over, and answers once the whole object is in dst: the first half of a
// restore that spools a session on its node's disk SEALED before any of it is
// opened, as a capture spools one before its upload ([SessionStore.Seal]).
//
// TWO HALVES, as a keep's are, so a restore learns that a session cannot be
// read BEFORE it writes a byte of it into a box: what [SessionStore.Open]
// streams is provisional until its end, and a restore streaming it into a box
// would find a cut or forged object part way through the write, with the run's
// brief already chosen for a session that never arrives. Spooled sealed, the
// session rests on no node's disk in the clear.
//
// Its errors are [SessionStore.Open]'s: a row naming no object, and an object
// gone or not the bytes the row names, are [ErrUnreadable]; a store that did
// not answer is its own error, for the caller to try again.
func (s *SessionStore) Fetch(ctx context.Context, session Session, dst io.Writer) error {
	if !session.HasObject() {
		return fmt.Errorf("%w: %s's turn %s names no session", ErrUnreadable,
			session.Handle, session.Turn)
	}
	body, err := s.objects.Open(ctx, session.Object)
	if err != nil {
		return unreadable(session, err)
	}
	defer func() { _ = body.Close() }()
	if _, err = io.Copy(dst, body); err != nil {
		return unreadable(session, err)
	}
	return nil
}

// OpenSpooled opens a sealed copy [SessionStore.Fetch] spooled, against the
// associated data of the row it was fetched for ([AAD]) — so a spool of another
// seat's or another turn's session, or of an object its row does not name,
// does not open — and streams the archive with every chunk authenticated
// before a byte of it is handed out. A seal that does not open is
// [ErrUnreadable], at the call or from the Read that reaches it; the caller
// owns sealed and closes it.
func (s *SessionStore) OpenSpooled(session Session, sealed io.Reader) (io.Reader, error) {
	plain, err := s.sealer.Open(sealed, AAD(session.Handle, session.Turn, session.Object.Key))
	if err != nil {
		return nil, unreadable(session, err)
	}
	return &openSession{session: session, plain: plain, body: io.NopCloser(sealed)}, nil
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
