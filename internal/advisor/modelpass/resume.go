package modelpass

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// A DEEP RUN RESUMES AFTER A TAKEOVER (ADR-0166). A run lasts minutes, longer
// than the duty's lease, and a holder that loses the lease stops it, leaving
// the person's request for the next holder. Each time where it stands moves
// (an item answered, a lead added, a read asked) the run writes a checkpoint:
// its agenda with each verdict, and each read it asked by the name it was
// cited under. The next holder starts from the newest: an item already
// answered keeps its verdict, and the run answers only the open ones, told
// which reads were made before so it reads again only what they need. The
// answers themselves are not kept, so nothing is cited from a read this
// holder did not make.

// Checkpoint is where a deep run stands.
type Checkpoint struct {
	Agenda []AgendaItem `json:"agenda"`
	Reads  []KeptRead   `json:"reads"`
}

// KeptRead is one read a run asked: the name it was cited under, and what to
// ask to read it again.
type KeptRead struct {
	As     string         `json:"as"`
	Name   string         `json:"name"`
	Params map[string]any `json:"params,omitempty"`
}

// resumedAgenda is fresh, the agenda read now, with every item kept answered
// before a takeover answered as it was, and the run's own items after it:
// how many items came answered.
func resumedAgenda(fresh, kept []AgendaItem) ([]AgendaItem, int) {
	answered := func(it AgendaItem) bool { return it.Verdict != "" && it.Verdict != VerdictOpen }
	out := slices.Clone(fresh)
	carried := 0
	for i := range out {
		at := slices.IndexFunc(kept, func(k AgendaItem) bool {
			return k.Source == out[i].Source && k.Question == out[i].Question && answered(k)
		})
		if at < 0 {
			continue
		}
		k := kept[at]
		out[i].Verdict, out[i].Note, out[i].Numbers, out[i].Read = k.Verdict, k.Note, k.Numbers, k.Read
		carried++
	}
	for _, k := range kept {
		if k.Source != AgendaModel {
			continue
		}
		k.ID = fmt.Sprintf("A%d", len(out)+1)
		if !answered(k) {
			k.Verdict, k.Note, k.Numbers, k.Read = "", "", []AgendaNumber{}, ""
		} else {
			carried++
		}
		out = append(out, k)
	}
	return out, carried
}

// lastRead is the highest read number reads were cited under, so a resumed
// run cites its own reads after them.
func lastRead(reads []KeptRead) int {
	n := 0
	for _, r := range reads {
		if v, err := strconv.Atoi(strings.TrimPrefix(r.As, "read_")); err == nil && v > n {
			n = v
		}
	}
	return n
}

// resumedSaid is what a resumed run is told of where it stands: which items
// keep their verdicts, and the reads made before.
func resumedSaid(c *Checkpoint, carried int) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nThis investigation was taken over by another server. %d agenda items keep the verdict "+
		"given before; answer the open ones.", carried)
	if len(c.Reads) > 0 {
		b.WriteString(" Before the takeover it read:")
		for _, r := range c.Reads {
			fmt.Fprintf(&b, " %s (%s);", r.As, askedAs(r.Name, r.Params))
		}
		fmt.Fprintf(&b, " those answers are not kept, so read again only what an open item needs. Your reads "+
			"are cited from read_%d on.", lastRead(c.Reads)+1)
	}
	return b.String()
}
