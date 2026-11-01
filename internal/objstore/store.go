package objstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"
	"time"
)

// ErrTooLarge is an object past the limit its caller set.
var ErrTooLarge = errors.New("objstore: the object is over its size limit")

// Store is the object store as every caller uses it: a [Backend] whose every
// put is counted and hashed on the way in and minted a key of its own, and
// whose every read is checked against the row that names the object on the
// way out.
type Store struct {
	backend Backend
	now     func() time.Time

	// stall is [Stall], a field so a test can watch a stall end a read
	// without waiting a minute for it.
	stall time.Duration
}

// NewStore builds the store over backend, minting keys on the wall clock.
func NewStore(backend Backend) (*Store, error) { return NewStoreAt(backend, time.Now) }

// NewStoreAt builds the store over backend, minting every key at the instant
// now answers — so a test can mint keys at the instant its own clock says,
// which is what a write and the collector both judge a key's age by.
func NewStoreAt(backend Backend, now func() time.Time) (*Store, error) {
	if backend == nil || now == nil {
		return nil, errors.New("objstore: a store needs a backend and a clock")
	}
	return &Store{backend: backend, now: now, stall: Stall}, nil
}

// Backend is the store's backend, for the collector and the backup.
func (s *Store) Backend() Backend { return s.backend }

// Put streams r into a new object, under a key minted for it, and answers what
// a row names it by: the key, and the digest and size of what r yielded.
//
// At most limit bytes are read; one more fails the put with [ErrTooLarge],
// so an upload over its limit is refused having read one byte past it rather
// than the whole of it.
//
// EVERY FAILURE REACHES THE BACKEND AS A FAILED READ — the limit, and ctx
// ending, as well as r's own — because a failed read is the one failure every
// backend cleans up after with a context that still works, leaving nothing
// anybody can find ([Backend.Put]). And the failure the read recorded is the
// one Put answers, whatever the backend wrapped it in, so a caller's
// errors.Is sees [ErrTooLarge] or its own reader's error on every backend.
//
// THE DIGEST AND THE SIZE ARE THIS STORE'S OWN, counted over the bytes it
// handed the backend: a backend's account of what it stored is never what a
// row records.
func (s *Store) Put(ctx context.Context, r io.Reader, limit int64, m PutMeta) (Object, error) {
	return s.put(ctx, KeyAt(s.now()), r, limit, m)
}

// PutMinted streams r into the object k names, a key this store minted
// ([Store.Mint]) for one upload a caller reserved before any byte moved, and
// answers what a row names it by, exactly as [Store.Put] does.
//
// # Why a key minted before the bytes arrive
//
// An upload that is reserved first (an artifact's upload intent) names its key
// when it is reserved, so the same key is the one a presigned put signs and the
// one the engine streams into: the reservation then knows, before any byte
// moves, every object it may have to answer for. The key is still minted here,
// on this store's clock, never by a client.
//
// A NAME IS PUT ONCE: the caller holds a claim that no other put of k runs, or
// has run to its end, before it calls this ([Backend]'s rule). A put that
// fails leaves nothing under the name, so a failed put may be followed by
// another under the same claim. And a key too old to be named is refused
// before a byte is read: bytes stored under it could never be recorded, and
// would wait for the collector with nothing reserving them.
func (s *Store) PutMinted(ctx context.Context, k Key, r io.Reader, limit int64, m PutMeta) (Object, error) {
	if k.IsZero() {
		return Object{}, errors.New("objstore: a put under a minted key names no key")
	}
	if age := s.now().Sub(k.Minted()); age > RecordWithin {
		return Object{}, fmt.Errorf("objstore: %s was minted %s ago, and no write may name "+
			"a key minted more than %s before it: mint a fresh key", k,
			age.Truncate(time.Second), RecordWithin)
	}
	return s.put(ctx, k, r, limit, m)
}

// put is the one put both [Store.Put] and [Store.PutMinted] make.
func (s *Store) put(ctx context.Context, key Key, r io.Reader, limit int64, m PutMeta) (Object, error) {
	if limit < 0 {
		return Object{}, fmt.Errorf("objstore: a put limited to %d bytes", limit)
	}
	in := &metered{ctx: ctx, r: r, limit: limit, digest: sha256.New()}
	err := s.backend.Put(ctx, key.Name(), in, m)
	switch {
	case in.err != nil:
		return Object{}, in.err
	case err != nil && ctx.Err() != nil:
		// THE CALLER'S OWN CONTEXT ENDED while the backend held the put,
		// which says so whatever the backend's library called it.
		return Object{}, fmt.Errorf("objstore: store the object: %w: %w", ctx.Err(), err)
	case err != nil && !errors.Is(err, ErrFull):
		return Object{}, fmt.Errorf("objstore: store the object: %w: %w", ErrUnanswered, err)
	case err != nil:
		return Object{}, fmt.Errorf("objstore: store the object: %w", err)
	case !in.ended:
		// A BACKEND THAT STOPPED READING BEFORE THE END stored part of
		// the content, and the row would record the part as the whole.
		return Object{}, fmt.Errorf("objstore: the backend stored %s after %d bytes, "+
			"before the content ended", key, in.n)
	}
	return Object{Key: key, Hash: Hash(hex.EncodeToString(in.digest.Sum(nil))), Size: in.n}, nil
}

// metered is a put's reader: counted, hashed, held to its limit and to its
// context, every failure recorded so [Store.Put] answers it.
type metered struct {
	ctx    context.Context
	r      io.Reader
	limit  int64
	digest hash.Hash

	n     int64
	ended bool
	err   error
}

func (m *metered) Read(p []byte) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	if m.ended {
		return 0, io.EOF
	}
	if err := m.ctx.Err(); err != nil {
		m.err = err
		return 0, err
	}
	n, err := m.r.Read(p)
	if m.n+int64(n) > m.limit {
		m.err = fmt.Errorf("%w (%d bytes)", ErrTooLarge, m.limit)
		return 0, m.err
	}
	m.n += int64(n)
	_, _ = m.digest.Write(p[:n])
	switch {
	case err == io.EOF:
		// THE END IS io.EOF ITSELF, for [Fill]'s reason: a wrapped one,
		// or a cut body's io.ErrUnexpectedEOF, is a failure.
		m.ended = true
		return n, io.EOF
	case err != nil:
		m.err = fmt.Errorf("objstore: read the object: %w", err)
		return n, m.err
	}
	return n, nil
}

// Open streams the object o names back, checked against it: every byte is
// counted and hashed, and the stream ends in [ErrCorrupt] — never io.EOF —
// where the backend's bytes are not o's.
//
// # The byte that completes the file is held back
//
// Until the backend has said the object ENDED and the bytes before it hash to
// o's digest, the reader hands out every byte but the last. Verifying at the
// end and handing everything out as it came would put the whole of a damaged
// file into a caller's hands — a response with a complete Content-Length, a
// backup file of the right size — before the damage was known; held back, a
// damaged object always reads short of its size, which is what a client and
// a backup can each see for themselves.
//
// # Every read is bounded twice
//
// The whole read runs under [ReadBudget] — or ctx's own deadline, where that
// is sooner — and every wait on the backend under [Stall]: the backend's
// open of the stream ([Store.begin]) and each Read of it after. A wait that
// lasts longer cancels the read, so a broker that lost the object's pieces, a
// connection that went away or a bucket that keeps failing the request a read
// begins with ends the read in a minute rather than at the end of a budget
// that may be hours long.
//
// A missing object is [ErrNotFound] here, before the first byte.
func (s *Store) Open(ctx context.Context, o Object) (io.ReadCloser, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, ReadBudget(o.Size))
	body, err := s.begin(ctx, cancel, o, 0, -1)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("objstore: open %s: %w", o.Key, err)
	}
	return &verifier{
		object: o, body: &watched{ctx: ctx, cancel: cancel, r: body, what: o.Key, stall: s.stall},
		digest: sha256.New(),
	}, nil
}

// verifier is an open object's reader: see [Store.Open].
type verifier struct {
	object Object
	body   *watched
	digest hash.Hash

	// n is how many bytes the backend has handed over, held ones included.
	n int64
	// held is the verified tail, handed out once the whole is checked.
	held []byte
	// end is set once the whole object is verified and every held byte
	// handed out, and err once the read has failed — both for good.
	end bool
	err error
}

func (v *verifier) Read(p []byte) (int, error) {
	switch {
	case len(v.held) > 0:
		n := copy(p, v.held)
		v.held = v.held[n:]
		return n, nil
	case v.err != nil:
		return 0, v.err
	case v.end:
		return 0, io.EOF
	case len(p) == 0:
		return 0, nil
	}
	if rest := v.object.Size - v.n; rest > 1 {
		// A READ THAT CANNOT COMPLETE THE FILE is handed out as it
		// comes: at most every byte but the last, which [verifier.tail]
		// reads and checks before anything of it is handed out.
		n, err := v.body.Read(p[:min(int64(len(p)), rest-1)])
		v.n += int64(n)
		_, _ = v.digest.Write(p[:n])
		switch {
		case err == io.EOF: //nolint:errorlint // the end is io.EOF itself, as [Fill] reads it
			v.err = fmt.Errorf("%w: %s ended at %d of the %d bytes it was stored as",
				ErrCorrupt, v.object.Key, v.n, v.object.Size)
			return n, v.err
		case err != nil:
			v.err = fmt.Errorf("objstore: read %s at %d of %d bytes: %w",
				v.object.Key, v.n, v.object.Size, err)
			return n, v.err
		}
		return n, nil
	}
	if err := v.tail(); err != nil {
		v.err = err
		return 0, err
	}
	v.end = true
	return v.Read(p)
}

// tail reads the object's last byte, if it has one, and the end after it, and
// checks the whole against the row before anything of it is handed out.
//
// ONE BYTE PAST THE SIZE IS ASKED FOR, so an object holding more than its row
// says fails here rather than ending as though it were whole.
func (v *verifier) tail() error {
	left := v.object.Size - v.n
	buf := make([]byte, left+1)
	n, end, err := Fill(v.body, buf)
	v.n += int64(n)
	switch {
	case err != nil:
		return fmt.Errorf("objstore: read %s at %d of %d bytes: %w",
			v.object.Key, v.n, v.object.Size, err)
	case !end || int64(n) > left:
		return fmt.Errorf("%w: %s holds more than the %d bytes it was stored as",
			ErrCorrupt, v.object.Key, v.object.Size)
	case int64(n) < left:
		return fmt.Errorf("%w: %s ended at %d of the %d bytes it was stored as",
			ErrCorrupt, v.object.Key, v.n, v.object.Size)
	}
	_, _ = v.digest.Write(buf[:n])
	if got := Hash(hex.EncodeToString(v.digest.Sum(nil))); got != v.object.Hash {
		return fmt.Errorf("%w: %s hashes to %s and its row says %s",
			ErrCorrupt, v.object.Key, got, v.object.Hash)
	}
	v.held = buf[:n]
	return nil
}

// Close ends the read, the backend's stream with it.
func (v *verifier) Close() error { return v.body.Close() }

// begin asks the backend for n bytes of o from off — to the end where n is
// negative — under the stall watchdog: a backend that has handed back no
// stream within [Stall] has the read's context cancelled (cancel), and the
// read fails as stalled ([ErrStalled]), as a Read that waits as long does
// ([watched]).
//
// THE OPEN IS A WAIT ON THE BACKEND LIKE ANY READ, and the longest one: a
// backend looks the object up and asks for its first bytes before it hands a
// stream back — S3's first range is a request answered before Get returns,
// the broker's a leader's read of its metadata and its consumer's first
// message — and none of that is a Read for [watched] to time. Unwatched, an
// open the backend kept failing and asking again — a gateway answering one
// object with a server error every time, a bucket refusing connections, a
// throttle that never lifts — held its read, and the node's slot it was read
// in, until the read's whole budget had passed: two minutes for a small file,
// hours for a pack bundle near its ceiling. Watched, it ends where a read that
// heard nothing for as long would, and one the backend merely slowed, and that
// begins inside the stall, is read as ever.
func (s *Store) begin(ctx context.Context, cancel context.CancelFunc, o Object, off, n int64) (io.ReadCloser, error) {
	timer := time.AfterFunc(s.stall, cancel)
	body, err := s.backend.Get(ctx, o.Key.Name(), off, n)
	if timer.Stop() {
		return body, err
	}
	// THE WATCHDOG FIRED, whatever the backend then answered: the read's
	// context is cancelled, so a stream it handed back as the watchdog
	// fired would fail at its first Read — and the open is what stalled.
	if err == nil {
		_ = body.Close()
		err = context.Cause(ctx)
	}
	return nil, fmt.Errorf("%w: the backend began no stream for %v: %w", ErrStalled, s.stall, err)
}

// watched is a backend's stream under the stall watchdog: a Read that waits
// longer than [Stall] cancels ctx — which every backend's stream answers
// by ending ([Backend.Get]) — and fails as stalled ([ErrStalled]).
type watched struct {
	ctx    context.Context
	cancel context.CancelFunc
	r      io.ReadCloser
	what   Key
	stall  time.Duration

	closeOnce sync.Once
	closeErr  error
}

func (w *watched) Read(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	stalled := false
	var mu sync.Mutex
	timer := time.AfterFunc(w.stall, func() {
		mu.Lock()
		stalled = true
		mu.Unlock()
		w.cancel()
	})
	n, err := w.r.Read(p)
	timer.Stop()
	mu.Lock()
	defer mu.Unlock()
	if stalled && err != nil && err != io.EOF {
		return n, fmt.Errorf("objstore: %s %w: the backend handed over nothing "+
			"for %v: %w", w.what, ErrStalled, w.stall, err)
	}
	return n, err
}

// Close ends the read once, the watchdog's context with it.
func (w *watched) Close() error {
	w.closeOnce.Do(func() {
		w.cancel()
		w.closeErr = w.r.Close()
	})
	return w.closeErr
}

// ReadAt answers the bytes of o from offset off, at most n of them: fewer at
// the end of the object, and none — without asking the backend — at or past
// it.
//
// CHECKED FOR ITS LENGTH ONLY: a page of an object cannot be checked against
// the whole object's digest, which is the one the row keeps. A page that is
// not the length the row's size implies is [ErrCorrupt], and so is a LAST
// page with a byte after it — an object held longer than its row — since the
// final page is the one read that can see the object's end; a page of the
// right length is whatever the backend holds there. It is what a tool reading a
// page into a model's context takes, where reading the whole object to check
// one page would be the wrong price. Bounded as [Store.Open] is — the stall
// watching its open and its Reads — by the budget of every byte up to the
// page's end, because a backend with no ranged read (the broker's) walks the
// object to the offset, reading its messages' headers alone inside the open.
func (s *Store) ReadAt(ctx context.Context, o Object, off, n int64) ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if off < 0 || n < 0 {
		return nil, fmt.Errorf("objstore: a read of %d bytes at offset %d", n, off)
	}
	end := min(off+n, o.Size)
	if off >= end {
		return []byte{}, nil
	}
	want := end - off
	// THE LAST PAGE ASKS FOR ONE BYTE MORE THAN THE ROW SAYS IS THERE.
	// Every backend caps its answer at the range it was asked for, so a
	// page that asked for exactly `want` could never see an object held
	// longer than its row; a range running past the end is answered short
	// ([Backend.Get]), so on a sound object the probe costs nothing. An
	// interior page asks for its own range: the byte after it is the
	// object's next one, which proves nothing.
	ask := want
	if end == o.Size {
		ask++
	}
	ctx, cancel := context.WithTimeout(ctx, ReadBudget(off+ask))
	body, err := s.begin(ctx, cancel, o, off, ask)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("objstore: read %s: %w", o.Key, err)
	}
	r := &watched{ctx: ctx, cancel: cancel, r: body, what: o.Key, stall: s.stall}
	defer func() { _ = r.Close() }()
	// ONE BYTE PAST THE PAGE, so a backend answering more than the page —
	// the probe's byte, or one that ignored its range — is caught rather
	// than cut to fit.
	page, err := io.ReadAll(io.LimitReader(r, want+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("objstore: read %s at %d: %w", o.Key, off, err)
	case int64(len(page)) > want && end == o.Size:
		return nil, fmt.Errorf("%w: %s holds more than the %d bytes its row says it has",
			ErrCorrupt, o.Key, o.Size)
	case int64(len(page)) != want:
		return nil, fmt.Errorf("%w: %s answered %d bytes at %d, and its row says %d are there",
			ErrCorrupt, o.Key, len(page), off, want)
	}
	return page, nil
}

// Stat answers what the backend holds under k — for the collector's audit and
// the backup, each of which checks that an object a required table names is
// there.
func (s *Store) Stat(ctx context.Context, k Key) (Info, error) {
	info, err := s.backend.Stat(ctx, k.Name())
	if err != nil {
		return Info{}, fmt.Errorf("objstore: stat %s: %w", k, err)
	}
	return info, nil
}

// Ranged reports whether the store's backend can list a range of its names in
// order ([Ranged]) — whether [Store.ListKeys] can answer at all.
func (s *Store) Ranged() bool {
	_, ok := s.backend.(Ranged)
	return ok
}

// ListKeys hands visit every object the store holds whose key comes after
// after — the zero key: from the first — and at or before through, in key
// order ([Key.Compare]), with what the backend lists of it; a name in the range
// that is not a key the store would have written is passed over, as the
// collector passes it over.
//
// THE ORDER IS CHECKED, NOT TRUSTED: a backend that hands back a name at or
// before one it already handed back, or outside the range, fails the listing
// with [ErrUnordered] — every caller reads an object missing from a range as
// one the store does not hold, so a listing that broke its order would have
// it read objects the store holds as lost. A backend that is not [Ranged]
// answers ErrUnordered at once.
func (s *Store) ListKeys(ctx context.Context, after, through Key, visit func(Key, Info) error) error {
	r, ok := s.backend.(Ranged)
	if !ok {
		return fmt.Errorf("%w: this backend keeps no order", ErrUnordered)
	}
	// EVERY KEY'S NAME SORTS AFTER THE NAMESPACE ITSELF, which is a proper
	// prefix of it — so from there is from the first key, past every name
	// outside the namespace that sorts before it.
	prev := namespace
	if !after.IsZero() {
		prev = after.Name()
	}
	last := through.Name()
	return r.ListRange(ctx, prev, last, func(info Info) error {
		if info.Name <= prev || info.Name > last {
			return fmt.Errorf("%w: %q came after %q, in a listing through %q",
				ErrUnordered, info.Name, prev, last)
		}
		prev = info.Name
		if k, err := KeyOfName(info.Name); err == nil {
			return visit(k, info)
		}
		return nil
	})
}

// Delete removes k's object. Deleting what is not there is not an error.
func (s *Store) Delete(ctx context.Context, k Key) error {
	if err := s.backend.Delete(ctx, k.Name()); err != nil {
		return fmt.Errorf("objstore: delete %s: %w", k, err)
	}
	return nil
}

// BatchDeletes reports whether the store's backend can delete many objects in
// one request ([BatchDeleter]) — whether [Store.DeleteKeys] can answer at all.
func (s *Store) BatchDeletes() bool {
	_, ok := s.backend.(BatchDeleter)
	return ok
}

// DeleteKeys removes the objects of keys — at most [MaxDeleteBatch] — in one
// batch deletion, answering how many the store says it removed: all of them
// when err is nil; all but the names a [*DeleteBatchError] gives when the store
// answered name by name; and none it can count otherwise — for [ErrNoBatch], a
// store that refused the request or a backend with no such verb, none at all.
// Deleting what is not there is not an error.
func (s *Store) DeleteKeys(ctx context.Context, keys []Key) (int, error) {
	b, ok := s.backend.(BatchDeleter)
	if !ok {
		return 0, fmt.Errorf("%w: this backend deletes one name a request", ErrNoBatch)
	}
	if len(keys) > MaxDeleteBatch {
		return 0, fmt.Errorf("objstore: a batch deletion of %d objects, past the %d one takes",
			len(keys), MaxDeleteBatch)
	}
	if len(keys) == 0 {
		return 0, nil
	}
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = k.Name()
	}
	err := b.DeleteBatch(ctx, names)
	if err == nil {
		return len(keys), nil
	}
	var refused *DeleteBatchError
	if errors.As(err, &refused) {
		return len(keys) - len(refused.Failed), fmt.Errorf("objstore: delete %d objects: %w", len(keys), err)
	}
	return 0, fmt.Errorf("objstore: delete %d objects: %w", len(keys), err)
}
