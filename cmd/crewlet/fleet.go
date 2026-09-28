package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
)

// `crewlet fleet broker` — the fleet broker's membership, and the removal of a
// member that is gone for good.
//
// # Why through a running node
//
// The metadata group is read and changed from inside a MEMBER, and a removal
// only through its system account, which nothing outside the member's process
// can reach — see internal/engine/fleetbroker.go. So `list` is a client of
// `GET /fleet/broker` and `remove` of its POST, as `crewlet objects` is of the
// placement map's routes.
//
// # What list is FOR
//
// The question an operator runs it with is "does the broker still count a
// member I have lost?" — after a host died for good, before relying on a
// quorum, and while replacing a member. The table puts what each node
// advertises beside what the metadata group counts, and the lines under it say
// in words what disagrees and what to do about it.

// fleetBrokerUsage is the group's usage line.
const fleetBrokerUsage = "usage: crewlet fleet broker list|remove [<config.yaml>] [-url] [-token]\n" +
	"       crewlet fleet broker remove <node-id> -confirm <node-id> [-force]\n" +
	"       crewlet fleet broker remove -peer <peer-id> -confirm <peer-id> [-force]"

// runFleet dispatches `crewlet fleet`.
func runFleet(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "broker":
		return runFleetBroker(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, fleetBrokerUsage)
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown fleet command %q", sub)
	}
}

// runFleetBroker dispatches `crewlet fleet broker`.
func runFleetBroker(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "list":
		return fleetBrokerList(rest, stdout, stderr)
	case "remove":
		return fleetBrokerRemove(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, fleetBrokerUsage)
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown fleet broker command %q", sub)
	}
}

// brokerView is GET /fleet/broker's answer as this command reads it.
//
// A SHAPE OF THIS COMMAND'S OWN, for `retention status`'s reason: a struct
// shared with the writer would tie an older binary to a newer node's answer.
// Its tests serve the committed rendering the engine's own test writes
// (internal/api/testdata/fleet_broker_answer.json), so the two cannot drift
// unnoticed.
type brokerView struct {
	Node     string `json:"node"`
	Kind     string `json:"kind"`
	External bool   `json:"external"`
	Nodes    []struct {
		Node  string   `json:"node"`
		Kind  string   `json:"kind"`
		Roles []string `json:"roles"`
	} `json:"nodes"`
	Group      *brokerGroupView `json:"group"`
	GroupFrom  string           `json:"group_from"`
	GroupError string           `json:"group_error"`
	Findings   []struct {
		Kind   string `json:"kind"`
		Node   string `json:"node"`
		Peer   string `json:"peer"`
		Detail string `json:"detail"`
	} `json:"findings"`
}

type brokerGroupView struct {
	Cluster string `json:"cluster"`
	Leader  string `json:"leader"`
	Peers   []struct {
		// Name is empty for a voter the answering member cannot name,
		// which Peer still identifies.
		Name    string        `json:"name"`
		Peer    string        `json:"peer"`
		Self    bool          `json:"self"`
		Leader  bool          `json:"leader"`
		Current bool          `json:"current"`
		Offline bool          `json:"offline"`
		Active  time.Duration `json:"active"`
	} `json:"peers"`
}

// fleetBrokerList is `crewlet fleet broker list`.
func fleetBrokerList(args []string, stdout, stderr io.Writer) error {
	var asJSON *bool
	client, err := nodeClientFor(args, "fleet broker list", stderr, func(fs *flag.FlagSet) {
		asJSON = fs.Bool("json", false, "print the node's answer as it came, for a script")
	})
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := client.get(context.Background(), "/fleet/broker", &raw); err != nil {
		return err
	}
	if *asJSON {
		_, err := fmt.Fprintf(stdout, "%s\n", raw)
		return err
	}
	var view brokerView
	if err := json.Unmarshal(raw, &view); err != nil {
		return fmt.Errorf("the node's broker answer is not the shape this build reads: %w", err)
	}
	return renderBroker(stdout, view)
}

// renderBroker prints the table, then every disagreement in words.
func renderBroker(w io.Writer, v brokerView) error {
	if v.External {
		_, err := fmt.Fprintf(w, "This fleet's broker is an external NATS cluster "+
			"(%s is its client): its membership belongs to whoever runs it, and "+
			"nothing here lists or changes it.\n", v.Node)
		return err
	}
	switch g := v.Group; {
	case g == nil:
		fmt.Fprintf(w, "The metadata group could not be read: %s\n", v.GroupError)
	case g.Leader == "":
		fmt.Fprintf(w, "Metadata group of %s, as %s reports it: NO LEADER — it is "+
			"electing one, or it has lost its quorum and can change nothing about "+
			"itself.\n", g.Cluster, v.GroupFrom)
	default:
		fmt.Fprintf(w, "Metadata group of %s, as %s reports it: %d voters, led by %s.\n",
			g.Cluster, v.GroupFrom, len(g.Peers), g.Leader)
	}
	fmt.Fprintln(w)

	// A VOTER IS MATCHED TO ITS NODE BY PEER ID, as the engine matches
	// them: the name is only what the answering member has heard, and a
	// voter whose name it has not is still the node whose id hashes to it.
	live := map[string]bool{}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tADVERTISES\tROLES\tVOTER\tPEER")
	for _, n := range v.Nodes {
		peer := jetstream.PeerIDOf(n.Node)
		live[peer] = true
		voter := voterOf(v.Group, peer)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", n.Node, n.Kind,
			orDash(strings.Join(n.Roles, ",")), voterState(v.Group, voter),
			peerColumn(voter, peer))
	}
	if v.Group != nil {
		for i, p := range v.Group.Peers {
			if live[p.Peer] {
				continue
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", orNameless(p.Name), "-", "-",
				voterState(v.Group, i), p.Peer)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(v.Findings) == 0 {
		_, err := fmt.Fprintln(w, "\nEvery member the group counts is live, and every live "+
			"member is counted.")
		return err
	}
	fmt.Fprintln(w)
	for _, f := range v.Findings {
		switch f.Kind {
		case string(engine.BrokerDeadMember):
			fmt.Fprintf(w, "DEAD MEMBER %s: %s. Once it is not coming back, "+
				"`%s` stops the group counting it.\n", findingSubject(f.Node, f.Peer),
				f.Detail, removeCommand(f.Node, f.Peer))
		case string(engine.BrokerNotInGroup):
			fmt.Fprintf(w, "NOT COUNTED %s: %s.\n", f.Node, f.Detail)
		case string(engine.BrokerUnknownKind):
			fmt.Fprintf(w, "UNKNOWN %s: %s.\n", f.Node, f.Detail)
		default:
			// A KIND A NEWER NODE SENDS, shown rather than dropped.
			fmt.Fprintf(w, "%s %s: %s.\n", strings.ToUpper(f.Kind),
				findingSubject(f.Node, f.Peer), f.Detail)
		}
	}
	return nil
}

// voterOf is the index of the voter with this peer id in the group, or -1 for
// a node the group does not count (or a group nobody could read).
func voterOf(g *brokerGroupView, peer string) int {
	if g == nil {
		return -1
	}
	for i, p := range g.Peers {
		if p.Peer == peer {
			return i
		}
	}
	return -1
}

// peerColumn is a live node's peer id where it is a voter, and a dash where
// it is not.
func peerColumn(voter int, peer string) string {
	if voter < 0 {
		return "-"
	}
	return peer
}

// orNameless is a voter's name, or what stands in for one nobody has heard.
func orNameless(name string) string {
	if name == "" {
		return "(name unknown)"
	}
	return name
}

// findingSubject is who a finding is about: the node where one is named, and
// the voter's peer id where only that is known.
func findingSubject(node, peer string) string {
	if node != "" {
		return node
	}
	return "(peer " + peer + ")"
}

// removeCommand is the exact removal a dead member's line prints: by node id
// where the answer names one, and by peer id where only that is known — the
// command must work as printed, and a peer id is not a node id.
func removeCommand(node, peer string) string {
	if node != "" {
		return fmt.Sprintf("crewlet fleet broker remove %s -confirm %s", node, node)
	}
	return fmt.Sprintf("crewlet fleet broker remove -peer %s -confirm %s", peer, peer)
}

// voterState is how the metadata group counts the voter at index i — a dash
// for none, a question mark for a group nobody could read.
func voterState(g *brokerGroupView, i int) string {
	switch {
	case g == nil:
		return "?"
	case i < 0:
		return "-"
	}
	p := g.Peers[i]
	switch {
	case p.Leader:
		return "leader"
	case p.Self, p.Current:
		return "current"
	case p.Offline:
		return "offline, last heard " + shortDuration(p.Active.Round(time.Second)) + " ago"
	default:
		return "behind"
	}
}

// brokerRemovedView is a removal's answer as this command reads it.
type brokerRemovedView struct {
	Node  string           `json:"node"`
	Peer  string           `json:"peer"`
	By    string           `json:"by"`
	Group *brokerGroupView `json:"group"`
}

// fleetBrokerRemoveWait is how long `remove` waits for the node's answer.
//
// THE ENGINE'S OWN WAIT FOR THE WHOLE GESTURE, and a minute more for the node
// that received the request to list the fleet's presence and reach the member
// carrying it. At the ten seconds every other call gets, a removal the
// metadata group was slow to commit was reported to the operator as failed
// while the group went on to commit it.
func fleetBrokerRemoveWait() time.Duration { return engine.BrokerRemoveWait() + time.Minute }

// fleetBrokerRemove is `crewlet fleet broker remove`: a voter named by node id,
// or by `-peer` for one the listing shows with no name.
func fleetBrokerRemove(args []string, stdout, stderr io.Writer) error {
	node, rest := splitSubject(args)
	var confirm, peer *string
	var force *bool
	client, err := nodeClientFor(rest, "fleet broker remove", stderr, func(fs *flag.FlagSet) {
		confirm = fs.String("confirm", "",
			"repeat the node id (or the peer id) — a removed member no longer counts "+
				"in any election")
		peer = fs.String("peer", "",
			"name the voter by the peer id the listing shows, for one no member can name")
		force = fs.Bool("force", false,
			"remove it although its node holds a live presence lease as a member: "+
				"only for a member wedged in a way that still renews it, since a "+
				"running member removed from the group rejoins it at its next restart")
	})
	if err != nil {
		return err
	}
	route, id := "/fleet/broker/remove/", node
	switch {
	case node != "" && *peer != "":
		fmt.Fprintln(stderr, fleetBrokerUsage)
		return errors.New("name the voter by its node id or by -peer, not both")
	case *peer != "":
		route, id = "/fleet/broker/remove-peer/", *peer
	}
	if id == "" || *confirm != id {
		fmt.Fprintln(stderr, fleetBrokerUsage)
		return errors.New("removing a member changes the quorum every election and every " +
			"create of the fleet's broker runs on — repeat the id in -confirm")
	}
	query := url.Values{"confirm": {id}}
	if *force {
		query.Set("force", "true")
	}
	var answer brokerRemovedView
	err = client.patiently(fleetBrokerRemoveWait()).post(context.Background(),
		route+url.PathEscape(id)+"?"+query.Encode(), &answer)
	if err != nil {
		var lost noAnswer
		if errors.As(err, &lost) {
			fmt.Fprintln(stderr, "The node did not answer, so whether the removal was "+
				"committed is unknown. `crewlet fleet broker list` reads the group; "+
				"asking again for a member already removed is answered as not a member.")
		}
		return err
	}
	fmt.Fprintf(stdout, "%s is no longer a voter in the metadata group (removed through "+
		"%s's system account).", findingSubject(answer.Node, answer.Peer), answer.By)
	if g := answer.Group; g != nil {
		names := make([]string, 0, len(g.Peers))
		for _, p := range g.Peers {
			names = append(names, orNameless(p.Name))
		}
		fmt.Fprintf(stdout, " It counts %d: %s.", len(names), strings.Join(names, ", "))
	}
	fmt.Fprintln(stdout, "\n  A member that restarts under that name joins the group again as a new voter.")
	return nil
}
