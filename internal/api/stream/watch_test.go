package stream_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// A WATCH IS DECIDED LIKE THE INBOX QUESTION ABOUT THE SAME SEAT.
//
// A watch installs this node's routing for a seat's `inbox_changed` frames —
// whose inbox moved, when, under which reason — so it takes the rule the
// `work_inbox` question takes about that seat: the seat's own holder, whoever
// leads it through the chart, or the admin grant. Any resolved caller could
// watch any seat before, which made the routing index a way to follow somebody
// else's inbox the question itself refused.
//
// Each refusal is a FRAME on a socket that stays open, never a close: 4403 is
// what the dashboard reads as "you may not have the live channel at all", and
// all that was refused is one seat's frames.
func TestAWatchIsDecidedLikeTheInboxQuestion(t *testing.T) {
	t.Parallel()
	chart := &leadChart{leads: map[[2]string]bool{{"platform-lead", "sarah-chen"}: true}}
	f := newWatchSocket(t, chart, map[string]string{
		"token:sarah":    "sarah-chen",
		"token:lead":     "platform-lead",
		"token:stranger": "ops-lead",
	}, config.APIToken{ID: "admin", Token: "admin-token-long-enough-to-pass",
		Grants: []iam.Grant{iam.GrantStateRead, iam.GrantFleetOperate}})

	for _, tc := range []struct {
		name, token string
		allowed     bool
	}{
		{"their own seat", "sarah-token-long-enough-to-pass", true},
		{"a lead's report", "lead-token-long-enough-to-pass", true},
		{"the admin grant", "admin-token-long-enough-to-pass", true},
		{"a stranger", "stranger-token-long-enough-to-pass", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// THE PREVIOUS CASE'S SOCKET GONE FIRST, or a count of one
			// could be its watch rather than this one's.
			waitFor(t, func() bool { return f.watchers(nil) == 0 },
				"the previous case's watch outlived its socket")
			conn := f.open(t, tc.token)
			write(t, conn, map[string]any{"kind": "watch", "seat": "sarah-chen"})
			if tc.allowed {
				waitFor(t, func() bool { return f.watchers(conn) == 1 },
					tc.name+"'s watch never reached the index")
				return
			}
			refusal := next(t, conn)
			if refusal["kind"] != stream.KindError || refusal["what"] != "watch" ||
				refusal["error"] != stream.CodeUnauthorized {
				t.Fatalf("a stranger's watch was answered %v, want a watch "+
					"refusal frame", refusal)
			}
			f.assertOpen(t, conn)
			if got := f.watchers(conn); got != 0 {
				t.Errorf("a refused watch installed %d rows in the index", got)
			}
		})
	}
}

// A WATCH THIS NODE CANNOT DECIDE IS NOT INSTALLED, AND THE SOCKET STAYS OPEN.
//
// The chart could not be read — a node booting, applying, or behind the chart
// log. Installing would hand a seat's frames to somebody this node could not
// show was allowed them; refusing would tell a lead they lead nobody because
// the NODE is behind. So neither: `unavailable`, which the client retries, and
// the socket carries on serving everything else.
func TestAWatchThisNodeCannotDecideIsNotInstalled(t *testing.T) {
	t.Parallel()
	chart := &leadChart{err: errors.New("the chart view is not built yet")}
	f := newWatchSocket(t, chart, map[string]string{"token:lead": "platform-lead"})
	conn := f.open(t, "lead-token-long-enough-to-pass")

	write(t, conn, map[string]any{"kind": "watch", "seat": "sarah-chen"})
	refusal := next(t, conn)
	if refusal["kind"] != stream.KindError || refusal["what"] != "watch" ||
		refusal["error"] != stream.CodeUnavailable {
		t.Fatalf("an undecidable watch was answered %v, want unavailable", refusal)
	}
	f.assertOpen(t, conn)
	if got := f.svc.Hub().Watchers("sarah-chen"); got != 0 {
		t.Errorf("an undecidable watch installed %d rows", got)
	}

	// AND ONE THAT NEEDS NO CHART IS STILL DECIDED: the holder's own seat.
	write(t, conn, map[string]any{"kind": "watch", "seat": "platform-lead"})
	waitFor(t, func() bool { return f.svc.Hub().Watchers("platform-lead") == 1 },
		"a watch of the holder's own seat waited on a chart it never needed")
}

// leadChart answers who leads whom from a table, or fails every lead question.
type leadChart struct {
	leads map[[2]string]bool
	err   error
}

func (c *leadChart) Leads(_ context.Context, actor, subject string) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	return c.leads[[2]string{actor, subject}], nil
}

func (c *leadChart) LeadsProject(context.Context, string, string) (bool, error) {
	return false, c.err
}

func (c *leadChart) LeadsUnit(context.Context, string, string) (bool, error) {
	return false, c.err
}

func (c *leadChart) LeadsContainer(context.Context, string, string) (bool, error) {
	return false, c.err
}

var _ authz.Chart = (*leadChart)(nil)

// watchFixture is a socket server whose tokens are bound to seats.
type watchFixture struct {
	svc *stream.Service
	url string
}

// newWatchSocket serves a socket whose Tier A tokens are named after the key of
// each binding, `<name>-token-long-enough-to-pass`, and bound to its seat.
func newWatchSocket(t *testing.T, chart authz.Chart, bound map[string]string,
	extra ...config.APIToken) *watchFixture {

	t.Helper()
	b := config.DefaultBootstrap()
	b.API.Auth.MaxGrants = iam.AllGrants
	for login := range bound {
		id := strings.TrimPrefix(login, "token:")
		b.API.Auth.Tokens = append(b.API.Auth.Tokens, config.APIToken{
			ID: id, Token: id + "-token-long-enough-to-pass",
			Grants: []iam.Grant{iam.GrantStateRead},
		})
	}
	b.API.Auth.Tokens = append(b.API.Auth.Tokens, extra...)
	guard := auth.New(&b).BindSeats(auth.SeatBindings{
		Directory: watchBindings(bound), Chart: watchSeats(bound)})
	svc := buildService(t, stream.Options{Chart: chart})
	srv := httptest.NewServer(stream.Handler(guard, svc, nil))
	t.Cleanup(srv.Close)
	return &watchFixture{svc: svc,
		url: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/stream"}
}

// open dials as one token and reads past the handshake snapshot.
func (f *watchFixture) open(t *testing.T, token string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(t.Context(), f.url+"?token="+token, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	if first := next(t, conn); first["kind"] != stream.KindSnapshot {
		t.Fatalf("first frame = %v, want the snapshot", first["kind"])
	}
	return conn
}

// watchers is how many clients watch sarah-chen, the seat every case asks for.
func (f *watchFixture) watchers(*websocket.Conn) int {
	return f.svc.Hub().Watchers("sarah-chen")
}

// assertOpen proves the socket survived: a ping is still answered.
func (f *watchFixture) assertOpen(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	write(t, conn, map[string]any{"kind": "ping"})
	if got := next(t, conn); got["kind"] != stream.KindPong {
		t.Fatalf("after a refused watch the socket answered %v, want a pong: a "+
			"refusal of one seat's frames closed the whole live channel", got)
	}
}

// watchBindings binds each Tier A token's login to its seat, as an active
// machine row decided at chart position 1.
type watchBindings map[string]string

func (b watchBindings) BoundSeat(_ context.Context, login string) (session.PersonRow, error) {
	seat, ok := b[login]
	if !ok {
		return session.PersonRow{}, nil
	}
	return session.PersonRow{Found: true, Stage: iam.StageActive, Login: login,
		Seat: seat, SeatAt: 1}, nil
}

// watchSeats holds every bound seat as a human seat, at a position covering
// every binding.
type watchSeats map[string]string

func (c watchSeats) Seat(_ context.Context, ref string) (session.Seat, bool, error) {
	for _, seat := range c {
		if seat == ref {
			return session.Seat{Handle: seat, Kind: "human"}, true, nil
		}
	}
	return session.Seat{}, false, nil
}

func (watchSeats) Position(context.Context) (uint64, time.Duration, error) {
	return 1, 0, nil
}
