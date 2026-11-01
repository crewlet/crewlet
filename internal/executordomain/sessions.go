package executordomain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/secrets"
)

// THE SESSION'S BYTES: one sealed object per capture in the fleet's object
// store, streamed in and out a chunk at a time.
//
// # Sealed, and bound to where it belongs
//
// Neither the estate nor the object store is encrypted at rest, and a session
// is a coding CLI's whole transcript: every file it read, every command's
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
var SessionObjectReferences = objstore.ReferenceTable{
	Domain: Domain{}.Name(), Table: "executor_sessions",
	Key: "object", Hash: "object_hash", Size: "object_size",
	Owner: []string{"agent_handle", "turn_id"},
}

// AAD is the associated data a session's object is sealed under: the seat, the
// turn and the object's key, so the object opens only where its row names it.
func AAD(handle, turn string, key objstore.Key) []byte {
	return []byte("executor/" + handle + "/" + turn + "/" + key.String())
}

// SealedObject is an uploaded session: the object a capture names and the
// keyring key it was sealed under, which the capture records as its content
// key.
type SealedObject struct {
	Object     objstore.Object
	ContentKey string
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

// Put seals the archive archive yields, as it streams, into a new object, and
// answers what a capture names it by. An archive past [SessionMaxBytes] is
// [ErrSessionTooLarge] and leaves nothing behind but what the store's own
// failed put cleans up.
//
// ONE GOROUTINE, owned here: it seals the archive into the pipe the upload
// reads, and Put waits for it to end before it returns, whatever the upload
// answered, so nothing outlives the call.
func (s *SessionStore) Put(ctx context.Context, handle, turn string, archive io.Reader) (SealedObject, error) {
	if err := checkTurn(handle, turn, "", ""); err != nil {
		return SealedObject{}, err
	}
	key := s.objects.Mint()
	read, write := io.Pipe()
	var (
		wg         sync.WaitGroup
		contentKey string
		sealErr    error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		contentKey, sealErr = s.seal(write, archive, AAD(handle, turn, key))
		_ = write.CloseWithError(sealErr)
	}()
	object, putErr := s.objects.PutMinted(ctx, key, read, SealedMaxBytes,
		objstore.PutMeta{ContentType: "application/octet-stream"})
	// UNBLOCK THE SEALER whatever the upload answered: a put that failed
	// stopped reading, and a sealer still writing would wait on the pipe
	// for ever.
	_ = read.CloseWithError(putErr)
	wg.Wait()
	switch {
	case errors.Is(sealErr, ErrSessionTooLarge):
		return SealedObject{}, sealErr
	case putErr != nil:
		return SealedObject{}, fmt.Errorf("executordomain: upload %s's session of turn "+
			"%s: %w", handle, turn, putErr)
	case sealErr != nil:
		return SealedObject{}, fmt.Errorf("executordomain: seal %s's session of turn "+
			"%s: %w", handle, turn, sealErr)
	}
	return SealedObject{Object: object, ContentKey: contentKey}, nil
}

// seal streams at most [SessionMaxBytes] of archive sealed onto dst, and
// answers the key it sealed under.
func (s *SessionStore) seal(dst io.Writer, archive io.Reader, aad []byte) (string, error) {
	w, keyID, err := s.sealer.Seal(dst, aad)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(w, io.LimitReader(archive, SessionMaxBytes+1))
	if err != nil {
		return "", err
	}
	if n > SessionMaxBytes {
		return "", fmt.Errorf("%w: the archive is past %d bytes", ErrSessionTooLarge,
			SessionMaxBytes)
	}
	return keyID, w.Close()
}

// Open streams one session's archive back out of the store, opened as it
// streams: the object checked byte for byte against its row by the object
// store, and every chunk authenticated against the session's own associated
// data before a byte of it is handed out. The caller closes what it answers.
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
