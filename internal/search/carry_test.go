package search_test

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/jsoncarry/jsoncarrytest"
	"github.com/crewlet/crewlet/internal/search"
)

// filledRecord is a vector record with every member on the wire, and an
// envelope and a vector its encoder accepts.
func filledRecord() search.VectorRecord {
	rec := jsoncarrytest.Filled[search.VectorRecord]()
	rec.V = search.RecordVersion
	rec.Subject = search.Subject{Source: search.SourcePage, ID: "doc-1", Chunk: 7}
	rec.Op = search.OpEmbed
	rec.Dim = 2
	rec.Embedding = []byte{1, 2, 3, 4, 5, 6, 7, 8}
	return rec
}

const recordGolden = `{"v":1,"op_id":"OpID","subject":{"source":"page","id":"doc-1","chunk":7},"op":"embed","created_at":"2026-01-02T03:04:05Z","gen":7,"writer":"Writer","scope":{"Paths":["Paths"]},"container":"Container","model":"Model","dim":2,"source_rev":7,"text_sha":"TextSHA","chunks":7,"embedding":"AQIDBAUGBwg="}`

// A VECTOR RECORD THIS BUILD WRITES IS THE BYTES IT ALWAYS WAS.
//
// The record is on every node's log, so its bytes are a contract between peers
// rather than a detail of this build. A record carrying nothing this build
// does not know encodes exactly as its struct does, and decoding those bytes
// and encoding them again changes nothing.
func TestAVectorRecordEncodesAsItAlwaysHas(t *testing.T) {
	t.Parallel()
	got, err := filledRecord().Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(got) != recordGolden {
		t.Fatalf("the record encodes as\n  %s\nand every node holds\n  %s", got, recordGolden)
	}
	back, err := search.Decode(got)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	again, err := back.Encode()
	if err != nil {
		t.Fatalf("encode again: %v", err)
	}
	if string(again) != recordGolden {
		t.Fatalf("decoded and encoded again it is\n  %s\nnot\n  %s", again, recordGolden)
	}
}

// EVERY OBJECT ON A VECTOR RECORD CARRIES WHAT IT DOES NOT KNOW — the scope,
// the framework's own type, among them — bar the subject, an address: every
// member of it is the log subject the record is published on
// ([search.Subject.String]).
func TestEveryObjectOnAVectorRecordCarries(t *testing.T) {
	t.Parallel()
	for _, missing := range jsoncarrytest.Uncarried(map[reflect.Type]string{
		reflect.TypeFor[search.Subject](): "an address: the log subject",
	}, reflect.TypeFor[search.VectorRecord]()) {
		t.Error(missing)
	}
}

// A MEMBER A NEWER BUILD ADDED TO A VECTOR RECORD SURVIVES THIS BUILD'S DECODE
// AND ENCODE, byte for byte.
func TestAMemberANewerBuildAddedSurvivesAVectorRecordsRoundTrip(t *testing.T) {
	t.Parallel()
	jsoncarrytest.Survives(t, reflect.TypeFor[search.VectorRecord](), []byte(recordGolden),
		func(raw []byte) ([]byte, error) {
			rec, err := search.Decode(raw)
			if err != nil {
				return nil, err
			}
			return rec.Encode()
		})
}
