package statelog

import (
	"context"
	"fmt"
)

// A NODE'S STANDING ON A LOG, READ OFF THE LOG ITSELF.
//
// # Why the rows are not enough, for the one reader this is for
//
// A node's eviction is a record on each identity-claiming log, and every node
// applies it into that domain's own table — which is what the trim and the
// write fence read, and for a node whose applier is running that table is the
// answer. It is not the answer for a node a peer re-anchored past. Such a node's
// applier stops before the first record of the new generation, so a record
// written after that stop — the eviction of the peer that stranded it — never
// reaches its rows; and whether that peer is evicted is exactly what it has to
// know for the peer's generation to stop counting as the fleet's. With the rows
// as the only source, the one gesture that releases such a fleet was one no
// stranded node could ever see, and every node waited for a donor that had been
// decommissioned.
//
// So the question is asked of the log, which is where the eviction lives: the
// LAST record on the node's eviction subject is its current standing on that
// log — an eviction, or the readmission that inverted one — whether or not this
// node's applier has reached it. A subject holding nothing (never gated, or
// trimmed) answers found=false, and the caller falls back to the rows, which
// then hold whatever the trim removed.
//
// ONE THING THE APPLIER ASKS THAT THIS DOES NOT: whether the record's own
// writer was evicted at that position, which drops it on every node. The write
// path's fence 0 refuses an evicted writer before it can publish, so such a
// record exists only where that fence was defeated — and the answer here is
// about the operator's latest decision, which is what the fleet-generation and
// reanchor questions it serves are asking.

// EvictionProbe is a domain whose evictions and readmissions can be read
// straight off its log, for [EvictedOnLog].
//
// OPTIONAL, like every seam a domain declares only when it has the thing: a
// domain with no eviction gate has no subject to read, and a node's standing
// there is nothing at all.
type EvictionProbe interface {
	// EvictionSubject is the subject a node's evictions and readmissions
	// are published on, in the domain's own naming.
	EvictionSubject(node string) Subject

	// Evicts decodes one record from that subject, reporting true for an
	// eviction and false for a readmission.
	Evicts(payload []byte) (bool, error)
}

// StandingLog is a log as a standing read needs it: the per-subject probe and a
// read of one record back.
type StandingLog interface {
	LastSeq(ctx context.Context, subject string) (seq uint64, found bool, err error)
	LogReader
}

// EvictedOnLog reports node's standing on domain d's log as the last record on
// its eviction subject states it: evicted true for an eviction and false for a
// readmission, and found false when the subject holds nothing or d keeps no
// eviction gate.
func EvictedOnLog(ctx context.Context, d Domain, log StandingLog, node string) (evicted, found bool, err error) {
	probe, gated := d.(EvictionProbe)
	if !gated {
		return false, false, nil
	}
	subject := d.Stream().SubjectPrefix + "." + probe.EvictionSubject(node).String()
	seq, held, err := log.LastSeq(ctx, subject)
	if err != nil {
		return false, false, fmt.Errorf("statelog: read %s's eviction subject on %s: %w",
			node, d.Stream().Name, err)
	}
	if !held {
		return false, false, nil
	}
	got, payload, _, ok, err := log.At(ctx, seq)
	switch {
	case err != nil:
		return false, false, fmt.Errorf("statelog: read %s's standing at sequence %d "+
			"of %s: %w", node, seq, d.Stream().Name, err)
	case !ok:
		// TRIMMED BETWEEN THE TWO READS: what the trim removed, the rows
		// hold, which is the caller's fallback for found=false.
		return false, false, nil
	case got != subject:
		return false, false, fmt.Errorf("statelog: the broker named sequence %d of %s "+
			"as the last on %s and holds a record of %s there", seq,
			d.Stream().Name, subject, got)
	}
	evicts, err := probe.Evicts(payload)
	if err != nil {
		return false, false, fmt.Errorf("statelog: decode %s's standing at sequence "+
			"%d of %s: %w", node, seq, d.Stream().Name, err)
	}
	return evicts, true, nil
}

// GenerationOpeners reads, off the log, which node opened each generation of
// domain d above `above`: the writer of the generation record on each one's
// subject. It walks up from the generation after `above` through `through` —
// the highest the caller already knows of — and past it for as long as each
// subject holds a record.
//
// # Why the log, when the register names every node's generation
//
// Because a reanchor appends its record before it commits its checkpoint and
// before it publishes its position, so a node that died in between opened a
// generation no row names. A later reanchor that derived that number from the
// register alone would lose to the dead node's record on every attempt — the
// append refused, the record read back, a writer that is not this node — and
// the operator would have no generation left to open.
//
// A domain that keeps no generation record answers an empty map: there is
// nothing on its log to find.
func GenerationOpeners(ctx context.Context, d Domain, enc GenerationEncoder,
	log StandingLog, above, through uint32) (map[uint32]string, error) {

	out := map[uint32]string{}
	prefix := d.Stream().SubjectPrefix + "."
	for gen := above + 1; gen > above && gen <= MaxGeneration; gen++ {
		subject, keeps := enc.GenerationSubject(gen)
		if !keeps {
			return out, nil
		}
		seq, held, err := log.LastSeq(ctx, prefix+subject.String())
		if err != nil {
			return nil, fmt.Errorf("statelog: read %s's generation %d subject: %w",
				d.Stream().Name, gen, err)
		}
		if !held {
			if gen >= through {
				return out, nil
			}
			continue
		}
		_, payload, _, ok, err := log.At(ctx, seq)
		switch {
		case err != nil:
			return nil, fmt.Errorf("statelog: read %s's generation %d record at "+
				"sequence %d: %w", d.Stream().Name, gen, seq, err)
		case !ok:
			continue
		}
		env, err := d.Envelope(payload)
		if err != nil {
			return nil, fmt.Errorf("statelog: decode %s's generation %d record at "+
				"sequence %d: %w", d.Stream().Name, gen, seq, err)
		}
		out[gen] = env.Writer
	}
	return out, nil
}
