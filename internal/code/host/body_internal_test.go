package host

import (
	"bytes"
	"math"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// A SMALL REQUEST COSTS ITS OWN SIZE: a clone's negotiation is a few hundred
// bytes, and a mebibyte allocated for each — and held while it waits for its
// sync and its transfer slot — is what a burst of fetches held for nothing.
// Whether the client declares its length or not, a 200-byte request allocates
// a few kilobytes, and gives back what it charged to the node's request
// memory.
func TestASmallRequestAllocatesItsOwnSize(t *testing.T) {
	s := &Server{requestMemory: RequestMemoryBytes, heads: newAllowance(RequestMemoryAllowance(64))}
	body := bytes.Repeat([]byte("x"), 200)
	for name, declared := range map[string]int64{"declared": int64(len(body)), "undeclared": -1} {
		t.Run(name, func(t *testing.T) {
			const runs = 200
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			for range runs {
				b, err := s.readRequest(t.Context(), bytes.NewReader(body), declared, "ceo", nil)
				if err != nil {
					t.Fatalf("read a request of %d bytes: %v", len(body), err)
				}
				if b.length() != int64(len(body)) {
					t.Fatalf("read %d bytes of a request of %d", b.length(), len(body))
				}
				b.close()
			}
			runtime.ReadMemStats(&after)
			if per := (after.TotalAlloc - before.TotalAlloc) / runs; per > 16<<10 {
				t.Errorf("a request of %d bytes allocated %d bytes, past the 16 KiB a few kilobytes of buffer "+
					"and its bookkeeping take", len(body), per)
			}
			if held := s.heads.inUse(); held != 0 {
				t.Errorf("the request memory holds %d bytes once every request was given back", held)
			}
		})
	}
}

// A DECLARED LENGTH IS THE CLIENT'S WORD, read like any other however large:
// net/http hands a handler a `Content-Length` up to math.MaxInt64, one past
// which is past every int64 — a first buffer sized declared+1 wrapped
// negative, took nothing, and was filled with nothing for ever, holding a core
// and the host's in-flight count. Every such length reads a short body
// promptly, held as large as it is.
func TestADeclaredLengthIsReadWhateverItsSize(t *testing.T) {
	body := []byte("0000")
	for _, declared := range []int64{math.MaxInt64, math.MaxInt64 - 1, RequestMemoryBytes, RequestMemoryBytes - 1,
		int64(len(body)), 0} {
		t.Run(strconv.FormatInt(declared, 10), func(t *testing.T) {
			s := &Server{requestMemory: RequestMemoryBytes, heads: newAllowance(RequestMemoryAllowance(64))}
			type read struct {
				b   *requestBody
				err error
			}
			done := make(chan read, 1)
			go func() {
				b, err := s.readRequest(t.Context(), bytes.NewReader(body), declared, "ceo", nil)
				done <- read{b, err}
			}()
			select {
			case r := <-done:
				if r.err != nil {
					t.Fatalf("read a request of %d bytes declaring %d: %v", len(body), declared, r.err)
				}
				if r.b.length() != int64(len(body)) {
					t.Errorf("read %d bytes of a request of %d declaring %d", r.b.length(), len(body), declared)
				}
				r.b.close()
			case <-time.After(5 * time.Second):
				t.Fatalf("a request of %d bytes declaring %d was not read in 5 s", len(body), declared)
			}
			if held := s.heads.inUse(); held != 0 {
				t.Errorf("the request memory holds %d bytes once the request was given back", held)
			}
		})
	}
}
