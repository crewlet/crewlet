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
// apply gate, and a domain that claims no identity has none — and that a domain
// claiming identity reads its node gates off its log ([statelog.EvictionProbe]).
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
// it publishes. What it cannot see from those is a second gate — a purge —
// answering true; [RunGates] holds that from a gating domain's own purge, and
// [RunEvictionGates] from every other gate an eviction-only domain names.
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
		if _, probes := c.Domain.(statelog.EvictionProbe); !probes {
			errs = append(errs, fmt.Errorf("%s claims identity and cannot read its "+
				"node gates off its log (statelog.EvictionProbe) — a node a peer "+
				"re-anchored past never applies that peer's eviction, so the log "+
				"is the only place it can learn of it", name))
		}
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
// eviction and pass the fences that stop every ordinary write. And the purge
// must SAY it installs one, or the case could not tell its node-gate answer
// from an ordinary write's.
//
// EXPORTED AND RETURNING THE VERDICT, so [RunGates] can bend the candidate's
// own domain and require the bend to be reported.
func GateNodeGates(c GateCandidate) error {
	return errors.Join(judgeNodeGates(c.Domain, []judgedRecord{
		{label: "its create", body: func() ([]byte, error) {
			return c.Create("node-gates-obj", gateCounted, "node-gates-create")
		}},
		{label: "its ordinary write", body: func() ([]byte, error) {
			return c.Write("node-gates-obj", gateCounted, "node-gates-write")
		}},
		{label: "its purge", installs: true, body: func() ([]byte, error) {
			return c.Purge("node-gates-obj", gateCounted, "node-gates-purge")
		}},
		{label: "its eviction of a node", nodeGate: true, installs: true,
			body: func() ([]byte, error) {
				return c.Evict(gateEvicted, gateCounted, "node-gates-evict")
			}},
		{label: "its readmission of a node", nodeGate: true, installs: true,
			body: func() ([]byte, error) {
				return c.Readmit(gateEvicted, gateCounted, "node-gates-readmit")
			}},
	})...)
}

// judgedRecord is one record a gating family's node-gate case encodes, and the
// two gate answers its domain owes for it.
type judgedRecord struct {
	label string
	body  func() ([]byte, error)

	// nodeGate is what [statelog.Domain.NodeGate] must answer.
	nodeGate bool

	// installs says [statelog.Domain.InstallsGate] must answer true. False
	// asks nothing: an ordinary write is held to its node-gate answer only.
	installs bool
}

// judgeNodeGates is the ONE judging loop both gating families' node-gate cases
// run — [GateNodeGates] over a domain with a deletion marker and
// [EvictionGateNodeGates] over one whose only apply-nowhere gate is the
// eviction — so the rule they hold a domain to is written once: the eviction
// probe, then each record's node-gate answer, then its apply-gate answer where
// it owes one. Written in each family, the two had already begun to differ in
// which of those they asked.
func judgeNodeGates(d statelog.Domain, records []judgedRecord) []error {
	name := d.Name()
	var errs []error
	if _, probes := d.(statelog.EvictionProbe); !probes {
		errs = append(errs, fmt.Errorf("%s installs an eviction gate and cannot "+
			"read its node gates off its log (statelog.EvictionProbe)", name))
	}
	for _, r := range records {
		payload, err := r.body()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: encode %s: %w", name, r.label, err))
			continue
		}
		env, err := d.Envelope(payload)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: the envelope of %s does not "+
				"decode: %w", name, r.label, err))
			continue
		}
		if got := d.NodeGate(env); got != r.nodeGate {
			errs = append(errs, fmt.Errorf("%s answers NodeGate = %v for %s, want "+
				"%v — only a node's eviction and readmission may pass the fences "+
				"and the reserve a node gate is excused", name, got, r.label, r.nodeGate))
		}
		// AN APPLY GATE THAT DOES NOT SAY SO is one a build that cannot
		// read it defers rather than stops at, and the case could not then
		// tell its node-gate answer from an ordinary write's.
		if r.installs && !d.InstallsGate(env) {
			errs = append(errs, fmt.Errorf("%s says %s installs no apply gate — a "+
				"build that cannot decode it would defer it rather than stop",
				name, r.label))
		}
	}
	return errs
}

// applyGateNodeGateLiar is the bend a domain's OTHER apply gate is what
// catches — the one [RunEvictionGates] cannot run on a domain whose eviction is
// its only gate, since there its apply gate and its node gate are one answer.
const applyGateNodeGateLiar = "answers with its apply gate, so a purge is a node gate"

// nodeGateLiars bend a gating candidate's own domain each way [judgeNodeGates]'s
// answers could be written wrong — which records are node gates, and that every
// gate it encodes says it installs one — for [RunGates] and [RunEvictionGates]
// to require every one reported. A clause no bend reaches is one an edit can
// drop with every suite still green.
var nodeGateLiars = map[string]func(statelog.Domain) statelog.Domain{
	applyGateNodeGateLiar: func(d statelog.Domain) statelog.Domain {
		return applyGateAsNodeGate{bent{d}}
	},
	"calls nothing a node gate, not even its eviction": func(d statelog.Domain) statelog.Domain {
		return noNodeGate{bent{d}}
	},
	"says nothing installs an apply gate, not even its eviction": func(d statelog.Domain) statelog.Domain {
		return noInstallsGate{bent{d}}
	},
}

// bent is a gating domain one of whose answers a liar replaces. It forwards the
// eviction probe of the domain it wraps, so each bend is reported for what it
// bends rather than for having lost the probe on the way in — a wrapper that
// dropped it would be caught by the probe check whatever else it did.
type bent struct{ statelog.Domain }

func (b bent) EvictionSubject(node string) statelog.Subject {
	if probe, ok := b.Domain.(statelog.EvictionProbe); ok {
		return probe.EvictionSubject(node)
	}
	return statelog.Subject{}
}

func (b bent) Evicts(payload []byte) (bool, error) {
	if probe, ok := b.Domain.(statelog.EvictionProbe); ok {
		return probe.Evicts(payload)
	}
	return false, fmt.Errorf("%s reads no node gate off its log", b.Name())
}

// applyGateAsNodeGate answers the node-gate question with the apply gate's
// answer — true for the purge as well as the eviction.
type applyGateAsNodeGate struct{ bent }

func (d applyGateAsNodeGate) NodeGate(env statelog.Envelope) bool {
	return d.InstallsGate(env)
}

// noNodeGate calls nothing a node gate, so the publisher refuses its eviction.
type noNodeGate struct{ bent }

func (noNodeGate) NodeGate(statelog.Envelope) bool { return false }

// noInstallsGate says no record installs an apply gate, so a build that cannot
// decode its eviction defers it rather than stopping. Its node-gate answers are
// the domain's own, which leaves the apply-gate clause the only one that can
// report it.
type noInstallsGate struct{ bent }

func (noInstallsGate) InstallsGate(statelog.Envelope) bool { return false }
