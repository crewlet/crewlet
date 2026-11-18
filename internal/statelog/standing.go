package statelog

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/recordseal"
)

// A NODE'S STANDING ON A LOG, READ OFF THE LOG ITSELF.
//
// The domain's rows answer for a node whose applier runs, not for one a peer
// re-anchored past: its applier stops before the new generation, so the
// eviction of the peer that stranded it, which ends that peer's generation
// counting as the fleet's, never reaches its rows. So the LAST record on the
// node's eviction subject (an eviction, or a readmission) is its standing,
// applied or not; an empty subject (never gated, or trimmed) answers
// found=false and the caller falls back to the rows. Unlike the applier, this
// ignores whether that record's writer was evicted: fence 0 refuses such a
// writer, and the question is the operator's latest decision.

// EvictionProbe is a domain whose evictions and readmissions can be read
// straight off its log, for [EvictedOnLog].
// Optional: a domain with no eviction gate has no subject to read.
type EvictionProbe interface {
	// EvictionSubject is the subject a node's evictions and readmissions
	// are published on, in the domain's own naming.
	EvictionSubject(node string) Subject

	// Evicts decodes one record from that subject, reporting true for an
	// eviction and false for a readmission.
	Evicts(payload []byte) (bool, error)
}

// StandingLog is a log as a standing read needs it: the per-subject probe and a
// read of one record's BODY back — a [VerifiedReader], because every read here
// decodes what it reads and the log holds signed frames.
type StandingLog interface {
	LastSeq(ctx context.Context, subject string) (seq uint64, found bool, err error)
	VerifiedReader
}

// SubjectLog is a domain's raw log as the broker answers it: a record by its
// sequence (the signed frame) and the last sequence on one subject.
type SubjectLog interface {
	LogReader
	LastSeq(ctx context.Context, subject string) (seq uint64, found bool, err error)
}

// StandingOf is log as a standing read takes it: its own per-subject probe,
// and every record read back opened under v — the one way to build a
// [StandingLog] from a domain's log, so no caller writes the pairing for
// itself.
func StandingOf(log SubjectLog, v *recordseal.Verifier) StandingLog {
	return standing{VerifiedReader: VerifiedLog{Log: log, Verifier: v}, last: log}
}

// standing is a [StandingLog] made of a verified reader and whatever answers
// the per-subject probe: a domain's raw log ([StandingOf]), or a publisher's
// appender beside the verified reader it already holds.
type standing struct {
	VerifiedReader
	last interface {
		LastSeq(ctx context.Context, subject string) (seq uint64, found bool, err error)
	}
}

func (s standing) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	return s.last.LastSeq(ctx, subject)
}

// EvictedOnLog reports node's standing on one of domain d's logs — the one
// spec names, which log reads — as the last record on its eviction subject
// states it: evicted true for an eviction and false for a readmission, and
// found false when the subject holds nothing or d keeps no eviction gate.
func EvictedOnLog(ctx context.Context, d Domain, spec StreamSpec, log StandingLog,
	node string) (evicted, found bool, err error) {
	probe, gated := d.(EvictionProbe)
	if !gated {
		return false, false, nil
	}
	_, evicted, found, err = lastStanding(ctx, probe, spec.SubjectPrefix, spec.Name, log, node)
	return evicted, found, err
}

// lastStanding is the last record on node's eviction subject of the log named
// stream, whose wire subjects carry prefix: its sequence, whether it evicts,
// and found false when the subject holds nothing. The sequence lets
// [Publisher.unresolved] place it against another node's copy of its
// operation: below the copy it decides it, above it says nothing.
func lastStanding(ctx context.Context, probe EvictionProbe, prefix, stream string,
	log StandingLog, node string) (seq uint64, evicts, found bool, err error) {

	subject := wireSubject(prefix, probe.EvictionSubject(node))
	seq, held, err := log.LastSeq(ctx, subject)
	if err != nil {
		return 0, false, false, fmt.Errorf("statelog: read %s's eviction subject on %s: %w",
			node, stream, err)
	}
	if !held {
		return 0, false, false, nil
	}
	got, payload, _, ok, err := log.At(ctx, seq)
	switch {
	case err != nil:
		return 0, false, false, fmt.Errorf("statelog: read %s's standing at sequence %d "+
			"of %s: %w", node, seq, stream, err)
	case !ok:
		// TRIMMED BETWEEN THE TWO READS: the rows, the caller's fallback,
		// hold what the trim removed.
		return 0, false, false, nil
	case got != subject:
		return 0, false, false, fmt.Errorf("statelog: the broker named sequence %d of %s "+
			"as the last on %s and holds a record of %s there", seq,
			stream, subject, got)
	}
	evicts, err = probe.Evicts(payload)
	if err != nil {
		return 0, false, false, fmt.Errorf("statelog: decode %s's standing at sequence "+
			"%d of %s: %w", node, seq, stream, err)
	}
	return seq, evicts, true, nil
}

// GenerationOpeners reads, off the log, which node opened each generation of
// domain d above `above`: the writer of the generation record on each one's
// subject. It walks up from the generation after `above` through `through` —
// the highest the caller already knows of — and past it for as long as each
// subject holds a record.
//
// The log, not the register: a reanchor appends its record before it commits
// its checkpoint or publishes its position, so a node that died in between
// opened a generation no row names, and a number taken from the register would
// lose to that record on every attempt. A domain that keeps no generation
// record answers an empty map.
func GenerationOpeners(ctx context.Context, d Domain, spec StreamSpec, enc GenerationEncoder,
	log StandingLog, above, through uint32) (map[uint32]string, error) {

	out := map[uint32]string{}
	for gen := above + 1; gen > above && gen <= MaxGeneration; gen++ {
		subject, keeps := enc.GenerationSubject(gen)
		if !keeps {
			return out, nil
		}
		seq, held, err := log.LastSeq(ctx, wireSubject(spec.SubjectPrefix, subject))
		if err != nil {
			return nil, fmt.Errorf("statelog: read %s's generation %d subject: %w",
				spec.Name, gen, err)
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
				"sequence %d: %w", spec.Name, gen, seq, err)
		case !ok:
			continue
		}
		env, err := d.Envelope(payload)
		if err != nil {
			return nil, fmt.Errorf("statelog: decode %s's generation %d record at "+
				"sequence %d: %w", spec.Name, gen, seq, err)
		}
		out[gen] = env.Writer
	}
	return out, nil
}

// OwnGeneration is the sequence of THIS node's own record opening generation
// gen of domain d, and false when the subject holds no record, another node's,
// or d keeps none: how a re-run of an interrupted reanchor finds its own
// earlier append.
//
// A reanchor that failed after its append leaves that record and nothing
// committed, and the re-run reads the log's end above it. Its walk back for
// records the rows lack would meet its own record first and refuse, hiding
// every later record beneath it, and a checkpoint at the end would leave the
// record applied nowhere, so both are bounded one below it
// ([ReanchorInputs.Opened]). Another node's record there bounds nothing: the
// transition reads it back and refuses ([Reanchor]).
func OwnGeneration(ctx context.Context, d Domain, spec StreamSpec, enc GenerationEncoder,
	log StandingLog, gen uint32, nodeID string) (uint64, bool, error) {

	subject, keeps := enc.GenerationSubject(gen)
	if !keeps {
		return 0, false, nil
	}
	seq, held, err := log.LastSeq(ctx, wireSubject(spec.SubjectPrefix, subject))
	if err != nil {
		return 0, false, fmt.Errorf("statelog: read %s's generation %d subject: %w",
			spec.Name, gen, err)
	}
	if !held {
		return 0, false, nil
	}
	_, payload, _, ok, err := log.At(ctx, seq)
	switch {
	case err != nil:
		return 0, false, fmt.Errorf("statelog: read %s's generation %d record at "+
			"sequence %d: %w", spec.Name, gen, seq, err)
	case !ok:
		return 0, false, nil
	}
	env, err := d.Envelope(payload)
	if err != nil {
		return 0, false, fmt.Errorf("statelog: decode %s's generation %d record at "+
			"sequence %d: %w", spec.Name, gen, seq, err)
	}
	return seq, env.Writer == nodeID, nil
}
