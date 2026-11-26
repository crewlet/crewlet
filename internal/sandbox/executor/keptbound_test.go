package executor_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/secrets"
)

// keptRead is the bytes each of the two reads carries, 13 MiB, so the two,
// each written twice, pack just past the text bound; keptTurn is the turn
// their session is kept under, in the engine's spelling.
const (
	keptRead = 13 << 20
	keptTurn = "0190c5d2-4c00-7000-8000-000000000001"
)

// appendRead appends to transcript a Claude Code line answering a whole-file
// read of n seeded random bytes, its base64 twice (the result's and the
// tool's own), beside planted; streamed, so no copy of the payload is whole.
func appendRead(t *testing.T, transcript string, seed uint64, n int64, planted string) {
	t.Helper()
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	payload := func() {
		enc := base64.NewEncoder(base64.StdEncoding, w)
		if _, err := io.CopyN(enc, rand.NewChaCha8([32]byte{byte(seed)}), n); err != nil {
			t.Fatal(err)
		}
		_ = enc.Close()
	}
	_, _ = w.WriteString(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":` +
		`[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"`)
	payload()
	_, _ = w.WriteString(`"}}]}]},"toolUseResult":{"type":"pdf","file":{"base64":"`)
	payload()
	_, _ = w.WriteString(`"}},"note":"` + planted + `"}` + "\n")
	if err = errors.Join(w.Flush(), f.Close()); err != nil {
		t.Fatal(err)
	}
}

// drainLedger is a take's room as its node keeps it: the scrub's spool
// taken and given back, the sealed spools' bytes beside it, and the most both
// held at once.
type drainLedger struct {
	mu                  sync.Mutex
	used, peak, spooled int64
	taken, given        int64
}

func (l *drainLedger) spool(dir string) sandbox.SessionSpool {
	return sandbox.SessionSpool{Dir: dir,
		Take: func(n int64) error {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.used, l.taken = l.used+n, l.taken+n
			l.peak = max(l.peak, l.used)
			return nil
		},
		Give: func(n int64) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.used, l.given = l.used-n, l.given+n
		},
		Spooled: func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.spooled = l.used
		},
	}
}

// sealing is w, its bytes counted in the ledger as a sealed spool's are.
func (l *drainLedger) sealing(w io.Writer) io.Writer { return &ledgerWriter{w: w, l: l} }

type ledgerWriter struct {
	w io.Writer
	l *drainLedger
}

func (w *ledgerWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.l.mu.Lock()
	w.l.used += int64(n)
	w.l.peak = max(w.l.peak, w.l.used)
	w.l.mu.Unlock()
	return n, err
}

// sessionStoreOver is a session store over a fresh object store under one key.
func sessionStoreOver(t *testing.T) *executordomain.SessionStore {
	t.Helper()
	objects, err := objstore.NewStore(memobj.New())
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := secrets.NewStreamSealer(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": bytes.Repeat([]byte("1"), 32)}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := executordomain.NewSessionStore(objects, sealer)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TWO WHOLE-FILE READS ARE KEPT AT THE KEPT BOUND AND RESTORED WHOLE
// (crewlet/crewlet#1626): a Claude Code session holding two 13 MiB reads, its
// box packing it past the text bound with its own tar and gzip, is refused at
// the text bound and taken at the kept bound, asked through the policy; its
// scrub's spool drains into its seals within one file of what it spooled;
// each payload is kept once, and the session opens back byte for byte as the
// archive its box keeps the digest of, a value planted beside a payload taken
// out. A larger third read passes the kept bound, refused before it is read.
// The kept bound's pace is TestTheMediaKeptApartFitTheKeepsPace's.
//
// Mutation: admit the text bound at the kept policy, or keep the scrub's
// files to its end, and it goes red.
func TestTwoWholeFileReadsAreKeptAtTheKeptBoundAndRestoredWhole(t *testing.T) {
	t.Parallel()
	const planted = "kept-bound-planted-value-0123456789"
	runner, b := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "begin"})
	transcript := transcriptOf(b, ran.SessionID)
	appendRead(t, transcript, 1, keptRead, planted)
	appendRead(t, transcript, 2, keptRead, planted)

	text := sandbox.SessionPolicy{Keep: true, MediaApart: true}
	if _, err := runner.TakeSession(t.Context(), b, text.Bound(sandbox.SessionTake{Pack: true,
		Spool: (&drainLedger{}).spool(t.TempDir())})); !errors.Is(err, executorkeep.ErrSessionTooLarge) {
		t.Fatalf("the two reads at the text bound came to %v, want them refused as too large", err)
	}

	kept := sandbox.SessionPolicy{Keep: true, MediaApart: true, KeptBound: true}
	ledger := &drainLedger{}
	taken, err := runner.TakeSession(t.Context(), b, kept.Bound(sandbox.SessionTake{Pack: true,
		Scrub: []string{planted}, Spool: ledger.spool(t.TempDir())}))
	if err != nil || taken.Body == nil {
		t.Fatalf("the two reads at the kept bound: %v", err)
	}
	if taken.Archive.Size <= executorkeep.SessionMaxBytes {
		t.Fatalf("the box packed the two reads in %d bytes, within the text bound's %d: the case no longer "+
			"holds a session only the kept bound keeps", taken.Archive.Size, executorkeep.SessionMaxBytes)
	}
	store, dir := sessionStoreOver(t), t.TempDir()
	type sealed struct {
		file  string
		spool executorkeep.MediaSpool
	}
	var media []sealed
	split := sandbox.SplitTakenBody(taken.Body, func(member io.Reader) (string, error) {
		f, err := os.CreateTemp(dir, "media-*")
		if err != nil {
			return "", err
		}
		spool, err := store.SealMedia("seat", member, ledger.sealing(f))
		if err = errors.Join(err, f.Close()); err != nil {
			return "", err
		}
		media = append(media, sealed{file: f.Name(), spool: spool})
		return spool.Digest, nil
	})
	textFile, err := os.CreateTemp(dir, "text-*")
	if err != nil {
		t.Fatal(err)
	}
	textSpool, err := store.Seal("seat", keptTurn, split, ledger.sealing(textFile))
	if err = errors.Join(err, textFile.Close(), taken.Body.Close()); err != nil {
		t.Fatalf("sealing the take: %v", err)
	}
	if err = taken.Keep(t.Context()); err != nil {
		t.Fatal(err)
	}

	if len(media) != 2 || media[0].spool.Digest == media[1].spool.Digest {
		t.Fatalf("the take kept %d payloads apart, want each of the two reads' once", len(media))
	}
	if ledger.given != ledger.taken {
		t.Errorf("the scrub took %d bytes of its room and gave back %d", ledger.taken, ledger.given)
	}
	if ledger.peak > ledger.spooled+executorkeep.SpoolSegmentBytes+256<<10 {
		t.Errorf("the take held %d bytes at once beside a scrub spool of %d, want at most one file (%d) and "+
			"the seals' slack past it", ledger.peak, ledger.spooled, executorkeep.SpoolSegmentBytes)
	}

	row := executordomain.Session{Handle: "seat", Turn: keptTurn, ArchiveBytes: split.Restored()}
	upload := func(file string, put func(io.Reader) error) {
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if err = put(f); err != nil {
			t.Fatalf("uploading %s: %v", file, err)
		}
	}
	upload(textFile.Name(), func(r io.Reader) error {
		object, err := store.Upload(t.Context(), textSpool, r)
		row.Object, row.ContentKey = object.Object, object.ContentKey
		return err
	})
	for _, m := range media {
		upload(m.file, func(r io.Reader) error {
			object, err := store.UploadMedia(t.Context(), m.spool, r)
			row.Media = append(row.Media, object)
			return err
		})
	}
	open := func() io.ReadCloser {
		r, err := store.Open(t.Context(), row)
		if err != nil {
			t.Fatalf("opening the kept session: %v", err)
		}
		return r
	}
	// ONE READ OF THE SESSION OPENED BACK feeds its digest and the scan for
	// the planted value.
	restored := open()
	defer func() { _ = restored.Close() }()
	pr, pw := io.Pipe()
	cksum := exec.Command("cksum")
	cksum.Stdin = pr
	var digest bytes.Buffer
	cksum.Stdout = &digest
	if err = cksum.Start(); err != nil {
		t.Fatal(err)
	}
	read := io.TeeReader(restored, pw)
	zr, err := gzip.NewReader(read)
	if err != nil {
		t.Fatal(err)
	}
	var tail []byte
	found := false
	for buf := make([]byte, 1<<20); ; {
		n, readErr := zr.Read(buf)
		window := append(tail, buf[:n]...)
		found = found || bytes.Contains(window, []byte(planted))
		tail = append(tail[:0], window[max(0, len(window)-len(planted)):]...)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
	}
	if found {
		t.Error("the session opened back holds the value planted beside a payload")
	}
	_, err = io.Copy(io.Discard, read)
	if err = errors.Join(err, pw.Close(), cksum.Wait()); err != nil {
		t.Fatal(err)
	}
	if digests := boxFile(t, b, paths(b).SessionDigests()); !strings.Contains(digests,
		ran.SessionID+" "+strings.TrimSpace(digest.String())+"\n") {
		t.Errorf("the session opened back digests %q, which its box does not keep: %q", digest.String(), digests)
	}

	appendRead(t, transcript, 3, 32<<20, planted)
	if _, err = runner.TakeSession(t.Context(), b, kept.Bound(sandbox.SessionTake{Pack: true,
		Spool: (&drainLedger{}).spool(t.TempDir())})); !errors.Is(err, executorkeep.ErrSessionTooLarge) {
		t.Errorf("a third read at the kept bound came to %v, want it refused as too large", err)
	}
}
