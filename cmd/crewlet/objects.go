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
	"github.com/crewlet/crewlet/internal/objstore/upkeep"
)

// `crewlet objects` — where the company's files are placed, and the operator's
// gestures on the placement map.
//
// # Why through a running node
//
// The map is one record in the coordination store, which on the default
// topology is the engine's own embedded broker: a command that opened it from
// outside would find nothing or corrupt it — see nodeclient.go. So `status`
// reads the fleet view (`GET /fleet`), and the four gestures are clients of the
// `/objects/*` routes, exactly as `crewlet retention evict` is of its route.
//
// # What status is FOR
//
// The question an operator runs it with is "may I stop the next data node
// yet?" — during a rolling restart, and while taking a node away for good,
// where the runbook is: take it out, wait until no member has a chunk pending
// and the node holds no strays, then stop it. The table carries every number
// that question turns on, and the lines under it say in words what is still
// being waited for, because a reader who has to know which column decides it
// has not been told.

// runObjects dispatches `crewlet objects`.
func runObjects(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "status":
		return objectsStatus(rest, stdout, stderr)
	case "out":
		return objectsMemberGesture(rest, stdout, stderr, true)
	case "in":
		return objectsMemberGesture(rest, stdout, stderr, false)
	case "hold":
		return objectsHold(rest, stdout, stderr)
	case "release":
		return objectsRelease(rest, stdout, stderr)
	case "", "help":
		fmt.Fprintln(stderr, "usage: crewlet objects status|out|in|hold|release "+
			"[<config.yaml>] [-url] [-token]")
		return flag.ErrHelp
	default:
		return fmt.Errorf("unknown objects command %q", sub)
	}
}

// objectsView is the fleet view's objects block as this command reads it.
//
// A SHAPE OF THIS COMMAND'S OWN, for `retention status`'s reason: a struct
// shared with the writer would tie an older binary to a newer node's answer.
// Its tests serve the WRITER's renderer (queries.RenderObjects), so the two
// cannot drift unnoticed.
type objectsView struct {
	State           string               `json:"state"`
	Generation      string               `json:"generation"`
	Epoch           uint64               `json:"epoch"`
	Replicas        int                  `json:"replicas"`
	Copies          int                  `json:"copies"`
	PGs             int                  `json:"pgs"`
	FailureDomain   string               `json:"failure_domain"`
	DistinctDomains int                  `json:"distinct_domains"`
	DomainLimited   bool                 `json:"domain_limited"`
	Hold            *objectsHoldView     `json:"hold"`
	DegradedGroups  int                  `json:"degraded_groups"`
	Balance         *objectsBalanceView  `json:"balance"`
	Members         []objectsMemberView  `json:"members"`
	Removed         []objectsRemovalView `json:"removed"`
}

type objectsBalanceView struct {
	Epoch            uint64  `json:"epoch"`
	DeviationPercent float64 `json:"deviation_percent"`
	TolerancePercent float64 `json:"tolerance_percent"`
	Converged        bool    `json:"converged"`
	Rounds           int     `json:"rounds"`
}

type objectsHoldView struct {
	Until  time.Time `json:"until"`
	By     string    `json:"by"`
	Reason string    `json:"reason"`
}

type objectsMemberView struct {
	Node         string  `json:"node"`
	Weight       int     `json:"weight"`
	Domain       string  `json:"domain"`
	Out          bool    `json:"out"`
	OutBy        string  `json:"out_by"`
	OutReason    string  `json:"out_reason"`
	SharePercent float64 `json:"share_percent"`
	Live         bool    `json:"live"`
	Probation    *struct {
		Present          int    `json:"present"`
		PlacedAfterTicks int    `json:"placed_after_ticks"`
		Reason           string `json:"reason"`
		Detail           string `json:"detail"`
	} `json:"probation"`
	Absence *struct {
		Ticks         int    `json:"ticks"`
		OutAfterTicks int    `json:"out_after_ticks"`
		Present       int    `json:"present"`
		Reason        string `json:"reason"`
		Detail        string `json:"detail"`
	} `json:"absence"`
	Health *struct {
		State       string  `json:"state"`
		Detail      string  `json:"detail"`
		UsedPercent float64 `json:"used_percent"`
	} `json:"health"`
	Repair *struct {
		Epoch       uint64 `json:"epoch"`
		Completed   bool   `json:"completed"`
		Pending     int    `json:"pending"`
		Unreachable int    `json:"unreachable"`
	} `json:"repair"`
	Scrub *struct {
		Rotten     int    `json:"rotten"`
		Unreadable int    `json:"unreadable"`
		Error      string `json:"error"`
	} `json:"scrub"`
	Strays *int `json:"strays"`
}

// placeable reports whether the map places copies on the member: neither
// taken out nor on probation — the one question every rule the map places by
// asks, and the one "settled" is judged over.
func (m objectsMemberView) placeable() bool { return !m.Out && m.Probation == nil }

type objectsRemovalView struct {
	Node             string `json:"node"`
	Reason           string `json:"reason"`
	Detail           string `json:"detail"`
	Gone             int    `json:"gone"`
	ForgetAfterTicks int    `json:"forget_after_ticks"`
	PlacedAfterTicks int    `json:"placed_after_ticks"`
}

// objectsStatus is `crewlet objects status`.
func objectsStatus(args []string, stdout, stderr io.Writer) error {
	var asJSON *bool
	client, err := nodeClientFor(args, "objects status", stderr, func(fs *flag.FlagSet) {
		asJSON = fs.Bool("json", false,
			"print the fleet view's objects block as the node answered it, for a "+
				"script waiting to stop the next data node")
	})
	if err != nil {
		return err
	}
	var answer struct {
		Objects json.RawMessage `json:"objects"`
	}
	if err := client.get(context.Background(), "/fleet", &answer); err != nil {
		return err
	}
	if len(answer.Objects) == 0 {
		return errors.New("this node's fleet view carries no object placement: it " +
			"runs no object store, so ask a node that does with -url")
	}
	if *asJSON {
		_, err := fmt.Fprintf(stdout, "%s\n", answer.Objects)
		return err
	}
	var view objectsView
	if err := json.Unmarshal(answer.Objects, &view); err != nil {
		return fmt.Errorf("the node's objects block is not the shape this build "+
			"reads: %w", err)
	}
	return renderObjects(stdout, view)
}

// renderObjects prints the block: the map line, the hold, the members, and what
// to wait for.
func renderObjects(w io.Writer, v objectsView) error {
	switch v.State {
	case "placed":
	case "unavailable":
		return errors.New("the placement map could not be read from the coordination " +
			"store; files already stored are where they were — ask again, or ask " +
			"another node with -url")
	case "no_map":
		_, err := fmt.Fprintln(w, "No placement map yet: no data node has joined the "+
			"object store, so no file can be stored. One is written within seconds of "+
			"the first data node starting.")
		return err
	case "unreadable":
		return errors.New("the placement map was written by a newer build than this " +
			"node's, which does not read it: ask a node running that build with -url, " +
			"or once the upgrade has finished")
	default:
		return fmt.Errorf("the node answered an object placement in state %q, which "+
			"this build does not know", v.State)
	}

	fmt.Fprintf(w, "Placement map epoch %d · %d of %d copies of every chunk · %d groups\n",
		v.Epoch, v.Copies, v.Replicas, v.PGs)
	if v.Copies < v.Replicas {
		fmt.Fprintf(w, "  SHORT: the company asks for %d copies and the map has only %d "+
			"placeable data nodes, so every file has fewer copies than configured.\n",
			v.Replicas, v.Copies)
	}
	if v.FailureDomain != "" {
		fmt.Fprintf(w, "  Copies are spread across %s: %d distinct values.\n",
			v.FailureDomain, v.DistinctDomains)
		if v.DomainLimited {
			fmt.Fprintf(w, "  DOMAIN-LIMITED: fewer %s values than copies, so some groups "+
				"keep two copies in one %s — losing it costs them both.\n",
				v.FailureDomain, v.FailureDomain)
		}
	}
	if line := balanceLine(v); line != "" {
		fmt.Fprintf(w, "  %s\n", line)
	}
	if h := v.Hold; h != nil {
		fmt.Fprintf(w, "  HELD until %s by %s", h.Until.UTC().Format(time.RFC3339), h.By)
		if h.Reason != "" {
			fmt.Fprintf(w, " (%s)", h.Reason)
		}
		fmt.Fprintln(w, ": no member is removed however long it is gone. "+
			"`crewlet objects release` ends it sooner.")
	}
	if v.DegradedGroups > 0 {
		fmt.Fprintf(w, "  DEGRADED: %d of %d groups have a copy on a member that is "+
			"absent or whose store has failed.\n", v.DegradedGroups, v.PGs)
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tDOMAIN\tWEIGHT\tSHARE\tPLACED\tABSENT\tHEALTH\tPENDING")
	for _, m := range v.Members {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%.1f%%\t%s\t%s\t%s\t%s\n", m.Node, orDash(m.Domain),
			m.Weight, m.SharePercent, memberPlaced(m), memberAbsence(m), memberHealth(m),
			memberPending(m, v.Epoch))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, m := range v.Members {
		p := m.Probation
		if p == nil {
			continue
		}
		// READ FROM AT ONCE, PLACED ON LATER: the member is in the map so
		// readers and repairs find what it held when it went, and takes no
		// new copies until it has proven itself stable.
		fmt.Fprintf(w, "\nON PROBATION: %s, removed for being %s and back %d of the %d "+
			"ticks in a row that place on it again. It is read from and repaired from "+
			"meanwhile and placed on nothing; a tick that misses it removes it again. "+
			"`crewlet objects in %s -confirm %s` vouches for it now.\n",
			m.Node, orDash(p.Reason), p.Present, p.PlacedAfterTicks, m.Node, m.Node)
	}
	for _, r := range v.Removed {
		fmt.Fprintf(w, "\nREMOVED and remembered: %s (%s", r.Node, r.Reason)
		if r.Detail != "" {
			fmt.Fprintf(w, ": %s", r.Detail)
		}
		fmt.Fprintf(w, "), not seen for %d ticks. The next time it is seen present and "+
			"healthy it rejoins on probation — read from at once, placed on after %d ticks "+
			"in a row; gone %d more ticks, it is forgotten and joins as a new node would. "+
			"`crewlet objects in %s -confirm %s` vouches for it now.\n",
			r.Gone, r.PlacedAfterTicks, max(r.ForgetAfterTicks-r.Gone, 0), r.Node, r.Node)
	}
	for _, m := range v.Members {
		for _, line := range scrubFindings(m) {
			fmt.Fprintf(w, "\nSCRUB %s\n", line)
		}
	}

	fmt.Fprintln(w)
	waits, done := objectsWaits(v)
	if len(waits) == 0 {
		fmt.Fprintf(w, "Settled at epoch %d: every member holds what the map places on "+
			"it. One data node may be stopped.\n", v.Epoch)
	} else {
		fmt.Fprintln(w, "Before stopping another data node, wait for:")
		for _, line := range waits {
			fmt.Fprintf(w, "  - %s\n", line)
		}
	}
	if len(done) > 0 {
		fmt.Fprintln(w)
	}
	for _, line := range done {
		fmt.Fprintf(w, "%s\n", line)
	}
	return nil
}

// shortDuration is a whole duration without its empty minutes and seconds —
// "1h" rather than "1h0m0s" — for the constants these lines name.
//
// ONLY A UNIT'S OWN ZERO IS DROPPED: the seconds when a larger unit precedes
// them, then the minutes when the hours do. Trimming the bare text "0s" and
// "0m" cut into a unit's own digits — "10s" came out "1", "50m0s" "5" and
// "1h30m0s" "1h3".
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// objectsWaits is what the fleet is still doing, in words — and, for each
// member taken out, whether it may be stopped for good.
//
// SETTLED MEANS EVERY PLACED COPY IS WHERE THE MAP SAYS: every member that is
// not out is live, its store has not failed, and its last repair completed AT
// THE MAP'S OWN EPOCH with nothing pending. An earlier epoch's pass says
// nothing about the groups the current map moved, and a pass that did not
// reach every group says nothing about the ones it missed — which is exactly
// how the maintainer judges a member clean enough to split the groups.
//
// A MEMBER TAKEN OUT IS EMPTY when, on top of that, it holds no strays: every
// chunk it held is confirmed on the members the map places it on. Its lease
// carries that count only from a collection that walked every slot at the map
// epoch it places by — ABSENT until one has, because a count from before the
// map moved was taken against a placement that no longer stands, and a member
// taken out at epoch N held no strays at all under N-1. That collection is the
// one every node runs once the fleet has settled at the epoch, so it starts
// within about half a minute of the last member finishing its repair rather
// than on the hourly schedule.
func objectsWaits(v objectsView) (waits, done []string) {
	settled := true
	for _, m := range v.Members {
		// ONLY WHAT THE MAP PLACES ON: a member out or on probation holds
		// no copy anybody is counting on, which is how the maintainer and
		// every node's collection judge a settled fleet too.
		if !m.placeable() {
			continue
		}
		var why string
		switch {
		case !m.Live && m.Absence != nil:
			why = fmt.Sprintf("%s to come back: it has been counted gone %d of the %d "+
				"ticks that remove it", m.Node, m.Absence.Ticks, m.Absence.OutAfterTicks)
		case !m.Live:
			why = fmt.Sprintf("%s to come back: it holds no objects lease", m.Node)
		case m.Health != nil && m.Health.State == "failed":
			why = fmt.Sprintf("%s's store to recover or the map to take it out: it "+
				"reports itself failed (%s)", m.Node, m.Health.Detail)
		case m.Repair == nil:
			why = fmt.Sprintf("%s to report a repair pass", m.Node)
		case m.Repair.Epoch != v.Epoch || !m.Repair.Completed:
			finished := "finished"
			if !m.Repair.Completed {
				finished = "did not finish"
			}
			why = fmt.Sprintf("%s to complete a repair at epoch %d: its last pass ran "+
				"at epoch %d and %s", m.Node, v.Epoch, m.Repair.Epoch, finished)
		case m.Repair.Pending > 0:
			why = fmt.Sprintf("%s to fetch the %d chunks the map places on it that it "+
				"does not hold", m.Node, m.Repair.Pending)
		}
		if why != "" {
			settled = false
			waits = append(waits, why)
		}
	}
	if v.DegradedGroups > 0 {
		waits = append(waits, fmt.Sprintf("the %d degraded groups to have every copy on "+
			"a present member", v.DegradedGroups))
	}
	for _, m := range v.Members {
		if !m.Out {
			continue
		}
		switch {
		case m.Strays == nil:
			done = append(done, fmt.Sprintf("%s is out and has not reported its strays "+
				"at epoch %d yet: they are counted by a collection that walks its whole "+
				"disk at the map's epoch, which starts within about half a minute of the "+
				"fleet settling there. Keep it running until it has reported them and "+
				"they are 0.", m.Node, v.Epoch))
		case *m.Strays > 0:
			done = append(done, fmt.Sprintf("%s is out and still holds %d strays: keep "+
				"it running until they reach 0 — each goes once every member the map "+
				"places it on has confirmed an intact copy, and its collector tries "+
				"again on a backoff capped at %s until none is left.", m.Node, *m.Strays,
				shortDuration(upkeep.CollectInterval)))
		case !settled:
			// NO STRAYS IS NOT EMPTY YET while a member is still fetching:
			// the count is as of the out member's last collection, and a
			// member that has not finished its repair may still be reading
			// from it.
			done = append(done, fmt.Sprintf("%s is out and held no strays at its last "+
				"collection, but the members above are still settling: keep it running "+
				"until they are.", m.Node))
		default:
			done = append(done, fmt.Sprintf("%s is out and holds no strays: every chunk "+
				"it held is on the members the map places it on, so it may be stopped "+
				"for good.", m.Node))
		}
	}
	return waits, done
}

// memberPlaced is why the map places nothing on a member, or a dash for one
// it places on: taken out, on probation with how far it has got, or both — an
// operator's decision and the maintainer's, each ending on its own.
func memberPlaced(m objectsMemberView) string {
	var why []string
	if m.Out {
		why = append(why, "out")
	}
	if p := m.Probation; p != nil {
		why = append(why, fmt.Sprintf("probation %d/%d", p.Present, p.PlacedAfterTicks))
	}
	if len(why) == 0 {
		return "-"
	}
	return strings.Join(why, ", ")
}

// scrubFindings is what a member's scrub found that an operator acts on, in
// words: chunks rotten or unreadable this cycle, and what stopped it.
//
// UNREADABLE IS SAID APART FROM ROTTEN because it points at the disk rather
// than at the bytes: the scrub steps past every chunk it could not read, so a
// count that keeps rising is a disk failing chunk by chunk, and nothing else on
// this screen would show it.
func scrubFindings(m objectsMemberView) []string {
	sc := m.Scrub
	if sc == nil {
		return nil
	}
	var lines []string
	if sc.Rotten > 0 || sc.Unreadable > 0 {
		lines = append(lines, fmt.Sprintf("%s: %d chunks rotten and %d its disk would not "+
			"read this cycle. Each one the store could remove is fetched again by repair "+
			"from a good copy; a count of unreadable chunks that keeps rising is a disk "+
			"failing chunk by chunk.", m.Node, sc.Rotten, sc.Unreadable))
	}
	if sc.Error != "" {
		lines = append(lines, fmt.Sprintf("%s: the scrub stopped (%s) and retries "+
			"shortly.", m.Node, sc.Error))
	}
	return lines
}

// balanceLine is how evenly the map spreads its copies over the members'
// weights, in words, or empty for a map nothing has measured.
//
// NOT CONVERGED IS SAID AS WHAT IT MEANS: a weight the map cannot keep. The
// balance writes the best map it measured either way, so this line is the one
// place the command says a member holds more or less than its weight asks — and
// the SHARE column is where to read which.
func balanceLine(v objectsView) string {
	b := v.Balance
	switch {
	case b == nil:
		return ""
	case b.Epoch != v.Epoch:
		return fmt.Sprintf("Balance last measured at epoch %d: the map's placement changed "+
			"since without one — a split, measured on the maintainer's next tick.", b.Epoch)
	case b.Converged:
		return fmt.Sprintf("Balanced within %.1f%%: every placeable member holds that close to "+
			"the copies its weight entitles it to (a balance aims within %.0f%%).",
			b.DeviationPercent, b.TolerancePercent)
	case b.Rounds == 0:
		return fmt.Sprintf("Within %.1f%% of every member's weight: a split's placement, "+
			"measured and left as it was, since one is rebalanced only past twice the "+
			"%.0f%% a balance aims within.", b.DeviationPercent, b.TolerancePercent)
	}
	return fmt.Sprintf("NOT CONVERGED: after %d rounds a member is still %.1f%% off the copies "+
		"its weight entitles it to, where a balance aims within %.0f%% — a failure domain "+
		"too crowded to spread, or members too light to be counted in whole copies. "+
		"SHARE says which.", b.Rounds, b.DeviationPercent, b.TolerancePercent)
}

// memberAbsence is how far a member's absence has run, with how long it has
// been back where it has — a member back for fewer ticks than clear the run
// keeps every tick it counted.
func memberAbsence(m objectsMemberView) string {
	a := m.Absence
	if a == nil {
		if !m.Live {
			return "no lease"
		}
		return "-"
	}
	s := fmt.Sprintf("%d/%d", a.Ticks, a.OutAfterTicks)
	if a.Reason != "" && a.Reason != "absent" {
		s += " " + a.Reason
	}
	if m.Live && a.Present > 0 {
		s += fmt.Sprintf(", back %d", a.Present)
	}
	return s
}

// memberHealth is the store's own state and how full its volume is, or a dash
// for a member that reported none — never "ok" for one that said nothing.
func memberHealth(m objectsMemberView) string {
	h := m.Health
	if h == nil {
		return "-"
	}
	return fmt.Sprintf("%s %.1f%%", h.State, h.UsedPercent)
}

// memberPending is the chunks its last repair left unheld, marked where that
// pass is not the map's own epoch or did not finish — the two readings that
// make a zero mean nothing. A member the map places nothing on has nothing
// pending: what it holds instead is its strays — a member taken out sheds
// them, and one on probation KEEPS them by rule, for when the map places on it
// again.
func memberPending(m objectsMemberView, epoch uint64) string {
	if !m.placeable() {
		switch {
		case m.Strays == nil:
			return "-"
		case m.Out:
			return fmt.Sprintf("strays %d", *m.Strays)
		default:
			return fmt.Sprintf("kept %d", *m.Strays)
		}
	}
	r := m.Repair
	if r == nil {
		return "-"
	}
	s := fmt.Sprintf("%d", r.Pending)
	if r.Epoch != epoch {
		s += fmt.Sprintf(" @%d", r.Epoch)
	}
	if !r.Completed {
		s += " unfinished"
	}
	return s
}

// objectsAnswerView is a gesture route's answer as this command reads it.
type objectsAnswerView struct {
	Landed bool   `json:"landed"`
	Epoch  uint64 `json:"epoch"`
	Member *struct {
		Out bool `json:"out"`
	} `json:"member"`
	Hold *objectsHoldView `json:"hold"`
	Hint string           `json:"hint"`
}

// objectsGesture posts one gesture and reports what the map now says.
//
// A GESTURE NOBODY ANSWERED is safe to send again, and the message says why —
// which is not the same for every gesture. An out, an in and a release the map
// already says change nothing, the record of who made the first one included,
// so the compare-and-set answers them as landed without a write. A hold always
// writes: it is the operator stating now how much longer the maintenance
// needs, so one sent again replaces the hold in force and ends its length
// after the resend. restarts says which kind this is.
func objectsGesture(client *nodeClient, path string, restarts bool,
	stderr io.Writer) (objectsAnswerView, error) {

	var answer objectsAnswerView
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
				"is unknown. `crewlet objects status` reads it; "+resend)
		}
		return answer, err
	}
	if !answer.Landed {
		return answer, fmt.Errorf("the gesture did not land: %s", answer.Hint)
	}
	return answer, nil
}

// objectsMemberGesture is `crewlet objects out` and `in`.
func objectsMemberGesture(args []string, stdout, stderr io.Writer, out bool) error {
	verb := "in"
	if out {
		verb = "out"
	}
	node, rest := splitSubject(args)
	var confirm, reason *string
	client, err := nodeClientFor(rest, "objects "+verb, stderr, func(fs *flag.FlagSet) {
		confirm = fs.String("confirm", "",
			"repeat the node id — this moves its share of the company's files across the fleet")
		if out {
			reason = fs.String("reason", "", "why, recorded on the map beside who did it")
		}
	})
	if err != nil {
		return err
	}
	if node == "" || *confirm != node {
		fmt.Fprintf(stderr, "usage: crewlet objects %s <node-id> -confirm <node-id>\n", verb)
		return fmt.Errorf("taking a member out or putting it back moves its share of "+
			"the company's files across the fleet — repeat the node id in -confirm to "+
			"run %s", verb)
	}
	query := url.Values{"confirm": {node}}
	if reason != nil && strings.TrimSpace(*reason) != "" {
		query.Set("reason", strings.TrimSpace(*reason))
	}
	answer, err := objectsGesture(client, fmt.Sprintf("/objects/%s/%s?%s", verb,
		url.PathEscape(node), query.Encode()), false, stderr)
	if err != nil {
		return err
	}
	switch {
	case out:
		fmt.Fprintf(stdout, "%s is out of the placement map at epoch %d: nothing new is "+
			"placed on it, and its share is copied to the other members while it keeps "+
			"serving what it holds.\n", node, answer.Epoch)
		fmt.Fprintf(stdout, "  Keep it running until `crewlet objects status` shows no "+
			"chunk pending on any member and no strays on %s; then it may be stopped.\n",
			node)
	case answer.Member == nil:
		fmt.Fprintf(stdout, "%s is no longer held back: the map places on it the next "+
			"time its maintainer sees it present and healthy.\n", node)
	default:
		fmt.Fprintf(stdout, "%s is back in the placement map at epoch %d: its share "+
			"moves back to it.\n", node, answer.Epoch)
	}
	return nil
}

// objectsHold is `crewlet objects hold`.
//
// NO DEFAULT LENGTH, for the route's reason: a hold pins every gone member in
// the map, its groups a copy short, and how long that is acceptable is the
// operator's to say.
func objectsHold(args []string, stdout, stderr io.Writer) error {
	var length *time.Duration
	var reason *string
	client, err := nodeClientFor(args, "objects hold", stderr, func(fs *flag.FlagSet) {
		length = fs.Duration("for", 0, "how long to hold the map, at most "+
			shortDuration(membership.MaxHold)+"; required")
		reason = fs.String("reason", "", "why, recorded on the map beside who held it")
	})
	if err != nil {
		return err
	}
	if *length <= 0 {
		fmt.Fprintln(stderr, "usage: crewlet objects hold -for DURATION [-reason TEXT]")
		return fmt.Errorf("name how long to hold the map in -for, like 30m or 2h (at "+
			"most %s): while it holds, a member that is gone keeps its groups a copy "+
			"short", shortDuration(membership.MaxHold))
	}
	query := url.Values{"for": {length.String()}}
	if r := strings.TrimSpace(*reason); r != "" {
		query.Set("reason", r)
	}
	answer, err := objectsGesture(client, "/objects/hold?"+query.Encode(), true, stderr)
	if err != nil {
		return err
	}
	if answer.Hold == nil {
		return errors.New("the node answered the hold as landed and names no hold in " +
			"force: read `crewlet objects status` before relying on it")
	}
	fmt.Fprintf(stdout, "The placement map is held until %s: no member is removed "+
		"however long it is gone. It ends by itself; `crewlet objects release` ends it "+
		"sooner.\n", answer.Hold.Until.UTC().Format(time.RFC3339))
	return nil
}

// objectsRelease is `crewlet objects release`.
func objectsRelease(args []string, stdout, stderr io.Writer) error {
	client, err := nodeClientFor(args, "objects release", stderr, nil)
	if err != nil {
		return err
	}
	if _, err := objectsGesture(client, "/objects/release", false, stderr); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "The hold is released: a member gone for %d ticks or more is "+
		"removed at the map maintainer's next tick.\n", membership.OutTicks)
	return nil
}
