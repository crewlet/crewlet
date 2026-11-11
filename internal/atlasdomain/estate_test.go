package atlasdomain_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE APPLY HARNESS: one data node's estate, records run through the shipped
// gate and applier at instants the case chooses, as the framework's loop runs
// them, with no broker in between. Writers are the write harness's business.

// estate is one node's replicated estate and how far it has applied.
type estate struct {
	t       *testing.T
	db      store.ReplicatedHandle
	applier *atlasdomain.Applier
	seq     uint64
}

// newEstate is a fresh estate at the migrated image.
func newEstate(t *testing.T) *estate {
	t.Helper()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return &estate{t: t, db: db, applier: atlasdomain.NewApplier()}
}

// position is the place of record seq on the log.
func position(seq uint64) statelog.Position {
	return statelog.Position{Stream: atlasdomain.Domain{}.Stream().Name, Generation: 1, Seq: seq}
}

// apply runs one record at the log's next position, stored at `at`, and
// answers the gate that dropped it, empty where it applied.
func (e *estate) apply(body []byte, at time.Time) statelog.Reason {
	e.t.Helper()
	env, err := atlasdomain.Domain{}.Envelope(body)
	if err != nil {
		e.t.Fatalf("envelope: %v", err)
	}
	e.seq++
	rec := statelog.Record{Envelope: env, Position: position(e.seq), Payload: body, StoredAt: at}
	var reason statelog.Reason
	if err := e.db.Tx(e.t.Context(), func(tx *sql.Tx) error {
		r, gated, err := e.applier.Gated(e.t.Context(), tx, rec)
		if err != nil || gated {
			reason = r
			return err
		}
		reason = ""
		_, err = e.applier.Apply(e.t.Context(), tx, rec, statelog.ApplyOptions{
			Now: at, StoredAt: at, MaxVariables: 999})
		return err
	}); err != nil {
		e.t.Fatalf("apply record %d: %v", e.seq, err)
	}
	return reason
}

// claim encodes one person's claim as the writer would, under opID, on its
// entry's own subject.
func claimRecord(t *testing.T, opID string, c atlas.Claim) []byte {
	t.Helper()
	body, err := rawRecord(atlasdomain.EntrySubject(c.Subject), "node-a", opID, atlasdomain.OpClaim,
		sentinel(), wireClaim(opID, c))
	if err != nil {
		t.Fatalf("encode a claim: %v", err)
	}
	return body
}

// wireClaim is the log's shape of c as a person's claim under opID.
func wireClaim(opID string, c atlas.Claim) atlasdomain.Claim {
	out := atlasdomain.Claim{V: 1, ID: atlasdomain.ClaimID(opID),
		Entry: atlasdomain.Ref{Kind: c.Subject.Kind, Key: c.Subject.Key}, Slot: c.Slot(),
		Facet: c.Facet.Kind, Fact: c.Facet.Fact, Relation: c.Facet.Relation,
		External: c.Facet.Target.External, Name: c.Facet.Target.Name, Denied: c.Denied,
		Source: atlasdomain.Source{Kind: c.Source.Kind, Name: c.Source.Name}}
	if t := c.Facet.Target.Entry; t != (atlas.Ref{}) {
		out.Target = &atlasdomain.Target{ID: t.ID().String(), Kind: t.Kind, Key: t.Key}
	}
	if c.Value != (atlas.Value{}) {
		out.Value = &atlasdomain.Value{Type: c.Value.Type, Text: c.Value.Text, Private: c.Value.Private}
	}
	return out
}

// purgeEntryRecord and purgeValueRecord are an administrator's purges under opID.
func purgeEntryRecord(t *testing.T, opID string, r atlas.Ref) []byte {
	t.Helper()
	body, err := rawRecord(atlasdomain.EntrySubject(r), "node-a", opID, atlasdomain.OpPurge, domain(),
		atlasdomain.Purge{V: atlasdomain.GateRecordVersion})
	if err != nil {
		t.Fatalf("encode a purge: %v", err)
	}
	return body
}

func purgeValueRecord(t *testing.T, opID, text string) []byte {
	t.Helper()
	body, err := rawRecord(atlasdomain.ValueSubject(text), "node-a", opID, atlasdomain.OpPurge, domain(),
		atlasdomain.Purge{V: atlasdomain.GateRecordVersion})
	if err != nil {
		t.Fatalf("encode a purge: %v", err)
	}
	return body
}

// expireRecord is the retention's expiry of the hour through falls in.
func expireRecord(t *testing.T, opID string, through time.Time) []byte {
	t.Helper()
	body, err := rawRecord(atlasdomain.ExpirySubject(through.Add(-time.Nanosecond)), "node-a", opID,
		atlasdomain.OpExpire, domain(), atlasdomain.Expire{V: 1, Through: through})
	if err != nil {
		t.Fatalf("encode an expiry: %v", err)
	}
	return body
}

// reader is this estate's read side, at what it has applied.
func (e *estate) reader() *atlasdomain.Reader {
	e.t.Helper()
	authority, err := statelogtest.LocalReader(atlasdomain.Domain{}, e.db, position(e.seq))
	if err != nil {
		e.t.Fatalf("build the read authority: %v", err)
	}
	r, err := atlasdomain.NewReader(atlasdomain.ReaderOptions{DB: e.db.Reader(), Log: authority})
	if err != nil {
		e.t.Fatalf("build the reader: %v", err)
	}
	return r
}

// live is every live claim this estate holds.
func (e *estate) live() []atlas.Claim {
	e.t.Helper()
	got, err := e.reader().Live(e.t.Context(), statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		e.t.Fatalf("read the live claims: %v", err)
	}
	return got.Claims
}

// count is how many rows a statement counts here.
func (e *estate) count(statement string, args ...any) int {
	e.t.Helper()
	var c int
	if err := e.db.Read(e.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(e.t.Context(), statement, args...).Scan(&c)
	}); err != nil {
		e.t.Fatalf("count: %v", err)
	}
	return c
}

// history is every claim recorded on r, newest first.
func (e *estate) history(r atlas.Ref) []atlasdomain.ClaimRow {
	e.t.Helper()
	page, err := e.reader().History(e.t.Context(), r.ID().String(), "", atlasdomain.MaxPageSize,
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		e.t.Fatalf("read %s's history: %v", r, err)
	}
	return page.Claims
}
