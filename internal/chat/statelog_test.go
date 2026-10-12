package chat_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE CHAT DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, and it is the
// FIFTH domain to run it — the third STRICT one.
//
// What it exercises that the four before it could not is a log carrying TWO
// ARBITRATION DISCIPLINES: the first two records the suite publishes contend
// on their own subjects, and the third is additive, on a subject that
// arbitrates nothing and whose apply mints a number from log order. A domain
// that declared those the same way would pass every case a single-discipline
// domain has, and diverge the first time two people talked at once.
func TestTheChatDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain: chat.Domain{},
			// A NIL OBSERVER, which is the live seam absent. The
			// socket sink belongs to the API layer, and a suite that
			// supplied one would be certifying that rather than the
			// applier — the contract is that nil is a no-op, and this
			// is the run that holds it to that.
			Applier: chat.NewApplier("suite-node", nil),
			// Migrate is nil: these tables ship in the replicated
			// estate's own migration
			// (`replicated/0043_the_chat_domain_lands.sql`), so a fresh
			// store already has them. A domain that created its tables
			// from test code would be a schema the suite proved and the
			// migration did not.
			Encode: encodeSuiteRecord,
			// THE VERSIONED-FIELD TABLE, empty while every shape here is
			// version 1's — and so no Carrying: the suite asks for a
			// record per field, and there is none to build.
			Fields: chat.VersionedFields(),
			Kinds:  suiteKinds(),
			Rows:   chat.NewRows,
			Write:  suiteWrite,
			// THE GATE RECORD, which this domain had an applier, a fence
			// and a table for and no writer — so the trim never learned an
			// evicted node had left this log.
			EncodeGate: encodeSuiteGate,
		}
	})
}

// suiteWrite is one write through chat's own [chat.Store] — the builder every
// gesture in the domain shares, which is where the framework's stamp is kept
// or lost.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	s, err := chat.NewStore(chat.Options{Publisher: pub, DB: db, Roster: suiteRoster{}})
	if err != nil {
		return err
	}
	_, err = s.CreateChannel(ctx, chat.Actor{Handle: "suite", Kind: chat.AuthorOperator},
		chat.NewChannel{Name: "suite-room", Kind: chat.KindPublic})
	return err
}

// suiteRoster is a company of one person, which is all a room's create asks
// about.
type suiteRoster struct{}

func (suiteRoster) Seat(handle string) (exists, human bool) { return handle == "suite", true }
func (suiteRoster) Lead(string, string) string              { return "" }

// encodeSuiteGate is the eviction record a peer's store publishes onto this
// log — or, with readmit, the inverse commit that takes the node back.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	op, payload, scope := suitePayload(chat.KindEviction, node)
	eviction := payload.(chat.Eviction)
	eviction.Readmitted = readmit
	body, err := json.Marshal(eviction)
	if err != nil {
		return nil, err
	}
	opID := "suite-evict-" + node
	if readmit {
		opID = "suite-readmit-" + node
	}
	return chat.Encode(chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			OpID: opID, Subject: chat.EvictionSubject(node), Op: op, Scope: scope,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Gen: 1,
			Writer: "suite-peer",
		},
		Mutation: body, Actor: "suite", ActorKind: chat.AuthorOperator,
	})
}

// suiteKinds is every kind, derived from the enum rather than typed again.
func suiteKinds() []string {
	out := make([]string, 0, len(chat.ObjectKinds))
	for _, k := range chat.ObjectKinds {
		out = append(out, string(k))
	}
	return out
}

// encodeSuiteRecord builds one valid record of each kind at an arbitrary
// version.
//
// IT ENCODES AT ANY VERSION, including one above this build's, because that is
// exactly what a newer peer publishes and what this build has to retain.
//
// EVERY ID IT DERIVES IS A PURE FUNCTION OF THE SUITE'S OWN, and that is not
// tidiness: the suite runs the same records into two fresh estates and
// compares what they hold, then runs them twice into one. A minted uuid
// anywhere here would write a second message on the second pass and report the
// applier as non-idempotent, which is the applier's contract failing for the
// test's reason.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	// A NAME CLAIM'S ID IS A COMPUTED TOKEN — the first sixteen bytes of
	// SHA-256 over the normalised name — so the plain token the suite
	// hands out is turned into a NAME and the subject is derived from
	// that. The applier recomputes the token from the payload and refuses
	// a record that arbitrated on one address while claiming another, so
	// using the suite's id raw would certify a record no node applies. The
	// suite never asserts the id round-trips; it asserts the domain can
	// encode, envelope and apply what it built.
	subject := chat.Subject{Kind: chat.ObjectKind(kind), ID: id}
	if chat.ObjectKind(kind) == chat.KindChannelName {
		name := suiteChannelName(id)
		if !chat.ValidName(name) {
			return nil, fmt.Errorf("the suite's object id %q became channel "+
				"name %q, which is not an address this build writes — a name "+
				"is lower-case alphanumerics and hyphens, at most %d bytes",
				id, name, chat.MaxChannelName)
		}
		subject = chat.ChannelNameSubject(name)
	}
	rec := chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			V:         version,
			OpID:      opID,
			Subject:   subject,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Gen:       1,
			Writer:    "suite-node",
		},
		Actor:     "suite",
		ActorKind: chat.AuthorOperator,
		// NO ROUTING SNAPSHOT. A wake is the write path's business and
		// nil is what "wakes nobody" means here — the same shape a
		// system line carries, and the one that keeps this suite about
		// the log.
	}
	op, payload, scope := suitePayload(chat.ObjectKind(kind), id)
	rec.Op = op
	rec.Scope = scope
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode the %s payload for %q: %w", op, id, err)
		}
		rec.Mutation = body
	}
	return chat.Encode(rec)
}

// suitePayload is one valid (op, payload, scope) per kind.
//
// # Why the name, the room and the message are the first three kinds
//
// The suite publishes the FIRST THREE kinds a candidate declares, twice each,
// in order — so [chat.ObjectKinds]'s declaration order decides what gets
// certified, and this domain's three are a real sequence rather than three
// unrelated records: a create on an ADDRESS whose payload names the room it
// makes, a patch on that room, and a message posted into it. A message in a
// room no create wrote is a malformed record under a strict replay, so any
// other order would certify a failure.
//
// THE CREATE'S SCOPE IS THE ONE WITH TWO TERMS, and it has to be: the record
// arbitrates on the name and its apply writes the room as well, so the
// exactly-the-subject sentinel would declare a blast radius smaller than what
// the record touches — and a node deferring it would let a later write to that
// room step over it.
func suitePayload(kind chat.ObjectKind, id string) (chat.OpKind, any, chat.ScopeSet) {
	switch kind {
	case chat.KindChannelName:
		name := suiteChannelName(id)
		create := chat.ChannelCreate{
			V: chat.DocumentVersion, ChannelID: id, Kind: chat.KindPublic,
			Name: name, Topic: "the suite's room",
			Members:       []chat.Member{{Handle: "suite", FollowAll: true}},
			CreatedBy:     "suite",
			CreatedByKind: chat.AuthorOperator,
		}
		return chat.OpCreate, create, chat.ScopeSet{Terms: []chat.ScopeTerm{
			{Kind: chat.TermName, ID: chat.ChannelToken(name)},
			{Kind: chat.TermChannel, ID: id},
		}}
	case chat.KindChannel:
		topic := "the suite's room, renamed in topic only"
		return chat.OpPatch, chat.ChannelPatch{
			V: chat.DocumentVersion, Topic: &topic,
		}, chat.ScopeSet{Subject: true}
	case chat.KindMessage:
		return chat.OpPost, chat.MessagePost{
			V: chat.DocumentVersion, MessageID: "message-" + id,
			Body: "the suite says something in " + id,
			// NO MENTIONS AND NO COLLECTIVE FLAG: both are routing,
			// and the record this suite publishes wakes nobody.
			Author: "suite", AuthorKind: chat.AuthorOperator,
		}, chat.ScopeSet{Subject: true}
	case chat.KindEviction:
		return chat.OpEviction, chat.Eviction{
			V: chat.GateRecordVersion, NodeID: id, EvictedBy: "suite",
			EvictedAt: time.Unix(1_700_000_000, 0).UTC(),
		}, chat.ScopeSet{Subject: true}
	case chat.KindGeneration:
		return chat.OpGeneration, chat.Generation{
			V: chat.GateRecordVersion, Generation: 2, By: "suite",
			StreamCreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		}, chat.ScopeSet{Subject: true}
	case chat.KindBarrier:
		return chat.OpBarrier, nil, chat.ScopeSet{Subject: true}
	}
	return chat.OpPost, nil, chat.ScopeSet{Subject: true}
}

// suiteChannelName is the address the suite's opaque object id stands for.
//
// THE SUITE HANDS OUT A TOKEN AND THIS DOMAIN'S FIRST KIND IS AN ADDRESS, so
// the two are held together HERE rather than by assuming the suite's ids are
// legal channel names. A name is lower-case alphanumerics and hyphens starting
// with an alphanumeric, at most [chat.MaxChannelName] bytes; the prefix is
// what guarantees the first character whatever the id was, and the refusal in
// [encodeSuiteRecord] is what says so out loud if the suite's ids ever change
// shape.
func suiteChannelName(id string) string {
	name := "room-" + strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(id))
	if len(name) > chat.MaxChannelName {
		name = name[:chat.MaxChannelName]
	}
	return name
}

// THE CHAT GATE READER KEEPS THE HALF OF THE SHARED RULE THIS DOMAIN HAS.
//
// Chat keeps no deletion marker that can make a record apply nowhere: an erase's
// marker is keyed on message ids that live in a payload, and a record about an
// erased message still applies — it writes its ledger row and nothing else —
// so the publisher resolves it from the ledger like any other. What is left is
// the eviction window, and the family for a reader of exactly that shape holds
// it to the window and to the applier, and holds the erase and the prune —
// gates by their op — to NOT being node gates.
func TestTheChatGateReaderKeepsTheSharedWindow(t *testing.T) {
	t.Parallel()
	statelogtest.RunEvictionGates(t, func(t *testing.T) statelogtest.EvictionGateCandidate {
		return statelogtest.EvictionGateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  chat.Domain{},
				Applier: chat.NewApplier("suite-node", nil),
				Encode:  encodeSuiteRecord,
				Kinds:   suiteKinds(),
			},
			Reader:       func(db store.ReplicatedReader) statelog.Gates { return chat.NewGates(db) },
			Kind:         string(chat.KindMessage),
			SubjectKinds: suiteKinds(),
			Create: func(id, writer, opID string) ([]byte, error) {
				op, payload, scope := suitePayload(chat.KindChannelName, id)
				return gateSuiteRecord(chat.ChannelNameSubject(suiteChannelName(id)),
					op, scope, writer, opID, payload)
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(chat.MessageSubject(id), chat.OpPost,
					chat.ScopeSet{Subject: true}, writer, opID, chat.MessagePost{
						V: chat.DocumentVersion, MessageID: "message-" + opID,
						Body: "the gate suite says " + opID, Author: "suite",
						AuthorKind: chat.AuthorOperator,
					})
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(chat.EvictionSubject(node), chat.OpEviction,
					chat.ScopeSet{Subject: true}, writer, opID, gateSuiteEviction(node, false))
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return gateSuiteRecord(chat.EvictionSubject(node), chat.OpEviction,
					chat.ScopeSet{Subject: true}, writer, opID, gateSuiteEviction(node, true))
			},
			// THE TWO OTHER GATES, each by its op on an ordinary room: both
			// delete rows, so a build that cannot read one must stop rather
			// than defer it — and neither is a node's eviction.
			ApplyGates: map[string]func(id, writer, opID string) ([]byte, error){
				"erase": func(id, writer, opID string) ([]byte, error) {
					return gateSuiteRecord(chat.ChannelSubject(id), chat.OpErase,
						chat.ScopeSet{Subject: true}, writer, opID, chat.MessageErase{
							V: chat.DocumentVersion, MessageIDs: []string{"message-" + id},
							By: "suite", ByKind: chat.AuthorOperator,
						})
				},
				"prune": func(id, writer, opID string) ([]byte, error) {
					return gateSuiteRecord(chat.ChannelSubject(id), chat.OpPrune,
						chat.ScopeSet{Subject: true}, writer, opID, chat.Prune{
							V: chat.DocumentVersion, Cutoff: time.Unix(1_600_000_000, 0).UTC(),
						})
				},
			},
		}
	})
}

// gateSuiteRecord is one record the gate family applies, written by writer.
func gateSuiteRecord(subject chat.Subject, op chat.OpKind, scope chat.ScopeSet,
	writer, opID string, payload any) ([]byte, error) {

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return chat.Encode(chat.MutationRecord{
		RecordEnvelope: chat.RecordEnvelope{
			OpID: opID, Subject: subject, Op: op, Scope: scope,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Gen: 1, Writer: writer,
		},
		Mutation: body, Actor: "suite", ActorKind: chat.AuthorOperator,
	})
}

// gateSuiteEviction is an eviction of node, or its readmission.
func gateSuiteEviction(node string, readmitted bool) chat.Eviction {
	return chat.Eviction{
		V: chat.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: time.Unix(1_700_000_000, 0).UTC(), Readmitted: readmitted,
	}
}
