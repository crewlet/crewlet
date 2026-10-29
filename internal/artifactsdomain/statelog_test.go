package artifactsdomain_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/foldername"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ARTIFACTS DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, from its
// declaration through its envelope, determinism, idempotency, versioned fields,
// stamp, evictions and node gates, exactly as every other domain is.
func TestTheArtifactsDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  artifactsdomain.Domain{},
			Applier: artifactsdomain.NewApplier("suite-node", nil),
			// Migrate is nil: these tables ship in the replicated estate's
			// own migration (0043), so a fresh store already has them.
			Encode: encodeSuiteRecord,
			// THE VERSIONED-FIELD TABLE names what version 2 added (the
			// folder tree, the brand, a move, a create's folder, a brand
			// kit's create and a version's brand pin), and carrying builds a
			// record of each.
			Fields:     artifactsdomain.VersionedFields(),
			Carrying:   carryingSuiteRecord,
			Kinds:      kindNames(),
			Rows:       artifactsdomain.NewRows,
			Write:      suiteWrite,
			EncodeGate: encodeSuiteGate,
			// THE PRODUCTION ENCODER the engine hands the trim (ADR-0050).
			EncodeTrimmed: artifactsdomain.EncodeTrimmed,
			// AND OF ITS READ INDEX'S BARRIER, which the engine registers
			// for this log: certified to write no row.
			EncodeBarrier: artifactsdomain.EncodeBarrier,
		}
	})
}

// THE ARTIFACTS GATE READER KEEPS THE RULE EVERY READER SHARES: the deletion
// marker first, over the subjects the applier's own marker covers (an
// artifact's and its assets'), the purge's own record exempt, then the
// writer's eviction window.
func TestTheArtifactsGateReaderKeepsTheSharedRule(t *testing.T) {
	t.Parallel()
	statelogtest.RunGates(t, func(t *testing.T) statelogtest.GateCandidate {
		return statelogtest.GateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  artifactsdomain.Domain{},
				Applier: artifactsdomain.NewApplier("suite-node", nil),
				// RAW IDS: the family asks the reader about the very ids it
				// handed out, so a record about one must name it on its
				// subject as given.
				Encode: encodeRawRecord,
				Kinds:  kindNames(),
			},
			Reader:       func(db store.ReplicatedReader) statelog.Gates { return artifactsdomain.NewGates(db) },
			Kind:         string(artifactsdomain.KindArtifact),
			SubjectKinds: kindNames(),
			Create: func(id, writer, opID string) ([]byte, error) {
				return rawRecord(artifactsdomain.KindArtifact, id, writer, opID,
					artifactsdomain.OpCreate, createPayload(id))
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				title := "renamed by " + opID
				return rawRecord(artifactsdomain.KindArtifact, id, writer, opID,
					artifactsdomain.OpPatch, artifactsdomain.PatchPayload{V: 1, Title: &title})
			},
			Purge: func(id, writer, opID string) ([]byte, error) {
				return rawRecord(artifactsdomain.KindArtifact, id, writer, opID,
					artifactsdomain.OpPurge, artifactsdomain.StatusPayload{V: 1})
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(artifactsdomain.KindEviction, node, writer, opID,
					artifactsdomain.OpEviction, evictionOf(node, false))
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(artifactsdomain.KindEviction, node, writer, opID,
					artifactsdomain.OpEviction, evictionOf(node, true))
			},
		}
	})
}

// kindNames is every kind the domain writes, from the enum.
func kindNames() []string {
	out := make([]string, 0, len(artifactsdomain.ObjectKinds))
	for _, k := range artifactsdomain.ObjectKinds {
		out = append(out, string(k))
	}
	return out
}

// suiteNamespace maps the suite's plain ids onto artifact ids, so the records
// the apply cases run create, extend and pin real artifacts.
var suiteNamespace = uuid.MustParse("0c6f4b8e-2a71-5d3c-9e14-7b2a5c8d1f60")

// suiteArtifact is the artifact the suite's id names.
func suiteArtifact(id string) string { return uuid.NewSHA1(suiteNamespace, []byte(id)).String() }

// suiteStamped is the instant every suite record is stamped and every suite
// object minted at.
var suiteStamped = time.Unix(1_700_000_000, 0).UTC()

// encodeSuiteRecord builds one valid record of each kind at any version, the id
// mapped onto an artifact id.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	return encodeRecord(kind, suiteArtifact(id), id, opID, version)
}

// encodeRawRecord is [encodeSuiteRecord] with the id used as given.
func encodeRawRecord(kind, id, opID string, version int) ([]byte, error) {
	return encodeRecord(kind, id, id, opID, version)
}

// encodeRecord is one record of kind about artifact (or naming id where the
// kind's object is not an artifact), at version.
func encodeRecord(kind, artifact, id, opID string, version int) ([]byte, error) {
	subject, op, payload := suitePayload(artifactsdomain.ObjectKind(kind), artifact, id)
	body, err := marshalPayload(payload)
	if err != nil {
		return nil, err
	}
	return artifactsdomain.Encode(artifactsdomain.MutationRecord{
		RecordEnvelope: artifactsdomain.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op,
			CreatedAt: suiteStamped, Gen: 1, Writer: "suite-node",
			Scope: suiteScope(subject, artifact),
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorOperator, OperatorID: "suite",
	})
}

// suiteScope is a suite record's scope: the subject alone, or the reader and
// source records' two terms.
func suiteScope(subject artifactsdomain.Subject, artifact string) artifactsdomain.ScopeSet {
	switch subject.Kind {
	case artifactsdomain.KindReader:
		return artifactsdomain.ScopeSet{Terms: []artifactsdomain.ScopeTerm{
			{Kind: artifactsdomain.TermReader, ID: subject.ID},
			{Kind: artifactsdomain.TermArtifact, ID: artifact},
		}}
	case artifactsdomain.KindSource:
		return artifactsdomain.ScopeSet{Terms: []artifactsdomain.ScopeTerm{
			{Kind: artifactsdomain.TermSource, ID: subject.ID},
			{Kind: artifactsdomain.TermArtifact, ID: artifact},
		}}
	}
	return artifactsdomain.ScopeSet{Subject: true}
}

// suitePayload is one valid (subject, op, payload) per kind.
//
// THE FIRST THREE KINDS ARE A REAL SEQUENCE: an artifact's create, an asset
// added to it, and a reader pinning it.
func suitePayload(kind artifactsdomain.ObjectKind, artifact, id string) (
	artifactsdomain.Subject, artifactsdomain.OpKind, any) {

	switch kind {
	case artifactsdomain.KindArtifact:
		return artifactsdomain.Subject{Kind: kind, ID: artifact}, artifactsdomain.OpCreate,
			createPayload(artifact)
	case artifactsdomain.KindAsset:
		asset := uuid.NewSHA1(suiteNamespace, []byte("asset-"+id)).String()
		return artifactsdomain.Subject{Kind: kind, ID: artifact + "." + asset},
			artifactsdomain.OpCreate, artifactsdomain.AssetPayload{
				V: 1, Name: "logo.png", ContentType: "image/png",
				Object:    suiteObject("asset-"+id, "png bytes of "+id),
				Authority: suiteAuthority(),
			}
	case artifactsdomain.KindReader:
		return artifactsdomain.ReaderSubject("reader-" + id), artifactsdomain.OpPin,
			artifactsdomain.ReaderPayload{V: 1, Artifact: artifact}
	case artifactsdomain.KindSource:
		sourced := uuid.NewSHA1(suiteNamespace, []byte("sourced-"+id)).String()
		p := createPayload(sourced)
		p.SourceKey = "src/" + id
		return artifactsdomain.SourceSubject(p.Owner, p.SourceKey), artifactsdomain.OpCreate, p
	case artifactsdomain.KindEviction:
		return artifactsdomain.EvictionSubject(id), artifactsdomain.OpEviction, evictionOf(id, false)
	case artifactsdomain.KindGeneration:
		return artifactsdomain.GenerationSubject(2), artifactsdomain.OpGeneration,
			artifactsdomain.Generation{V: 1, Generation: 2, By: "suite", StreamAt: suiteStamped}
	case artifactsdomain.KindBarrier:
		return artifactsdomain.BarrierSubject(), artifactsdomain.OpBarrier, nil
	case artifactsdomain.KindTrimmed:
		return artifactsdomain.TrimmedSubject(statelog.Position{Generation: 1, Seq: 2}),
			artifactsdomain.OpTrimmed, nil
	case artifactsdomain.KindFolder:
		return artifactsdomain.FolderSubject(), artifactsdomain.OpCreate, folderCreatePayload(id)
	case artifactsdomain.KindBrand:
		return artifactsdomain.BrandSubject(), artifactsdomain.OpAdopt, brandAdoptPayload(artifact)
	}
	return artifactsdomain.Subject{Kind: kind, ID: id}, artifactsdomain.OpPatch, nil
}

// folderCreatePayload is one top-level folder's create, named for id.
func folderCreatePayload(id string) artifactsdomain.FolderCreatePayload {
	name, key, err := foldername.Check("Suite " + id)
	if err != nil {
		panic(err)
	}
	return artifactsdomain.FolderCreatePayload{
		V: 1, Owner: "suite", OwnerKind: iam.ActorHuman, MaxDepth: artifacts.MaxFolderDepth,
		MaxFolders: artifacts.MaxFolders, MaxChildren: artifacts.MaxFolderChildren,
		Folders: []artifactsdomain.FolderSpec{{
			ID: artifactsdomain.FolderIDOf("suite-folder-"+id, 0).String(), Name: name, NameKey: key,
		}},
		Authority: suiteAuthority(),
	}
}

// brandAdoptPayload is an adoption of version 1 of kit.
func brandAdoptPayload(kit string) artifactsdomain.BrandAdoptPayload {
	css := []byte(":root{--brand-primary:#2450e6}")
	sum := objstore.HashOf(css)
	return artifactsdomain.BrandAdoptPayload{
		V: 1, Kit: kit, KitVersion: 1, BrandJSON: []byte(`{"v":1}`), CSSSha: string(sum),
		Files: []artifactsdomain.BrandFile{{
			Slot: "css", ContentType: "text/css; charset=utf-8",
			Object: suiteObject("brand-css-"+kit, string(css)),
		}},
		Authority: suiteAuthority(),
	}
}

// carryingSuiteRecord is a record carrying exactly one versioned field, its
// version left for the encoder to stamp.
func carryingSuiteRecord(field statelog.VersionedField) ([]byte, error) {
	artifact := suiteArtifact("carrying")
	var (
		subject = artifactsdomain.ArtifactSubject(mustID(artifact))
		op      artifactsdomain.OpKind
		payload any
	)
	switch field.Name {
	case "Subject.Kind folder":
		subject, op, payload = artifactsdomain.FolderSubject(), artifactsdomain.OpCreate,
			folderCreatePayload("carrying")
	case "Subject.Kind brand":
		subject, op, payload = artifactsdomain.BrandSubject(), artifactsdomain.OpAdopt,
			brandAdoptPayload(artifact)
	case "RecordEnvelope.Op move":
		op, payload = artifactsdomain.OpMove, artifactsdomain.MovePayload{
			V: 1, To: artifactsdomain.FolderIDOf("suite-folder-carrying", 0).String(),
			Authority: suiteAuthority(),
		}
	case "CreatePayload.Folder":
		p := createPayload(artifact)
		p.Folder = artifactsdomain.FolderIDOf("suite-folder-carrying", 0).String()
		op, payload = artifactsdomain.OpCreate, p
	case "VersionPayload.Brand":
		p := createPayload(artifact)
		p.Version.Brand = brandAdoptPayload(artifact).CSSSha
		op, payload = artifactsdomain.OpCreate, p
	case "CreatePayload.Kind brand":
		op, payload = artifactsdomain.OpCreate, createPayloadOfKind(artifact, artifacts.KindBrand)
	default:
		return nil, fmt.Errorf("no suite record carries %s: add one here with its row", field.Name)
	}
	body, err := marshalPayload(payload)
	if err != nil {
		return nil, err
	}
	return artifactsdomain.Encode(artifactsdomain.MutationRecord{
		RecordEnvelope: artifactsdomain.RecordEnvelope{
			OpID: "suite-carrying", Subject: subject, Op: op, CreatedAt: suiteStamped,
			Gen: 1, Writer: "suite-node", Scope: artifactsdomain.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorOperator, OperatorID: "suite",
	})
}

// mustID parses a suite artifact id.
func mustID(raw string) artifacts.ID {
	id, err := artifacts.ParseID(raw)
	if err != nil {
		panic(err)
	}
	return id
}

// suiteAuthority is the stamp every suite record that consumes quota carries.
func suiteAuthority() artifacts.Authority {
	return artifacts.Authority{
		Actor: iam.Actor{Name: "suite", Kind: iam.ActorOperator, OperatorID: "suite"},
		Role:  artifacts.RoleOwner, Limits: limits(), Origin: artifacts.OriginAPI,
	}
}

// suiteObject is a deterministic object for name holding content.
func suiteObject(name, content string) objstore.Object {
	return objstore.Object{
		Key: keyAt(name, suiteStamped), Hash: objstore.HashOf([]byte(content)),
		Size: int64(len(content)),
	}
}

// createPayload is a one-page HTML artifact's create.
func createPayload(artifact string) artifactsdomain.CreatePayload {
	content := "<!doctype html><title>" + artifact + "</title>"
	return artifactsdomain.CreatePayload{
		V: 1, Artifact: artifact, Kind: artifacts.KindHTML, Title: "Suite " + artifact,
		Owner: "suite", OwnerKind: iam.ActorHuman,
		Version: artifactsdomain.VersionPayload{
			Number: 1, VersionID: uuid.NewSHA1(suiteNamespace, []byte("v1-"+artifact)).String(),
			Contract: "1.0.0",
			Manifest: artifacts.Manifest{Entry: "index.html", Files: []artifacts.Entry{{
				Path: "index.html", ContentType: "text/html; charset=utf-8",
				Object: suiteObject("page-"+artifact, content),
			}}},
		},
		Authority: suiteAuthority(),
	}
}

// createPayloadOfKind is a create of kind's artifact, its first version the
// kind's own entry.
func createPayloadOfKind(artifact string, kind artifacts.Kind) artifactsdomain.CreatePayload {
	p := createPayload(artifact)
	p.Kind = kind
	spec, ok := kind.Spec()
	if !ok || len(spec.Entry.Defaults) == 0 {
		return p
	}
	entry := spec.Entry.Defaults[0]
	p.Version.Manifest = artifacts.Manifest{Entry: entry, Files: []artifacts.Entry{{
		Path: entry, ContentType: "application/octet-stream",
		Object: suiteObject(string(kind)+"-"+artifact, "{}"),
	}}}
	return p
}

// baseRecordKinds are the artifact kinds record version 1 shipped with. FROZEN:
// a kind added since is a value an older build refuses, and needs its row.
var baseRecordKinds = []artifacts.Kind{
	artifacts.KindHTML, artifacts.KindDoc, artifacts.KindSlides,
	artifacts.KindDashboard, artifacts.KindSheet, artifacts.KindDesign,
}

// A KIND THE BASE FORMAT LACKED STAMPS ITS CREATE PAST IT: an older build's
// create apply refuses a kind it does not serve, so a create stamped at the
// base version would be refused by an older node and applied by a newer one,
// and the fleet would diverge. Every kind served now and missing from the base
// format must be held back by its own row. Mutation: drop the brand kind's row
// and the brand kit's create is stamped 1.
func TestEveryArtifactKindTheBaseFormatLackedHasAVersionedRow(t *testing.T) {
	t.Parallel()
	for _, kind := range artifacts.Kinds {
		body, err := createRecordOfKind(kind)
		if err != nil {
			t.Fatalf("%s: encode a create: %v", kind, err)
		}
		env, err := artifactsdomain.Domain{}.Envelope(body)
		if err != nil {
			t.Fatalf("%s: envelope: %v", kind, err)
		}
		base := slices.Contains(baseRecordKinds, kind)
		if got := env.ReadableBy(1); got != base {
			t.Errorf("a create of kind %q is stamped %d: a build reading the base format "+
				"would apply it = %v, want %v; a kind added since the base format needs a "+
				"versioned-field row at mutation.kind (record.go)", kind, env.V, got, base)
		}
	}
}

// createRecordOfKind is an encoded create of kind's artifact, stamped by the
// domain's own encoder.
func createRecordOfKind(kind artifacts.Kind) ([]byte, error) {
	artifact := suiteArtifact("kind-" + string(kind))
	body, err := marshalPayload(createPayloadOfKind(artifact, kind))
	if err != nil {
		return nil, err
	}
	return artifactsdomain.Encode(artifactsdomain.MutationRecord{
		RecordEnvelope: artifactsdomain.RecordEnvelope{
			OpID: "suite-kind-" + string(kind), Subject: artifactsdomain.ArtifactSubject(mustID(artifact)),
			Op: artifactsdomain.OpCreate, CreatedAt: suiteStamped, Gen: 1, Writer: "suite-node",
			Scope: artifactsdomain.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorOperator, OperatorID: "suite",
	})
}

// evictionOf is node's eviction, or its readmission.
func evictionOf(node string, readmit bool) artifactsdomain.Eviction {
	return artifactsdomain.Eviction{
		V: artifactsdomain.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: suiteStamped, Readmitted: readmit,
	}
}

// rawRecord is one record the gate family applies, written by writer.
func rawRecord(kind artifactsdomain.ObjectKind, id, writer, opID string, op artifactsdomain.OpKind,
	payload any) ([]byte, error) {

	body, err := marshalPayload(payload)
	if err != nil {
		return nil, err
	}
	return artifactsdomain.Encode(artifactsdomain.MutationRecord{
		RecordEnvelope: artifactsdomain.RecordEnvelope{
			V: artifactsdomain.RecordVersion, OpID: opID,
			Subject: artifactsdomain.Subject{Kind: kind, ID: id}, Op: op,
			CreatedAt: suiteStamped, Gen: 1, Writer: writer,
			Scope: artifactsdomain.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorOperator,
	})
}

// marshalPayload is a payload's bytes, nil for the barrier's absent one.
func marshalPayload(payload any) ([]byte, error) {
	if payload == nil {
		return nil, nil
	}
	return json.Marshal(payload)
}

// encodeSuiteGate is the eviction record a peer publishes onto this log.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	opID := "suite-evict-" + node
	if readmit {
		opID = "suite-readmit-" + node
	}
	return rawRecord(artifactsdomain.KindEviction, node, "suite-peer", opID,
		artifactsdomain.OpEviction, evictionOf(node, readmit))
}

// suiteWrite is one write through the domain's own production writer, which is
// where the framework's stamp is kept or lost.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	w, err := artifactsdomain.NewWriter(artifactsdomain.WriterDeps{
		Publisher: pub, DB: db, SeatKind: seatKinds,
		Now: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return err
	}
	content := "<!doctype html><title>suite</title>"
	obj := objstore.Object{
		Key: objstore.KeyAt(time.Now()), Hash: objstore.HashOf([]byte(content)),
		Size: int64(len(content)),
	}
	_, err = w.Create(ctx, artifactsdomain.Call{Authority: suiteAuthority()},
		artifactsdomain.CreateInput{
			Kind: artifacts.KindHTML, Title: "Suite", Owner: "suite", OwnerKind: iam.ActorHuman,
			First: artifactsdomain.PublishInput{
				Contract: "1.0.0", Uploaded: []objstore.Key{obj.Key},
				Manifest: artifacts.Manifest{Entry: "index.html", Files: []artifacts.Entry{{
					Path: "index.html", ContentType: "text/html; charset=utf-8", Object: obj,
				}}},
			},
		})
	return err
}
