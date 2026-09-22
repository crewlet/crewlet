package iamdomain_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
)

// THE IDENTITY ESTATE IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, and it is the
// FIFTH domain to run it — the fourth STRICT one.
//
// What the four before it could not exercise is the case this one is: a domain
// whose ROWS ARE CIPHERTEXT. The suite's determinism half compares table
// CONTENT rather than counting rows, which it has to here — two nodes writing
// different ciphertext for one record would pass a count and fail the claim
// that their tables are byte-identical. That the comparison holds at all is
// what says the sealing happens at the WRITER rather than on each node, which
// is the property the whole removal mechanism rests on.
//
// It is also the first domain whose declaration NARROWS: it runs on ingress
// and workers and not on a seats-only satellite. The suite does not exercise
// participation — that is the engine's — but the declaration half checks the
// pairing that makes narrowing legal, which is that a domain gating seat
// admission may not be one a node might not run.
func TestTheIdentityDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain: iamdomain.Domain{},
			// NO SHREDDER, which is the honest shape here rather than a
			// saving: the suite has no key store, and a removal on a
			// node with none deletes the rows and leaves the key to a
			// peer. That is the documented behaviour of a nil one, so
			// the suite exercises it rather than a stub that pretends.
			Applier: iamdomain.NewApplier("suite-node", nil),
			// Migrate is nil: these tables ship in the replicated
			// estate's own migrations, so a fresh store already has
			// them. A domain that created its tables from test code
			// would be a schema the suite proved and the migration did
			// not.
			Encode: encodeSuiteRecord,
			Kinds:  suiteKinds(),
		}
	})
}

// suiteKinds is every kind this domain declares, as the suite takes them.
func suiteKinds() []string {
	out := make([]string, 0, len(iamdomain.ObjectKinds))
	for _, kind := range iamdomain.ObjectKinds {
		out = append(out, string(kind))
	}
	return out
}

func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	rec := iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V:         version,
			OpID:      opID,
			Subject:   iamdomain.Subject{Kind: iamdomain.ObjectKind(kind), ID: id},
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Gen:       1,
			Writer:    "suite-node",
		},
		Actor:     "suite",
		ActorKind: iam.KindMachine,
	}
	op, person, payload, scope := suitePayload(iamdomain.ObjectKind(kind), id)
	rec.Op = op
	rec.Person = person
	rec.Scope = scope
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		rec.Mutation = body
	}
	return iamdomain.Encode(rec)
}

// suitePerson is the one person every suite record is about, so the first
// three kinds are a real sequence rather than three unrelated records.
const suitePerson = "018f3a9c-0000-7000-8000-00000000f00d"

// suitePayload is one valid (op, person, payload, scope) per kind.
//
// # Why the person, the address and the login are the first three kinds
//
// The suite publishes the FIRST THREE kinds a candidate declares, twice each,
// in order. So the declaration order decides what gets certified — and this
// domain's three are chosen to be a real sequence: a person is minted, their
// address is claimed for them, and their login is claimed for them. A claim
// naming a person nobody minted is a record whose apply has nothing to attach
// to, so any other order would certify a failure rather than a domain.
func suitePayload(kind iamdomain.ObjectKind, id string) (
	iamdomain.OpKind, string, any, iamdomain.ScopeSet) {

	person := iamdomain.PeopleScope(suitePerson)
	switch kind {
	case iamdomain.KindPerson:
		return iamdomain.OpEnrol, id, iamdomain.Person{
			V: iamdomain.DocumentVersion, Kind: iam.KindPerson,
			Stage:      iam.StageActive,
			NameSealed: "sealed:name", EmailSealed: "sealed:email",
			Grants: []iam.Grant{iam.GrantConfigWrite},
		}, iamdomain.PeopleScope(id)
	case iamdomain.KindEmail:
		return iamdomain.OpClaim, suitePerson, iamdomain.Claim{
			V: iamdomain.DocumentVersion, Person: suitePerson,
			Sealed: "sealed:address",
		}, person
	case iamdomain.KindLogin, iamdomain.KindSeat:
		return iamdomain.OpClaim, suitePerson, iamdomain.Claim{
			V: iamdomain.DocumentVersion, Person: suitePerson,
		}, person
	case iamdomain.KindSession:
		return iamdomain.OpOpen, suitePerson, iamdomain.Session{
			V: iamdomain.DocumentVersion, Person: suitePerson, Epoch: 1,
			AbsoluteExpiresAt: time.Unix(1_800_000_000, 0).UTC(),
		}, person
	case iamdomain.KindBootstrap:
		return iamdomain.OpBootstrap, "", iamdomain.Bootstrap{
			V: iamdomain.DocumentVersion, ID: "suite-code",
			Verifier: "sealed:verifier", MintedBy: "suite-node",
			ExpiresAt: time.Unix(1_800_000_000, 0).UTC(),
		}, iamdomain.BucketScope(iamdomain.BootstrapBucket())
	case iamdomain.KindSweep:
		return iamdomain.OpSweep, "", iamdomain.Sweep{
			V: iamdomain.DocumentVersion, Bucket: 0,
		}, iamdomain.BucketScope(0)
	case iamdomain.KindEviction:
		return iamdomain.OpEviction, "", iamdomain.Eviction{
			V: iamdomain.GateRecordVersion, From: 1 << 20, By: "suite",
		}, iamdomain.RootScope()
	case iamdomain.KindGeneration:
		return iamdomain.OpGeneration, "", iamdomain.Generation{
			V: iamdomain.DocumentVersion, Generation: 1, By: "suite",
		}, iamdomain.RootScope()
	case iamdomain.KindBarrier:
		return iamdomain.OpBarrier, "", nil, iamdomain.RootScope()
	}
	return iamdomain.OpUpdate, suitePerson, nil, person
}
