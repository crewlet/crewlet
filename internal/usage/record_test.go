package usage_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
)

// newerKind is a record a later build writes about a kind this one has never
// heard of, stamped the way that build would stamp it: above this build's
// [usage.RecordVersion], and keyed on a field — a team — this build cannot
// decode, with the identity that build composed from it stated beside.
func newerKind(t *testing.T, v int) []byte {
	t.Helper()
	return newerKindFor(t, v, "payments")
}

func newerKindFor(t *testing.T, v int, team string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"v":      v,
		"writer": "node-b",
		"subject": map[string]any{
			"kind": "a_kind_from_a_later_build", "node": "node-b", "day": "2026-09-23",
			"team": team,
		},
		"subject_id": coord.DocumentKey("node-b", "2026-09-23", team),
		"scope":      statelog.ScopeSet{Paths: []string{"u.2026-09-23.node-b.later." + team}},
		"tokens":     []map[string]any{{"phase": "auxiliary", "total": 42, "calls": 1}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return body
}

// TWO OBJECTS OF A NEWER BUILD'S KIND ARE TWO SUBJECTS, on one node's one day.
//
// The subject is what this compacted domain supersedes on: a deferred record
// is retained under it with whatever an earlier one held there deleted first
// (statelog's retain). A kind a later build adds is keyed on fields this build
// cannot decode, and the subject composed from the ones it can — node, day,
// an empty seat — made every object of that kind on one day the same subject,
// so retaining the second team's day deleted the first's. Once this node was
// upgraded only the last was applied, and its publisher re-derives no older
// day: the replicated copies disagreed for good. The identity the writer
// stated is what files each.
//
// Mutation: compose the subject from the decoded fields for every kind, and
// the two teams collide.
func TestTwoObjectsOfANewerKindAreTwoSubjects(t *testing.T) {
	t.Parallel()
	payments, err := usage.Domain{}.Envelope(newerKindFor(t, usage.RecordVersion+1, "payments"))
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	search, err := usage.Domain{}.Envelope(newerKindFor(t, usage.RecordVersion+1, "search"))
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if payments.Subject == search.Subject {
		t.Fatalf("both teams' days are filed under %+v: retaining one deletes the other",
			payments.Subject)
	}
	if want := coord.DocumentKey("node-b", "2026-09-23", "search"); search.Subject.ID != want {
		t.Errorf("subject = %q, want the identity its writer stated, %q", search.Subject.ID, want)
	}
}

// A STATED IDENTITY IS HELD TO THE SUBJECT IT NAMES.
//
// For a kind this build knows, the identity must be the one its fields compose
// — filed under either, a record naming another object would supersede the
// wrong row. For a kind it does not, the identity is required (every build
// that can write such a kind states it) and must lie under the record's own
// node and day.
func TestAStatedIdentityIsHeldToItsSubject(t *testing.T) {
	t.Parallel()
	rewrite := func(payload []byte, id any) []byte {
		t.Helper()
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Fatal(err)
		}
		if id == nil {
			delete(body, "subject_id")
		} else {
			body["subject_id"] = id
		}
		out, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	newer := newerKind(t, usage.RecordVersion+1)
	for name, id := range map[string]any{
		"none":         nil,
		"another node": coord.DocumentKey("node-c", "2026-09-23", "payments"),
		"no object":    coord.DocumentKey("node-b", "2026-09-23"),
		"another day":  coord.DocumentKey("node-b", "2026-09-24", "payments"),
	} {
		if _, err := (usage.Domain{}).Envelope(rewrite(newer, id)); err == nil {
			t.Errorf("a newer kind stating %s as its identity was admitted", name)
		}
	}

	seat := usage.Record{RecordEnvelope: usage.RecordEnvelope{
		Subject: usage.Subject{Kind: usage.KindSeat, Node: "node-b", Day: "2026-09-23", Seat: "agent-1"},
	}, Turns: &usage.Turns{}}
	encoded, err := seat.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// EVERY WRITER STATES IT, its own kinds included: a build that adds a
	// kind is a build that writes this, and so is every build after this one.
	stated, err := usage.DecodeEnvelope(encoded)
	if err != nil || stated.SubjectID != seat.Subject.ID() {
		t.Fatalf("a seat record states its identity as %q (%v), want %q",
			stated.SubjectID, err, seat.Subject.ID())
	}
	env, err := (usage.Domain{}).Envelope(encoded)
	if err != nil {
		t.Fatalf("a seat record's own envelope: %v", err)
	}
	if env.Subject != seat.Subject.Wire() {
		t.Errorf("a seat record is filed under %+v, want its own %+v", env.Subject, seat.Subject.Wire())
	}
	if _, err := (usage.Domain{}).Envelope(rewrite(encoded, coord.DocumentKey("node-b", "2026-09-23", "agent-2"))); err == nil {
		t.Error("a seat record stating another seat's identity was admitted")
	}
}

// A NEWER BUILD'S KIND IS DEFERRED, NOT A STOP.
//
// The envelope is the half every build must read, and the framework stops a
// domain's whole applier on one it cannot (statelog's decode): no record after
// it is applied on that node until an operator intervenes. Kind's own doc
// promised that a kind a newer build publishes is DEFERRED, and the envelope
// refused every kind it did not know — so the first record of any kind a
// later build added would have stopped the usage domain on every node still
// running this one. A record stamped above this build's version is a newer
// build's, so its envelope is read and the record is retained to be applied by
// a build that knows it.
func TestANewerBuildsKindIsDeferredNotAStop(t *testing.T) {
	t.Parallel()
	payload := newerKind(t, usage.RecordVersion+1)
	env, err := usage.Domain{}.Envelope(payload)
	if err != nil {
		t.Fatalf("the envelope of a newer build's kind = %v — an unreadable envelope "+
			"stops this domain's applier on every node still on this build", err)
	}
	if env.Kind != "a_kind_from_a_later_build" || env.Subject.Kind != env.Kind {
		t.Fatalf("envelope = %+v, want the newer build's kind carried through", env)
	}
	_, err = usage.Decode(payload)
	var future *usage.ErrFutureVersion
	if !errors.As(err, &future) {
		t.Fatalf("decode = %v, want ErrFutureVersion: the record is retained for a "+
			"build that can apply it, never applied as something it is not", err)
	}

	// THE APPLIER SAYS THE SAME, which is the answer the framework defers on.
	db := openStore(t)
	rec := statelog.Record{
		Envelope: env,
		Position: statelog.Position{Stream: usage.Domain{}.Stream().Name, Generation: 1, Seq: 1},
		Payload:  payload,
	}
	if err := applyRecord(context.Background(), db, rec); !errors.As(err, &future) {
		t.Fatalf("apply = %v, want ErrFutureVersion", err)
	}
}

// AN UNKNOWN KIND AT A VERSION THIS BUILD READS IS A WRITER FAULT.
//
// A version this build reads is one whose kinds it knows, so a kind it does not
// know at that version was written wrong rather than by a later build — and
// retaining it would wait for a build that is never coming.
func TestAnUnknownKindAtAKnownVersionIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := (usage.Domain{}).Envelope(newerKind(t, usage.RecordVersion)); err == nil {
		t.Fatal("an unknown kind at this build's own version was read as a record")
	}
}

// A NEWER BUILD'S KIND STILL NAMES ITS NODE AND ITS DAY: the two segments
// every kind's identity begins with are the only ones this build can check,
// and it checks them.
func TestANewerBuildsKindStillNamesItsNodeAndDay(t *testing.T) {
	t.Parallel()
	var body map[string]any
	if err := json.Unmarshal(newerKind(t, usage.RecordVersion+1), &body); err != nil {
		t.Fatal(err)
	}
	body["subject"].(map[string]any)["day"] = "the twenty-third"
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (usage.Domain{}).Envelope(payload); err == nil {
		t.Fatal("a newer build's record with no readable day was admitted")
	}
}

// A FULL SEAT-DAY FITS ONE MESSAGE. A record is published whole, so one the
// transport cannot carry is refused on every attempt and its day never
// replicates. [usage.ReadsPerSeatDay] is what keeps a seat-day small, and this
// measures it at its worst: every read entry present, every id a UUID, every
// entry carrying the longest query a search accepts — once as typed and once
// spelled in the bytes JSON escapes to six — beside a generous set of spend
// cells and a full duration histogram. The record has to fit
// [queue.MaxPayloadBytes], and it is held under the 1 MiB that ADR-0020 and
// the cap's own comment state, so a record that outgrows the figure goes red
// here rather than leaving the prose to go stale.
//
// Mutation: raise ReadsPerSeatDay to 4096, and the escaped case outgrows the
// transport while the typed one outgrows the documented figure.
func TestAFullSeatDayRecordFitsOneMessage(t *testing.T) {
	t.Parallel()
	const documented = 1 << 20
	for name, query := range map[string]string{
		"a full-length query as typed":         strings.Repeat("q", knowledge.MaxQueryBytes),
		"a full-length query JSON escapes all": strings.Repeat("<", knowledge.MaxQueryBytes),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 9, 23, 23, 59, 59, 999_999_999, time.UTC)
			r := usage.Record{
				RecordEnvelope: usage.RecordEnvelope{Writer: "node-b",
					Subject: usage.Subject{Kind: usage.KindSeat, Node: "node-b",
						Day: "2026-09-23", Seat: uuid.NewString()}},
				Handle: strings.Repeat("h", 64), Role: strings.Repeat("r", 128),
				Turns: &usage.Turns{Count: 1000, LastEndedAt: at},
			}
			for i := range 1000 {
				r.Turns.Durations.Add(time.Duration(i) * 7 * time.Second)
			}
			for _, phase := range []string{"execute", "review", "subagent", "auxiliary", "judge", "onboarding"} {
				for m := range 4 {
					for _, key := range []string{"primary", "fallback"} {
						r.Tokens = append(r.Tokens, usage.Tokens{Phase: phase,
							Worker: "worker-" + strconv.Itoa(m), Model: "model-" + strconv.Itoa(m),
							ProviderKey: key, Input: math.MaxInt32, Output: math.MaxInt32,
							CacheRead: math.MaxInt32, CacheWrite: math.MaxInt32,
							Total: math.MaxInt64, Calls: math.MaxInt32})
					}
				}
			}
			for range usage.ReadsPerSeatDay {
				r.Reads = append(r.Reads, usage.Read{PageID: uuid.NewString(),
					Backend: "confluence", Via: "skill_injected", Count: math.MaxInt64,
					LastAt: at, LastTurnID: uuid.NewString(), LastWorkKey: uuid.NewString(),
					LastQuery: query})
			}
			r.ReadsElided = math.MaxInt32
			encoded, err := r.Encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			t.Logf("a seat-day at the cap encodes to %d bytes (%.0f KiB)",
				len(encoded), float64(len(encoded))/1024)
			if len(encoded) >= queue.MaxPayloadBytes {
				t.Fatalf("a seat-day at the cap is %d bytes and the transport carries "+
					"at most %d: the record could never be published", len(encoded),
					queue.MaxPayloadBytes)
			}
			if len(encoded) >= documented {
				t.Errorf("a seat-day at the cap is %d bytes, past the %d ADR-0020 and "+
					"usage.ReadsPerSeatDay state — measure it again and correct both",
					len(encoded), documented)
			}
		})
	}
}
