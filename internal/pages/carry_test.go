package pages_test

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
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
// every depth, through methods of its own — the terms of a scope written by a
// method of its own among them — bar its subject, an address every member of
// which is recovered from the log subject a record is published on
// ([pages.ParseSubject]).
//
// THE TYPED PAYLOADS ARE NOT WALKED, because nothing re-encodes one: a record
// carries its mutation as the bytes its writer published, so a relay writes
// back exactly those, and the one decode of a payload is the applier's, which
// writes rows from the members it reads — a carry on a payload would hand
// what it kept to nothing.
func TestEveryObjectTheKnowledgeBaseSharesCarries(t *testing.T) {
	t.Parallel()
	roots := []reflect.Type{}
	for _, tc := range wireCases() {
		roots = append(roots, tc.typ)
	}
	for _, missing := range jsoncarrytest.Uncarried(map[reflect.Type]string{
		reflect.TypeFor[pages.Subject](): "an address recovered from the log subject",
	}, roots...) {
		t.Error(missing)
	}
}

// A CONTAINER LISTING IS WRITTEN WITH ITS PAGE COUNT, AND WITH WHAT ITS
// CONTAINER CARRIES. It embeds the container, whose MarshalJSON would be
// promoted onto it and write the listing as the bare container; the listing's
// own methods (read.go) write the container's members, what the container
// carries and the count, in the order encoding/json lays an embedding out — so
// a listing whose container carries nothing is the bytes it always was.
//
// Mutation: delete ContainerListing's MarshalJSON and the count is gone.
func TestAContainerListingIsWrittenWithItsCount(t *testing.T) {
	t.Parallel()
	listing := pages.ContainerListing{Container: pages.Container{V: 1, Key: "ENG", Name: "Engineering"}, Pages: 3}
	out, err := json.Marshal(listing)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const golden = `{"v":1,"key":"ENG","name":"Engineering","created_at":"0001-01-01T00:00:00Z","pages":3}`
	if string(out) != golden {
		t.Errorf("the listing is written as\n  %s\nnot\n  %s", out, golden)
	}

	// What the container carries is written once, after the listing's own
	// members; a carried member under a name the listing decodes is the
	// listing's.
	listing.Extra = map[string]json.RawMessage{"lane": json.RawMessage(`"urgent"`), "pages": json.RawMessage(`9`)}
	out, err = json.Marshal(listing)
	if err != nil {
		t.Fatalf("marshal with a carry: %v", err)
	}
	const carried = `{"v":1,"key":"ENG","name":"Engineering","created_at":"0001-01-01T00:00:00Z","pages":3,"lane":"urgent"}`
	if string(out) != carried {
		t.Errorf("the listing carrying a member is written as\n  %s\nnot\n  %s", out, carried)
	}
	var back pages.ContainerListing
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Pages != 3 || back.Key != "ENG" || len(back.Extra) != 1 || string(back.Extra["lane"]) != `"urgent"` {
		t.Errorf("the listing reads back as %+v", back)
	}
}

// A MEMBER A NEWER BUILD ADDED INSIDE A SCOPE'S TERM SURVIVES THIS BUILD'S
// DECODE AND ENCODE of the record. The scope is written by a method of its own
// as the list of its terms, so the member-by-member plant never reaches inside
// one; this is that term, planted by hand.
//
// Mutation: drop ScopeTerm's UnmarshalJSON in scope.go and the member is gone.
func TestAMemberInsideAScopeTermSurvivesARecordsRoundTrip(t *testing.T) {
	t.Parallel()
	rec := record(pages.PageSubject("page-1"), pages.OpPatch, "op-save", nil, pages.ScopeSet{
		Terms: []pages.ScopeTerm{
			{Kind: pages.TermObject, Container: "ENG", ID: "page-1"},
			{Kind: pages.TermContainer, ID: "OPS"},
		},
	})
	body, err := pages.Encode(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	const plain = `"scope":[{"k":"object","c":"ENG","i":"page-1"},{"k":"container","i":"OPS"}]`
	if !strings.Contains(string(body), plain) {
		t.Fatalf("the enumerated scope is written as something other than %s: %s", plain, body)
	}
	newer := strings.Replace(string(body), `"i":"OPS"}`,
		`"i":"OPS","`+jsoncarrytest.PlantedName+`":`+jsoncarrytest.PlantedValue+`}`, 1)
	back, err := pages.Decode([]byte(newer))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	again, err := pages.Encode(back)
	if err != nil {
		t.Fatalf("encode again: %v", err)
	}
	if string(again) != newer {
		t.Errorf("the record came back as\n  %s\nnot\n  %s", again, newer)
	}
}

// A CONTAINER'S SETTINGS CHANGE KEEPS WHAT A NEWER BUILD WROTE ON ITS
// DOCUMENT, AND THE INSTANT IT WAS CREATED. The apply lays the settings onto
// the stored document rather than building one from the payload, so neither
// is rewritten by a rename: the member is carried, and the document's
// created_at stays the one its column holds.
//
// Mutation: build the document from the payload alone in applyContainer and
// both are gone.
func TestAContainersSettingsChangeKeepsItsDocument(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	settings := func(opID, name string) pages.MutationRecord {
		return record(pages.ContainerSubject("OPS"), pages.OpPatch, opID,
			pages.ContainerPayload{V: pages.DocumentVersion, Key: "OPS", Name: name},
			pages.ScopeSet{Subject: true})
	}
	if _, gate, err := h.apply(settings("op-create", "Operations")); err != nil || gate != "" {
		t.Fatalf("create the space: %v (gate %q)", err, gate)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal([]byte(h.scalar(`SELECT CAST(document AS TEXT) FROM pages_containers WHERE key = 'OPS'`)), &stored); err != nil {
		t.Fatalf("read the container: %v", err)
	}
	// A NEWER BUILD'S MEMBER, and a creation instant other than the
	// broker's time for every record here, so a document stamped anew by
	// the rename would be seen to be.
	const created = "2030-01-02T03:04:05Z"
	stored["lane"] = json.RawMessage(`"urgent"`)
	stored["created_at"] = json.RawMessage(`"` + created + `"`)
	newer, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("encode the newer container: %v", err)
	}
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE pages_containers SET document = ? WHERE key = 'OPS'`, newer)
		return err
	}); err != nil {
		t.Fatalf("write the newer container: %v", err)
	}
	if _, gate, err := h.apply(settings("op-rename", "Ops")); err != nil || gate != "" {
		t.Fatalf("rename the space: %v (gate %q)", err, gate)
	}
	for path, want := range map[string]string{"$.lane": "urgent", "$.name": "Ops", "$.created_at": created} {
		if got := h.scalar(`SELECT json_extract(CAST(document AS TEXT), '` + path + `')
			FROM pages_containers WHERE key = 'OPS'`); got != want {
			t.Errorf("after the rename the container holds %s = %q, want %q", path, got, want)
		}
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

// AN INTEGER PAST 2^53 LEAVES THE FEED AS THE WRITER WROTE IT — the version a
// record was decided against, and one a newer build carried inside the
// notification. The body is a map, and a number decoded into `any` is a
// float64; the body holds each as the digits it was read from instead.
//
// Mutation: decode recordBody without UseNumber and both come out rounded.
func TestAnIntegerPast2To53LeavesTheFeedExactly(t *testing.T) {
	t.Parallel()
	rec := record(pages.PageSubject("page-1"), pages.OpPatch, "op-save", nil,
		pages.ScopeSet{Subject: true, Container: "ENG"})
	rec.Expect = 1<<60 + 1
	rec.Notify = &pages.Notify{Kind: pages.ChangeSaved, PageID: "page-1", Recipients: []string{"bo"},
		Extra: map[string]json.RawMessage{"later": json.RawMessage(`9007199254740993`)}}
	payload, err := pages.Encode(rec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	delivery, wakes, err := pages.NewTranslator(nil).Translate(t.Context(),
		changefeed.Record{Payload: payload, Key: "k"})
	if err != nil || !wakes {
		t.Fatalf("Translate = %v, %v", wakes, err)
	}
	out, err := json.Marshal(delivery.Body)
	if err != nil {
		t.Fatalf("encode the body: %v", err)
	}
	for _, exact := range []string{`"expect":1152921504606846977`, `"later":9007199254740993`} {
		if !strings.Contains(string(out), exact) {
			t.Errorf("the body lost %s: %s", exact, out)
		}
	}
}
