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

// runFleet dispatches `crewlet fleet`.
func runFleet(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "broker":
		return runFleetBroker(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, "usage: crewlet fleet broker list|remove [<config.yaml>] [-url] [-token]")
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
		fmt.Fprintln(stderr, "usage: crewlet fleet broker list|remove [<config.yaml>] [-url] [-token]")
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
		Detail string `json:"detail"`
	} `json:"findings"`
}

type brokerGroupView struct {
	Cluster string `json:"cluster"`
	Leader  string `json:"leader"`
	Peers   []struct {
		Name    string        `json:"name"`
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

	advertised := map[string]int{}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tADVERTISES\tROLES\tVOTER")
	for i, n := range v.Nodes {
		advertised[n.Node] = i
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n.Node, n.Kind, orDash(strings.Join(n.Roles, ",")),
			voterState(v.Group, n.Node))
	}
	if v.Group != nil {
		for _, p := range v.Group.Peers {
			if _, live := advertised[p.Name]; live {
				continue
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, "-", "-", voterState(v.Group, p.Name))
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
				"`crewlet fleet broker remove %s -confirm %s` stops the group counting it.\n",
				f.Node, f.Detail, f.Node, f.Node)
		case string(engine.BrokerNotInGroup):
			fmt.Fprintf(w, "NOT COUNTED %s: %s.\n", f.Node, f.Detail)
		case string(engine.BrokerUnknownKind):
			fmt.Fprintf(w, "UNKNOWN %s: %s.\n", f.Node, f.Detail)
		default:
			// A KIND A NEWER NODE SENDS, shown rather than dropped.
			fmt.Fprintf(w, "%s %s: %s.\n", strings.ToUpper(f.Kind), f.Node, f.Detail)
		}
	}
	return nil
}

// voterState is how the metadata group counts a node, or a dash for one it
// does not.
func voterState(g *brokerGroupView, node string) string {
	if g == nil {
		return "?"
	}
	for _, p := range g.Peers {
		if p.Name != node {
			continue
		}
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
	return "-"
}

// brokerRemovedView is a removal's answer as this command reads it.
type brokerRemovedView struct {
	Node  string           `json:"node"`
	By    string           `json:"by"`
	Group *brokerGroupView `json:"group"`
}

// fleetBrokerRemoveWait is how long `remove` waits for the node's answer.
//
// THE ENGINE'S OWN WAIT ON THE MEMBER CARRYING IT, and a minute more for the
// node that received the request to list the fleet's presence and reach that
// member. At the ten seconds every other call gets, a removal the metadata
// group was slow to commit was reported to the operator as failed while the
// group went on to commit it.
func fleetBrokerRemoveWait() time.Duration { return engine.BrokerRemoveWait() + time.Minute }

// fleetBrokerRemove is `crewlet fleet broker remove`.
func fleetBrokerRemove(args []string, stdout, stderr io.Writer) error {
	node, rest := splitSubject(args)
	var confirm *string
	var force *bool
	client, err := nodeClientFor(rest, "fleet broker remove", stderr, func(fs *flag.FlagSet) {
		confirm = fs.String("confirm", "",
			"repeat the node id — a removed member no longer counts in any election")
		force = fs.Bool("force", false,
			"remove it although it holds a live presence lease: only for a member "+
				"wedged in a way that still renews it, since a running member removed "+
				"from the group rejoins it at its next restart")
	})
	if err != nil {
		return err
	}
	if node == "" || *confirm != node {
		fmt.Fprintln(stderr, "usage: crewlet fleet broker remove <node-id> -confirm <node-id> [-force]")
		return errors.New("removing a member changes the quorum every election and every " +
			"create of the fleet's broker runs on — repeat the node id in -confirm")
	}
	query := url.Values{"confirm": {node}}
	if *force {
		query.Set("force", "true")
	}
	var answer brokerRemovedView
	err = client.patiently(fleetBrokerRemoveWait()).post(context.Background(),
		fmt.Sprintf("/fleet/broker/remove/%s?%s", url.PathEscape(node), query.Encode()), &answer)
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
		"%s's system account).", answer.Node, answer.By)
	if g := answer.Group; g != nil {
		names := make([]string, 0, len(g.Peers))
		for _, p := range g.Peers {
			names = append(names, p.Name)
		}
		fmt.Fprintf(stdout, " It counts %d: %s.", len(names), strings.Join(names, ", "))
	}
	fmt.Fprintln(stdout, "\n  A process that restarts under that name joins the group again as a new voter.")
	return nil
}
