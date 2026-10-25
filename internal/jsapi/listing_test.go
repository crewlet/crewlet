package jsapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
)

// fakeAPI is a broker with no JetStream at all, where the case itself answers
// one API subject: every page a listing asks for, when, and whether at all.
// A real leader cannot be made to answer one page late and the next on time,
// to cut a page of three, or to name a consumer missing, and those are the
// answers the listings are written against.
type fakeAPI struct {
	js jetstream.JetStream

	mu sync.Mutex
	// asked is every request the fake was sent, in order.
	asked []json.RawMessage
}

// fakeBroker starts the broker and has answer reply to every request on
// subject — on a goroutine of each request's own, so a slow answer holds no
// other — with the reply it returns, or with nothing where it returns nil.
func fakeBroker(t *testing.T, subject string, answer func(n int, req []byte) any) *fakeAPI {
	t.Helper()
	srv, err := server.NewServer(&server.Options{
		ServerName: "jsapi-listing-test", DontListen: true, NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatalf("configure the broker: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(30 * time.Second) {
		srv.Shutdown()
		t.Fatal("the broker did not become ready")
	}
	t.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	connect := func() *nats.Conn {
		nc, err := nats.Connect("", nats.InProcessServer(srv))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(nc.Close)
		return nc
	}
	js, err := jsapi.Account().Client(connect())
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAPI{js: js}
	var answering sync.WaitGroup
	t.Cleanup(answering.Wait)
	responder := connect()
	if _, err := responder.Subscribe(subject, func(msg *nats.Msg) {
		f.mu.Lock()
		f.asked = append(f.asked, slices.Clone(msg.Data))
		n := len(f.asked)
		f.mu.Unlock()
		answering.Go(func() {
			reply := answer(n, msg.Data)
			if reply == nil {
				return
			}
			data, err := json.Marshal(reply)
			if err != nil {
				t.Errorf("encode the fake's answer: %v", err)
				return
			}
			_ = msg.Respond(data)
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := responder.Flush(); err != nil {
		t.Fatal(err)
	}
	return f
}

// offsets is the page each request the fake was sent asked for.
func (f *fakeAPI) offsets(t *testing.T) []int {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, len(f.asked))
	for i, req := range f.asked {
		var paged server.ApiPagedRequest
		if err := json.Unmarshal(req, &paged); err != nil {
			t.Fatalf("decode request %d: %v", i, err)
		}
		out[i] = paged.Offset
	}
	return out
}

// offsetOf is the page req asks for.
func offsetOf(req []byte) int {
	var paged server.ApiPagedRequest
	_ = json.Unmarshal(req, &paged)
	return paged.Offset
}

// subjectsPage is the leader's answer to a subject listing of list from
// offset, size subjects a page.
func subjectsPage(list []string, offset, size int) server.JSApiStreamInfoResponse {
	page := map[string]uint64{}
	for _, subject := range list[min(offset, len(list)):min(offset+size, len(list))] {
		page[subject] = 1
	}
	return server.JSApiStreamInfoResponse{
		ApiResponse: server.ApiResponse{Type: server.JSApiStreamInfoResponseType},
		ApiPaged:    server.ApiPaged{Total: len(list), Offset: offset, Limit: size},
		StreamInfo: &server.StreamInfo{
			Config:  server.StreamConfig{Name: "LOG", Storage: server.FileStorage},
			State:   server.StreamState{Subjects: page},
			Cluster: &server.ClusterInfo{Leader: "leader"},
		},
	}
}

// listSubjects lists every subject of LOG under log.> through f, a page at a
// time, and answers them in the order visited.
func listSubjects(ctx context.Context, t *testing.T, f *fakeAPI) ([]string, error) {
	t.Helper()
	leader, err := jsapi.NewLeader(f.js, "LOG")
	if err != nil {
		t.Fatal(err)
	}
	return listWith(ctx, leader)
}

// listWith lists every subject of LOG under log.> through leader.
func listWith(ctx context.Context, leader *jsapi.Leader) ([]string, error) {
	var got []string
	err := leader.Subjects(ctx, "log.>", func(page map[string]uint64) error {
		for subject := range page {
			got = append(got, subject)
		}
		return nil
	})
	slices.Sort(got)
	return got, err
}

// A PAGE A MEMBER DROPS IS ASKED AGAIN, AND ONLY THAT PAGE.
//
// Only a stream's leader answers its subject listing, a page at a time, and a
// member that still believes in a leader its group has just lost drops a page
// without a word. The listing was asked whole as one read: a dropped page —
// or merely a slow one, every page of a large listing being a sort of every
// subject the stream holds — started it again from its first page, every
// earlier request still running on the leader. Asked a page at a time, a
// dropped page costs that page.
func TestASubjectListingAsksAgainOnlyThePageNobodyAnswered(t *testing.T) {
	t.Parallel()
	list := []string{"log.a", "log.b", "log.c", "log.d", "log.e", "log.f", "log.g"}
	subject := fmt.Sprintf(server.JSApiStreamInfoT, "LOG")
	var dropped sync.Once
	f := fakeBroker(t, subject, func(_ int, req []byte) any {
		offset := offsetOf(req)
		answer := true
		if offset > 0 {
			dropped.Do(func() { answer = false })
		}
		if !answer {
			return nil
		}
		return subjectsPage(list, offset, 3)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 4*(jsprovision.ReadTerm+jsprovision.ReAsk))
	defer cancel()
	got, err := listSubjects(ctx, t, f)
	if err != nil {
		t.Fatalf("a listing whose second page was dropped once failed: %v", err)
	}
	if !slices.Equal(got, list) {
		t.Errorf("listed %v, want %v", got, list)
	}
	if offsets, want := f.offsets(t), []int{0, 2, 2, 4}; !slices.Equal(offsets, want) {
		t.Errorf("the listing asked pages from %v, want %v: the dropped page asked "+
			"again, and no page before it", offsets, want)
	}
}

// A PAGE IS GIVEN WHAT THE PAGE BEFORE IT TOOK.
//
// The leader sorts every subject under the filter to cut each page, so each
// page of a listing costs what the whole set does: a page that takes longer
// than the read term was presumed lost and asked again while the leader was
// still sorting for it, and the same work was asked of it twice. A reader's
// first page ever cannot know, and is asked again at the term; every page
// after it — the next listing's first included — is the same work, so it is
// given the read term beside what the one before took.
func TestALaterPageIsGivenWhatThePageBeforeItTook(t *testing.T) {
	t.Parallel()
	list := []string{"log.a", "log.b", "log.c", "log.d"}
	const slow = 2*jsprovision.ReadTerm + jsprovision.ReadTerm/2 // past a term and a re-ask
	subject := fmt.Sprintf(server.JSApiStreamInfoT, "LOG")
	f := fakeBroker(t, subject, func(_ int, req []byte) any {
		time.Sleep(slow)
		return subjectsPage(list, offsetOf(req), 3)
	})
	leader, err := jsapi.NewLeader(f.js, "LOG")
	if err != nil {
		t.Fatal(err)
	}
	for listing := range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), 4*slow)
		got, err := listWith(ctx, leader)
		cancel()
		if err != nil {
			t.Fatalf("slow listing %d failed: %v", listing, err)
		}
		if !slices.Equal(got, list) {
			t.Errorf("listing %d listed %v, want %v", listing, got, list)
		}
	}
	firsts, later := 0, 0
	for _, offset := range f.offsets(t) {
		if offset == 0 {
			firsts++
		} else {
			later++
		}
	}
	// The first listing's first page asked twice — a term and a re-ask
	// before its answer — and every other page once.
	if firsts != 3 || later != 2 {
		t.Errorf("first pages were asked %d times and second pages %d, want 3 and 2: "+
			"every page but the reader's first is the same work as the one before, "+
			"which took %v, and was asked again before that", firsts, later, slow)
	}
}

// A PAGE A MEMBER ANSWERED WITHOUT A LEADER IS NO PAGE.
//
// A stream's info is the leader's except while its group has none, when every
// member answers from its own copy — one that had not applied the last writes
// names less than the quorum holds. A listing during an election is the third
// answer, never a short one.
func TestASubjectPageAnsweredWithoutALeaderIsRefused(t *testing.T) {
	t.Parallel()
	subject := fmt.Sprintf(server.JSApiStreamInfoT, "LOG")
	f := fakeBroker(t, subject, func(_ int, req []byte) any {
		page := subjectsPage([]string{"log.a"}, offsetOf(req), 3)
		page.Cluster.Leader = ""
		return page
	})
	if _, err := listSubjects(t.Context(), t, f); !errors.Is(err, jsapi.ErrLeaderless) {
		t.Fatalf("a page answered with no leader = %v, want jsapi.ErrLeaderless", err)
	}
}

// THE STREAM NAMES ARE ASKED A PAGE AT A TIME, a dropped page asked again —
// only the metadata leader answers them, and the backup and the mailbox sweep
// list them — and every stream there throughout is named once.
func TestTheStreamNamesAreAskedAPageAtATime(t *testing.T) {
	t.Parallel()
	names := []string{"A", "B", "C", "D", "E", "F", "G"}
	var mu sync.Mutex
	current := slices.Clone(names)
	pages := 0
	var dropped sync.Once
	f := fakeBroker(t, server.JSApiStreams, func(_ int, req []byte) any {
		offset := offsetOf(req)
		answer := true
		if offset > 0 {
			dropped.Do(func() { answer = false })
		}
		if !answer {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		list := slices.Clone(current)
		if pages++; pages == 1 {
			// TWO STREAMS DELETED behind the first page: offset paging
			// would step over the two that move back across the
			// boundary.
			current = current[2:]
		}
		return server.JSApiStreamNamesResponse{
			ApiResponse: server.ApiResponse{Type: server.JSApiStreamNamesResponseType},
			ApiPaged:    server.ApiPaged{Total: len(list), Offset: offset, Limit: 3},
			Streams:     list[min(offset, len(list)):min(offset+3, len(list))],
		}
	})
	meta, err := jsapi.NewMeta(f.js)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*(jsprovision.ReadTerm+jsprovision.ReAsk))
	defer cancel()
	var got []string
	if err := meta.StreamNames(ctx, func(name string) error {
		got = append(got, name)
		return nil
	}); err != nil {
		t.Fatalf("a listing whose second page was dropped once failed: %v", err)
	}
	if !slices.Equal(got, names) {
		t.Errorf("named %v, want %v once each, in order: the first page's and every "+
			"stream there throughout", got, names)
	}
	if offsets := f.offsets(t); len(offsets) < 3 || offsets[0] != 0 || offsets[1] != offsets[2] {
		t.Errorf("the listing asked pages from %v, want the first once and the dropped "+
			"one asked again", offsets)
	}
}

// A CONSUMER LISTING SAYS WHAT EVERY CONSUMER IT COVERED IS, OR FAILS.
//
// The metadata leader gathers a page from every consumer's own leader and
// waits four seconds for them: a consumer whose leader did not answer is
// named missing and carries no info. The client's lister dropped it, and
// counted its page by the infos it carried — the consumer after it listed
// twice, the one it stood for not at all — and a mailbox sweep that read the
// listing judged a live mailbox absent. So an unread consumer fails the
// listing; one every peer of which is offline is listed from the leader's own
// record, as the leader lists it; and one a newer server made with what this
// one does not support is listed with the reason, and no info.
func TestAConsumerListingSaysWhatEveryConsumerIs(t *testing.T) {
	t.Parallel()
	subject := fmt.Sprintf(server.JSApiConsumerListT, "S")
	info := func(name string) *server.ConsumerInfo {
		return &server.ConsumerInfo{Stream: "S", Name: name,
			Config: &server.ConsumerConfig{Durable: name}}
	}
	page := func(consumers []*server.ConsumerInfo, missing []string, offline map[string]string) server.JSApiConsumerListResponse {
		return server.JSApiConsumerListResponse{
			ApiResponse: server.ApiResponse{Type: server.JSApiConsumerListResponseType},
			ApiPaged:    server.ApiPaged{Total: 4, Offset: 0, Limit: 256},
			Consumers:   consumers, Missing: missing, Offline: offline,
		}
	}
	list := func(t *testing.T, answer any) ([]jsapi.Consumer, error) {
		t.Helper()
		f := fakeBroker(t, subject, func(int, []byte) any { return answer })
		meta, err := jsapi.NewMeta(f.js)
		if err != nil {
			t.Fatal(err)
		}
		var got []jsapi.Consumer
		err = meta.Consumers(t.Context(), "S", func(c jsapi.Consumer) error {
			got = append(got, c)
			return nil
		})
		return got, err
	}

	t.Run("listed_offline_and_missing_with_info", func(t *testing.T) {
		t.Parallel()
		got, err := list(t, page(
			[]*server.ConsumerInfo{info("a"), info("c"), info("d")},
			[]string{"b", "d"}, map[string]string{"b": "unsupported feature"}))
		if err != nil {
			t.Fatalf("Consumers: %v", err)
		}
		var names []string
		for _, c := range got {
			names = append(names, c.Name)
			switch {
			case c.Name == "b" && (c.Info != nil || c.Offline != "unsupported feature"):
				t.Errorf("the offline consumer = %+v, want its reason and no info", c)
			case c.Name != "b" && (c.Info == nil || c.Info.Config.Durable != c.Name):
				t.Errorf("consumer %s = %+v, want its info", c.Name, c)
			}
		}
		if want := []string{"a", "b", "c", "d"}; !slices.Equal(names, want) {
			t.Errorf("listed %v, want %v once each, in order", names, want)
		}
	})
	t.Run("one_unread", func(t *testing.T) {
		t.Parallel()
		_, err := list(t, page(
			[]*server.ConsumerInfo{info("a"), info("c"), info("d")},
			[]string{"b"}, nil))
		var unread *jsapi.UnreadError
		if !errors.As(err, &unread) || !slices.Equal(unread.Consumers, []string{"b"}) {
			t.Fatalf("a page naming b missing = %v, want an UnreadError naming b", err)
		}
	})
	t.Run("no_stream", func(t *testing.T) {
		t.Parallel()
		_, err := list(t, server.JSApiConsumerListResponse{
			ApiResponse: server.ApiResponse{Type: server.JSApiConsumerListResponseType,
				Error: server.NewJSStreamNotFoundError()}})
		if !errors.Is(err, jsapi.ErrNoStream) {
			t.Fatalf("a stream the leader does not hold = %v, want jsapi.ErrNoStream", err)
		}
	})
}

// A READ WHOSE ANSWER IS A WHOLE PIECE IS GIVEN ITS CROSSING.
//
// The broker sends a message read's reply whole, and the client hears nothing
// until its last byte is in — so a piece crossing a link at the object
// store's floor takes seconds before the reader sees any of it, and presumed
// lost at the read term it was asked again, and again, each request another
// piece on the link the first was still crossing. Carrying a piece's size,
// the read is presumed lost only once a reply of that size has had time to
// cross; the plain read, the control, is asked again at the term.
func TestAReadOfAWholePieceIsGivenItsCrossing(t *testing.T) {
	t.Parallel()
	const piece = 128 << 10
	// THE TERM IS THE CROSSING: a reply of a piece base64-encoded and its
	// envelope, at the pace, beside the read term.
	plain := mustLeader(t, fakeBroker(t, "unused", func(int, []byte) any { return nil }).js)
	pace := 30 * time.Second
	want := jsprovision.ReadTerm + time.Duration(float64(pace)*float64(174764+1024)/float64(1<<20))
	if got := plain.Carrying(piece, pace).Term(); got != want {
		t.Errorf("a piece's term at 30 s a mebibyte = %v, want %v", got, want)
	}
	if got := plain.Term(); got != jsprovision.ReadTerm {
		t.Errorf("a plain read's term = %v, want the read term %v", got, jsprovision.ReadTerm)
	}

	// A reply that takes longer than a term and a re-ask, and less than a
	// piece's crossing at a pace that puts that crossing at about three
	// seconds.
	const slow = 2*jsprovision.ReadTerm + jsprovision.ReadTerm/2
	for _, tc := range []struct {
		name  string
		pace  time.Duration
		asked int
	}{
		{"carrying", 12 * time.Second, 1},
		{"control", 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			subject := fmt.Sprintf(server.JSApiMsgGetT, "OBJ")
			f := fakeBroker(t, subject, func(int, []byte) any {
				time.Sleep(slow)
				return server.JSApiMsgGetResponse{
					ApiResponse: server.ApiResponse{Type: server.JSApiMsgGetResponseType},
					Message:     &server.StoredMsg{Subject: "obj.c.x", Sequence: 1, Data: []byte("piece")},
				}
			})
			leader := mustLeader(t, f.js)
			if tc.pace > 0 {
				leader = leader.Carrying(piece, tc.pace)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 4*slow)
			defer cancel()
			if _, err := leader.First(ctx, "obj.c.x"); err != nil {
				t.Fatalf("First: %v", err)
			}
			if got := len(f.offsets(t)); got != tc.asked {
				t.Errorf("the read was asked %d times, want %d", got, tc.asked)
			}
		})
	}
}

// mustLeader is a leader reader of the stream OBJ through js.
func mustLeader(t *testing.T, js jetstream.JetStream) *jsapi.Leader {
	t.Helper()
	leader, err := jsapi.NewLeader(js, "OBJ")
	if err != nil {
		t.Fatal(err)
	}
	return leader
}
