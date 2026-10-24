package host_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// ceoOperatorID is the operator id a log line names ceo's token by: its login,
// `token:ceo`, as every record names a credential (iam.ActorFor).
var ceoOperatorID = `"operator_id":"` + iam.TokenLogin("ceo") + `"`

// roomy is an allowance no case here fills unless it means to: the
// production's at a lane of sixty-four slots.
var roomy = host.SpoolSizesFor(64)

// sizes is roomy with the fields a case sets.
func sizes(set func(*host.SpoolSizes)) host.SpoolSizes {
	z := roomy
	set(&z)
	return z
}

// O19: BLOBLESS CHECKOUTS BEHIND ONE TOKEN ALL SUCCEED, however many more of
// them there are than the token's share of the spool holds at once. A checkout
// of a blobless clone asks for every blob it lacks in ONE lazy fetch, which
// git does not retry, and behind one CI token the fetch past the credential's
// share was answered 503 at once — so the checkout failed, under no abuse at
// all. Here a credential's share holds one such fetch and not two, four
// checkouts' lazy fetches arrive at once, and the first is held in its
// upload-pack — its body spooled whole, as through a cold sync — until the
// other three are seen WAITING for room rather than refused; then all four
// are served.
//
// Mutation: answer a request that must wait 503 at once (spools.start
// refusing rather than queueing) and the three are never seen waiting: they
// are refused, and their checkouts fail.
func TestBloblessCheckoutsBehindOneTokenAllSucceed(t *testing.T) {
	fake, hold, started := holdFirst(t)
	c := newCase(t, codetest.SoloOptions{GitPath: fake}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	// A KIBIBYTE IN MEMORY, so a checkout of a hundred files spools its
	// fetch — about five kibibytes of wants — without megabytes of fixture.
	host.SetRequestMemory(node.Host(), 1<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	files := map[string]string{}
	for i := range 100 {
		files[fmt.Sprintf("f%03d.txt", i)] = fmt.Sprintf("file %d\n", i)
	}
	head := p.Commit(work, files, "a tree whose checkout's lazy fetch spools")
	c.MustPush(p, work, "main")
	const checkouts = 4
	clones := make([]string, checkouts)
	for i := range clones {
		clones[i] = filepath.Join(t.TempDir(), "clone")
		if _, stderr, err := p.Git().Try(t, "", nil, "-c", "protocol.version=2", "clone", "--quiet",
			"--filter=blob:none", "--no-checkout", c.URL(0), clones[i]); err != nil {
			t.Fatalf("git clone --filter=blob:none: %v\n%s", err, stderr)
		}
	}
	// ONE FETCH'S WORTH OF SHARE: about five kibibytes of wants fit in eight,
	// and two do not — and git declares no length for a body it compresses,
	// so each claims the whole eight while it arrives.
	host.SetSpools(node.Host(), sizes(func(z *host.SpoolSizes) { z.Share, z.Request = 8<<10, 8<<10 }))
	armHold(t, hold)
	errs := make([]error, checkouts)
	var wg sync.WaitGroup
	for i, clone := range clones {
		wg.Go(func() {
			if _, stderr, err := p.Git().TryContext(c.Ctx, clone, nil, "-c", "protocol.version=2", "checkout",
				"--quiet", "main"); err != nil {
				errs[i] = fmt.Errorf("%w\n%s", err, stderr)
			}
		})
	}
	eventually(t, "one lazy fetch held in its upload-pack and three waiting for room", func() bool {
		_, err := os.Stat(started)
		_, open := host.SpoolsInUse(node.Host())
		return err == nil && open >= 1 && host.SpoolWaiting(node.Host()) == checkouts-1
	})
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("checkout %d of %d behind one token failed: %v", i+1, checkouts, err)
			continue
		}
		if got := p.Head(clones[i]); got != head {
			t.Errorf("checkout %d is at %s, want %s", i+1, got, head)
		}
		for rel, want := range files {
			if got, err := os.ReadFile(filepath.Join(clones[i], rel)); err != nil || string(got) != want {
				t.Errorf("checkout %d's %s is %q (%v), want %q", i+1, rel, got, err, want)
				break
			}
		}
	}
	if got := counted(node.Metrics(), metrics.CodeHostSpoolWaits, "granted"); got < checkouts-1 {
		t.Errorf("%s{outcome=granted} = %d, want at least the %d checkouts that waited",
			metrics.CodeHostSpoolWaits, got, checkouts-1)
	}
	assertNoSpool(t, c)
}

// A SPOOLED FETCH WAITS ITS TURN — for the room a body arriving claims, the
// node's or its credential's, or for room the spools that ended hold — and is
// served once that is given back, while another credential's request its own
// turn admits is served at once rather than behind the waiter; and a request
// past what this node spools for one request, which it can never take, is
// answered 413 at once, never made to wait.
func TestASpooledFetchWaitsItsTurn(t *testing.T) {
	fake, hold, started := holdFirst(t)
	c := newCase(t, codetest.SoloOptions{GitPath: fake}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	body := repeatedWants(head, 8<<10)
	room := int64(len(body)) + 2<<10

	// A CLAIM OF THE WHOLE: the first request's body, undeclared as git's
	// are, claims what one request spools — here the node's whole allowance,
	// or its credential's whole share — and takes its room a kibibyte at a
	// time, so the room is free and the claim is what holds the second back.
	claimed := int64(64 << 10)
	cases := []struct {
		name string
		z    host.SpoolSizes
		// arriving holds the first request's body part-way; otherwise its
		// body arrives whole and its upload-pack is held, its spool with it.
		arriving bool
		// other is whether another credential's request is served at once
		// while the second waits: true where the CREDENTIAL holds the second
		// back, false where the NODE does — whose turns go in arrival order.
		other bool
	}{
		{"the node's room a body arriving claims", host.SpoolSizes{Limit: claimed, Share: claimed,
			Request: claimed, Grain: 1 << 10, Places: 64, PlacesPerCredential: 64}, true, false},
		{"a credential's room a body arriving claims", sizes(func(z *host.SpoolSizes) {
			z.Share, z.Request, z.Grain = claimed, claimed, 1<<10
		}), true, true},
		{"the node's room", host.SpoolSizes{Limit: room, Share: room, Request: room, Places: 64,
			PlacesPerCredential: 64}, false, false},
		{"a credential's room", sizes(func(z *host.SpoolSizes) { z.Share, z.Request = room, room }), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host.SetSpools(node.Host(), tc.z)
			var first *open
			var firstDone <-chan reply
			if tc.arriving {
				first = openFetch(t, c, "ceo", body)
				eventually(t, "the first request's body arriving", func() bool {
					return host.SpoolsArriving(node.Host()) == 1
				})
			} else {
				rearm(t, hold, started)
				firstDone = fetchAsync(t, c, "ceo", body)
				eventually(t, "the first request's body spooled whole and its upload-pack held", func() bool {
					_, err := os.Stat(started)
					return err == nil && host.SpoolsArriving(node.Host()) == 0
				})
			}
			who := "ceo"
			if !tc.other {
				who = "dev"
			}
			second := fetchAsync(t, c, who, body)
			eventually(t, "the second request waiting", func() bool { return host.SpoolWaiting(node.Host()) == 1 })
			if tc.other {
				if a := <-fetchAsync(t, c, "dev", body); a.status != http.StatusOK {
					t.Errorf("another credential's request answered %d %q while ceo's waited", a.status,
						short(a.body))
				}
			}
			select {
			case a := <-second:
				t.Fatalf("the second request answered %d %q while the first held its turn", a.status, short(a.body))
			default:
			}
			if tc.arriving {
				if a := first.finish(); a.status != http.StatusOK {
					t.Errorf("the first request answered %d %q", a.status, short(a.body))
				}
			} else {
				if err := os.Remove(hold); err != nil {
					t.Fatal(err)
				}
				if a := <-firstDone; a.status != http.StatusOK {
					t.Errorf("the first request answered %d %q", a.status, short(a.body))
				}
			}
			if a := <-second; a.status != http.StatusOK || !strings.Contains(a.body, "packfile") {
				t.Errorf("the request that waited answered %d %q", a.status, short(a.body))
			}
			assertNoSpool(t, c)
		})
	}
	t.Run("past what one request spools", func(t *testing.T) {
		// THE NODE'S ROOM CLAIMED by another credential's request arriving,
		// so a request made to wait would be seen to.
		host.SetSpools(node.Host(), host.SpoolSizes{Limit: 32 << 10, Share: 32 << 10, Request: 32 << 10, Places: 64,
			PlacesPerCredential: 64})
		held := openFetch(t, c, "dev", repeatedWants(head, 16<<10))
		eventually(t, "the other credential's body arriving", func() bool {
			return host.SpoolsArriving(node.Host()) == 1
		})
		// Built before the clock starts, so what is timed is the node's
		// answer and never the case's own request.
		past := repeatedWants(head, 80<<10)
		start := time.Now()
		resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, past)
		if resp.StatusCode != http.StatusRequestEntityTooLarge ||
			!strings.Contains(answer, "larger than this node spools for one request") ||
			!strings.Contains(answer, "a sparse checkout asks only for the files it checks out") {
			t.Errorf("a request past what one request spools answered %d %q", resp.StatusCode, answer)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("a request this node can never take was answered after %v, not at once", took)
		}
		held.abandon()
		assertNoSpool(t, c)
	})
}

// A WAIT PAST ITS BOUND IS 503 WITH A RETRY-AFTER, saying what held it back
// and what to do about a token behind several runners; it is logged with the
// credential, counted `expired`, and gives its place back.
func TestAWaitPastItsBoundIs503(t *testing.T) {
	var lines logs
	timing := fast
	timing.SpoolWait = 300 * time.Millisecond
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{Timing: timing}, Logger: lines.logger()},
		codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	body := repeatedWants(head, 8<<10)
	host.SetSpools(node.Host(), sizes(func(z *host.SpoolSizes) { z.Share, z.Request, z.Grain = 64<<10, 64<<10, 1<<10 }))
	first := openFetch(t, c, "ceo", body)
	eventually(t, "the first request's body arriving", func() bool { return host.SpoolsArriving(node.Host()) == 1 })
	start := time.Now()
	resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, body)
	took := time.Since(start)
	if resp.StatusCode != http.StatusServiceUnavailable ||
		resp.Header.Get("Retry-After") != strconv.Itoa(int(host.BusyRetryAfter/time.Second)) ||
		!strings.Contains(answer, "this node's room for large fetch requests is in use: this fetch waited") ||
		!strings.Contains(answer, "behind this credential's own requests") ||
		!strings.Contains(answer, "give each CI runner a token of its own") {
		t.Errorf("a wait past its bound answered %d %q (Retry-After %q)", resp.StatusCode, answer,
			resp.Header.Get("Retry-After"))
	}
	if took < timing.SpoolWait {
		t.Errorf("the wait was answered after %v, before its bound of %v", took, timing.SpoolWait)
	}
	if n := lines.count("code_fetch_spool_unserved", `"reason":"expired"`, ceoOperatorID); n != 1 {
		t.Errorf("logged code_fetch_spool_unserved for the expired wait %d times, want once", n)
	}
	if got := counted(node.Metrics(), metrics.CodeHostSpoolWaits, "expired"); got != 1 {
		t.Errorf("%s{outcome=expired} = %d, want 1", metrics.CodeHostSpoolWaits, got)
	}
	if n, _ := observed(node.Metrics(), metrics.CodeHostSpoolWait, map[string]string{"outcome": "expired"}); n != 1 {
		t.Errorf("%s{outcome=expired} observed %d waits, want 1", metrics.CodeHostSpoolWait, n)
	}
	if n, p := host.SpoolWaiting(node.Host()), host.SpoolPlaces(node.Host()); n != 0 || p != 0 {
		t.Errorf("%d requests still wait and %d places are held after the only waiter was answered", n, p)
	}
	if a := first.finish(); a.status != http.StatusOK {
		t.Errorf("the first request answered %d %q", a.status, short(a.body))
	}
	assertNoSpool(t, c)
}

// A REQUEST THAT MUST WAIT AND FINDS NO PLACE IS REFUSED AT ONCE — its
// credential's quarter of the waiting room taken, or the room full — answered
// 503 and counted `refused`, while one that finds a place waits; and a request
// that need not wait is served at once whatever fills the room, since it takes
// no place.
func TestAWaiterWithNoPlaceIsRefusedAtOnce(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	body := repeatedWants(head, 8<<10)

	t.Run("a credential's", func(t *testing.T) {
		host.SetSpools(node.Host(), sizes(func(z *host.SpoolSizes) {
			z.Share, z.Request, z.Grain, z.PlacesPerCredential = 64<<10, 64<<10, 1<<10, 1
		}))
		first := openFetch(t, c, "ceo", body)
		eventually(t, "the first request's body arriving", func() bool {
			return host.SpoolsArriving(node.Host()) == 1
		})
		second := fetchAsync(t, c, "ceo", body)
		eventually(t, "the second waiting", func() bool { return host.SpoolWaiting(node.Host()) == 1 })
		resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, body)
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" ||
			!strings.Contains(answer, "this credential already has 1 fetches waiting to spool") ||
			!strings.Contains(answer, "give each CI runner a token of its own") {
			t.Errorf("a credential's third request with its one place taken answered %d %q", resp.StatusCode,
				answer)
		}
		// ANOTHER CREDENTIAL'S request starts at once beside them.
		if a := <-fetchAsync(t, c, "dev", body); a.status != http.StatusOK {
			t.Errorf("another credential's request answered %d %q", a.status, short(a.body))
		}
		if a := first.finish(); a.status != http.StatusOK {
			t.Errorf("the first answered %d %q", a.status, short(a.body))
		}
		if a := <-second; a.status != http.StatusOK {
			t.Errorf("the one that waited answered %d %q", a.status, short(a.body))
		}
		assertNoSpool(t, c)
	})
	t.Run("the node's", func(t *testing.T) {
		host.SetSpools(node.Host(), sizes(func(z *host.SpoolSizes) {
			z.Share, z.Request, z.Grain, z.Places, z.PlacesPerCredential = 64<<10, 64<<10, 1<<10, 2, 2
		}))
		first := openFetch(t, c, "ceo", body)
		eventually(t, "the first request's body arriving", func() bool {
			return host.SpoolsArriving(node.Host()) == 1
		})
		var waiters []<-chan reply
		for i := range 2 {
			waiters = append(waiters, fetchAsync(t, c, "ceo", body))
			eventually(t, "a request waiting", func() bool { return host.SpoolWaiting(node.Host()) == i+1 })
		}
		// A REQUEST THAT CAN START IS SERVED beside the full room: it takes
		// no place.
		if a := <-fetchAsync(t, c, "dev", body); a.status != http.StatusOK {
			t.Errorf("a request that could start beside a full waiting room answered %d %q", a.status,
				short(a.body))
		}
		// ONE THAT MUST WAIT is refused: dev's own body arriving, and the room
		// full.
		devFirst := openFetch(t, c, "dev", body)
		eventually(t, "dev's body arriving", func() bool { return host.SpoolsArriving(node.Host()) == 2 })
		resp, answer := post(t, c, 0, "dev", "git-upload-pack", v2, body)
		if resp.StatusCode != http.StatusServiceUnavailable ||
			!strings.Contains(answer, "2 fetches already wait to spool, as many as this node keeps waiting") {
			t.Errorf("a request that must wait beside a full waiting room answered %d %q", resp.StatusCode, answer)
		}
		for _, o := range []*open{first, devFirst} {
			if a := o.finish(); a.status != http.StatusOK {
				t.Errorf("a first answered %d %q", a.status, short(a.body))
			}
		}
		for _, w := range waiters {
			if a := <-w; a.status != http.StatusOK {
				t.Errorf("a request that waited answered %d %q", a.status, short(a.body))
			}
		}
		assertNoSpool(t, c)
	})
	if got := counted(node.Metrics(), metrics.CodeHostSpoolWaits, "refused"); got != 2 {
		t.Errorf("%s{outcome=refused} = %d, want 2", metrics.CodeHostSpoolWaits, got)
	}
}

// A WAITER WHOSE CLIENT LEAVES GIVES UP ITS PLACE AT ONCE, where the transport
// says the client left: over HTTP/2 the client's reset ends the request's
// context while the waiter reads nothing, and the request behind it starts as
// soon as the turn it waited behind comes, without waiting for the wait's
// bound. (Over HTTP/1.1 net/http learns that a client hung up only by reading
// its connection, which a waiter does not — spool.go.)
func TestAWaiterWhoseClientLeavesGivesUpItsPlace(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	body := repeatedWants(head, 8<<10)
	host.SetSpools(node.Host(), host.SpoolSizes{Limit: 64 << 10, Share: 64 << 10, Request: 64 << 10, Grain: 1 << 10,
		Places: 64, PlacesPerCredential: 64})
	// THE HOST'S FETCH ROUTE OVER HTTP/2, admitting every request as ceo —
	// the principal the API's guard resolves ceo's token to: the token's
	// login, acting as the human seat the identity directory binds it to.
	ceo := iam.Principal{Login: iam.TokenLogin("ceo"), Kind: iam.KindPerson, Seat: "ceo",
		Stage: iam.StageActive}
	mux := http.NewServeMux()
	mux.Handle(host.PatternUploadPack, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node.Host().UploadPack().ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), ceo)))
	}))
	h2 := httptest.NewUnstartedServer(mux)
	h2.EnableHTTP2 = true
	h2.StartTLS()
	t.Cleanup(h2.Close)
	send := func(ctx context.Context, payload io.Reader) <-chan reply {
		out := make(chan reply, 1)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h2.URL+"/git/"+codetest.RepoName+
			"/git-upload-pack", payload)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
		req.Header.Set("Git-Protocol", v2)
		go func() { out <- replyOf(h2.Client().Do(req)) }()
		return out
	}
	held, release := io.Pipe()
	first := send(c.Ctx, held)
	if _, err := io.WriteString(release, body[:len(body)-4]); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first request's body arriving", func() bool { return host.SpoolsArriving(node.Host()) == 1 })
	leaving, leave := context.WithCancel(c.Ctx)
	gone := send(leaving, strings.NewReader(body))
	eventually(t, "the leaving request waiting", func() bool { return host.SpoolWaiting(node.Host()) == 1 })
	behind := send(c.Ctx, strings.NewReader(body))
	eventually(t, "a request waiting behind it", func() bool { return host.SpoolWaiting(node.Host()) == 2 })
	leave()
	<-gone
	eventually(t, "the place given up", func() bool {
		return host.SpoolWaiting(node.Host()) == 1 && host.SpoolPlaces(node.Host()) == 1
	})
	if got := counted(node.Metrics(), metrics.CodeHostSpoolWaits, "abandoned"); got != 1 {
		t.Errorf("%s{outcome=abandoned} = %d, want 1", metrics.CodeHostSpoolWaits, got)
	}
	_, _ = io.WriteString(release, body[len(body)-4:])
	_ = release.Close()
	for i, ch := range []<-chan reply{first, behind} {
		if a := <-ch; a.err != nil || a.status != http.StatusOK {
			t.Errorf("request %d answered %d %q (%v)", i+1, a.status, short(a.body), a.err)
		}
	}
	assertNoSpool(t, c)
}

// A SPOOL HOLDS WHAT ITS CLIENT SENT, AND NO MEMORY: held in upload-pack before
// its first byte, a spooled request's spool holds its whole body — the part
// first read into memory moved in beside the rest — its request memory is let
// go, and its body no longer counts among those arriving, so it holds its
// credential's turn no longer than its body took to arrive.
//
// Mutation: keep the head in memory once spooled (body.go's move) and the
// request memory is held; reserve the repository's bound (start asked for
// it) and the spool holds that until its body ends.
func TestASpoolHoldsWhatItsClientSentAndNoMemory(t *testing.T) {
	fake, hold, started := holdFirst(t)
	c := newCase(t, codetest.SoloOptions{GitPath: fake}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	body := repeatedWants(head, 8<<10)
	armHold(t, hold)
	done := fetchAsync(t, c, "ceo", body)
	want := int64(len(body))
	var bytes int64
	var open int
	eventually(t, "the request held in its upload-pack, its spool what it spooled", func() bool {
		_, err := os.Stat(started)
		bytes, open = host.SpoolsInUse(node.Host())
		return err == nil && bytes == want && open == 1
	})
	if n := host.SpoolsArriving(node.Host()); n != 0 {
		t.Errorf("%d bodies count as arriving after the only one arrived whole", n)
	}
	if held := host.RequestMemoryInUse(node.Host()); held != 0 {
		t.Errorf("the request memory holds %d bytes for a request whose body is in its spool", held)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	if a := <-done; a.status != http.StatusOK {
		t.Errorf("the request answered %d %q", a.status, short(a.body))
	}
	assertNoSpool(t, c)
}

// A FETCH HELD PAST THE WAIT DOES NOT HOLD ITS CREDENTIAL'S TURN: a spooled
// request whose body has arrived waits for its sync, its transfer slot and
// upload-pack's read — a cold sync is a minute or two — and counted among its
// credential's bodies until then, the requests behind it waited out their
// bound and were refused, a checkout each. Here the first request's body,
// undeclared as git's are, claims its credential's whole share while it
// arrives; its upload-pack is held well past the wait's bound, and the
// request behind it is served all the same.
//
// Mutation: keep a body among those arriving until its spool is given back
// (leaveLocked in release only) and the second request is answered 503 when
// its wait expires.
func TestAFetchHeldPastTheWaitDoesNotHoldItsCredentialsTurn(t *testing.T) {
	fake, hold, started := holdFirst(t)
	timing := fast
	timing.SpoolWait = 300 * time.Millisecond
	c := newCase(t, codetest.SoloOptions{GitPath: fake, Shape: codetest.Shape{Timing: timing}}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	body := repeatedWants(head, 8<<10)
	host.SetSpools(node.Host(), sizes(func(z *host.SpoolSizes) { z.Share, z.Request, z.Grain = 64<<10, 64<<10, 1<<10 }))
	armHold(t, hold)
	first := fetchUndeclared(t, c, "ceo", body)
	eventually(t, "the first request held in its upload-pack", func() bool {
		_, err := os.Stat(started)
		return err == nil
	})
	start := time.Now()
	if a := <-fetchAsync(t, c, "ceo", body); a.status != http.StatusOK || !strings.Contains(a.body, "packfile") {
		t.Errorf("the request behind a held one answered %d %q after %v", a.status, short(a.body),
			time.Since(start))
	}
	if held := time.Since(start); held < 2*timing.SpoolWait {
		time.Sleep(2*timing.SpoolWait - held)
	}
	select {
	case a := <-first:
		t.Fatalf("the first request was answered (%d) before it was released: the case proves nothing", a.status)
	default:
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	if a := <-first; a.status != http.StatusOK {
		t.Errorf("the first request answered %d %q", a.status, short(a.body))
	}
	assertNoSpool(t, c)
}

// A CREDENTIAL'S ENDED SPOOLS OUTLASTING THE WAIT REFUSE WHAT WAITS BEHIND
// THEM — the one cost of the share a token per runner removes: a spool holds
// its room until upload-pack has read it, so a request behind its own
// credential's spools that ended waits through their syncs, and a cold sync
// outlasts the wait. Here ceo's first request has spooled most of its
// credential's share and is held in its upload-pack past the wait's bound, as
// through a cold sync; ceo's next is answered 503 when its wait expires,
// naming the bytes its credential's spools hold, while dev's is served at
// once — so a token behind more lazy fetches than its quarter holds fails the
// excess on a cold node, which the pages say in figures.
//
// Mutation: charge an ended spool's bytes to nobody's share (giveLocked in
// ended for the whole spool) and ceo's next is served while the first is
// held.
func TestACredentialsEndedSpoolsOutlastingTheWait(t *testing.T) {
	fake, hold, started := holdFirst(t)
	var lines logs
	timing := fast
	timing.SpoolWait = 300 * time.Millisecond
	c := newCase(t, codetest.SoloOptions{GitPath: fake, Shape: codetest.Shape{Timing: timing},
		Logger: lines.logger()}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	body := repeatedWants(head, 8<<10)
	share := int64(len(body)) + 4<<10
	host.SetSpools(node.Host(), sizes(func(z *host.SpoolSizes) { z.Share, z.Request = share, share }))
	armHold(t, hold)
	first := fetchAsync(t, c, "ceo", body)
	eventually(t, "the first request spooled whole and held in its upload-pack", func() bool {
		_, err := os.Stat(started)
		return err == nil && host.SpoolsArriving(node.Host()) == 0
	})
	start := time.Now()
	resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, body)
	took := time.Since(start)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" ||
		!strings.Contains(answer, "behind this credential's own spooled requests (they hold") ||
		!strings.Contains(answer, fmt.Sprintf("of the %d bytes one credential's may", share)) ||
		!strings.Contains(answer, "give each CI runner a token of its own") {
		t.Errorf("a request behind its credential's held spool answered %d %q (Retry-After %q)", resp.StatusCode,
			answer, resp.Header.Get("Retry-After"))
	}
	if took < timing.SpoolWait {
		t.Errorf("it was answered after %v, before the wait's bound of %v", took, timing.SpoolWait)
	}
	if n := lines.count("code_fetch_spool_unserved", `"reason":"expired"`, ceoOperatorID); n != 1 {
		t.Errorf("logged code_fetch_spool_unserved for the expired wait %d times, want once", n)
	}
	if a := <-fetchAsync(t, c, "dev", body); a.status != http.StatusOK {
		t.Errorf("another credential's request beside the held spool answered %d %q", a.status, short(a.body))
	}
	select {
	case a := <-first:
		t.Fatalf("the first request was answered (%d) before it was released: the case proves nothing", a.status)
	default:
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	if a := <-first; a.status != http.StatusOK {
		t.Errorf("the first request answered %d %q", a.status, short(a.body))
	}
	assertNoSpool(t, c)
}

// O19, AND A REPOSITORY PAST THE SHARE: a request to a repository whose bound —
// fifty bytes an object it holds — is far past its credential's share claims
// what one request spools and holds what it sent, so a credential's requests
// to it spool as many at once as their claims fit beside each other, and the
// node's as many as it takes, whoever sends them. Reserved by the bound, each
// held a whole share while its body arrived, and one credential's arrived one
// at a time.
//
// Mutation: claim a body's repository bound (start handed RequestBound for
// its claim) and the first is refused 413, past what one request spools.
func TestARepositoryPastTheShareSpoolsAsManyAsTheNodeTakes(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	z := host.SpoolSizes{Limit: 32 << 10, Share: 16 << 10, Request: 4 << 10, Places: 64, PlacesPerCredential: 16}
	if bound := host.RequestBound(c.Rows().Repo); bound <= z.Share {
		t.Fatalf("the repository's bound %d is not past the share %d: the case proves nothing", bound, z.Share)
	}
	host.SetSpools(node.Host(), z)
	body := repeatedWants(head, 3<<10)
	var opens []*open
	for i, who := range []string{"ceo", "ceo", "ceo", "ceo", "dev", "dev", "dev", "dev"} {
		opens = append(opens, openFetch(t, c, who, body))
		eventually(t, fmt.Sprintf("%d bodies arriving at once", i+1), func() bool {
			return host.SpoolsArriving(node.Host()) == i+1 && host.SpoolWaiting(node.Host()) == 0
		})
	}
	for i, o := range opens {
		if a := o.finish(); a.status != http.StatusOK {
			t.Errorf("request %d answered %d %q", i+1, a.status, short(a.body))
		}
	}
	assertNoSpool(t, c)
}

// SLOW UPLOADS HOLD WHAT THEY SENT, NEVER A TURN: on a four-core node's own
// allowance, twenty requests of four credentials whose bodies are arriving —
// more than the lane is wide, and five of each credential, more than a
// quarter of it — every one sent but its last bytes, as a client at the pace
// floor holds one for half an hour, all spool at once, and a fifth
// credential's request is served beside them. Counted as a whole request each
// while it arrived, sixteen such uploads held every turn the node had and the
// requests behind them waited out their bound; held to what they sent, they
// hold a few mebibytes of the node's gibibyte, and the oldest's claim is the
// one room kept from the rest.
//
// Mutation: charge every body arriving its whole claim in the room the rest
// may take (boundsLocked reading b.claim for b.held) and ceo's fifth waits
// for its credential's first four, the seventeenth for the node's first
// sixteen.
func TestSlowUploadsHoldWhatTheySentNeverATurn(t *testing.T) {
	timing := fast
	timing.SpoolWait = 2 * time.Second
	people := []string{"ceo", "dev", "p3", "p4", "p5"}
	c := newCase(t, codetest.SoloOptions{People: people, Shape: codetest.Shape{Timing: timing}},
		codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	host.SetSpools(node.Host(), host.SpoolSizesFor(16))
	body := repeatedWants(head, 8<<10)
	var opens []*open
	for i := range 20 {
		who := people[i%4]
		opens = append(opens, openFetch(t, c, who, body))
		eventually(t, fmt.Sprintf("%d bodies arriving at once, none waiting", i+1), func() bool {
			return host.SpoolsArriving(node.Host()) == i+1 && host.SpoolWaiting(node.Host()) == 0
		})
	}
	if a := <-fetchAsync(t, c, "p5", body); a.status != http.StatusOK {
		t.Errorf("another credential's request beside twenty slow uploads answered %d %q", a.status, short(a.body))
	}
	for i, o := range opens {
		if a := o.finish(); a.status != http.StatusOK {
			t.Errorf("slow upload %d answered %d %q", i+1, a.status, short(a.body))
		}
	}
	if got := counted(node.Metrics(), metrics.CodeHostSpoolWaits, "granted"); got != 0 {
		t.Errorf("%s{outcome=granted} = %d: a request waited", metrics.CodeHostSpoolWaits, got)
	}
	assertNoSpool(t, c)
}

// A QUEUE BEHIND ONE TOKEN LEAVES THE REQUEST MEMORY TO EVERYBODY ELSE: the
// requests waiting to spool hold their heads in places of the spool's waiting
// room, never in the request memory, so a fetch of another credential — an
// ls-refs, a negotiation of a few hundred bytes — is read into memory and
// served at once, never sent to the spool and its queue.
//
// Mutation: leave a waiting head charged to the request memory (start's
// waiting callback a no-op) and the four waiters spend it, so the ls-refs goes
// to the spool, finds the node's room claimed by the body arriving and waits.
func TestAQueueBehindOneTokenLeavesTheRequestMemoryToEverybodyElse(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	host.SetRequestMemoryAllowance(node.Host(), 4<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	body := repeatedWants(head, 8<<10)
	host.SetSpools(node.Host(), host.SpoolSizes{Limit: 64 << 10, Share: 64 << 10, Request: 64 << 10, Grain: 1 << 10,
		Places: 64, PlacesPerCredential: 16})
	first := openFetch(t, c, "ceo", body)
	eventually(t, "the first request's body arriving", func() bool { return host.SpoolsArriving(node.Host()) == 1 })
	var waiters []<-chan reply
	for i := range 4 {
		waiters = append(waiters, fetchAsync(t, c, "ceo", body))
		eventually(t, "a request waiting", func() bool { return host.SpoolWaiting(node.Host()) == i+1 })
	}
	if held := host.RequestMemoryInUse(node.Host()); held != 0 {
		t.Errorf("the request memory holds %d bytes for requests waiting to spool", held)
	}
	small := fetchWants([]string{head})
	for _, tc := range []struct{ name, body string }{{"an ls-refs", lsRefs()}, {"a small fetch", small}} {
		done := fetchAsync(t, c, "dev", tc.body)
		select {
		case a := <-done:
			if a.status != http.StatusOK {
				t.Errorf("%s beside the queue answered %d %q", tc.name, a.status, short(a.body))
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s beside the queue was not answered in five seconds", tc.name)
		}
		if n := host.SpoolWaiting(node.Host()); n != 4 {
			t.Errorf("%d requests wait after %s, want the queue's four", n, tc.name)
		}
	}
	if a := first.finish(); a.status != http.StatusOK {
		t.Errorf("the first answered %d %q", a.status, short(a.body))
	}
	for _, w := range waiters {
		if a := <-w; a.status != http.StatusOK {
			t.Errorf("a request that waited answered %d %q", a.status, short(a.body))
		}
	}
	assertNoSpool(t, c)
}

// THE WAIT IS THE IDLE WINDOW LESS A KEEPALIVE. A waiting fetch has written
// nothing and reads nothing, so what ends it from outside is a balancer's idle
// timeout, which the deployment page sets at a minute or more on /git/ — the
// silence the node allows a git transfer (gitproc.TransferStall) — and its
// 503 must come a keepalive interval before it. The figures the pages and the
// metric reference state are the ones the code waits on.
//
// Mutation: change Production.SpoolWait or the page's minute and this fails.
func TestTheSpoolWaitIsTheIdleWindowLessAKeepalive(t *testing.T) {
	t.Parallel()
	if gitproc.TransferStall != time.Minute {
		t.Fatalf("gitproc.TransferStall is %v, not the minute the deployment page states", gitproc.TransferStall)
	}
	if got, want := host.Production.SpoolWait, gitproc.TransferStall-host.Production.Keepalive; got != want {
		t.Fatalf("Production.SpoolWait is %v, want the idle window less a keepalive, %v", got, want)
	}
	wait := strconv.Itoa(int(host.Production.SpoolWait/time.Second)) + " s"
	for page, wants := range map[string][]string{
		filepath.Join("docs", "guides", "deployment.md"): {
			"an idle timeout of a minute or more on `/git/`",
			"waits up to " + wait + " for its turn",
		},
		filepath.Join("docs", "concepts", "code-host.md"): {"waits its turn for it, up to " + wait},
	} {
		requireSays(t, page, wants...)
	}
	for _, inst := range metrics.Catalogue() {
		switch inst.Name {
		case metrics.CodeHostSpoolWaits:
			if !strings.Contains(inst.Shows, "after waiting "+wait) {
				t.Errorf("%s's reference does not say a wait expires after %s", inst.Name, wait)
			}
			for _, outcome := range host.SpoolOutcomes() {
				if !strings.Contains(inst.Shows, "`"+outcome+"`") {
					t.Errorf("%s's reference does not name the outcome %q", inst.Name, outcome)
				}
			}
		case metrics.CodeHostSpoolWait:
			if !strings.Contains(inst.Shows, "nearing "+wait) {
				t.Errorf("%s's reference does not name the %s bound", inst.Name, wait)
			}
		}
	}
}

// EVERY LANE'S SPOOL SIZES LET EVERY BODY FINISH, and the figures the pages
// state are what the allowance admits: at any lane width the largest claim
// fits the allowance and a credential's share (spoolSizes.check), and on a
// four-core node's sixteen transfer slots and a sixteen-core node's sixty-four
// the largest request, the shares, the room the bodies arriving may hold
// before one waits on another, the waiting room and its memory, and how many
// lazy fetches of 20 MB fill a token's quarter and the node's room, are the
// ones the deployment page, the code guide, the concept page and the metric
// reference say — whatever the repository, since a spool holds what its
// client sent.
//
// Mutation: make a credential's share a third of the allowance, a place a
// lane's width, or the largest request the whole share, and this fails.
func TestTheSpoolFiguresAreWhatTheAllowanceAdmits(t *testing.T) {
	t.Parallel()
	for transfers := 1; transfers <= 1024; transfers++ {
		z := host.SpoolSizesFor(transfers)
		if err := host.SpoolSizesCheck(z); err != nil {
			t.Fatalf("the allowance for %d transfer slots: %v", transfers, err)
		}
		if z.PlacesPerCredential < 1 {
			t.Fatalf("the allowance for %d transfer slots lets a credential nothing: %+v", transfers, z)
		}
	}
	mib := func(n int64) string { return strconv.FormatInt(n>>20, 10) + " MiB" }
	four, sixteen := host.SpoolSizesFor(16), host.SpoolSizesFor(64)
	wants := four.Request / int64(host.WantBytes)
	if four.Request != sixteen.Request || wants < 1_300_000 || wants >= 1_400_000 {
		t.Fatalf("the largest request is %d bytes on sixteen slots and %d on sixty-four, %d wants: the pages say "+
			"64 MiB, about 1.3 million wants, on both", four.Request, sixteen.Request, wants)
	}
	const lazyFetch = 20_000_000 // a blobless checkout's lazy fetch of 400 000 blobs
	figures := []struct {
		got, want int64
		what      string
	}{
		{four.Limit, 1 << 30, "the node's room on four cores"},
		{four.Share, 256 << 20, "a credential's quarter on four cores"},
		{four.Limit - four.Request, 960 << 20, "what the bodies arriving may hold on four cores"},
		{four.Share - four.Request, 192 << 20, "what a credential's bodies arriving may hold on four cores"},
		{four.Share / lazyFetch, 13, "lazy fetches of 20 MB in a token's quarter on four cores"},
		{four.Limit / lazyFetch, 53, "lazy fetches of 20 MB in the node's room on four cores"},
		{int64(four.PlacesPerCredential), 16, "a credential's places on four cores"},
		{int64(sixteen.PlacesPerCredential), 64, "a credential's places on sixteen cores"},
	}
	for _, f := range figures {
		if f.got != f.want {
			t.Errorf("%s is %d, the pages say %d", f.what, f.got, f.want)
		}
	}
	room := func(z host.SpoolSizes) int64 { return int64(z.Places) * host.RequestMemoryBytes }
	if room(four) != 64<<20 || room(sixteen) != 256<<20 {
		t.Errorf("the waiting room's memory is %s and %s, the pages say 64 MiB and 256 MiB", mib(room(four)),
			mib(room(sixteen)))
	}
	requireSays(t, filepath.Join("docs", "guides", "deployment.md"),
		"256 MiB on a four-core node, each up to 64 MiB, about 1.3 million objects",
		"16 on a four-core node, past which the excess is answered 503 at once",
		"thirteen lazy fetches of 20 MB fill a four-core node's 256 MiB",
		"fifty-three such fetches on a four-core node's gibibyte",
		"960 MiB of a four-core node's gibibyte, 192 MiB of a token's quarter",
	)
	requireSays(t, filepath.Join("docs", "concepts", "code-host.md"),
		"64 MiB for each transfer slot",
		"at most 64 MiB, about 1.3 million objects",
		"960 MiB of a four-core node's gibibyte",
		"256 MiB on a four-core node",
		"thirteen lazy fetches of 20 MB fill a four-core node's 256 MiB",
		"fifty-three such fetches on a four-core node",
		"64 MiB on a four-core node and 256 MiB on sixteen cores",
	)
	requireSays(t, filepath.Join("docs", "guides", "code.md"),
		"256 MiB on a four-core node",
		"thirteen lazy fetches of 20 MB fill a four-core node's quarter",
	)
	for _, inst := range metrics.Catalogue() {
		if inst.Name == metrics.CodeHostSpoolWaits && !strings.Contains(inst.Shows, "a lane's width") {
			t.Errorf("%s's reference does not say how many of one credential's may wait", inst.Name)
		}
	}
}

// requireSays fails t unless page, read from the tree with its whitespace
// collapsed, says every one of wants.
func requireSays(t *testing.T, page string, wants ...string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), page))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("%s does not say %q", page, want)
		}
	}
}

// holdFirst is a stand-in git whose first upload-pack once hold exists
// (armHold) writes started and waits, before it runs, until hold is removed;
// every other upload-pack runs at once. rearm makes the next one held again.
func holdFirst(t *testing.T) (fake, hold, started string) {
	t.Helper()
	dir := t.TempDir()
	hold, started = filepath.Join(dir, "hold"), filepath.Join(dir, "started")
	claimed := filepath.Join(dir, "claimed")
	fake = gittest.Wrap(t, `case " $* " in
*" --advertise-refs "*) ;;
" upload-pack "*) if [ -e `+shellWord(hold)+` ] && mkdir `+shellWord(claimed)+` 2>/dev/null; then
  : > `+shellWord(started)+`
  while [ -e `+shellWord(hold)+` ]; do sleep 0.02; done
fi ;;
esac`)
	return fake, hold, started
}

// armHold makes the next upload-pack holdFirst's stand-in starts wait on hold.
func armHold(t *testing.T, hold string) {
	t.Helper()
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// rearm makes holdFirst's stand-in hold its next upload-pack again.
func rearm(t *testing.T, hold, started string) {
	t.Helper()
	for _, p := range []string{started, filepath.Join(filepath.Dir(hold), "claimed")} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	armHold(t, hold)
}

// repeatedWants is a v2 fetch of head, asked for again until the request is
// past size bytes: the fewest wants, at least one, that take it past size.
//
// The count is ARITHMETIC — the request's fixed frame and one fixed-length
// line a want — never found by building the request again for every want
// added: that was quadratic, and the 80 KiB request took 5.6 s to build under
// the race detector on an idle machine, which the case that holds a refusal to
// being answered at once timed as the node's answer.
func repeatedWants(head string, size int) string {
	frame := len(fetchWants(nil))
	line := len(pkt("want " + head + "\n"))
	n := 1
	if size >= frame {
		n = (size-frame)/line + 1
	}
	wants := make([]string, n)
	for i := range wants {
		wants[i] = head
	}
	return fetchWants(wants)
}

// reply is what a request sent off the case's goroutine came to.
type reply struct {
	status int
	header http.Header
	body   string
	err    error
}

// replyOf reads a response whole.
func replyOf(resp *http.Response, err error) reply {
	if err != nil {
		return reply{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, rerr := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, header: resp.Header, body: string(raw), err: rerr}
}

// fetchAsync posts body to node 0's upload-pack as who, off the case's
// goroutine, declaring its length: its answer arrives on the channel.
func fetchAsync(t *testing.T, c *codetest.Case, who, body string) <-chan reply {
	t.Helper()
	return fetchOf(t, c, who, strings.NewReader(body))
}

// fetchUndeclared is fetchAsync declaring no length — chunked, as git sends
// every request past its http.postBuffer — so its body claims what one
// request spools while it arrives.
func fetchUndeclared(t *testing.T, c *codetest.Case, who, body string) <-chan reply {
	t.Helper()
	return fetchOf(t, c, who, struct{ io.Reader }{strings.NewReader(body)})
}

// fetchOf posts body to node 0's upload-pack as who, off the case's goroutine.
func fetchOf(t *testing.T, c *codetest.Case, who string, body io.Reader) <-chan reply {
	t.Helper()
	node := c.Fleet.Nodes()[0]
	req, err := http.NewRequestWithContext(c.Ctx, http.MethodPost,
		node.URL()+"/git/"+codetest.RepoName+"/git-upload-pack", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", v2)
	req.SetBasicAuth("crewlet", c.Fleet.Token(t, who))
	out := make(chan reply, 1)
	go func() { out <- replyOf(node.Client().Do(req)) }()
	return out
}

// open is a fetch whose last bytes are held back.
type open struct {
	w    *io.PipeWriter
	tail string
	out  chan reply
}

// openFetch starts a fetch of body as who on node 0 and sends all of it but
// its last four bytes: a request whose body is arriving, or waits to, until
// finish or abandon.
func openFetch(t *testing.T, c *codetest.Case, who, body string) *open {
	t.Helper()
	node := c.Fleet.Nodes()[0]
	r, w := io.Pipe()
	req, err := http.NewRequestWithContext(c.Ctx, http.MethodPost,
		node.URL()+"/git/"+codetest.RepoName+"/git-upload-pack", r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", v2)
	req.SetBasicAuth("crewlet", c.Fleet.Token(t, who))
	o := &open{w: w, tail: body[len(body)-4:], out: make(chan reply, 1)}
	go func() { o.out <- replyOf(node.Client().Do(req)) }()
	if _, err := io.WriteString(w, body[:len(body)-4]); err != nil {
		t.Fatal(err)
	}
	return o
}

// finish sends the held bytes and answers what the request came to.
func (o *open) finish() reply {
	_, _ = io.WriteString(o.w, o.tail)
	_ = o.w.Close()
	return <-o.out
}

// abandon ends the request's body short, as a client that leaves does.
func (o *open) abandon() {
	_ = o.w.CloseWithError(errors.New("the client left"))
	<-o.out
}

// counted is rec's counter name for outcome.
func counted(rec *metrics.Recorder, name, outcome string) uint64 {
	var n uint64
	for _, s := range rec.Read() {
		if s.Name == name && s.Attrs["outcome"] == outcome {
			n += s.Total
		}
	}
	return n
}
