package assistantdomain_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// files is a node's file store over an in-memory object store on the
// harness's clock, and the backend under it.
type files struct {
	n       *node
	store   *assistantdomain.FileStore
	backend *memobj.Backend
	objects *objstore.Store
}

func newFiles(t *testing.T, n *node) *files {
	t.Helper()
	at := func() time.Time { return monday }
	backend := memobj.NewAt(at)
	objects, err := objstore.NewStoreAt(backend, at)
	if err != nil {
		t.Fatal(err)
	}
	store, err := assistantdomain.NewFileStore(n.writer, n.reader, objects)
	if err != nil {
		t.Fatal(err)
	}
	return &files{n: n, store: store, backend: backend, objects: objects}
}

// png is image content carrying a marker a test looks for in the clear.
func png(marker string) []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), []byte(marker)...)
}

// attach keeps an image in conversation for c's owner and answers its record.
func (f *files) attach(c assistantdomain.Call, conversation uuid.UUID, name string,
	content []byte) assistantdomain.File {
	f.n.t.Helper()
	stored, _, err := f.store.Store(f.n.t.Context(), fresh(c), assistantdomain.NewFile{
		ID: uuid.New(), Conversation: conversation, Origin: wire.FileAttached,
		Name: name, MediaType: "image/png", Content: content,
	})
	if err != nil {
		f.n.t.Fatalf("attach %s: %v", name, err)
	}
	return stored
}

// collector is a collector over the assistant domain's declared tables, read
// through this node's estate, judging objects at now.
func (f *files) collector(t *testing.T, now time.Time) *collect.Collector {
	t.Helper()
	var tables []objstore.ReferenceTable
	for _, table := range references.All {
		if table.Domain == (assistantdomain.Domain{}).Name() {
			tables = append(tables, table)
		}
	}
	sources, err := collect.Sources(tables, pinnedEstate{assistantdomain.ObjectEstate{Reader: f.n.reader}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := collect.New(collect.Options{Store: f.objects, References: sources,
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// pinnedEstate is the domain's estate pinned at whatever this node holds: the
// harness's read authority has no read index, so the linearizable barrier a
// running node takes is the engine's to prove, and every record here applied
// before its write answered.
type pinnedEstate struct{ assistantdomain.ObjectEstate }

func (pinnedEstate) Barrier(context.Context) (statelog.Position, error) {
	return statelog.Position{}, nil
}

// held reports whether the object store still holds the object key names.
func (f *files) held(t *testing.T, key string) bool {
	t.Helper()
	var held bool
	if err := f.backend.List(t.Context(), func(info objstore.Info) error {
		held = held || strings.HasSuffix(info.Name, key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return held
}

// objectOf is the object key file's row names, read off the row.
func (f *files) objectOf(t *testing.T, file assistantdomain.File) string {
	t.Helper()
	var key string
	if err := f.n.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT object FROM assistant_files WHERE id = ?`,
			file.ID.String()).Scan(&key)
	}); err != nil {
		t.Fatalf("read file %s's object: %v", file.ID, err)
	}
	return key
}

// A FILE IS ITS OWNER'S, IN ITS OWN CONVERSATION: another person's read of it,
// or its owner's read of it under another conversation, is not found, by Stat
// and by Open alike, and the owner reads back exactly the bytes they kept.
// Mutation: drop the owner from the file read's WHERE, or the conversation, and
// grace or the wrong conversation reads it.
func TestAnotherPersonsFileIsNotFound(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	f := newFiles(t, n)
	ada, grace := person(uuid.New(), "ada"), person(uuid.New(), "grace")
	mine, other := n.conversation(ada, "Mine"), n.conversation(ada, "Other")
	content := png("a screenshot of the payroll page")
	kept := f.attach(ada, mine, "payroll.png", content)

	stat, err := f.store.Stat(t.Context(), ada.Owner, mine, kept.ID)
	if err != nil || stat.Name != "payroll.png" || stat.Size != int64(len(content)) ||
		stat.MediaType != "image/png" || stat.Origin != wire.FileAttached {
		t.Fatalf("ada's Stat = %+v, %v", stat, err)
	}
	if ref := stat.Ref(); ref.Download != wire.FilePath(mine, kept.ID) {
		t.Errorf("the download route is %q", ref.Download)
	}
	_, got, err := f.store.Open(t.Context(), ada.Owner, mine, kept.ID)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("ada's Open = %q, %v", got, err)
	}
	for name, read := range map[string]struct{ owner, conversation uuid.UUID }{
		"grace":                 {grace.Owner, mine},
		"ada, another of hers":  {ada.Owner, other},
		"ada, a made-up thread": {ada.Owner, uuid.New()},
	} {
		if _, err := f.store.Stat(t.Context(), read.owner, read.conversation, kept.ID); !errors.Is(err, assistantdomain.ErrNotFound) {
			t.Errorf("%s's Stat = %v, want ErrNotFound", name, err)
		}
		if _, _, err := f.store.Open(t.Context(), read.owner, read.conversation, kept.ID); !errors.Is(err, assistantdomain.ErrNotFound) {
			t.Errorf("%s's Open = %v, want ErrNotFound", name, err)
		}
	}
	// AND NOBODY KEEPS A FILE IN SOMEBODY ELSE'S CONVERSATION, before a
	// byte is uploaded.
	if _, _, err := f.store.Store(t.Context(), fresh(grace), assistantdomain.NewFile{
		ID: uuid.New(), Conversation: mine, Origin: wire.FileAttached, Name: "x.png",
		MediaType: "image/png", Content: png("x"),
	}); !errors.Is(err, assistantdomain.ErrNotFound) {
		t.Errorf("grace keeping a file in ada's conversation = %v, want ErrNotFound", err)
	}
	if listed := f.listed(t); listed != 1 {
		t.Errorf("the object store holds %d objects, want only ada's one", listed)
	}
}

// listed is how many objects the store holds.
func (f *files) listed(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.backend.List(t.Context(), func(objstore.Info) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	return n
}

// A FILE'S BYTES ARE SEALED IN THE OBJECT STORE, AND ITS NAME IN ITS ROW: the
// bucket holds a keyring envelope and none of the content, the row holds
// neither the name nor the content, and the object opens only as the file it
// was sealed for. Mutation: put the content into the store unsealed, or seal
// it under another file's associated data, and the marker is found or the
// read fails.
func TestAFilesBytesAreSealedInTheObjectStore(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	f := newFiles(t, n)
	ada := person(uuid.New(), "ada")
	conversation := n.conversation(ada, "")
	kept := f.attach(ada, conversation, "PAYROLL-NAME.png", png("PAYROLL-CONTENT"))
	other := f.attach(ada, conversation, "other.png", png("other"))

	if err := f.backend.List(t.Context(), func(info objstore.Info) error {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		body, err := f.backend.Get(ctx, info.Name, 0, -1)
		if err != nil {
			return err
		}
		defer func() { _ = body.Close() }()
		var raw bytes.Buffer
		if _, err := raw.ReadFrom(body); err != nil {
			return err
		}
		if bytes.Contains(raw.Bytes(), []byte("PAYROLL")) || bytes.Contains(raw.Bytes(), []byte("PNG")) {
			t.Errorf("object %s holds the file in the clear", info.Name)
		}
		if !secrets.IsEnvelope(raw.String()) {
			t.Errorf("object %s is not a keyring envelope", info.Name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := n.table("assistant_files"); strings.Contains(got, "PAYROLL") {
		t.Errorf("assistant_files holds the file in the clear:\n%s", got)
	}
	// SWAP THE TWO ROWS' OBJECTS: each now names bytes sealed for the other,
	// which must not open.
	if err := n.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			UPDATE assistant_files SET
				object = CASE id WHEN ? THEN (SELECT object FROM assistant_files WHERE id = ?)
					ELSE (SELECT object FROM assistant_files WHERE id = ?) END,
				hash = CASE id WHEN ? THEN (SELECT hash FROM assistant_files WHERE id = ?)
					ELSE (SELECT hash FROM assistant_files WHERE id = ?) END,
				object_size = CASE id WHEN ? THEN (SELECT object_size FROM assistant_files WHERE id = ?)
					ELSE (SELECT object_size FROM assistant_files WHERE id = ?) END
			WHERE id IN (?, ?)`,
			kept.ID.String(), other.ID.String(), kept.ID.String(),
			kept.ID.String(), other.ID.String(), kept.ID.String(),
			kept.ID.String(), other.ID.String(), kept.ID.String(),
			kept.ID.String(), other.ID.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.Open(t.Context(), ada.Owner, conversation, kept.ID); !errors.Is(err, assistantdomain.ErrUnreadable) {
		t.Errorf("a file opened over another file's object = %v, want ErrUnreadable", err)
	}
}

// A PURGE AND AN ERASURE TAKE A FILE'S ROW, AND THE COLLECTOR THEN ITS OBJECT:
// while a row names it the collector keeps it however old it is, and once the
// conversation is cleared, or its owner erased, the next pass deletes it.
// Mutation: drop assistant_files from the conversation tables, or its
// declaration from internal/objstore/references, and an object outlives its
// conversation or is deleted under a live one.
func TestAPurgedConversationsFilesAreCollected(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	f := newFiles(t, n)
	ada := person(uuid.New(), "ada")
	cleared, erased := n.conversation(ada, "Cleared"), n.conversation(ada, "Erased")
	first := f.objectOf(t, f.attach(ada, cleared, "one.png", png("one")))
	second := f.objectOf(t, f.attach(ada, erased, "two.png", png("two")))
	later := monday.Add(72 * time.Hour)

	if r, err := f.collector(t, later).Collect(t.Context()); err != nil || r.Deleted != 0 ||
		r.Referenced != 2 {
		t.Fatalf("a pass over two live files = %+v, %v", r, err)
	}
	if _, err := n.writer.ClearConversation(t.Context(), fresh(ada), cleared); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n.count("assistant_files", "conversation_id", cleared.String()) != 0 {
		t.Fatal("the cleared conversation's file row survived its purge")
	}
	if r, err := f.collector(t, later).Collect(t.Context()); err != nil || r.Deleted != 1 {
		t.Fatalf("a pass after the purge = %+v, %v", r, err)
	}
	if f.held(t, first) || !f.held(t, second) {
		t.Fatalf("after the purge the store holds the cleared file %v and the live one %v",
			f.held(t, first), f.held(t, second))
	}
	if _, err := n.writer.EraseOwner(t.Context(), fresh(ada)); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if n.rows("assistant_files") != 0 {
		t.Fatal("the erased person's file row survived the erasure")
	}
	if r, err := f.collector(t, later).Collect(t.Context()); err != nil || r.Deleted != 1 ||
		f.listed(t) != 0 {
		t.Fatalf("a pass after the erasure = %+v, %v, %d left", r, err, f.listed(t))
	}
}

// A CONVERSATION KEEPS AT MOST ITS BOUND OF ATTACHMENTS: the one past it is
// refused before a byte is uploaded, and an upload retried under its id is the
// file it already kept. Mutation: drop the writer's count and the bound's
// upload is published.
func TestAConversationKeepsAtMostItsBoundOfAttachments(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	f := newFiles(t, n)
	ada := person(uuid.New(), "ada")
	conversation := n.conversation(ada, "")
	var first assistantdomain.File
	for i := range assistantdomain.AttachmentsPerConversation {
		kept := f.attach(ada, conversation, "shot.png", png("shot"))
		if i == 0 {
			first = kept
		}
	}
	before := f.listed(t)
	_, _, err := f.store.Store(t.Context(), fresh(ada), assistantdomain.NewFile{
		ID: uuid.New(), Conversation: conversation, Origin: wire.FileAttached,
		Name: "one-more.png", MediaType: "image/png", Content: png("more"),
	})
	if !errors.Is(err, assistantdomain.ErrTooManyFiles) {
		t.Fatalf("the attachment past the bound = %v, want ErrTooManyFiles", err)
	}
	if f.listed(t) != before {
		t.Error("the refused attachment was uploaded anyway")
	}
	again, _, err := f.store.Store(t.Context(), fresh(ada), assistantdomain.NewFile{
		ID: first.ID, Conversation: conversation, Origin: wire.FileAttached,
		Name: "shot.png", MediaType: "image/png", Content: png("shot"),
	})
	if err != nil || again.ID != first.ID || f.listed(t) != before {
		t.Errorf("a retried upload = %+v, %v (%d objects, want %d)", again, err, f.listed(t), before)
	}
}

// THE BOUND IS DECIDED AGAIN AT THE APPLY: two uploads decided from one
// snapshot are both published, and the one that applies past the bound writes
// no row on any node. Mutation: drop the apply's count and the conversation
// keeps one file past its bound.
func TestTheFileBoundIsDecidedAgainAtTheApply(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ada := person(uuid.New(), "ada")
	conversation := n.conversation(ada, "")
	bound := assistantdomain.AttachmentsPerConversation
	for i := range bound + 1 {
		body, err := rawRecord(assistantdomain.KindConversation, conversation.String(), "node-a",
			op("file"), assistantdomain.OpFileStored, assistantdomain.FileRecorded{
				V: 1, File: uuid.NewString(), Owner: ada.Owner.String(),
				Origin: wire.FileAttached, NameSealed: "sealed-name", MediaType: "image/png",
				Size: 10, Object: objstore.KeyAt(monday).String(), Hash: hex64("a"),
				ObjectSize: 40, ContentKey: "k1",
			})
		if err != nil {
			t.Fatal(err)
		}
		n.applyRaw(uint64(1000+i), body)
	}
	if got := n.count("assistant_files", "conversation_id", conversation.String()); got != bound {
		t.Errorf("the conversation keeps %d attachments, want its bound %d", got, bound)
	}
}

// applyRaw applies one record body at seq through the shipped applier, outside
// the log: a record no writer of this build would publish.
func (n *node) applyRaw(seq uint64, body []byte) {
	n.t.Helper()
	spec := assistantdomain.Domain{}.Stream()
	envelope, err := assistantdomain.Domain{}.Envelope(body)
	if err != nil {
		n.t.Fatal(err)
	}
	record := statelog.Record{Envelope: envelope,
		Position: statelog.Position{Stream: spec.Name, Generation: envelope.Gen, Seq: seq},
		Payload:  body, StoredAt: monday}
	if err := n.db.Tx(n.t.Context(), func(tx *sql.Tx) error {
		_, err := n.applier.Apply(n.t.Context(), tx, record, statelog.ApplyOptions{
			Now: monday, StoredAt: monday, MaxVariables: 999})
		return err
	}); err != nil {
		n.t.Fatalf("apply record %d: %v", seq, err)
	}
}

// A REPLAY FROM ZERO REPRODUCES A CONVERSATION'S FILES AND ITS MESSAGES'
// ATTACHMENTS, byte for byte, on a node that applied none of them as they were
// written. Mutation: let the applier stamp a file's row with its own clock, or
// drop a column from its record, and the two nodes' rows differ.
func TestAReplayReproducesAConversationsFiles(t *testing.T) {
	t.Parallel()
	log := openLog(t)
	a := newNode(t, log, openStore(t, "a.db"), "node-a")
	f := newFiles(t, a)
	ada := person(uuid.New(), "ada")
	conversation := a.conversation(ada, "")
	kept := f.attach(ada, conversation, "chart.png", png("chart"))
	turn := a.openTurn(ada, conversation)
	if _, err := a.writer.RecordMessage(t.Context(), fresh(ada), turn, assistantdomain.NewMessage{
		ID: assistantdomain.MessageID(turn, 1), Role: wire.RoleUser, Text: "What is this?",
		Attachments: []wire.FileRef{kept.Ref()},
	}); err != nil {
		t.Fatalf("record the message: %v", err)
	}
	b := newNode(t, log, openStore(t, "b.db"), "node-b")
	b.drain()
	for _, table := range []string{"assistant_files", "assistant_messages"} {
		if got, want := b.table(table), a.table(table); got != want || want == "" {
			t.Errorf("%s replayed as\n%s\nwant\n%s", table, got, want)
		}
	}
}

// A USER MESSAGE CARRIES ITS ATTACHMENTS, SEALED: the transcript reads them
// back with the conversation's own download route, and a message that is not
// the person's own, or one past the per-question bound, is refused.
// Mutation: drop the attachments from the message's apply or its read, and the
// transcript shows none.
func TestAMessageCarriesItsAttachmentsSealed(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	f := newFiles(t, n)
	ada := person(uuid.New(), "ada")
	conversation := n.conversation(ada, "")
	kept := f.attach(ada, conversation, "chart.png", png("chart"))
	ref := kept.Ref()
	ref.Download = "/somewhere/else"
	turn := n.openTurn(ada, conversation)
	if _, err := n.writer.RecordMessage(t.Context(), fresh(ada), turn, assistantdomain.NewMessage{
		ID: assistantdomain.MessageID(turn, 1), Role: wire.RoleUser, Text: "What is this?",
		Attachments: []wire.FileRef{ref},
	}); err != nil {
		t.Fatalf("record the message: %v", err)
	}
	transcript, err := n.reader.Transcript(t.Context(), ada.Owner, conversation)
	if err != nil {
		t.Fatal(err)
	}
	got := transcript.Messages[0].Wire().Attachments
	if len(got) != 1 || got[0] != kept.Ref() {
		t.Fatalf("the message's attachments read back as %+v, want %+v", got, kept.Ref())
	}
	if table := n.table("assistant_messages"); strings.Contains(table, "chart.png") {
		t.Errorf("assistant_messages holds an attachment's name in the clear:\n%s", table)
	}
	for name, m := range map[string]assistantdomain.NewMessage{
		"an assistant message": {ID: assistantdomain.MessageID(turn, 2),
			Role: wire.RoleAssistant, Text: "Here", Attachments: []wire.FileRef{kept.Ref()}},
		"five attachments": {ID: assistantdomain.MessageID(turn, 3), Role: wire.RoleUser,
			Text: "These", Attachments: fiveRefs(kept)},
	} {
		if _, err := n.writer.RecordMessage(t.Context(), fresh(ada), turn, m); !errors.Is(err, assistantdomain.ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
}

// fiveRefs is one more attachment than a question carries.
func fiveRefs(f assistantdomain.File) []wire.FileRef {
	out := make([]wire.FileRef, wire.AttachmentsPerTurn+1)
	for i := range out {
		ref := f.Ref()
		ref.FileID = uuid.New()
		out[i] = ref
	}
	return out
}

// A COMPLIANCE READ OPENS ONE FILE ONLY FOR ITS RECORDED READ: the receipt
// opens the file somebody else owns, a node the read has not reached yet opens
// nothing (so the owner can see the read wherever the file was opened), a
// receipt nobody recorded opens nothing, and a file of another conversation is
// not found under it. Mutation: open the file without checking the read
// applied, and the node it has not reached opens it.
func TestAComplianceReadOpensOneFileOnlyForItsRecord(t *testing.T) {
	t.Parallel()
	log := openLog(t)
	n := newNode(t, log, openStore(t, "a.db"), "node-a")
	f := newFiles(t, n)
	ada := person(uuid.New(), "ada")
	conversation, other := n.conversation(ada, ""), n.conversation(ada, "")
	content := png("evidence")
	kept := f.attach(ada, conversation, "evidence.png", content)
	elsewhere := f.attach(ada, other, "elsewhere.png", png("elsewhere"))
	if _, _, err := f.store.AuditOpen(t.Context(), assistantdomain.AuditReceipt{}, kept.ID); !errors.Is(err, assistantdomain.ErrInvalid) {
		t.Errorf("a file opened for no receipt = %v, want ErrInvalid", err)
	}
	behind := newNode(t, log, openStore(t, "b.db"), "node-b")
	behind.drain()
	elsewhereStore, err := assistantdomain.NewFileStore(behind.writer, behind.reader, f.objects)
	if err != nil {
		t.Fatal(err)
	}
	admin := assistantdomain.Call{By: iam.Actor{Name: "grace", Kind: iam.ActorHuman}, OpID: op("audit")}
	receipt, err := n.writer.RecordAuditRead(t.Context(), admin, conversation, "A legal hold")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := elsewhereStore.AuditOpen(t.Context(), receipt, kept.ID); !errors.Is(err, assistantdomain.ErrAuditPending) {
		t.Errorf("a node the read has not reached opened the file: %v, want ErrAuditPending", err)
	}
	got, opened, err := f.store.AuditOpen(t.Context(), receipt, kept.ID)
	if err != nil || !bytes.Equal(opened, content) || got.Owner != ada.Owner {
		t.Fatalf("the audited open = %+v %q, %v", got, opened, err)
	}
	_, _, err = f.store.AuditOpen(t.Context(), receipt, elsewhere.ID)
	if !errors.Is(err, assistantdomain.ErrNotFound) || errors.Is(err, assistantdomain.ErrAuditPending) {
		t.Errorf("another conversation's file under the receipt = %v, want a plain ErrNotFound", err)
	}
}

// A FILE'S CONTENT IS COUNTED BY ITS KEY: a rotation must not drop a key an
// object is still sealed under, so the content counts beside the name.
// Mutation: drop the content count from SealedKeys and one fewer is counted.
func TestAFilesContentIsCountedByItsKey(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	f := newFiles(t, n)
	ada := person(uuid.New(), "ada")
	conversation := n.conversation(ada, "")
	before, err := n.reader.SealedKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.attach(ada, conversation, "a.png", png("a"))
	after, err := n.reader.SealedKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := after.Values["k1"] - before.Values["k1"]; got != 2 {
		t.Errorf("a kept file added %d sealed values under k1, want its name and its content", got)
	}
}

// A FILE OF A TYPE ITS ORIGIN MAY NOT BE IS REFUSED, and so is a name a
// download cannot carry, before anything is uploaded. Mutation: drop the
// origin's type list and an attached SVG is kept.
func TestAFileTheDomainCannotKeepIsRefused(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	f := newFiles(t, n)
	ada := person(uuid.New(), "ada")
	conversation := n.conversation(ada, "")
	for name, file := range map[string]assistantdomain.NewFile{
		"an attached svg": {MediaType: "image/svg+xml", Name: "a.svg", Origin: wire.FileAttached},
		"html":            {MediaType: "text/html", Name: "a.html", Origin: wire.FileDelivered},
		"a quoted name":   {MediaType: "image/png", Name: `a".png`, Origin: wire.FileAttached},
		"a path":          {MediaType: "image/png", Name: "../a.png", Origin: wire.FileAttached},
		"no origin":       {MediaType: "image/png", Name: "a.png"},
	} {
		file.ID, file.Conversation, file.Content = uuid.New(), conversation, png("x")
		if _, _, err := f.store.Store(t.Context(), fresh(ada), file); !errors.Is(err, assistantdomain.ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
	if f.listed(t) != 0 {
		t.Error("a refused file was uploaded")
	}
}

// A FILE ID NAMES ONE FILE FOR EVER: an id another conversation's file holds is
// refused rather than published as a record that would apply as nothing and
// read back as a file that is not there. Mutation: drop the writer's check and
// the second store answers a bound it never reached.
func TestAFileIDNamesOneFile(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	f := newFiles(t, n)
	ada := person(uuid.New(), "ada")
	first, second := n.conversation(ada, ""), n.conversation(ada, "")
	kept := f.attach(ada, first, "a.png", png("a"))
	_, _, err := f.store.Store(t.Context(), fresh(ada), assistantdomain.NewFile{
		ID: kept.ID, Conversation: second, Origin: wire.FileAttached, Name: "b.png",
		MediaType: "image/png", Content: png("b"),
	})
	if !errors.Is(err, assistantdomain.ErrInvalid) {
		t.Errorf("a file id reused in another conversation = %v, want ErrInvalid", err)
	}
}
