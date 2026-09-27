package pages_test

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/jsoncarry/jsoncarrytest"
	"github.com/crewlet/crewlet/internal/pages"
)

// wireCases is every object the knowledge base stores as a document or
// carries on its log, filled so that every member it has is on the wire, and
// encoded and decoded the way the package itself does it.
func wireCases() map[string]wireCase {
	return map[string]wireCase{
		"container": documentCase(pages.EncodeContainer, pages.DecodeContainer),
		"page":      documentCase(pages.EncodePage, pages.DecodePage),
		"revision":  documentCase(pages.EncodeRevision, pages.DecodeRevision),
		"comment":   documentCase(pages.EncodeComment, pages.DecodeComment),
		"claim":     documentCase(pages.EncodeClaim, pages.DecodeClaim),
		"change":    documentCase(pages.EncodeChange, pages.DecodeChange),
		"record": {
			typ: reflect.TypeFor[pages.MutationRecord](),
			encode: func() ([]byte, error) {
				rec := jsoncarrytest.Filled[pages.MutationRecord]()
				rec.V = pages.RecordVersion
				rec.Subject = pages.PageSubject("p-1")
				rec.Op = pages.OpPatch
				return pages.Encode(rec)
			},
			decode: func(b []byte) ([]byte, error) {
				rec, err := pages.Decode(b)
				if err != nil {
					return nil, err
				}
				return pages.Encode(rec)
			},
		},
	}
}

type wireCase struct {
	typ    reflect.Type
	encode func() ([]byte, error)
	decode func([]byte) ([]byte, error)
}

// documentCase fills a document at the version this build writes, which is
// the only version its decoder accepts.
func documentCase[T any](encode func(T) ([]byte, error), decode func([]byte) (T, error)) wireCase {
	return wireCase{
		typ: reflect.TypeFor[T](),
		encode: func() ([]byte, error) {
			v := jsoncarrytest.Filled[T]()
			atVersion(&v)
			return encode(v)
		},
		decode: func(b []byte) ([]byte, error) {
			v, err := decode(b)
			if err != nil {
				return nil, err
			}
			return encode(v)
		},
	}
}

func atVersion(v any) {
	switch d := v.(type) {
	case *pages.Container:
		d.V = pages.DocumentVersion
	case *pages.Page:
		d.V = pages.DocumentVersion
	case *pages.Revision:
		d.V = pages.DocumentVersion
	case *pages.Comment:
		d.V = pages.DocumentVersion
	case *pages.TitleClaim:
		d.V = pages.DocumentVersion
	case *pages.Change:
		d.V = pages.DocumentVersion
	}
}

// wireGolden is each case's encoding: the bytes every node running this format
// holds for it.
var wireGolden = map[string]string{
	"change":    `{"v":1,"id":"ID","page_id":"PageID","kind":"Kind","actor":"Actor","actor_kind":"ActorKind","operator_id":"OperatorID","comment_id":"CommentID","excerpt":"Excerpt","turn_id":"TurnID","chain":["Chain"],"quiet":true,"created_at":"2026-01-02T03:04:05Z"}`,
	"claim":     `{"v":1,"container":"Container","title":"Title","page_id":"PageID","created_at":"2026-01-02T03:04:05Z"}`,
	"comment":   `{"v":1,"id":"ID","page_id":"PageID","author":"Author","author_kind":"AuthorKind","body":"Body","mentions":["Mentions"],"reply_to":"ReplyTo","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}`,
	"container": `{"v":1,"key":"Key","name":"Name","purpose":"Purpose","created_at":"2026-01-02T03:04:05Z"}`,
	"page":      `{"v":1,"id":"ID","container":"Container","parent_id":"ParentID","title":"Title","body":"Body","status":"Status","labels":["Labels"],"watchers":["Watchers"],"muted":["Muted"],"version":7,"author":"Author","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z","trashed_at":"2026-01-02T03:04:05Z"}`,
	"record":    `{"v":1,"op_id":"OpID","subject":{"k":"page","i":"p-1"},"op":"patch","created_at":"2026-01-02T03:04:05Z","gen":7,"writer":"Writer","scope":"s/Container","expect":7,"mutation":{"raw":true},"actor":"Actor","actor_kind":"ActorKind","operator_id":"OperatorID","turn_id":"TurnID","chain":["Chain"],"notify":{"kind":"Kind","page_id":"PageID","version":7,"recipients":["Recipients"],"mentions":["Mentions"],"excerpt":"Excerpt","container":"Container","title":"Title"}}`,
	"revision":  `{"v":1,"id":"ID","page_id":"PageID","version":7,"title":"Title","body":"Body","message":"Message","author":"Author","created_at":"2026-01-02T03:04:05Z"}`,
}

// EVERY OBJECT THIS BUILD WRITES IS THE BYTES IT ALWAYS WAS.
//
// A document and a record are shared by every node that holds the log, so the
// bytes are a contract between peers rather than a detail of this build. An
// object carrying nothing this build does not know encodes exactly as its
// struct does, and decoding those bytes and encoding them again changes
// nothing.
func TestEveryObjectEncodesAsItAlwaysHas(t *testing.T) {
	t.Parallel()
	for name, tc := range wireCases() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want, pinned := wireGolden[name]
			if !pinned {
				t.Fatalf("%s has no pinned encoding", name)
			}
			got, err := tc.encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(got) != want {
				t.Fatalf("%s encodes as\n  %s\nand every node holds\n  %s", name, got, want)
			}
			again, err := tc.decode(got)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if string(again) != want {
				t.Fatalf("%s decoded and encoded again is\n  %s\nnot\n  %s", name, again, want)
			}
		})
	}
}

// EVERY OBJECT THE KNOWLEDGE BASE SHARES CARRIES WHAT IT DOES NOT KNOW, at
// every depth, through methods of its own — bar its subject, an address every
// member of which is recovered from the log subject a record is published on
// ([pages.ParseSubject]), and the container, which carries through the
// functions that read and write it.
func TestEveryObjectTheKnowledgeBaseSharesCarries(t *testing.T) {
	t.Parallel()
	roots := []reflect.Type{}
	for _, tc := range wireCases() {
		roots = append(roots, tc.typ)
	}
	for _, missing := range jsoncarrytest.Uncarried(map[reflect.Type]string{
		reflect.TypeFor[pages.Subject]():   "an address recovered from the log subject",
		reflect.TypeFor[pages.Container](): "carried by EncodeContainer and DecodeContainer, since ContainerListing embeds it",
	}, roots...) {
		t.Error(missing)
	}
}

// A CONTAINER LISTING IS WRITTEN WITH ITS PAGE COUNT. It embeds the
// container, so a MarshalJSON on the container would be promoted onto it and
// write the listing as the bare container — which is why the container
// carries through the functions that read and write it instead.
func TestAContainerListingIsWrittenWithItsCount(t *testing.T) {
	t.Parallel()
	out, err := json.Marshal(pages.ContainerListing{Container: pages.Container{Key: "ENG"}, Pages: 3})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(out, &members); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(members["pages"]) != "3" || string(members["key"]) != `"ENG"` {
		t.Errorf("the listing is written as %s", out)
	}
}

// A MEMBER A NEWER BUILD ADDED INSIDE ANY OBJECT SURVIVES THIS BUILD'S DECODE
// AND ENCODE of every document and record, byte for byte.
func TestAMemberANewerBuildAddedSurvivesInsideEveryObject(t *testing.T) {
	t.Parallel()
	for name, tc := range wireCases() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			jsoncarrytest.Survives(t, tc.typ, []byte(wireGolden[name]), tc.decode)
		})
	}
}

// A MEMBER A NEWER BUILD WROTE INTO A PAGE'S HEAD SURVIVES A SAVE ON IT.
//
// The save decodes the stored head, changes it and encodes it again, so a
// member this build has no field for would be gone from this node's row and
// kept on every peer that knows it. The head is written here as a newer
// build's apply would have left it.
func TestAMemberInAStoredHeadSurvivesASaveOnIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, gate, err := h.apply(create("page-1", "ENG", "Deploy Runbook", "# Deploy\n")); err != nil || gate != "" {
		t.Fatalf("create: %v (gate %q)", err, gate)
	}
	var head map[string]json.RawMessage
	if err := json.Unmarshal([]byte(h.scalar(`SELECT CAST(document AS TEXT) FROM pages_heads WHERE id = 'page-1'`)), &head); err != nil {
		t.Fatalf("read the head: %v", err)
	}
	head["lane"] = json.RawMessage(`"urgent"`)
	newer, err := json.Marshal(head)
	if err != nil {
		t.Fatalf("encode the newer head: %v", err)
	}
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE pages_heads SET document = ? WHERE id = 'page-1'`, newer)
		return err
	}); err != nil {
		t.Fatalf("write the newer head: %v", err)
	}
	body := "# Deploy, again\n"
	if _, gate, err := h.apply(record(pages.PageSubject("page-1"), pages.OpPatch, "op-save",
		pages.PagePatch{V: pages.DocumentVersion, Body: &body},
		pages.ScopeSet{Subject: true, Container: "ENG"})); err != nil || gate != "" {
		t.Fatalf("save: %v (gate %q)", err, gate)
	}
	for path, want := range map[string]string{"$.lane": "urgent", "$.body": body} {
		if got := h.scalar(`SELECT json_extract(CAST(document AS TEXT), '` + path + `')
			FROM pages_heads WHERE id = 'page-1'`); got != want {
			t.Errorf("after the save the head holds %s = %q, want %q", path, got, want)
		}
	}
}

// A MEMBER A NEWER BUILD ADDED INSIDE A RECORD'S NOTIFICATION REACHES THE
// FEED'S BODY — a parser on a newer node reading a record an older node
// relayed sees what the writer wrote there as much as at the top.
func TestAMemberInsideANotificationReachesTheFeedBody(t *testing.T) {
	t.Parallel()
	rec := record(pages.PageSubject("page-1"), pages.OpPatch, "op-save", nil,
		pages.ScopeSet{Subject: true, Container: "ENG"})
	rec.Notify = &pages.Notify{Kind: pages.ChangeSaved, PageID: "page-1", Recipients: []string{"bo"}}
	payload, err := pages.Encode(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw["notify"].(map[string]any)["lane"] = "notify"
	raw["lane"] = "record"
	if payload, err = json.Marshal(raw); err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	delivery, wakes, err := pages.NewTranslator(nil).Translate(t.Context(),
		changefeed.Record{Payload: payload, Key: "k"})
	if err != nil || !wakes {
		t.Fatalf("Translate = %v, %v", wakes, err)
	}
	notify, _ := delivery.Body["notify"].(map[string]any)
	if notify["lane"] != "notify" || delivery.Body["lane"] != "record" {
		t.Errorf("the body lost a member a newer build wrote: %v", delivery.Body)
	}
}
