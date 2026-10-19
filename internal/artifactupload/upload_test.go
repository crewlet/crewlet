package artifactupload_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// NO BYTE IS ACCEPTED BEFORE ITS RESERVATION: a transfer to an
// upload nobody reserved, or of a file its intent did not declare, stores
// nothing; an intent the budget refuses answers nothing to transfer and holds
// nothing; an intent that is answered holds every byte it declared before the
// first one moves, whether it is streamed or signed for the bucket; and the
// pipeline's own objects are held to the same budget before they are stored.
func TestNoByteIsAcceptedBeforeItsReservation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	c := h.person("ada", "")
	if err := h.svc.Transfer(t.Context(), c, "4e2e6a2e-8a5d-4e57-a7c1-6c0f5a7b9a11", "a.txt",
		strings.NewReader("x"), 1); !errors.Is(err, artifactupload.ErrUploadNotFound) {
		t.Fatalf("a transfer to an upload nobody reserved answered %v", err)
	}
	files := map[string]string{"index.html": "<p>hello</p>"}
	intent, err := h.svc.Intend(t.Context(), c, artifacts.ID{}, declare(files))
	if err != nil {
		t.Fatal(err)
	}
	if got := h.held(); got != int64(len(files["index.html"])) {
		t.Fatalf("the intent holds %d bytes before any transfer, want the %d it declared", got, len(files["index.html"]))
	}
	if err := h.svc.Transfer(t.Context(), c, intent.Upload, "other.html", strings.NewReader("<p>x</p>"),
		8); !errors.Is(err, artifactupload.ErrInvalid) {
		t.Fatalf("a transfer of an undeclared file answered %v", err)
	}
	if len(h.stored()) != 0 {
		t.Fatalf("refused transfers stored %d objects", len(h.stored()))
	}

	// THE BUDGET REFUSES BEFORE A KEY IS MINTED.
	h.setLimits(func(l *artifacts.Limits) { l.PendingBytes = 64 })
	big := map[string]string{"big.txt": strings.Repeat("b", 100)}
	refused, err := h.svc.Intend(t.Context(), h.person("ada", ""), artifacts.ID{}, declare(big))
	var pending *artifactupload.UploadsOutstanding
	if !errors.As(err, &pending) || pending.Scope != "principal" || refused.Upload != "" || len(refused.Transfers) != 0 {
		t.Fatalf("an intent past the budget answered %+v, %v", refused, err)
	}
	if got := h.meter.count(metrics.ArtifactsPublishRefusals, metrics.Attrs{"reason": "uploads_outstanding"}); got != 1 {
		t.Fatalf("the refusal was counted %d times", got)
	}

	// THE PIPELINE'S OWN BYTES ARE HELD TO IT TOO: inline content past the
	// budget is refused before it is stored.
	_, _, err = h.svc.Create(t.Context(), h.person("ada", h.opKey("create")), artifactupload.CreateRequest{
		Kind: artifacts.KindDoc, Title: "x", Owner: "ada", OwnerKind: "human", Entry: "a.md",
		Files: []artifactupload.FileSpec{{Path: "a.md", Inline: []byte(strings.Repeat("m", 100))}},
	})
	if !errors.As(err, &pending) {
		t.Fatalf("inline content past the budget answered %v", err)
	}
	if len(h.stored()) != 0 {
		t.Fatalf("content past the budget was stored: %d objects", len(h.stored()))
	}

	// A SIGNED PUT IS HANDED OUT ONLY ONCE ITS BYTES ARE HELD.
	d := newHarness(t, options{presign: true})
	direct, err := d.svc.Intend(t.Context(), d.person("ada", ""), artifacts.ID{}, declare(files))
	if err != nil {
		t.Fatal(err)
	}
	if len(direct.Transfers) != 1 || !direct.Transfers[0].Direct || d.held() != int64(len(files["index.html"])) {
		t.Fatalf("a direct intent answered %+v holding %d bytes", direct, d.held())
	}
	// A TOOL NEVER GETS ONE: a sandbox is not handed a credential.
	tool, err := d.svc.Intend(t.Context(), d.agent("bot", "turn-1", ""), artifacts.ID{}, declare(files))
	if err != nil {
		t.Fatal(err)
	}
	if len(tool.Transfers) != 1 || tool.Transfers[0].Direct {
		t.Fatalf("an agent's tool was handed a direct transfer: %+v", tool)
	}
}

// FINALIZE REFUSES BYTES THAT DO NOT MATCH THEIR DIGEST: a stream
// through the engine of other bytes is refused and its object deleted at once,
// the file left to send again; bytes a client put straight to the bucket are
// read back through the verifying reader before any record names them.
func TestFinalizeRefusesBytesThatDoNotMatchTheirDigest(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	c := h.person("ada", "")
	files := map[string]string{"index.html": "<p>the real page</p>"}
	intent, err := h.svc.Intend(t.Context(), c, artifacts.ID{}, declare(files))
	if err != nil {
		t.Fatal(err)
	}
	forged := "<p>the forged one</p>"
	forged = forged[:len(files["index.html"])]
	err = h.svc.Transfer(t.Context(), c, intent.Upload, "index.html", strings.NewReader(forged), int64(len(forged)))
	var mismatch *artifactupload.UploadMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("a stream of other bytes answered %v", err)
	}
	if len(h.stored()) != 0 {
		t.Fatal("the forged bytes were left in the store")
	}
	if _, states, _ := artifactupload.UploadState(t.Context(), h.svc, intent.Upload); states["index.html"] != "pending" {
		t.Fatalf("the file is %q after a refused stream, want pending", states["index.html"])
	}
	if err := h.svc.Transfer(t.Context(), c, intent.Upload, "index.html",
		strings.NewReader(files["index.html"]), -1); err != nil {
		t.Fatalf("the real bytes were refused after a forged stream: %v", err)
	}
	if err := h.svc.Transfer(t.Context(), c, intent.Upload, "index.html",
		strings.NewReader(files["index.html"]), -1); !errors.Is(err, artifactupload.ErrUploadConsumed) {
		t.Fatalf("a second put of a stored file answered %v", err)
	}

	// THE BUCKET'S BYTES ARE READ BACK.
	d := newHarness(t, options{presign: true})
	dc := d.person("ada", d.opKey("create"))
	direct, err := d.svc.Intend(t.Context(), dc, artifacts.ID{}, declare(files))
	if err != nil {
		t.Fatal(err)
	}
	create := artifactupload.CreateRequest{Kind: artifacts.KindHTML, Title: "x", Owner: "ada",
		OwnerKind: "human", Upload: direct.Upload, Entry: "index.html", Files: specs(files)}
	if _, _, err := d.svc.Create(t.Context(), dc, create); !errorsAsMissing(err) {
		t.Fatalf("a publish before the client's put answered %v", err)
	}
	key, err := artifactupload.UploadKey(t.Context(), d.svc, direct.Upload, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	k, err := objstore.ParseKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.backend.Put(t.Context(), k.Name(), strings.NewReader(forged), objstore.PutMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.svc.Create(t.Context(), dc, create); !errors.As(err, &mismatch) {
		t.Fatalf("a publish of bytes the bucket holds under another digest answered %v", err)
	}
}

func errorsAsMissing(err error) bool {
	var missing *artifactupload.UploadMissing
	return errors.As(err, &missing)
}

// UNNAMED BYTES NEVER EXCEED THE RESERVATION BUDGET: through
// uploads, transfers, publishes, a refused publish, an abandoned upload and a
// window closing, every byte the store holds that no row names is held by a
// reservation, and an abandoned upload the engine streamed is released, its
// bytes deleted, once its window closes.
func TestUnnamedBytesNeverExceedTheReservationBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	check := func(step string) {
		t.Helper()
		if unnamed, held := h.unnamedBytes(), h.held(); unnamed > held {
			t.Fatalf("after %s the store holds %d bytes nothing names and the reservations hold %d",
				step, unnamed, held)
		}
	}
	ada := h.person("ada", h.opKey("create"))
	created := h.create(ada, map[string]string{"index.html": page})
	check("a create")

	abandoned := h.upload(h.person("ada", ""), created.Artifact, map[string]string{"draft.txt": "a draft"})
	check("an upload nothing published")

	c := h.person("ada", h.opKey("publish"))
	up := h.upload(c, created.Artifact, map[string]string{"index.html": strings.Replace(page, "Quarterly", "Annual", 1)})
	if _, _, err := h.svc.Publish(t.Context(), c, artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch, Upload: up,
		Files: []artifactupload.FileSpec{{Path: "index.html", Upload: true}},
	}); err != nil {
		t.Fatal(err)
	}
	check("a publish that superseded its upload's page with the linted one")

	refusedUp := h.upload(h.person("ada", ""), created.Artifact, map[string]string{"x.html": "<script src=\"https://evil.example/x.js\"></script>"})
	if _, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("bad")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 2, Mode: manifest.ModePatch, Upload: refusedUp,
		Files: []artifactupload.FileSpec{{Path: "x.html", Upload: true}},
	}); !errors.Is(err, lint.ErrRefused) {
		t.Fatalf("a publish of a remote script answered %v", err)
	}
	check("a refused publish")

	h.clock.advance(objstore.MaxPresignTerm + time.Minute)
	if _, err := h.svc.Intend(t.Context(), h.person("bob", ""), created.Artifact,
		declare(map[string]string{"n.txt": "next"})); err != nil {
		t.Fatal(err)
	}
	check("windows closing")
	if state, _, _ := artifactupload.UploadState(t.Context(), h.svc, abandoned); state != "" {
		t.Fatalf("the abandoned upload's record is %q after its window closed, want released", state)
	}
	if unnamed := h.unnamedBytes(); unnamed != int64(len("next")) && unnamed != 0 {
		t.Fatalf("after the early releases %d bytes nothing names remain", unnamed)
	}

	h.clock.advance(artifactupload.MaxHold)
	if _, err := h.svc.Intend(t.Context(), h.person("bob", ""), created.Artifact,
		declare(map[string]string{"m.txt": "more"})); err != nil {
		t.Fatal(err)
	}
	check("every hold lapsing")
}

// A REFUSED PUBLISH LEAVES NOTHING BEHIND: every object the pipeline wrote for
// it (the linted page, the copy of its CDN file, inline content) is deleted and
// its reservation released, and the caller's upload is open again, its own
// bytes still held, so a corrected publish of it lands.
func TestARefusedPublishLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": "<p>v1</p>"})
	if _, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("label")), artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch, Label: "release",
		Files: []artifactupload.FileSpec{{Path: "notes.txt", Inline: []byte("v2 notes")}},
	}); err != nil {
		t.Fatal(err)
	}
	before := h.stored()

	c := h.person("ada", h.opKey("refused"))
	files := map[string]string{"index.html": strings.ReplaceAll(page, "chart.js@4.4.1", "chart.js@4.4.2")}
	up := h.upload(c, created.Artifact, files)
	req := artifactupload.PublishRequest{
		Artifact: created.Artifact, Base: 2, Mode: manifest.ModePatch, Upload: up, Label: "release",
		Files: []artifactupload.FileSpec{
			{Path: "index.html", Upload: true},
			{Path: "extra.txt", Inline: []byte("inline bytes")},
		},
	}
	_, _, err := h.svc.Publish(t.Context(), c, req)
	if !errors.Is(err, artifactsdomain.ErrLabelTaken) {
		t.Fatalf("a publish taking a held label answered %v", err)
	}
	after := h.stored()
	for k := range after {
		if _, had := before[k]; had {
			continue
		}
		if size, isUpload := after[k], int64(len(files["index.html"])); size != isUpload {
			t.Errorf("the refused publish left object %s (%d bytes) behind", k, size)
		}
	}
	if len(after) != len(before)+1 {
		t.Errorf("the store holds %d objects after the refusal, want the %d before and the upload's page",
			len(after), len(before))
	}
	if held := h.held(); held != int64(len(files["index.html"])) {
		t.Errorf("the reservations hold %d bytes after the refusal, want the upload's own %d",
			held, len(files["index.html"]))
	}
	if state, _, _ := artifactupload.UploadState(t.Context(), h.svc, up); state != "open" {
		t.Errorf("the upload is %q after the refusal, want open", state)
	}
	req.Label = "next"
	if _, _, err := h.svc.Publish(t.Context(), h.person("ada", h.opKey("retry")), req); err != nil {
		t.Fatalf("the corrected publish of the same upload was refused: %v", err)
	}
	if h.held() != 0 || h.unnamedBytes() != 0 {
		t.Fatalf("the corrected publish leaves %d held and %d unnamed", h.held(), h.unnamedBytes())
	}
}

// THE PER-TURN GUARDRAILS (D28) refuse an agent's turn past twenty publishes,
// ten of them on one artifact, or past 128 MiB written, before a byte moves,
// and a person's call is held to none of them.
func TestATurnIsHeldToItsGuardrails(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	h.setLimits(func(l *artifacts.Limits) { l.PendingBytes = 1 << 30 })
	// A FULL PUBLISH'S WORTH, declared and never sent: the guardrail refuses
	// before a byte moves, so none has to.
	full := func(prefix string) artifacts.UploadRequest {
		var req artifacts.UploadRequest
		for i := range 4 {
			req.Entries = append(req.Entries, artifacts.UploadEntry{
				Path: prefix + string(rune('a'+i)) + ".txt", SHA256: objstore.HashOf([]byte(prefix)),
				Size: artifacts.MaxTextFileBytes - 1024})
		}
		return req
	}
	turn := h.agent("bot", "turn-1", "")
	for _, prefix := range []string{"a", "b"} {
		if _, err := h.svc.Intend(t.Context(), turn, artifacts.ID{}, full(prefix)); err != nil {
			t.Fatalf("intent %s: %v", prefix, err)
		}
	}
	_, err := h.svc.Intend(t.Context(), turn, artifacts.ID{}, full("c"))
	var quota *artifactupload.TurnQuota
	if !errors.As(err, &quota) || quota.What != "bytes" {
		t.Fatalf("a third full intent in one turn answered %v", err)
	}
	if _, err := h.svc.Intend(t.Context(), h.agent("bot", "turn-2", ""), artifacts.ID{}, full("c")); err != nil {
		t.Fatalf("the next turn was refused: %v", err)
	}

	created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": "<p>x</p>"})
	head := 1
	for i := range artifacts.TurnPublishesPerArtifact + 1 {
		got, _, err := h.svc.Publish(t.Context(), h.agent("bot", "turn-3", h.opKey("p")), artifactupload.PublishRequest{
			Artifact: created.Artifact, Base: head, Mode: manifest.ModePatch,
			Files: []artifactupload.FileSpec{{Path: "n.txt", Inline: []byte{byte('a' + i)}}},
		})
		if i < artifacts.TurnPublishesPerArtifact {
			if err != nil {
				t.Fatalf("publish %d: %v", i, err)
			}
			head = got.Number
			continue
		}
		if !errors.As(err, &quota) || quota.What != "publishes_on_artifact" {
			t.Fatalf("publish %d on one artifact answered %v", i, err)
		}
	}
}

// AN AGENT NEVER PUBLISHES A CREDENTIAL, and a file is what its type says.
func TestContentIsHeldToItsTypeAndAgentsToNoCredential(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	key := "AKIA" + "IOSFODNN7EXAMPLE"
	_, _, err := h.svc.Create(t.Context(), h.agent("bot", "turn-1", h.opKey("c")), artifactupload.CreateRequest{
		Kind: artifacts.KindDoc, Title: "x", Owner: "bot", OwnerKind: "agent", Entry: "a.md",
		Files: []artifactupload.FileSpec{{Path: "a.md", Inline: []byte("# Setup\n\nkey: " + key + "\n")}},
	})
	if !errors.Is(err, artifactupload.ErrCredential) || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("an agent's credential answered %v", err)
	}
	if _, _, err := h.svc.Create(t.Context(), h.person("ada", h.opKey("c")), artifactupload.CreateRequest{
		Kind: artifacts.KindDoc, Title: "x", Owner: "ada", OwnerKind: "human", Entry: "a.md",
		Files: []artifactupload.FileSpec{{Path: "a.md", Inline: []byte("# Setup\n\nkey: " + key + "\n")}},
	}); err != nil {
		t.Fatalf("a person's page was held to the agent rule: %v", err)
	}
	c := h.person("ada", h.opKey("png"))
	files := map[string]string{"index.html": "<p>x</p>", "logo.png": "<html><script>alert(1)</script>"}
	up := h.upload(c, artifacts.ID{}, files)
	_, _, err = h.svc.Create(t.Context(), c, artifactupload.CreateRequest{Kind: artifacts.KindHTML,
		Title: "x", Owner: "ada", OwnerKind: "human", Upload: up, Entry: "index.html", Files: specs(files)})
	var content *artifactupload.ContentRefused
	if !errors.As(err, &content) || content.Path != "logo.png" {
		t.Fatalf("an HTML document published as a PNG answered %v", err)
	}
}

// AN ASSET IS FINALIZED ONE FILE AT A TIME, and its reservation goes with the
// last of them.
func TestAnAssetUploadIsFinalizedFileByFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": "<p>x</p>"})
	c := h.person("ada", h.opKey("asset"))
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16)
	files := map[string]string{"a.png": png, "notes.txt": "asset notes"}
	intent, err := h.svc.Intend(t.Context(), c, created.Artifact, artifacts.UploadRequest{
		Entries: declare(files).Entries, Asset: true})
	if err != nil {
		t.Fatal(err)
	}
	for p, body := range files {
		if err := h.svc.Transfer(t.Context(), c, intent.Upload, p, strings.NewReader(body), -1); err != nil {
			t.Fatal(err)
		}
	}
	asset, err := h.svc.AddAsset(t.Context(), h.person("ada", h.opKey("a1")), created.Artifact, intent.Upload, "a.png")
	if err != nil {
		t.Fatal(err)
	}
	if asset.Name != "a.png" || asset.ContentType != "image/png" {
		t.Fatalf("the asset is %+v", asset)
	}
	if h.held() != int64(len(files["notes.txt"])) {
		t.Fatalf("after one asset the reservations hold %d bytes", h.held())
	}
	if _, err := h.svc.AddAsset(t.Context(), h.person("ada", h.opKey("a2")), created.Artifact, intent.Upload, "notes.txt"); err != nil {
		t.Fatal(err)
	}
	if h.held() != 0 || h.unnamedBytes() != 0 {
		t.Fatalf("after every asset %d bytes are held and %d unnamed", h.held(), h.unnamedBytes())
	}
}

// AN UPLOAD IS ITS PRINCIPAL'S, AND ITS WINDOW CLOSES: another principal is
// told it does not exist, and nothing is transferred or finalized after.
func TestAnUploadIsItsPrincipalsAndClosesWithItsWindow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	files := map[string]string{"index.html": "<p>x</p>"}
	intent, err := h.svc.Intend(t.Context(), h.person("ada", ""), artifacts.ID{}, declare(files))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Transfer(t.Context(), h.person("eve", ""), intent.Upload, "index.html",
		strings.NewReader(files["index.html"]), -1); !errors.Is(err, artifactupload.ErrUploadNotFound) {
		t.Fatalf("another principal's transfer answered %v", err)
	}
	if _, err := h.svc.Lookup(t.Context(), h.person("eve", ""), intent.Upload); !errors.Is(err, artifactupload.ErrUploadNotFound) {
		t.Fatalf("another principal's lookup answered %v", err)
	}
	h.clock.advance(intent.ExpiresAt.Sub(h.clock.Now()) + time.Second)
	if err := h.svc.Transfer(t.Context(), h.person("ada", ""), intent.Upload, "index.html",
		strings.NewReader(files["index.html"]), -1); !errors.Is(err, artifactupload.ErrUploadExpired) {
		t.Fatalf("a transfer after the window answered %v", err)
	}
}

// A RETRIED INTENT IS THE SAME UPLOAD: the same keys, nothing held twice.
func TestARetriedIntentIsTheSameUpload(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	c := h.person("ada", h.opKey("intent"))
	files := map[string]string{"index.html": "<p>x</p>"}
	first, err := h.svc.Intend(t.Context(), c, artifacts.ID{}, declare(files))
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.svc.Intend(t.Context(), c, artifacts.ID{}, declare(files))
	if err != nil {
		t.Fatal(err)
	}
	if again.Upload != first.Upload || h.held() != int64(len(files["index.html"])) {
		t.Fatalf("the retry answered upload %s (first %s) holding %d", again.Upload, first.Upload, h.held())
	}
}

// THE COORDINATION STORE KEEPS A RESERVATION AS LONG AS ITS HOLD: a record
// aged out while its hold was live would stop the ledger counting bytes the
// store still has.
func TestTheUploadRecordsOutliveTheLongestHold(t *testing.T) {
	t.Parallel()
	if coord.UploadRetention <= artifactupload.MaxHold {
		t.Fatalf("coord.UploadRetention %v does not outlive the longest hold %v",
			coord.UploadRetention, artifactupload.MaxHold)
	}
}

// EVERY BINARY TYPE THE MEDIA TABLE SERVES IS HELD TO ITS SIGNATURE: a type
// added to the table with none would be refused at every upload.
func TestEveryBinaryTypeHasASignature(t *testing.T) {
	t.Parallel()
	for _, row := range mediatype.Table() {
		for _, typ := range row.Types {
			if !typ.Text && !artifactupload.Signed(typ.Essence()) {
				t.Errorf("%s (%s) has no signature its bytes are held to", typ.Essence(), row.Extension)
			}
		}
	}
}

// ONE RUN OF AN OPERATION AT A TIME: a second run of the same publish beside
// one still within its lease is refused, so two attempts never compose two
// versions from one operation, and a run that died gives the upload back when
// the lease lapses.
func TestASecondRunOfOnePublishWaitsForTheFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	created := h.create(h.person("ada", h.opKey("create")), map[string]string{"index.html": "<p>x</p>"})
	c := h.person("ada", h.opKey("publish"))
	up := h.upload(c, created.Artifact, map[string]string{"n.txt": "notes"})
	if err := artifactupload.ClaimUpload(t.Context(), h.svc, c, up, created.Artifact); err != nil {
		t.Fatal(err)
	}
	req := artifactupload.PublishRequest{Artifact: created.Artifact, Base: 1, Mode: manifest.ModePatch,
		Upload: up, Files: []artifactupload.FileSpec{{Path: "n.txt", Upload: true}}}
	if _, _, err := h.svc.Publish(t.Context(), c, req); !errors.Is(err, artifactupload.ErrUploadConsumed) {
		t.Fatalf("a second run beside a live one answered %v", err)
	}
	h.clock.advance(20 * time.Minute)
	if _, _, err := h.svc.Publish(t.Context(), c, req); err != nil {
		t.Fatalf("the run after the lease lapsed was refused: %v", err)
	}
}
