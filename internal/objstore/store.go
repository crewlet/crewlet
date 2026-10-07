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

	// stall is [ReadStall], a field so a test can watch a stall end a read
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
	return &Store{backend: backend, now: now, stall: ReadStall}, nil
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
	if limit < 0 {
		return Object{}, fmt.Errorf("objstore: a put limited to %d bytes", limit)
	}
	key := KeyAt(s.now())
	in := &metered{ctx: ctx, r: r, limit: limit, digest: sha256.New()}
	err := s.backend.Put(ctx, key.Name(), in, m)
	switch {
	case in.err != nil:
		return Object{}, in.err
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
// is sooner — and each Read of the backend under [ReadStall]: a Read that
// waits longer cancels the stream, so a broker that lost the object's pieces
// or a connection that went away ends the read in a minute rather than at the
// end of a budget that may be hours long.
//
// A missing object is [ErrNotFound] here, before the first byte.
func (s *Store) Open(ctx context.Context, o Object) (io.ReadCloser, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, ReadBudget(o.Size))
	body, err := s.backend.Get(ctx, o.Key.Name(), 0, -1)
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

// watched is a backend's stream under the stall watchdog: a Read that waits
// longer than [ReadStall] cancels ctx — which every backend's stream answers
// by ending ([Backend.Get]) — and fails as stalled.
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
		return n, fmt.Errorf("objstore: %s stalled: the backend handed over nothing "+
			"for %v: %w", w.what, w.stall, err)
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
// one page would be the wrong price. Bounded as [Store.Open] is, by the
// budget of every byte up to the page's end, because a backend with no ranged
// read (the broker's) walks the object to the offset.
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
	body, err := s.backend.Get(ctx, o.Key.Name(), off, ask)
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

// Stat answers what the backend holds under k — for the backup, which checks
// that an object its copy names is there.
func (s *Store) Stat(ctx context.Context, k Key) (Info, error) {
	info, err := s.backend.Stat(ctx, k.Name())
	if err != nil {
		return Info{}, fmt.Errorf("objstore: stat %s: %w", k, err)
	}
	return info, nil
}

// Delete removes k's object. Deleting what is not there is not an error.
func (s *Store) Delete(ctx context.Context, k Key) error {
	if err := s.backend.Delete(ctx, k.Name()); err != nil {
		return fmt.Errorf("objstore: delete %s: %w", k, err)
	}
	return nil
}
