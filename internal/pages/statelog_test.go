package pages_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
)

// THE PAGES DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, and it is the
// THIRD domain to run it — the second STRICT one.
//
// What the pair before it could not exercise is the case a single strict
// domain makes invisible: with one strict domain and one compacted, a
// checkpoint, a readiness input and an identity claim are each one value, and
// "per domain" and "per node" are indistinguishable. Two strict domains on one
// node is what tells them apart.
func TestThePagesDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain: pages.Domain{},
			// A NIL SKILL DETECTOR, which is the case the Divergent
			// class exists for: this build has no parser wired, so it
			// writes skill = 0 where a build with one writes 1, and
			// the identity claim must still hold. A suite run with a
			// detector would prove the easy half.
			Applier: pages.NewApplier("suite-node", nil, nil),
			// Migrate is nil: these tables ship in the replicated
			// estate's own migration, so a fresh store already has
			// them. A domain that created its tables from test code
			// would be a schema the suite proved and the migration
			// did not.
			Encode: encodeSuiteRecord,
			Kinds:  suiteKinds(),
		}
	})
}

// suiteKinds is every kind, derived from the enum rather than typed again.
func suiteKinds() []string {
	out := make([]string, 0, len(pages.ObjectKinds))
	for _, k := range pages.ObjectKinds {
		out = append(out, string(k))
	}
	return out
}

// encodeSuiteRecord builds one valid record of each kind at an arbitrary
// version.
//
// IT ENCODES AT ANY VERSION, including one above this build's, because that is
// exactly what a newer peer publishes and what this build has to retain.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	// A TITLE'S ID IS COMPOSED — "<CONTAINER>.<normalised title>" — so the
	// plain token the suite hands out is placed into the title position
	// rather than used raw. The suite never asserts the id round-trips; it
	// asserts the domain can encode, envelope and apply what it built.
	subject := pages.Subject{Kind: pages.ObjectKind(kind), ID: id}
	if pages.ObjectKind(kind) == pages.KindTitle {
		subject = pages.TitleSubject(suiteContainer, "Page "+id)
	}
	rec := pages.MutationRecord{
		RecordEnvelope: pages.RecordEnvelope{
			V:         version,
			OpID:      opID,
			Subject:   subject,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Gen:       1,
			Writer:    "suite-node",
		},
		Actor:     "suite",
		ActorKind: pages.AuthorOperator,
	}
	op, payload, scope := suitePayload(pages.ObjectKind(kind), id)
	rec.Op = op
	rec.Scope = scope
	if payload != nil {
		body, err := marshal(payload)
		if err != nil {
			return nil, err
		}
		rec.Mutation = body
	}
	return pages.Encode(rec)
}

// suitePayload is one valid (op, payload, scope) per kind.
//
// # Why the container, the title and the page are the first three kinds
//
// The suite publishes the FIRST THREE kinds a candidate declares, twice each,
// in order. So the declaration order decides what gets certified — and this
// domain's three are chosen to be a real sequence rather than three unrelated
// records: a container, a create on a title whose payload names the page, and
// then a patch on that page. A page patch on a page no create wrote is a
// malformed record under a strict replay, so any other order would certify a
// failure.
func suitePayload(kind pages.ObjectKind, id string) (pages.OpKind, any, pages.ScopeSet) {
	switch kind {
	case pages.KindContainer:
		return pages.OpPatch, pages.ContainerPayload{
			V: pages.DocumentVersion, Key: id, Name: "The " + id + " space",
		}, pages.ScopeSet{Subject: true}
	case pages.KindTitle:
		return pages.OpCreate, pages.CreatePayload{
			V: pages.DocumentVersion, PageID: id, Container: suiteContainer,
			Title: "Page " + id, Body: "# " + id + "\n\nbody\n",
			Status: pages.StatusPublished, Author: "suite",
		}, pages.ScopeSet{Subject: true, Container: suiteContainer}
	case pages.KindPage:
		body := "# " + id + "\n\nedited\n"
		return pages.OpPatch, pages.PagePatch{
			V: pages.DocumentVersion, Body: &body,
			Labels: []string{"runbook", "suite"},
		}, pages.ScopeSet{Subject: true, Container: suiteContainer}
	case pages.KindEviction:
		return pages.OpEviction, pages.Eviction{
			V: pages.GateRecordVersion, NodeID: id, EvictedBy: "suite",
			EvictedAt: time.Unix(1_700_000_000, 0).UTC(),
		}, pages.ScopeSet{Subject: true}
	case pages.KindGeneration:
		return pages.OpGeneration, pages.Generation{
			V: pages.GateRecordVersion, Generation: 2, By: "suite",
			StreamCreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		}, pages.ScopeSet{Subject: true}
	case pages.KindBarrier:
		return pages.OpBarrier, nil, pages.ScopeSet{Subject: true}
	}
	return pages.OpPatch, nil, pages.ScopeSet{Subject: true}
}

// suiteContainer is the space every suite page lives in.
const suiteContainer = "SUITE"

// marshal is json.Marshal, named so the encode path reads as one step.
func marshal(v any) ([]byte, error) { return json.Marshal(v) }
