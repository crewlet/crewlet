package jetstreamtest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// A MEMBER BROUGHT BACK IS THE SAME MEMBER: on its own route port and under its
// own name, routed to every peer again, counted by the metadata group as the
// voter it was, and serving the replicated records written while it was down —
// what a fleet's node restarting over its own files is, and what Gitlet's
// multi-node suite kills and restarts a member as.
//
// NOT PARALLEL, for the reason TestAMemberGoneForGoodIsRemovedAfterEverySurvivorRestarted
// gives: the member comes back on the route port it had, and a cluster another
// case reserved ports for in the same moment could take it while it is down.
func TestARestartedMemberRejoinsAsItself(t *testing.T) {
	c := StartCluster(t, 3, js.Config{})
	leaderOf(t, c)
	const member = 2
	q := c.Client(t, 0)
	spec := js.DomainStream{Name: "CREWLET_RESTART_LOG", Subjects: []string{"crewlet.restart.>"},
		MaxBytes: 1 << 20, Duplicates: time.Minute}
	if err := q.EnsureDomainStream(t.Context(), spec); err != nil {
		t.Fatalf("provision a replicated log: %v", err)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	appendN := func(from, to int) {
		t.Helper()
		for n := from; n < to; n++ {
			if err := landAppend(t.Context(), c, log, spec.Name, "crewlet.restart.a", fmt.Sprintf("m-%d", n),
				[]byte(fmt.Sprintf("record %d", n))); err != nil {
				t.Fatalf("append record %d: %v", n, err)
			}
		}
	}
	appendN(0, 5)

	c.StopMember(member)
	// WRITTEN WHILE IT IS DOWN, so what it serves once back is what it
	// caught up on rather than what it held.
	appendN(5, 10)
	back := c.RestartMember(t, member)

	// ITS OWN ROUTE PORT, asked as a peer asks it: what answers on the port its
	// peers dial must be this member, or no peer would ever route to it.
	if got, want := routeListener(t, c.Configs[member].ClusterHost, c.Configs[member].ClusterPort),
		c.Configs[member].ServerName; got != want {
		t.Errorf("the route port its peers dial, %d, is answered by %q, not the restarted member %q",
			c.Configs[member].ClusterPort, got, want)
	}
	others := []string{c.Configs[0].ServerName, c.Configs[1].ServerName}
	deadline := time.Now().Add(30 * time.Second)
	for !routesTo(c.Servers[member].RoutePeers(), others) {
		if time.Now().After(deadline) {
			t.Fatalf("the restarted member routes to %v, want %v", c.Servers[member].RoutePeers(), others)
		}
		time.Sleep(50 * time.Millisecond) //fixedwait:liveness
	}
	group := awaitGroup(t, c.Servers[member], func(g js.MetaGroup) bool {
		return g.Leader != "" && len(g.Peers) == 3
	})
	if !group.Counts(js.PeerIDOf(c.Configs[member].ServerName)) {
		t.Errorf("the metadata group no longer counts the restarted member: %+v", group.Peers)
	}

	backLog, err := back.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log through the restarted member: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, last, err := backLog.Bounds(ctx)
	if err != nil || last != 10 {
		t.Fatalf("through the restarted member the log ends at %d (%v), want 10", last, err)
	}
	for seq := uint64(1); seq <= last; seq++ {
		_, payload, _, ok, err := backLog.At(ctx, seq)
		if want := fmt.Sprintf("record %d", seq-1); err != nil || !ok || string(payload) != want {
			t.Errorf("record %d through the restarted member = %q, %v, %v; want %q", seq, payload, ok,
				err, want)
		}
	}
}

// landAppend appends one record to the replicated log named stream through log
// and answers once it has landed, asked again under the same message id while
// nobody answers.
//
// NOBODY ANSWERING IS NOT A REFUSAL, and a member coming or going is when nobody
// does. A stream's subjects are served by its leader alone, so a publish through
// any member needs a leader — a member that stops hands its leadership to a peer
// it believes healthy, and a transfer that does not take waits out an election
// of up to nine seconds (server/raft.go) — and that leader's interest in the
// subject on a route to this member, which a member's route peers do not prove:
// they name a peer from the first connection of its pool, and the stream's
// account may ride a later one (server/route.go). Until both hold the publish
// meets no responders, and the client gives up after two more tries a quarter of
// a second apart. Half a second against a window of seconds is why a single
// append through a member just restarted failed one run in many, with "no
// response from stream": measured on the rotation case, one restart in 27
// left no member leading the stream for 3.5 s, and the seventh ask landed.
//
// THE ENGINE'S WRITER READS THAT AS NO ANSWER, never as a failure
// (internal/statelog's classify): it reads the log at [jsprovision.LookupBudget]
// to learn whether its record landed, then publishes again under the same
// operation id. So this asks again under the same message id, which the log's
// duplicate window collapses onto the first copy if an acknowledgement was what
// went missing — a case holding the log's end to its records notices one that
// landed twice.
//
// AT MOST [jsprovision.LookupBudget], the longest silence the server keeps: a
// group past it has an answer to give, so a member's appends still unanswered
// are a member that is broken rather than slow, and the error says so, naming
// what it last heard and who believes they lead the stream. Anything but silence
// — a refusal, a closed connection — is answered at once: no wait changes it.
func landAppend(ctx context.Context, c *Cluster, log *js.DomainLog, stream, subject, msgID string, body []byte) error {
	deadline := time.Now().Add(jsprovision.LookupBudget)
	for {
		_, _, err := log.Append(ctx, subject, msgID, nil, body)
		switch {
		case err == nil:
			return nil
		case !jsprovision.Unanswered(ctx, err) && !errors.Is(err, jetstream.ErrNoStreamResponse):
			return err
		case time.Now().After(deadline):
			return fmt.Errorf("still unanswered after %s, the longest silence the server keeps, with %v "+
				"leading the stream: %w", jsprovision.LookupBudget, c.streamLeaders(stream), err)
		}
	}
}

// streamLeaders names the members that believe, right now, that they lead
// stream: asked of each in process, so a member that does not lead it says so
// rather than staying silent ([js.Server.LeadsStream]).
func (c *Cluster) streamLeaders(stream string) []string {
	var leaders []string
	for i, srv := range c.Servers {
		if srv.LeadsStream(stream) {
			leaders = append(leaders, c.Configs[i].ServerName)
		}
	}
	return leaders
}

// routeListener dials a member's route port as a peer does and answers the
// server name in the INFO a route listener opens every connection with.
//
// THE PEER'S VIEW, never the member's own report of what it bound: a peer
// knows a member only by the port it dials, so that is the fact a member
// brought back has to hold.
func routeListener(t *testing.T, host string, port int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial the route port %d: %v", port, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read the INFO the route port %d opens with: %v", port, err)
	}
	payload, ok := strings.CutPrefix(strings.TrimSpace(line), "INFO ")
	if !ok {
		t.Fatalf("the route port %d opened with %q, not an INFO", port, line)
	}
	var info struct {
		Name string `json:"server_name"`
	}
	if err := json.Unmarshal([]byte(payload), &info); err != nil {
		t.Fatalf("decode the INFO the route port %d opened with: %v", port, err)
	}
	return info.Name
}

// routesTo reports whether peers names every one of want.
func routesTo(peers, want []string) bool {
	for _, w := range want {
		if !slices.Contains(peers, w) {
			return false
		}
	}
	return true
}
