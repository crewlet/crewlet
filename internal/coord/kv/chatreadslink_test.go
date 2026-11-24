package kv_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore"
)

// chatReadsStream is the chat reads bucket's stream under kvtest's prefix.
const chatReadsStream = "KV_slowleaf_chat_reads"

// chatReadsLinkRate is a link's pace for a record at the class's bound: above
// the floor, and slow enough that the widest record's reply (about 185 KB)
// outlasts a term read's second several times over, so a read given the term
// alone asks again or fails.
const chatReadsLinkRate = 48 << 10

// EVERY READ OF A SEAT'S CHAT READ RECORD AT ITS BOUND CROSSES A LEAF'S LINK
// AT ITS FIRST ASK. The widest record a writer makes
// (coordtest.WidestChatReadDelta), on a link at or above the slowest pace
// allowed, is read by its key and decided on by a flush each with one leader
// read, and a watch of the bucket, its consumer beating at the class's quiet,
// delivers it whole. Requests are counted as they reach the member.
//
// Mutation: read the class at the term (replyTerm), and the key's read and
// the flush ask again or fail, and the watch's consumer beats at 5 s.
func TestEveryChatReadAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk(t *testing.T) {
	t.Parallel()
	// ALL NINE DIGITS OF ITS NANOSECONDS: a record's instant is stored with
	// its trailing zeros cut, so a flush stamped by the clock stores one a
	// byte or more shorter, and its record is no longer the widest.
	at := time.Date(2026, 11, 22, 10, 0, 0, 123456789, time.UTC)
	for _, c := range []struct {
		name string
		// read reads the record through the leaf, answering it as stored.
		read func(ctx context.Context, t *testing.T, across kvtest.Across) coord.ChatReadState
		// asks is how many leader reads it makes: one for a key's read and
		// a flush's, none for a watch's pass that delivers it.
		asks int
	}{
		{"a seat's record read by its key", func(ctx context.Context, t *testing.T, across kvtest.Across) coord.ChatReadState {
			t.Helper()
			state, err := across.Leaf.ChatRead(ctx, "founder")
			if err != nil {
				t.Fatalf("read the record across the link: %v", err)
			}
			return state
		}, 1},
		{"a flush deciding on the record", func(ctx context.Context, t *testing.T, across kvtest.Across) coord.ChatReadState {
			t.Helper()
			// A cursor past every other: the stalest goes, and the record
			// stays at its widest.
			state, err := across.Leaf.AdvanceChatRead(ctx, "founder", coord.ChatReadDelta{
				Cursors: map[string]int64{uuid.NewString(): 1}, At: at.Add(time.Minute)})
			if err != nil {
				t.Fatalf("flush the record across the link: %v", err)
			}
			return state
		}, 1},
		{"a watch of the bucket", func(ctx context.Context, t *testing.T, across kvtest.Across) coord.ChatReadState {
			t.Helper()
			updates, err := across.Leaf.WatchChatReads(ctx)
			if err != nil {
				t.Fatalf("watch the records across the link: %v", err)
			}
			// THE FIGURES STATED: a pass's idle of 5 s, and the raw crossing
			// of the record at its bound and the most header a write carries,
			// 342 bytes, at the floor.
			want := 5*time.Second + jsapi.Sending(coord.MaxChatReadBytes+342, objstore.MiBPace)
			if beat := watchHeartbeat(ctx, t, across); beat != want {
				t.Errorf("the watch's consumer beats every %v, want the class's quiet %v", beat, want)
			}
			for u := range updates {
				if u.Handle == "founder" && u.State != nil {
					return *u.State
				}
			}
			t.Fatalf("the watch ended before it delivered the record: %v", context.Cause(ctx))
			return coord.ChatReadState{}
		}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if floor := int64(1<<20) * int64(time.Second) / int64(objstore.MiBPace); chatReadsLinkRate < floor {
				t.Fatalf("premise: the link carries %d bytes a second, below the floor's %d",
					chatReadsLinkRate, floor)
			}
			across := kvtest.SlowLeaf(t, chatReadsLinkRate)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			written, err := across.Member.AdvanceChatRead(ctx, "founder", coordtest.WidestChatReadDelta(at))
			if err != nil {
				t.Fatalf("flush the widest record through the member: %v", err)
			}
			if exists, err := across.Member.SetChatReadAbsence(ctx, "founder", at); err != nil || !exists {
				t.Fatalf("stamp the widest record absent = %v, %v", exists, err)
			}
			written, _ = written.WithAbsence(at)
			want, err := coord.EncodeChatRead("founder", written)
			if err != nil {
				t.Fatal(err)
			}
			if len(want) < coord.MaxChatReadBytes*9/10 {
				t.Fatalf("premise: the widest record is %d bytes, not near its bound %d", len(want),
					coord.MaxChatReadBytes)
			}
			asked := leaderReads(t, across, chatReadsStream)
			got, err := coord.EncodeChatRead("founder", c.read(ctx, t, across))
			if err != nil || len(got) != len(want) {
				t.Fatalf("%s across the link answered %d bytes (%v), want the %d written", c.name, len(got),
					err, len(want))
			}
			if c.asks == 0 && !bytes.Equal(got, want) {
				t.Errorf("the watch delivered a record other than the one written")
			}
			if n := asked(); n != c.asks {
				t.Errorf("%s at its bound across the link asked the leader %d times, want %d", c.name, n,
					c.asks)
			}
		})
	}
}

// watchHeartbeat is the heartbeat of the one consumer the chat reads stream
// holds, as the member's broker holds it.
func watchHeartbeat(ctx context.Context, t *testing.T, across kvtest.Across) time.Duration {
	t.Helper()
	nc, err := across.MemberBroker.Conn()
	if err != nil {
		t.Fatalf("connect to the member: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(ctx, chatReadsStream)
	if err != nil {
		t.Fatal(err)
	}
	var beats []time.Duration
	consumers := stream.ListConsumers(ctx)
	for info := range consumers.Info() {
		beats = append(beats, info.Config.IdleHeartbeat)
	}
	if err := consumers.Err(); err != nil || len(beats) != 1 {
		t.Fatalf("the chat reads stream's consumers beat at %v, %v: want the watch's alone", beats, err)
	}
	return beats[0]
}
