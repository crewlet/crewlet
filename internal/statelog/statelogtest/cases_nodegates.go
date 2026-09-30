package statelogtest

import (
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// runNodeGates reports what [NodeGates] found.
func runNodeGates(t *testing.T, new Factory) {
	t.Helper()
	t.Run("a node gate is the domain's own eviction and installs a gate",
		func(t *testing.T) {
			if err := NodeGates(new(t)); err != nil {
				t.Fatal(err)
			}
		})
}

// NodeGates certifies what a domain says is a NODE GATE ([statelog.Domain]'s
// NodeGate): its own eviction and readmission are, every node gate installs an
// apply gate, and a domain that claims no identity has none.
//
// # Why the publisher's answer rests on this
//
// A write flagged [statelog.Request.NodeGate] is excused three fences — the
// passed generation, a peer's truncated rows and the gate reserve — and the
// publisher holds the flag to this predicate, over the record it is about to
// append. A domain whose own eviction answers false has an eviction the
// publisher refuses, so the gesture that unpins a full log can never be made;
// one that answers true for anything else lets that record past every fence
// an ordinary write is held to. And a node gate that installed no apply gate
// would be DEFERRED by a build that could not decode it, rather than stop its
// applier — a node that goes on applying the records of a machine the fleet
// has evicted.
//
// EXPORTED AND RETURNING THE VERDICT, for [Declaration]'s reason. The records
// it reads are the candidate's: its eviction pair, and one record of every kind
// it publishes. What it cannot see from those is a second gate — the purge —
// answering true; [RunGates] holds that, from a gating domain's own purge.
func NodeGates(c Candidate) error {
	name := c.Domain.Name()
	var errs []error
	judge := func(label string, payload []byte, err error) (bool, error) {
		if err != nil {
			return false, fmt.Errorf("%s: encode %s: %w", name, label, err)
		}
		env, err := c.Domain.Envelope(payload)
		if err != nil {
			return false, fmt.Errorf("%s: the envelope of %s does not decode: %w",
				name, label, err)
		}
		gates := c.Domain.NodeGate(env)
		switch {
		case gates && !c.Domain.InstallsGate(env):
			return gates, fmt.Errorf("%s calls %s a node gate and says it installs "+
				"no apply gate — a build that cannot decode it would defer it "+
				"rather than stop, and go on applying the evicted node's "+
				"records", name, label)
		case gates && !c.Domain.ClaimsIdentity():
			return gates, fmt.Errorf("%s claims no identity and calls %s a node "+
				"gate — no node is counted on its log, so none is evicted from "+
				"it, and a record flagged one passes the fences and the reserve "+
				"every other write is held to", name, label)
		}
		return gates, nil
	}

	for _, kind := range c.Kinds {
		label := fmt.Sprintf("a %s record", kind)
		payload, encoded := c.Encode(kind, "node-gates-"+kind, "node-gates-"+kind,
			c.Domain.RecordVersion())
		if _, err := judge(label, payload, encoded); err != nil {
			errs = append(errs, err)
		}
	}

	if c.Domain.ClaimsIdentity() {
		if c.EncodeGate == nil {
			errs = append(errs, fmt.Errorf("%s claims identity and supplies no "+
				"eviction record, so nothing can show the publisher takes its "+
				"eviction as a node gate", name))
		}
		for _, readmit := range []bool{false, true} {
			if c.EncodeGate == nil {
				break
			}
			label := "its eviction of a node"
			if readmit {
				label = "its readmission of a node"
			}
			payload, err := c.EncodeGate("node-gates-away", readmit)
			gates, err := judge(label, payload, err)
			switch {
			case err != nil:
				errs = append(errs, err)
			case !gates:
				errs = append(errs, fmt.Errorf("%s does not call %s a node gate — "+
					"the publisher refuses a node gate whose record is not one, so "+
					"the gesture that unpins a log a gone node filled could never "+
					"be written", name, label))
			}
		}
	}
	return errors.Join(errs...)
}

// GateNodeGates certifies, over a gating domain's own records, that exactly its
// eviction and readmission are node gates — and that its create, its ordinary
// write and its PURGE are not.
//
// The purge is the case [NodeGates] cannot reach: it installs an apply gate
// too, so a predicate spelled as the apply gate's passes every other check
// here while letting a purge flagged by mistake spend the reserve kept for an
// eviction and pass the fences that stop every ordinary write.
//
// EXPORTED AND RETURNING THE VERDICT, so [RunGates] can bend the candidate's
// own domain and require the bend to be reported.
func GateNodeGates(c GateCandidate) error {
	name := c.Domain.Name()
	var errs []error
	for _, r := range []struct {
		label string
		body  func() ([]byte, error)
		want  bool
	}{
		{"its create", func() ([]byte, error) {
			return c.Create("node-gates-obj", gateCounted, "node-gates-create")
		}, false},
		{"its ordinary write", func() ([]byte, error) {
			return c.Write("node-gates-obj", gateCounted, "node-gates-write")
		}, false},
		{"its purge", func() ([]byte, error) {
			return c.Purge("node-gates-obj", gateCounted, "node-gates-purge")
		}, false},
		{"its eviction of a node", func() ([]byte, error) {
			return c.Evict(gateEvicted, gateCounted, "node-gates-evict")
		}, true},
		{"its readmission of a node", func() ([]byte, error) {
			return c.Readmit(gateEvicted, gateCounted, "node-gates-readmit")
		}, true},
	} {
		payload, err := r.body()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: encode %s: %w", name, r.label, err))
			continue
		}
		env, err := c.Domain.Envelope(payload)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: the envelope of %s does not "+
				"decode: %w", name, r.label, err))
			continue
		}
		if got := c.Domain.NodeGate(env); got != r.want {
			errs = append(errs, fmt.Errorf("%s answers NodeGate = %v for %s, want "+
				"%v — only a node's eviction and readmission may pass the fences "+
				"and the reserve a node gate is excused", name, got, r.label, r.want))
		}
	}
	return errors.Join(errs...)
}

// nodeGateLiars bend a gating candidate's own domain each way its node-gate
// answer could be written wrong, for [RunGates] to require every one reported.
var nodeGateLiars = map[string]func(statelog.Domain) statelog.Domain{
	"answers with its apply gate, so a purge is a node gate": func(d statelog.Domain) statelog.Domain {
		return applyGateAsNodeGate{d}
	},
	"calls nothing a node gate, not even its eviction": func(d statelog.Domain) statelog.Domain {
		return noNodeGate{d}
	},
}

// applyGateAsNodeGate answers the node-gate question with the apply gate's
// answer — true for the purge as well as the eviction.
type applyGateAsNodeGate struct{ statelog.Domain }

func (d applyGateAsNodeGate) NodeGate(env statelog.Envelope) bool {
	return d.InstallsGate(env)
}

// noNodeGate calls nothing a node gate, so the publisher refuses its eviction.
type noNodeGate struct{ statelog.Domain }

func (noNodeGate) NodeGate(statelog.Envelope) bool { return false }
