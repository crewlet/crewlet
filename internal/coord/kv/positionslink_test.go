package kv_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv"
	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
)

// positionsStream is the positions register's stream under kvtest's prefix.
const positionsStream = "KV_slowleaf_statelog_positions"

// randomText is n bytes of base64 of random bytes: text JSON writes as it is,
// which no link compresses.
func randomText(t *testing.T, n int) string {
	t.Helper()
	random := make([]byte, base64.StdEncoding.DecodedLen(n)+3)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(random)[:n]
}

// widest is the largest n in 0..most whose value build(n) encodes, as size
// says, within bound: a value at its bound to within one step of n.
//
// BUILD IS A FUNCTION OF n ALONE: it runs once per probe and once more for the
// value returned, so a clock read inside it could make the value returned
// encode wider than the one measured (a time drops its trailing zero digits).
func widest[T any](t *testing.T, most, bound int, build func(n int) T, size func(T) int) T {
	t.Helper()
	n := sort.Search(most+1, func(n int) bool { return size(build(n)) > bound }) - 1
	if n < 0 {
		t.Fatalf("no value fits its bound %d", bound)
	}
	return build(n)
}

// leaderReads counts the leader reads of stream asked from here on, as the
// member's broker hears them: on the account's own API, which the fleet's
// domain maps the leaf's requests onto. Never on the leaf, where a
// subscription on the domain's API is taken for a responder to it.
func leaderReads(t *testing.T, across kvtest.Across, stream string) func() int {
	t.Helper()
	subject := fmt.Sprintf(server.JSApiMsgGetT, stream)
	nc, err := across.MemberBroker.Conn()
	if err != nil {
		t.Fatalf("connect to the member: %v", err)
	}
	t.Cleanup(nc.Close)
	sub, err := nc.SubscribeSync(subject)
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return func() int {
		t.Helper()
		// A ROUND TRIP AFTER THE READ: the broker queued every request it
		// saw to this connection before the answer to its ping.
		if err := nc.Flush(); err != nil {
			t.Fatal(err)
		}
		n, _, err := sub.Pending()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
}

// EVERY POSITIONS CLASS AT ITS BOUND IS READ ACROSS A LEAF'S LINK AT ITS FIRST
// ASK. A value at its class's bound, on a link at or above the slowest pace
// allowed, crosses past a term read's re-ask or a pass's idle: a read given its
// class's crossing asks the leader once, and a pass given its quiet delivers
// the value, asking none. A class whose reply crosses inside the re-ask is held
// a second behind a reply sent ahead of it. Requests are counted as they reach
// the member. The two classes read at the term cross the floor inside it
// (TestASmallClassCrossesTheFloorInsideTheReadTerm).
//
// Mutation: read a class at the term (replyTerm), and its row asks again or
// fails.
func TestEveryPositionsClassAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		rate int64
		// behind is how long the link holds the member's bytes as the read
		// begins: none, or a second past a term read's re-ask.
		behind time.Duration
		// asks is how many leader reads the read makes: one for a key's
		// read, none for a pass that delivers its value.
		asks int
		// write puts a value at its class's bound through the member, and
		// read reads it through the leaf.
		write func(ctx context.Context, t *testing.T, member *kv.FleetStore)
		read  func(ctx context.Context, leaf *kv.FleetStore) error
	}{
		{"an MCP listing", 64 << 10, 0, 0, putListing, func(ctx context.Context, leaf *kv.FleetStore) error {
			updates, err := leaf.WatchMCPListings(ctx)
			if err != nil {
				return err
			}
			for u := range updates {
				if u.Current {
					return nil
				}
			}
			return fmt.Errorf("the watch ended before its first answer: %w", context.Cause(ctx))
		}},
		{"an MCP definition", 64 << 10, 0, 1, putDefinition, func(ctx context.Context, leaf *kv.FleetStore) error {
			_, found, err := leaf.MCPToolDefinition(ctx, linkDigest)
			return found1(found, err)
		}},
		{"an MCP check", 36 << 10, time.Second, 1, putCheck, func(ctx context.Context, leaf *kv.FleetStore) error {
			_, found, err := leaf.MCPCheck(ctx, "search")
			return found1(found, err)
		}},
		{"an access review", 36 << 10, time.Second, 1, putAccessReview, func(ctx context.Context, leaf *kv.FleetStore) error {
			_, _, found, err := leaf.AccessReview(ctx, "sre")
			return found1(found, err)
		}},
		{"the seat access records", 40 << 10, 0, 0, putSeatAccess, func(ctx context.Context, leaf *kv.FleetStore) error {
			records, err := leaf.SeatAccessRecords(ctx, "gitlab")
			return found1(len(records) == 1, err)
		}},
		{"one seat's access record", 40 << 10, 0, 1, putSeatAccess, func(ctx context.Context, leaf *kv.FleetStore) error {
			_, found, err := leaf.SeatAccessRecord(ctx, "gitlab", "sre")
			return found1(found, err)
		}},
		{"a Home board", 96 << 10, 0, 1, putHomeBoard, func(ctx context.Context, leaf *kv.FleetStore) error {
			_, _, found, err := leaf.HomeBoard(ctx, "founder")
			return found1(found, err)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if floor := int64(1<<20) * int64(time.Second) / int64(objstore.MiBPace); c.rate < floor {
				t.Fatalf("premise: the link carries %d bytes a second, below the floor's %d", c.rate, floor)
			}
			across := kvtest.SlowLeaf(t, c.rate)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			c.write(ctx, t, across.Member)
			asked := leaderReads(t, across, positionsStream)
			if c.behind > 0 {
				across.Link.Delay(c.behind)
			}
			if err := c.read(ctx, across.Leaf); err != nil {
				t.Fatalf("read %s at its bound across the link: %v", c.name, err)
			}
			if n := asked(); n != c.asks {
				t.Errorf("reading %s at its bound across the link asked the leader %d times, want %d",
					c.name, n, c.asks)
			}
		})
	}
}

// found1 is a read's answer as a case judges it: found, and no error.
func found1(found bool, err error) error {
	if err == nil && !found {
		return fmt.Errorf("the value written was not found")
	}
	return err
}

// linkDigest names the definition the cases store.
var linkDigest = strings.Repeat("d", 64)

func putListing(ctx context.Context, t *testing.T, member *kv.FleetStore) {
	t.Helper()
	const tools = 600
	words := make([]string, tools)
	for i := range words {
		words[i] = randomText(t, coord.MaxMCPToolDescriptionBytes)
	}
	unknown := coord.MCPToolHints{ReadOnly: coord.HintUnknown, Destructive: coord.HintUnknown,
		Idempotent: coord.HintUnknown, OpenWorld: coord.HintUnknown}
	part := strings.Repeat("e", coord.MCPToolPartLength)
	at := time.Now().UTC() // one instant: see widest
	l := widest(t, coord.MaxMCPToolDescriptionBytes, coord.MaxMCPListingBytes, func(n int) coord.MCPListing {
		l := coord.MCPListing{Server: "search", Node: "node-a", Epoch: 1, At: at, Instances: 1}
		for i := range tools {
			name := fmt.Sprintf("tool_%04d", i)
			l.Tools = append(l.Tools, coord.MCPListedTool{Name: name, Raw: name, Description: words[i][:n],
				Hints: unknown, Declared: unknown, Digest: linkDigest, Instances: 1,
				Parts: coord.MCPToolParts{Title: part, Description: part, Schema: part, Hints: part}})
		}
		return l
	}, coord.MCPListing.EncodedSize)
	if _, err := member.PutMCPListing(ctx, l); err != nil {
		t.Fatalf("put a listing at its bound: %v", err)
	}
}

func putDefinition(ctx context.Context, t *testing.T, member *kv.FleetStore) {
	t.Helper()
	frame := len(`{"d":""}`)
	d := coord.MCPToolDefinition{Digest: linkDigest, At: time.Now().UTC(),
		Description: randomText(t, coord.MaxMCPToolWordsBytes),
		Input:       json.RawMessage(`{"d":"` + randomText(t, coord.MaxMCPToolSchemaBytes-frame) + `"}`)}
	if created, err := member.CreateMCPToolDefinition(ctx, d); err != nil || !created {
		t.Fatalf("store a definition at its bound = %v, %v", created, err)
	}
}

func putCheck(ctx context.Context, t *testing.T, member *kv.FleetStore) {
	t.Helper()
	// A TEXT OF ITS OWN FOR EVERY FIELD, so no link compresses the record.
	const fields, steps = 13, 7
	texts := make([][]string, coord.MaxMCPCheckNodes)
	for i := range texts {
		for range fields {
			texts[i] = append(texts[i], randomText(t, coord.MaxMCPErrorBytes))
		}
	}
	at := time.Now().UTC() // one instant: see widest
	c := widest(t, coord.MaxMCPErrorBytes, coord.MaxMCPCheckBytes, func(n int) coord.MCPCheck {
		c := coord.MCPCheck{Server: "search", At: at, By: "jane-doe", ByKind: iam.ActorHuman}
		info := min(n, coord.MaxMCPServerInfoBytes)
		for i, text := range texts {
			node := coord.MCPCheckNode{Node: fmt.Sprintf("node-%02d", i), Answered: true, Reason: text[0][:n],
				Failure: &coord.MCPInstanceFailure{Class: "exited", Said: text[1][:n], At: at},
				Server: coord.MCPServerInfo{Name: text[2][:info], Title: text[3][:info], Version: text[4][:info],
					Protocol: text[5][:info]}}
			for j := range steps {
				node.Steps = append(node.Steps, coord.MCPCheckStep{Stage: "launch", State: "failed",
					Detail: text[6+j][:n]})
			}
			c.Nodes = append(c.Nodes, node)
		}
		return c
	}, coord.MCPCheck.EncodedSize)
	if _, err := member.PutMCPCheck(ctx, c); err != nil {
		t.Fatalf("put a check at its bound: %v", err)
	}
}

func putAccessReview(ctx context.Context, t *testing.T, member *kv.FleetStore) {
	t.Helper()
	value := []byte(`"` + randomText(t, coord.MaxAccessReviewBytes-2) + `"`)
	if created, err := member.CreateAccessReview(ctx, "sre", value); err != nil || !created {
		t.Fatalf("create an access review at its bound = %v, %v", created, err)
	}
}

func putSeatAccess(ctx context.Context, t *testing.T, member *kv.FleetStore) {
	t.Helper()
	value := []byte(`"` + randomText(t, coord.MaxSeatAccessBytes-2) + `"`)
	if err := member.PutSeatAccessRecord(ctx, "gitlab", "sre", value); err != nil {
		t.Fatalf("put a seat's access record at its bound: %v", err)
	}
}

func putHomeBoard(ctx context.Context, t *testing.T, member *kv.FleetStore) {
	t.Helper()
	frame := len(`{"widgets":[],"pad":""}`)
	board := json.RawMessage(`{"widgets":[],"pad":"` + randomText(t, coord.MaxHomeBoardBytes-frame) + `"}`)
	if _, saved, err := member.SaveHomeBoard(ctx, "founder", board, time.Now(), 0); err != nil || !saved {
		t.Fatalf("save a Home board at its bound = %v, %v", saved, err)
	}
}
