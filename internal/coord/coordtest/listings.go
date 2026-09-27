package coordtest

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- whole-register listings under their own writers ------------------- //

// listingCases certify that a register listing answers every record that
// exists throughout it, ONCE, while the record is being rewritten.
//
// Every register here is rewritten by its own writers as a matter of course —
// a node's positions row on every heartbeat, a mailbox record on every sweep
// that judges it — and read whole by somebody deciding what to delete. The
// memory twin answers these from a map under a lock and cannot get them wrong;
// the embedded KV reads them in one pass over a stream whose every overwrite
// REMOVES the revision the pass was about to read, and before its listings
// were certified it lost a row being rewritten on 23% of listings and,
// more rarely, listed one row twice.
var listingCases = []fleetCase{{
	// The trim takes a MINIMUM across these rows. A row the listing
	// misses raises the floor past a node that has not applied what the
	// trim then deletes; a row listed twice is a second, staler reading
	// of one node beside its current one.
	name: "a positions row heartbeated throughout a listing is in every listing, once",
	fn: func(h *fleetHarness) {
		nodes := []string{"n0", "n1", "n2", "n3", "n4"}
		for _, n := range nodes {
			h.putPositions(n, 1)
		}
		c := startChurn([]string{"n0", "n2", "n4"}, func(n string) error {
			return h.f.PutPositions(h.ctx, coord.NodePositions{
				NodeID:  n,
				Domains: map[string]coord.DomainPosition{"tracker": {Seq: 2, AppliedThrough: 2}},
			})
		})
		raced, wrong := c.readThrough(h.ctx, func() (string, bool) {
			rows, err := h.f.Positions(h.ctx)
			if err != nil {
				return fmt.Sprintf("Positions: %v", err), true
			}
			got := make([]string, 0, len(rows))
			for _, r := range rows {
				got = append(got, r.NodeID)
			}
			slices.Sort(got)
			if !slices.Equal(got, nodes) {
				return strings.Join(got, ","), false
			}
			return "", false
		})
		c.verdict(h.t, "Positions", raced, wrong)
	},
}, {
	// The retention sweep lists the registry to decide which removed
	// seats' mailboxes to retire, and it rewrites the records it judges
	// while other nodes' sweeps are listing them.
	name: "a mailbox record rewritten throughout a listing is in every listing, once",
	fn: func(h *fleetHarness) {
		handles := []string{"ceo", "cto", "eng", "ops", "pm"}
		// A cell per handle, written only by that handle's rewriter.
		type held struct{ rec coord.MailboxRecord }
		records := map[string]*held{}
		for _, handle := range handles {
			rec, created := h.createMailbox(coord.MailboxRecord{Handle: handle})
			if !created {
				h.t.Fatalf("CreateMailbox(%s) found a record in an empty store", handle)
			}
			records[handle] = &held{rec}
		}
		c := startChurn([]string{"ceo", "eng", "pm"}, func(handle string) error {
			cell := records[handle]
			next := cell.rec
			next.AbsentSince = h.now().Add(time.Duration(next.Version) * time.Second)
			stored, ok, err := h.f.UpdateMailbox(h.ctx, next)
			if err == nil && !ok {
				err = fmt.Errorf("lost a race nobody else was in, at version %d", next.Version)
			}
			if err != nil {
				return err
			}
			cell.rec = stored
			return nil
		})
		raced, wrong := c.readThrough(h.ctx, func() (string, bool) {
			recs, err := h.f.Mailboxes(h.ctx)
			if err != nil {
				return fmt.Sprintf("Mailboxes: %v", err), true
			}
			got := make([]string, 0, len(recs))
			for _, r := range recs {
				got = append(got, r.Handle)
			}
			if !slices.Equal(got, handles) {
				return strings.Join(got, ","), false
			}
			return "", false
		})
		c.verdict(h.t, "Mailboxes", raced, wrong)
	},
}}

func (h *fleetHarness) putPositions(node string, seq uint64) {
	h.t.Helper()
	if err := h.f.PutPositions(h.ctx, coord.NodePositions{
		NodeID:  node,
		Domains: map[string]coord.DomainPosition{"tracker": {Seq: seq, AppliedThrough: seq}},
	}); err != nil {
		h.t.Fatalf("PutPositions(%s): %v", node, err)
	}
}
