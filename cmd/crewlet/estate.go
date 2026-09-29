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

	"github.com/crewlet/crewlet/internal/membership"
)

// `crewlet estate` — which data nodes hold each partition of the replicated
// estate, and the operator's gestures on the estate map.
//
// # Why through a running node
//
// The map is one record in the coordination store, which on the default
// topology is the engine's own embedded broker: `map` reads the estate question
// (`GET /estate`), and the gestures are clients of the `/estate/*` routes, as
// `crewlet objects` is of its own.
//
// # What it says at layout 0
//
// Every fleet on this build runs layout 0, where the estate is not divided and
// every data node holds the whole of it. `map` prints the sentence every
// surface gives that layout and the data nodes that hold the estate; every
// gesture is refused by the node in the same words.

// runEstate dispatches `crewlet estate`.
func runEstate(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "map":
		return estateMap(rest, stdout, stderr)
	case "out":
		return estateMemberGesture(rest, stdout, stderr, true)
	case "in":
		return estateMemberGesture(rest, stdout, stderr, false)
	case "hold":
		return estateHold(rest, stdout, stderr)
	case "release":
		return estateRelease(rest, stdout, stderr)
	case "move":
		return estateMove(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, "usage: crewlet estate map|out|in|hold|release|move "+
			"[<config.yaml>] [-url] [-token]")
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown estate command %q", sub)
	}
}

// estateView is the estate question's answer as this command reads it.
//
// A SHAPE OF THIS COMMAND'S OWN, for `objects status`'s reason: a struct shared
// with the writer would tie an older binary to a newer node's answer. Its tests
// serve the WRITER's renderers (queries.RenderEstate, RenderEstateWhole), so
// the two cannot drift unnoticed.
type estateView struct {
	State  string `json:"state"`
	Layout int    `json:"layout"`
	Detail string `json:"detail"`

	// layout 0
	Partition string `json:"partition"`
	Holders   []struct {
		Node  string           `json:"node"`
		Lease *estateLeaseView `json:"lease"`
		// Reports is what its lease says of the one partition.
		Reports string `json:"reports"`
	} `json:"holders"`

	// a placed map
	Generation string `json:"generation"`
	Epoch      uint64 `json:"epoch"`
	Spaces     []struct {
		Space      string `json:"space"`
		Partitions int    `json:"partitions"`
	} `json:"spaces"`
	Replicas      int    `json:"replicas"`
	Copies        int    `json:"copies"`
	FailureDomain string `json:"failure_domain"`
	DomainLimited bool   `json:"domain_limited"`
	Hold          *struct {
		Until  time.Time `json:"until"`
		By     string    `json:"by"`
		Reason string    `json:"reason"`
	} `json:"hold"`
	Balance *struct {
		DeviationPercent float64 `json:"deviation_percent"`
		TolerancePercent float64 `json:"tolerance_percent"`
		Converged        bool    `json:"converged"`
		Rounds           int     `json:"rounds"`
	} `json:"balance"`
	Unserved   int                   `json:"unserved"`
	Short      int                   `json:"short"`
	Joining    int                   `json:"joining"`
	Leaving    int                   `json:"leaving"`
	Moves      int                   `json:"moves"`
	Members    []estateMemberView    `json:"members"`
	Removed    []objectsRemovalView  `json:"removed"`
	Partitions []estatePartitionView `json:"partitions"`
}

type estateLeaseView struct {
	Layout  *int   `json:"layout"`
	Healthy *bool  `json:"healthy"`
	Detail  string `json:"detail"`
	Able    bool   `json:"able"`
}

type estateMemberView struct {
	Node         string   `json:"node"`
	Weight       int      `json:"weight"`
	Domain       string   `json:"domain"`
	Out          bool     `json:"out"`
	OutBy        string   `json:"out_by"`
	OutReason    string   `json:"out_reason"`
	SharePercent float64  `json:"share_percent"`
	Serving      int      `json:"serving"`
	Joining      int      `json:"joining"`
	Leaving      int      `json:"leaving"`
	MovedOff     []string `json:"moved_off"`
	Live         bool     `json:"live"`
	Probation    *struct {
		Present          int    `json:"present"`
		PlacedAfterTicks int    `json:"placed_after_ticks"`
		Reason           string `json:"reason"`
	} `json:"probation"`
	Absence *struct {
		Ticks         int    `json:"ticks"`
		OutAfterTicks int    `json:"out_after_ticks"`
		Present       int    `json:"present"`
		Reason        string `json:"reason"`
	} `json:"absence"`
	Lease *estateLeaseView `json:"lease"`
}

type estatePartitionView struct {
	ID      string   `json:"id"`
	Target  []string `json:"target"`
	Serving int      `json:"serving"`
	Wanted  int      `json:"wanted"`
	Holders []struct {
		Node    string `json:"node"`
		State   string `json:"state"`
		Since   uint64 `json:"since"`
		Reports string `json:"reports"`
		Able    bool   `json:"able"`
	} `json:"holders"`
	Moves []struct {
		Node    string `json:"node"`
		By      string `json:"by"`
		Reason  string `json:"reason"`
		Waiting bool   `json:"waiting"`
	} `json:"moves"`
}

// settled reports whether a partition needs nothing: as many copies as its
// target wants, every holder serving, and no move in force.
func (p estatePartitionView) settled() bool {
	if p.Serving < p.Wanted || len(p.Moves) > 0 {
		return false
	}
	for _, h := range p.Holders {
		if h.State != "serving" || !h.Able {
			return false
		}
	}
	return true
}

// estateMap is `crewlet estate map`.
func estateMap(args []string, stdout, stderr io.Writer) error {
	var asJSON, all *bool
	client, err := nodeClientFor(args, "estate map", stderr, func(fs *flag.FlagSet) {
		asJSON = fs.Bool("json", false, "print the estate map as the node answered it")
		all = fs.Bool("all", false, "list every partition, settled or not")
	})
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := client.get(context.Background(), "/estate", &raw); err != nil {
		return err
	}
	if *asJSON {
		_, err := fmt.Fprintf(stdout, "%s\n", raw)
		return err
	}
	var view estateView
	if err := json.Unmarshal(raw, &view); err != nil {
		return fmt.Errorf("the node's estate map is not the shape this build reads: %w", err)
	}
	return renderEstate(stdout, view, *all)
}

// renderEstate prints the estate map: what layout 0 is, or the map line, the
// hold, the members and what is not settled.
func renderEstate(w io.Writer, v estateView, all bool) error {
	switch v.State {
	case "placed":
	case "whole":
		return renderWholeEstate(w, v)
	case "no_map":
		_, err := fmt.Fprintf(w, "No estate map yet — %s.\n", v.Detail)
		return err
	case "unavailable", "unreadable":
		return errors.New(v.Detail)
	default:
		return fmt.Errorf("the node answered an estate map in state %q, which this build "+
			"does not know", v.State)
	}

	fmt.Fprintf(w, "Estate map, layout %d, epoch %d · %d of %d copies of each partition · "+
		"generation %s\n", v.Layout, v.Epoch, v.Copies, v.Replicas, v.Generation)
	var spaces []string
	total := 0
	for _, s := range v.Spaces {
		spaces = append(spaces, fmt.Sprintf("%s %d", s.Space, s.Partitions))
		total += s.Partitions
	}
	fmt.Fprintf(w, "  %d partitions: %s.\n", total, strings.Join(spaces, ", "))
	if v.Copies < v.Replicas {
		fmt.Fprintf(w, "  SHORT: the company asks for %d copies and the map has only %d "+
			"placeable data nodes, so every partition has fewer copies than configured.\n",
			v.Replicas, v.Copies)
	}
	if v.FailureDomain != "" && v.DomainLimited {
		fmt.Fprintf(w, "  DOMAIN-LIMITED: fewer %s values than copies, so some partitions "+
			"keep two copies in one %s.\n", v.FailureDomain, v.FailureDomain)
	}
	if b := v.Balance; b != nil && !b.Converged {
		fmt.Fprintf(w, "  Not converged after %d rounds: a member is %.1f%% off the partitions "+
			"its weight entitles it to, where the balance aimed within %.1f%%. SHARE says "+
			"which.\n", b.Rounds, b.DeviationPercent, b.TolerancePercent)
	}
	if h := v.Hold; h != nil {
		fmt.Fprintf(w, "  HELD until %s by %s", h.Until.UTC().Format(time.RFC3339), h.By)
		if h.Reason != "" {
			fmt.Fprintf(w, " (%s)", h.Reason)
		}
		fmt.Fprintln(w, ": no member is removed however long it is gone. "+
			"`crewlet estate release` ends it sooner.")
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tDOMAIN\tWEIGHT\tSHARE\tSERVING\tJOINING\tLEAVING\tPLACED\tABSENT\tSTORE")
	for _, m := range v.Members {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%.1f%%\t%d\t%d\t%d\t%s\t%s\t%s\n", m.Node, orDash(m.Domain),
			m.Weight, m.SharePercent, m.Serving, m.Joining, m.Leaving, estatePlaced(m),
			estateAbsence(m), estateStore(m.Live, m.Lease))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, r := range v.Removed {
		fmt.Fprintf(w, "\nREMOVED and remembered: %s (%s", r.Node, r.Reason)
		if r.Detail != "" {
			fmt.Fprintf(w, ": %s", r.Detail)
		}
		fmt.Fprintf(w, "), not seen for %d ticks. It rejoins on probation the next time it is "+
			"seen present and healthy; `crewlet estate in %s -confirm %s` vouches for it now.\n",
			r.Gone, r.Node, r.Node)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Partitions unserved %d · short of copies %d · holders joining %d · "+
		"leaving %d · moves in force %d\n", v.Unserved, v.Short, v.Joining, v.Leaving, v.Moves)
	listed := 0
	for _, p := range v.Partitions {
		if !all && p.settled() {
			continue
		}
		if listed == 0 {
			fmt.Fprintln(w)
			tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "PARTITION\tCOPIES\tTARGET\tHOLDERS\tMOVES")
		}
		listed++
		fmt.Fprintf(tw, "%s\t%d/%d\t%s\t%s\t%s\n", p.ID, p.Serving, p.Wanted,
			strings.Join(p.Target, ","), estateHolders(p), estateMoves(p))
	}
	if listed > 0 {
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if listed == 0 && !all {
		fmt.Fprintf(w, "Settled at epoch %d: every partition is held by its target, every "+
			"copy serving.\n", v.Epoch)
	}
	return nil
}

// renderWholeEstate prints layout 0: the sentence, and the data nodes that each
// hold the whole estate.
func renderWholeEstate(w io.Writer, v estateView) error {
	fmt.Fprintf(w, "Estate: %s.\n\n", v.Detail)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "NODE\t%s\tSTORE\n", strings.ToUpper(v.Partition))
	for _, h := range v.Holders {
		reports := h.Reports
		if h.Lease == nil {
			reports = "no estate lease"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", h.Node, orDash(reports), estateStore(h.Lease != nil, h.Lease))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(v.Holders) == 0 {
		fmt.Fprintln(w, "No live data node: nothing holds the estate from here.")
	}
	return nil
}

// estatePlaced is why the map places nothing on a member, or a dash: out, on
// probation with how far it has got, and the partitions an operator moved off
// it.
func estatePlaced(m estateMemberView) string {
	var why []string
	if m.Out {
		why = append(why, "out")
	}
	if p := m.Probation; p != nil {
		why = append(why, fmt.Sprintf("probation %d/%d", p.Present, p.PlacedAfterTicks))
	}
	if len(m.MovedOff) > 0 {
		why = append(why, fmt.Sprintf("moved off %d", len(m.MovedOff)))
	}
	if len(why) == 0 {
		return "-"
	}
	return strings.Join(why, ", ")
}

// estateAbsence is how far a member's absence has run.
func estateAbsence(m estateMemberView) string {
	a := m.Absence
	if a == nil {
		return "-"
	}
	s := fmt.Sprintf("%d/%d", a.Ticks, a.OutAfterTicks)
	if a.Reason != "" && a.Reason != "absent" {
		s += " " + a.Reason
	}
	if a.Present > 0 {
		s += fmt.Sprintf(", back %d", a.Present)
	}
	return s
}

// estateStore is what a node's estate lease says of its store: "no lease",
// "ok", "failed: why", or "unsaid" for a lease that does not say — which the map
// counts exactly as failed.
func estateStore(live bool, l *estateLeaseView) string {
	switch {
	case !live || l == nil:
		return "no lease"
	case l.Healthy == nil:
		return "unsaid"
	case !*l.Healthy:
		return "failed: " + orDash(l.Detail)
	case !l.Able:
		return "not counted: " + orDash(l.Detail)
	}
	return "ok"
}

// estateHolders is a partition's holders as `node:state`, with what the node's
// own lease says where it differs, and `!` on a node the map counts absent.
func estateHolders(p estatePartitionView) string {
	var out []string
	for _, h := range p.Holders {
		s := h.Node + ":" + h.State
		if h.Reports != "" && h.Reports != h.State {
			s += "(" + h.Reports + ")"
		}
		if !h.Able {
			s += "!"
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, " ")
}

// estateMoves is a partition's moves as `off node (by)` — `(by, waiting)` for
// one the members left could not hold the copy for, whose node is in the
// target again until a member returns.
func estateMoves(p estatePartitionView) string {
	var out []string
	for _, m := range p.Moves {
		who := m.By
		if m.Waiting {
			who += ", waiting"
		}
		out = append(out, "off "+m.Node+" ("+who+")")
	}
	if len(out) == 0 {
		return "-"
	}
	return strings.Join(out, ", ")
}

// estateAnswerView is a gesture route's answer as this command reads it.
type estateAnswerView struct {
	Landed bool   `json:"landed"`
	Epoch  uint64 `json:"epoch"`
	Member *struct {
		Out bool `json:"out"`
	} `json:"member"`
	Move *struct {
		By string `json:"by"`
	} `json:"move"`
	Target []string         `json:"target"`
	Hold   *objectsHoldView `json:"hold"`
	Hint   string           `json:"hint"`
}

// estateGesture posts one gesture and reports what the map now says, saying
// what a resend does when nobody answered — see [objectsGesture].
func estateGesture(client *nodeClient, path string, restarts bool,
	stderr io.Writer) (estateAnswerView, error) {

	var answer estateAnswerView
	if err := client.post(context.Background(), path, &answer); err != nil {
		var lost noAnswer
		if errors.As(err, &lost) {
			resend := "running the same gesture again is safe, since one the map " +
				"already says changes nothing and writes nothing."
			if restarts {
				resend = "running the same hold again is safe, but it replaces the one in " +
					"force: its length is counted from the resend, not from this attempt."
			}
			fmt.Fprintln(stderr, "The node did not answer, so whether the map changed "+
				"is unknown. `crewlet estate map` reads it; "+resend)
		}
		return answer, err
	}
	if !answer.Landed {
		return answer, fmt.Errorf("the gesture did not land: %s", answer.Hint)
	}
	return answer, nil
}

// estateMemberGesture is `crewlet estate out` and `in`.
func estateMemberGesture(args []string, stdout, stderr io.Writer, out bool) error {
	verb := "in"
	if out {
		verb = "out"
	}
	node, rest := splitSubject(args)
	var confirm, reason *string
	client, err := nodeClientFor(rest, "estate "+verb, stderr, func(fs *flag.FlagSet) {
		confirm = fs.String("confirm", "",
			"repeat the node id — this moves its copies of the company's partitions across the fleet")
		if out {
			reason = fs.String("reason", "", "why, recorded on the map beside who did it")
		}
	})
	if err != nil {
		return err
	}
	if node == "" || *confirm != node {
		fmt.Fprintf(stderr, "usage: crewlet estate %s <node-id> -confirm <node-id>\n", verb)
		return fmt.Errorf("taking a node out of the estate map or putting it back moves its "+
			"copies of the company's partitions across the fleet — repeat the node id in "+
			"-confirm to run %s", verb)
	}
	query := url.Values{"confirm": {node}}
	if reason != nil && strings.TrimSpace(*reason) != "" {
		query.Set("reason", strings.TrimSpace(*reason))
	}
	answer, err := estateGesture(client, fmt.Sprintf("/estate/%s/%s?%s", verb,
		url.PathEscape(node), query.Encode()), false, stderr)
	if err != nil {
		return err
	}
	switch {
	case out:
		fmt.Fprintf(stdout, "%s is out of the estate map: no partition's target names it, so "+
			"each copy it holds is rebuilt on another member while it keeps serving, and then "+
			"released.\n", node)
		fmt.Fprintf(stdout, "  Keep it running until `crewlet estate map` shows it serving, "+
			"joining and leaving nothing; then it may be stopped.\n")
	case answer.Member == nil:
		fmt.Fprintf(stdout, "%s is no longer held back: the map places partitions on it the "+
			"next time its maintainer sees it present and healthy.\n", node)
	default:
		fmt.Fprintf(stdout, "%s is back in the estate map: partitions are placed on it again.\n",
			node)
	}
	return nil
}

// unconfirmedByGeneration is err, unless it is the node's refusal of a hold or
// a release confirmed by no map generation — which it says in this command's
// own words, naming the flag.
//
// THE NODE JUDGES THE CONFIRMATION, and this command never does first: a
// generation is something only a map has, so whether one was repeated means
// nothing until the node has said there is a map. Checked here before sending,
// an operator at layout 0 — where `crewlet estate map` prints no generation,
// since there is no map — was sent to copy one that does not exist, and never
// heard the node say there is nothing to hold.
func unconfirmedByGeneration(err error, verb string) error {
	var refusal *nodeRefusal
	if !errors.As(err, &refusal) || refusal.Code != "confirm_required" {
		return err
	}
	return fmt.Errorf("repeat the estate map's generation, which the first line of "+
		"`crewlet estate map` prints, in -confirm to run %s: it acts on the whole map, and "+
		"the generation is what says it is the map of the fleet you meant", verb)
}

// estateHold is `crewlet estate hold`.
func estateHold(args []string, stdout, stderr io.Writer) error {
	var length *time.Duration
	var confirm, reason *string
	client, err := nodeClientFor(args, "estate hold", stderr, func(fs *flag.FlagSet) {
		length = fs.Duration("for", 0, "how long to hold the map, at most "+
			shortDuration(membership.MaxHold)+"; required")
		confirm = fs.String("confirm", "", "repeat the estate map's generation, which "+
			"`crewlet estate map` prints — a hold acts on the whole map; the node asks for "+
			"it only where there is a map")
		reason = fs.String("reason", "", "why, recorded on the map beside who held it")
	})
	if err != nil {
		return err
	}
	if *length <= 0 {
		fmt.Fprintln(stderr, "usage: crewlet estate hold -for DURATION -confirm GENERATION "+
			"[-reason TEXT]")
		return fmt.Errorf("name how long to hold the map in -for, like 30m or 2h (at most "+
			"%s): while it holds, a member that is gone keeps every partition it holds a "+
			"copy short", shortDuration(membership.MaxHold))
	}
	query := url.Values{"for": {length.String()}}
	if *confirm != "" {
		query.Set("confirm", *confirm)
	}
	if r := strings.TrimSpace(*reason); r != "" {
		query.Set("reason", r)
	}
	answer, err := estateGesture(client, "/estate/hold?"+query.Encode(), true, stderr)
	if err != nil {
		return unconfirmedByGeneration(err, "hold")
	}
	if answer.Hold == nil {
		return errors.New("the node answered the hold as landed and names no hold in " +
			"force: read `crewlet estate map` before relying on it")
	}
	fmt.Fprintf(stdout, "The estate map is held until %s: no member is removed however "+
		"long it is gone. It ends by itself; `crewlet estate release` ends it sooner.\n",
		answer.Hold.Until.UTC().Format(time.RFC3339))
	return nil
}

// estateRelease is `crewlet estate release`.
func estateRelease(args []string, stdout, stderr io.Writer) error {
	var confirm *string
	client, err := nodeClientFor(args, "estate release", stderr, func(fs *flag.FlagSet) {
		confirm = fs.String("confirm", "", "repeat the estate map's generation, which "+
			"`crewlet estate map` prints — a release acts on the whole map; the node asks "+
			"for it only where there is a map")
	})
	if err != nil {
		return err
	}
	path := "/estate/release"
	if *confirm != "" {
		path += "?" + url.Values{"confirm": {*confirm}}.Encode()
	}
	if _, err := estateGesture(client, path, false, stderr); err != nil {
		return unconfirmedByGeneration(err, "release")
	}
	fmt.Fprintf(stdout, "The hold is released: a member gone for %d ticks or more is "+
		"removed at the map maintainer's next tick.\n", membership.OutTicks)
	return nil
}

// estateMove is `crewlet estate move`, and its cancel.
func estateMove(args []string, stdout, stderr io.Writer) error {
	partition, rest := splitSubject(args)
	var from, confirm, reason *string
	var cancel *bool
	client, err := nodeClientFor(rest, "estate move", stderr, func(fs *flag.FlagSet) {
		from = fs.String("from", "", "the node whose copy of the partition moves; required")
		confirm = fs.String("confirm", "", "repeat the -from node — the copy is rebuilt "+
			"elsewhere and the one there released")
		reason = fs.String("reason", "", "why, recorded on the map beside who moved it")
		cancel = fs.Bool("cancel", false, "lift the move instead, so the partition's target "+
			"may name the node again")
	})
	if err != nil {
		return err
	}
	if partition == "" || *from == "" || *confirm != *from {
		fmt.Fprintln(stderr, "usage: crewlet estate move <partition> -from <node-id> "+
			"-confirm <node-id> [-reason TEXT] [-cancel]")
		return errors.New("name the partition, the node its copy moves off in -from, and " +
			"repeat that node in -confirm: a move rebuilds the copy on another member and " +
			"then releases the one on that node")
	}
	query := url.Values{"from": {*from}, "confirm": {*confirm}}
	path := "/estate/move/" + url.PathEscape(partition)
	if *cancel {
		path += "/cancel"
	} else if r := strings.TrimSpace(*reason); r != "" {
		query.Set("reason", r)
	}
	answer, err := estateGesture(client, path+"?"+query.Encode(), false, stderr)
	if err != nil {
		return err
	}
	if *cancel {
		fmt.Fprintf(stdout, "The move of %s off %s is lifted: its target may name %s "+
			"again.\n", partition, *from, *from)
		return nil
	}
	fmt.Fprintf(stdout, "%s moves off %s: its target is now %s, so the copy is rebuilt "+
		"there and then released on %s. It stays in force until `crewlet estate move %s "+
		"-from %s -confirm %s -cancel`, or %s leaves the map.\n", partition, *from,
		strings.Join(answer.Target, ", "), *from, partition, *from, *from, *from)
	return nil
}
