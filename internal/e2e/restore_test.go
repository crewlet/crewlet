package e2e

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/backup/backuptest"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
)

// A NODE RESTORED FROM A BACKUP, by the documented runbook (docs/guides/
// backup.md, "Restoring" and "Restoring into a new bucket"): its two store
// files put back, its stream snapshots restored into a broker started for the
// purpose, and the backup's `objects/` copied into a NEW, EMPTY bucket under
// the configured prefix. ONE node, because restore is an operator procedure on
// a stopped fleet and one data node holds the company's whole state; what the
// three-member restore would add is the cold start of a quorum, which the
// fleet cases already make.

// restoreNodeID is the restored node's id, the same before and after: the
// runbook keeps node identity, because a clustered member's replicas are
// placed by it.
const restoreNodeID = "restore-node"

// restoreBucket and restorePrefix are where the restored company keeps its
// files. A PREFIX, so the copy into the new bucket is the runbook's "under the
// configured prefix" rather than the bucket's root by luck.
const (
	restoreBucket = "code"
	restorePrefix = "nimbus/"
)

// objectStoreRecord is the coordination bucket that records which object store
// the fleet's files are in — the stream a restore into a new bucket leaves
// out, by the name the runbook gives it.
const objectStoreRecord = "KV_crewlet_objects"

// s3Objects is the store.objects block for an s3fake at endpoint: the s3
// backend, path-style as MinIO and most gateways need, with placeholder keys —
// the fake checks no signature.
func s3Objects(endpoint string) config.StoreObjects {
	return config.StoreObjects{Backend: config.ObjectBackendS3, S3: config.ObjectsS3{
		Endpoint: endpoint, Region: "us-east-1", Bucket: restoreBucket, Prefix: restorePrefix,
		PathStyle: true, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
	}}
}

// restoreBoot is the restored node's Tier A on its files under dir and its
// bucket at endpoint: coordination on the broker, as every fleet runs it, so
// the backup carries the coordination buckets — the activation pointer, the
// sealed secrets, the object store's record — and the restore has to put them
// back; and the founder's and the developer's tokens.
func restoreBoot(endpoint, dir string) func(*config.Bootstrap) {
	return func(b *config.Bootstrap) {
		b.Node.ID = restoreNodeID
		b.Coordination.Type = config.CoordinationEmbeddedKV
		b.Store.Path = filepath.Join(dir, "crewlet.db")
		b.Stream.StoreDir = filepath.Join(dir, "stream")
		b.Store.Objects = s3Objects(endpoint)
		b.API.Auth.Tokens = []config.APIToken{
			{ID: e2eOperatorID, Token: e2eOperatorToken},
			{ID: e2eDevID, Token: e2eDevToken},
		}
	}
}

// takeBackup asks n for a backup into dir through POST /backup, as an
// operator takes one, and answers its manifest.
func takeBackup(t *testing.T, n *node, dir string) backup.Manifest {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		n.server.URL+"/backup?dir="+url.QueryEscape(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e2eOperatorToken)
	resp, err := n.server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /backup: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the backup's answer: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /backup answered %d: %s", resp.StatusCode, body)
	}
	var m backup.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("the backup's manifest: %v\n%s", err, body)
	}
	return m
}

// restoreInto restores the backup in dir as the runbook does, into fresh paths
// under a new directory it answers — the store copies at the node's
// store.path and replicated path, with no -wal beside either, and every stream
// snapshot opts does not leave out restored into a BARE broker on the node's
// stream.store_dir, which is then stopped so the node's own broker starts on
// it. Bare, because a node's boot provisions its own streams and a restore
// over an existing stream is refused.
func restoreInto(t *testing.T, dir string, m backup.Manifest, opts ...backuptest.Option) string {
	t.Helper()
	into := t.TempDir()
	nodePath := filepath.Join(into, "crewlet.db")
	for _, copied := range m.Stores {
		var to string
		switch copied.Estate {
		case store.EstateNode:
			to = nodePath
		case store.EstateReplicated:
			to = store.ReplicatedPath(nodePath, "")
		default:
			t.Fatalf("the manifest names a store copy of estate %q", copied.Estate)
		}
		copyFile(t, filepath.Join(dir, copied.File), to)
	}

	broker, err := jetstream.StartServer(t.Context(), jetstream.Config{
		StoreDir: filepath.Join(into, "stream"), ServerName: restoreNodeID, Replicas: 1,
	})
	if err != nil {
		t.Fatalf("start the broker to restore into: %v", err)
	}
	defer broker.Shutdown()
	conn, err := broker.Conn()
	if err != nil {
		t.Fatalf("connect to the broker to restore into: %v", err)
	}
	defer conn.Close()
	if err := backuptest.RestoreStreams(t.Context(), conn, jetstream.Config{}.API(), dir, m,
		opts...); err != nil {
		t.Fatalf("restore the streams: %v", err)
	}
	return into
}

// copyFile copies one file, whole, to a path that does not exist yet.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatalf("open %s: %v", from, err)
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create %s: %v", to, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		t.Fatalf("copy %s to %s: %v", from, to, err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close %s: %v", to, err)
	}
}

// copyObjects copies the backup's objects into bucket under the configured
// prefix, each at its relative name — the `aws s3 sync objects/ s3://…/<prefix>`
// of the runbook — and answers how many it put.
func copyObjects(t *testing.T, bucket *s3fake.Server, dir string, m backup.Manifest) int {
	t.Helper()
	if m.Objects == nil || m.Objects.Dir == "" {
		t.Fatalf("the manifest carries no objects directory: %+v", m.Objects)
	}
	root := filepath.Join(dir, m.Objects.Dir)
	put := 0
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		bucket.Put(restorePrefix+filepath.ToSlash(rel), data)
		put++
		return nil
	}); err != nil {
		t.Fatalf("copy the backup's objects: %v", err)
	}
	return put
}

// A RESTORED NODE SERVES EVERY REF WHOLE.
//
// The claim the backup exists for, for the code host: a node restored from
// `crewlet backup` — its store files, its stream snapshots, and the backup's
// objects copied into a new, empty bucket — answers a clone with every
// branch, the tag and its peeled line exactly as the node backed up did, and
// the clone is a whole repository, a 2 MiB blob in a pack of its own
// included. The restored node's cache starts empty, so every byte it serves
// came from the rows the restore put back and the objects the backup carried:
// a backup that carried a pack's row and not its object, or a restore whose
// code log no longer matched its rows, is a clone that fails here.
//
// THE CONTROL is the runbook's own warning: restored WITH the old store's
// record, a node pointed at the new bucket is refused by the object store
// agreement, which names the backed-up node's store as the fleet's beside the
// new one — the defect the "into a new bucket" step exists for, and the
// failure dropping the skip from the main case turns it into.
func TestARestoredNodeServesEveryRefWhole(t *testing.T) {
	t.Parallel()
	const repo = "restored"

	fakeA := s3fake.Start(t, restoreBucket)
	a := startNode(t, nodeSpec{company: withCode(repo), boot: restoreBoot(fakeA.URL, t.TempDir())})
	awaitRepo(t, repo, code.RepoStatusActive, a)

	// FOUR PUSHES, SO FOUR PACKS — one object each: three commits on main,
	// one carrying an incompressible blob so one pack is not trivially
	// small, then a branch with a commit of its own and an annotated tag
	// pushed together.
	dev := codetest.PersonWithToken(t, e2eDevToken)
	work := dev.Init()
	push := func(refspecs ...string) {
		t.Helper()
		pushed := dev.Push(t.Context(), work, gitURL(a, repo), refspecs...)
		if pushed.Err != nil {
			t.Fatalf("git push %v: %s", refspecs, pushed)
		}
	}
	dev.Commit(work, map[string]string{"README.md": "# restored\n"}, "first")
	push("main")
	big := make([]byte, 2<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	second := dev.Commit(work, map[string]string{"assets/big.bin": string(big)}, "a large file")
	blob := strings.TrimSpace(dev.Git().Run(t, work, "rev-parse", second+":assets/big.bin"))
	push("main")
	third := dev.Commit(work, map[string]string{"README.md": "# restored\n\nthird\n"}, "third")
	push("main")
	dev.Git().Run(t, work, "checkout", "--quiet", "-b", "topic")
	dev.Commit(work, map[string]string{"TOPIC.md": "a topic\n"}, "on topic")
	dev.Git().Run(t, work, "tag", "-a", "v1", "-m", "release one", third)
	push("refs/heads/topic", "refs/tags/v1")
	want := map[int]map[string]string{}
	for _, v := range []int{0, 2} {
		want[v] = dev.LsRemote(gitURL(a, repo), v)
	}
	if want[2]["refs/tags/v1^{}"] != third || want[2]["refs/heads/topic"] == "" {
		t.Fatalf("the node backed up advertises %v, without the topic branch or the tag's "+
			"peeled line — the history this case restores is not the one it pushed", want[2])
	}

	backupDir := filepath.Join(t.TempDir(), "backup")
	manifest := takeBackup(t, a, backupDir)
	if manifest.Objects == nil || manifest.Objects.Objects != 4 {
		t.Fatalf("the backup carries %+v, want the four objects its four pushes stored",
			manifest.Objects)
	}
	if !slices.ContainsFunc(manifest.Streams, func(s backup.StreamArtifact) bool {
		return s.Name == topics.CodeLogStream
	}) {
		t.Fatalf("the backup holds no snapshot of %s, the log the restored rows are keyed to",
			topics.CodeLogStream)
	}
	a.stop()
	oldBucket := fakeA.Requests()

	restored := restoreInto(t, backupDir, manifest, backuptest.Skip(objectStoreRecord))
	fakeB := s3fake.Start(t, restoreBucket)
	if put := copyObjects(t, fakeB, backupDir, manifest); put != manifest.Objects.Objects {
		t.Fatalf("the backup's objects directory holds %d objects and its manifest says %d",
			put, manifest.Objects.Objects)
	}
	b := startNode(t, nodeSpec{company: withCode(repo), boot: restoreBoot(fakeB.URL, restored)})

	for _, v := range []int{0, 2} {
		if got := dev.LsRemote(gitURL(b, repo), v); !maps.Equal(got, want[v]) {
			t.Errorf("the restored node advertises %v under protocol %d, and the node backed "+
				"up advertised %v", got, v, want[v])
		}
	}
	clone := dev.Clone(gitURL(b, repo))
	dev.Git().Run(t, clone, "fsck", "--full", "--strict")
	size := strings.TrimSpace(dev.Git().Run(t, clone, "cat-file", "-s", blob))
	if size != strconv.Itoa(len(big)) {
		t.Errorf("the clone's large file is %s bytes, want %d", size, len(big))
	}
	// EVERY BYTE FROM THE NEW BUCKET, EACH ONCE: the restored node's cache
	// began empty, so it read each pack the clone holds from the bucket it
	// was pointed at — ONE GET A BUNDLE, with no HEAD before it, and none of
	// the requests after the first full round (the second advertisement,
	// the clone) reading a pack that round installed — and the old bucket
	// was asked nothing after its node stopped. EXACT, because a cold sync
	// is bound by its GETs at S3's first byte: one that asked twice a bundle
	// would serve the same clone at half the rate with nothing else failing.
	if gets := fakeB.RequestsOf("GetObject"); gets != manifest.Objects.Objects {
		t.Errorf("the restored node sent its bucket %d GETs serving %d packs from an empty "+
			"cache, want one a bundle", gets, manifest.Objects.Objects)
	}
	if heads := fakeB.RequestsOf("HeadObject"); heads != 0 {
		t.Errorf("the restored node sent its bucket %d HEADs serving its packs, want none: "+
			"a pack's row already says what its one GET holds", heads)
	}
	if asked := fakeA.Requests() - oldBucket; asked != 0 {
		t.Errorf("the old bucket was sent %d requests after its node stopped", asked)
	}

	t.Run("ARestoreThatKeepsTheOldStoresRecordIsRefused", func(t *testing.T) {
		kept := restoreInto(t, backupDir, manifest)
		fakeC := s3fake.Start(t, restoreBucket)
		copyObjects(t, fakeC, backupDir, manifest)
		doc := withCode(repo)(fmt.Sprintf(companyDoc, b.model.url))
		cfg, err := config.ParseCompany([]byte(doc))
		if err != nil {
			t.Fatalf("company config: %v", err)
		}
		boot := config.DefaultBootstrap()
		restoreBoot(fakeC.URL, kept)(&boot)
		e, err := engine.New(t.Context(), engine.Options{
			Bootstrap: &boot, Company: cfg, ActivatedAt: harnessActivation,
			Environment: nodeEnvironment(nil), GitHook: codetest.HookCommand(t),
		})
		if err == nil {
			e.Stop(t.Context())
			t.Fatal("a node restored with the old store's record booted against a new bucket — " +
				"the company's files would be split between the two stores")
		}
		// THE AGREEMENT'S OWN REFUSAL, naming the OLD store as the fleet's:
		// the setting fails in other ways that name store.objects too — the
		// record unreadable, the new bucket unreachable — and a control that
		// passed on one of those would pass with the old record never put
		// back.
		var mismatch *engine.ObjectStoreMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("the restored node was refused, but not by the object store agreement: %v", err)
		}
		old, now := s3Objects(fakeA.URL).Identity(), s3Objects(fakeC.URL).Identity()
		if mismatch.Fleet != old || mismatch.Configured != now {
			t.Errorf("the refusal names %q as the fleet's store and %q as this node's, want the "+
				"backed-up node's %q and the new bucket's %q", mismatch.Fleet,
				mismatch.Configured, old, now)
		}
	})
}
