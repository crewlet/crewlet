package host

import (
	"bytes"
	"testing"
)

// A CLONE IS A FETCH THAT OFFERS NOTHING, under either protocol: v2's haves are
// arguments before the flush; v0's follow its wants' flush, where a clone's
// `done` comes at once. A fetch offering its tips is no clone, in one round or
// several; an ls-refs, or a fetch wanting nothing, fetches no history at all.
//
// Mutation: stop a v0 request at its first flush, and a v0 fetch offering its
// tips reads as a clone.
func TestACloneIsAFetchThatOffersNothing(t *testing.T) {
	t.Parallel()
	pkt := func(line string) string { return string(pktString(line + "\n")) }
	v2 := pkt("command=fetch") + pkt("agent=git/2.56.0") + delimPkt
	for _, tc := range []struct {
		name  string
		v2    bool
		req   string
		clone bool
	}{
		{"a v2 clone", true, v2 + pkt("thin-pack") + pkt("ofs-delta") + pkt("want "+oidA) + pkt("done") +
			flushPkt, true},
		{"a v2 partial clone's fetch of what it lacks", true, v2 + pkt("filter blob:none") + pkt("want "+oidA) +
			pkt("want "+oidB) + pkt("done") + flushPkt, true},
		{"a v2 fetch offering its tips", true, v2 + pkt("want "+oidA) + pkt("have "+oidB) + pkt("done") +
			flushPkt, false},
		{"a v2 round offering its tips before its wants are done", true, v2 + pkt("want "+oidA) +
			pkt("have "+oidB) + flushPkt, false},
		{"a v2 ls-refs", true, pkt("command=ls-refs") + delimPkt + pkt("peel") + flushPkt, false},
		{"a v2 fetch wanting nothing", true, v2 + pkt("have "+oidB) + pkt("done") + flushPkt, false},
		{"a v0 clone", false, pkt("want "+oidA+" multi_ack_detailed side-band-64k") + flushPkt + pkt("done"),
			true},
		{"a v0 shallow clone", false, pkt("want "+oidA+" shallow") + pkt("deepen 1") + flushPkt + pkt("done"),
			true},
		{"a v0 fetch offering its tips", false, pkt("want "+oidA+" multi_ack_detailed") + flushPkt +
			pkt("have "+oidB) + pkt("done"), false},
		{"a v0 round offering its tips", false, pkt("want "+oidA) + flushPkt + pkt("have "+oidB) + flushPkt,
			false},
		{"a v0 request that ends at its wants", false, pkt("want "+oidA) + flushPkt, true},
	} {
		n, err := parseNegotiation(bytes.NewReader([]byte(tc.req)), int64(len(tc.req)), tc.v2)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := n.clone(); got != tc.clone {
			t.Errorf("%s: read as a clone %t, want %t (wants %t, haves %t)", tc.name, got, tc.clone, n.wants,
				n.haves)
		}
	}
}
