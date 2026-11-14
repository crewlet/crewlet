package llmapi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
)

// TestedWindow is how long a candidate's passed test answers for a write of
// the same candidate: ten minutes, the span between a wizard's test and its
// save with a review read in between, and far inside the lifetime of any
// credential a vendor issues (a headless token lasts a year, a key until it
// is revoked). A write past it tests again.
const TestedWindow = 10 * time.Minute

// maxTested bounds the remembered passes. Only an admin's passed tests land
// here, a handful an hour at most, so the bound exists for the process's
// memory rather than for any load: at it, the oldest pass goes.
const maxTested = 64

// tested remembers, on this node and in memory, which candidates passed their
// test and when, so the write that follows a dry run does not run the harness a
// second time for the answer it was just given.
//
// THE WRITE AFTER A TEST REPEATED THE TEST. A person tests a credential (the
// route's dry run) and then saves it (the route itself), seconds apart, with
// the same values for the same entry. Each ran the harness, so the save
// waited a whole second run to learn what the first had said (measured: a
// run is 5 to 6 s at its shortest, 3 to 20 s when it answers).
//
// A PASS IS REMEMBERED BY A KEYED DIGEST, never by the value: an HMAC, under
// a key made at start and held nowhere else, of who asked, the entry's key,
// the entry as it was tested (the draft, or the active entry) and each name
// with its value. So the store holds nothing a memory dump could read a
// credential from, a pass answers only for exactly what was tested, by the
// principal who tested it, and an entry edited in between is a different
// digest and tests again.
//
// THE GUARANTEE IS UNCHANGED: a value is sealed only if the entry's harness
// signed in with it on this node, within [TestedWindow]. A write another node
// answers, or one after a restart, finds no pass and runs the test itself.
type tested struct {
	mu   sync.Mutex
	key  []byte
	rows map[[sha256.Size]byte]testedRow
}

type testedRow struct {
	check Check
	at    time.Time
}

func newTested() *tested {
	key := make([]byte, sha256.Size)
	// crypto/rand.Read never fails (it aborts the process rather than
	// return short), so the key is always whole.
	_, _ = rand.Read(key)
	return &tested{key: key, rows: map[[sha256.Size]byte]testedRow{}}
}

// digest names one candidate: who, which entry, the entry as tested, and
// every name with its value, each field length-prefixed so no two inputs
// spell the same bytes.
func (t *tested) digest(operator, key string, entry *config.LLMProvider,
	values map[string]string, names []string) ([sha256.Size]byte, bool) {

	var out [sha256.Size]byte
	spelled, err := json.Marshal(entry)
	if err != nil {
		return out, false
	}
	mac := hmac.New(sha256.New, t.key)
	field := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		mac.Write(n[:])
		mac.Write(b)
	}
	field([]byte(operator))
	field([]byte(key))
	field(spelled)
	for _, name := range names {
		field([]byte(name))
		field([]byte(values[name]))
	}
	copy(out[:], mac.Sum(nil))
	return out, true
}

// remember keeps a passed test for [TestedWindow].
func (t *tested) remember(id [sha256.Size]byte, check Check, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweep(now)
	if len(t.rows) >= maxTested {
		var oldest [sha256.Size]byte
		first := true
		for k, row := range t.rows {
			if first || row.at.Before(t.rows[oldest].at) {
				oldest, first = k, false
			}
		}
		delete(t.rows, oldest)
	}
	t.rows[id] = testedRow{check: check, at: now}
}

// recall is the passed test of id, if one is within [TestedWindow].
func (t *tested) recall(id [sha256.Size]byte, now time.Time) (Check, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweep(now)
	row, ok := t.rows[id]
	return row.check, ok
}

// sweep drops every pass older than [TestedWindow], and any dated ahead of
// now (a clock set back), which no window measures.
func (t *tested) sweep(now time.Time) {
	for k, row := range t.rows {
		if age := now.Sub(row.at); age < 0 || age > TestedWindow {
			delete(t.rows, k)
		}
	}
}
