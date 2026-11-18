package kv

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// A LEASE RECORD IS WRITTEN WITHOUT HTML ESCAPES, the encoding
// [coord.LeaseMetaBytes] measures the Meta's bound in: encoding/json's default
// spells `<`, `>` and `&` as six bytes each, and the Meta's stored span must
// equal that measurement exactly.
//
// Mutation: write the record with json.Marshal and the stored meta escapes <,
// its span longer than measured.
func TestALeaseRecordIsWrittenWithoutHTMLEscapes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, embeddedNATS(t), time.Minute)
	node := coord.NodeResource("n1")
	meta := map[string]any{
		"labels": map[string]any{"tier": "<gold> & <silver>"},
		"roles":  []any{"seats"},
	}
	if lease, _, err := s.TryAcquire(ctx, node, coord.AcquireOptions{
		Owner: "n1:a", TTL: time.Minute, Ungated: true, Meta: meta}); err != nil || lease == nil {
		t.Fatalf("claim = (%v, %v)", lease, err)
	}
	kve, err := s.leases.kv.Get(ctx, encodeResource(node))
	if err != nil {
		t.Fatalf("read the stored record: %v", err)
	}
	stored := kve.Value()
	if !bytes.Contains(stored, []byte("<gold> & <silver>")) {
		t.Fatalf("the stored record escapes what it carries: %s", stored)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(stored, &fields); err != nil {
		t.Fatalf("decode the stored record: %v", err)
	}
	want, err := coord.LeaseMetaBytes(meta)
	if err != nil {
		t.Fatalf("measure the meta: %v", err)
	}
	if got := len(fields["meta"]); got != want {
		t.Fatalf("the stored meta is %d bytes, and coord.LeaseMetaBytes measured %d: %s",
			got, want, fields["meta"])
	}
}
