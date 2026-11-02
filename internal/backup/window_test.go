package backup_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsapi/jsapitest"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
)

// trackerLogStream is the tracker's log, as a backup's declaration names it.
var trackerLogStream = worklet.Domain{}.Stream().Name

// trackerLog creates the tracker's log on nc's broker.
func trackerLog(t *testing.T, nc *nats.Conn) jetstream.Stream {
	t.Helper()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.CreateOrUpdateStream(t.Context(), jetstream.StreamConfig{
		Name: trackerLogStream, Subjects: worklet.Domain{}.Stream().Subjects,
		Storage: jetstream.FileStorage,
	})
	if err != nil {
		t.Fatalf("create the tracker's log: %v", err)
	}
	return stream
}

// suiteRing is the keyring every record this suite puts on a log is signed
// under, and the one each backup it takes is handed: a log holds signed frames
// and nothing else (ADR-0031), so a record a case appends is sealed exactly as
// the framework's own publisher seals one.
var suiteRing = statelog.OneKey("suite", "backup-suite-material")

// seal frames body as the tracker's write authority appends it, signed under
// ring's active key.
func seal(t *testing.T, ring statelog.Keyring, body []byte) []byte {
	t.Helper()
	signer, err := statelog.NewSigner(worklet.Domain{}.Name(), ring)
	if err != nil {
		t.Fatal(err)
	}
	return signer.Seal(body)
}

// appendRecord publishes body onto the tracker's log, sealed under the suite's
// keyring, and answers its sequence.
func appendRecord(t *testing.T, nc *nats.Conn, body []byte) uint64 {
	t.Helper()
	return appendFrame(t, nc, seal(t, suiteRing, body))
}

// appendFrame publishes framed onto the tracker's log as it is — a record a
// case seals itself, under another key or none — and answers its sequence.
func appendFrame(t *testing.T, nc *nats.Conn, framed []byte) uint64 {
	t.Helper()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := js.Publish(t.Context(), "crewlet.tracker.log.test", framed)
	if err != nil {
		t.Fatalf("append to the tracker's log: %v", err)
	}
	return ack.Sequence
}

// appendBarriers appends n of the read index's barriers — records that write
// no row of any table.
func appendBarriers(t *testing.T, nc *nats.Conn, n int) {
	t.Helper()
	for range n {
		payload, err := worklet.EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind})
		if err != nil {
			t.Fatal(err)
		}
		appendRecord(t, nc, payload)
	}
}

// fileRecord is the record a write of project/path leaves on the tracker's
// log: naming o, or — o nil — removing the file.
func fileRecord(t *testing.T, project, path string, o *stored) []byte {
	t.Helper()
	file := worklet.File{V: worklet.DocumentVersion, Project: project, Path: path,
		CreatedAt: clock, UpdatedAt: clock}
	if o != nil {
		file.Hash, file.Size, file.Object = o.object.Hash, o.object.Size, o.object.Key
	} else {
		at := clock
		file.RemovedAt, file.RemovedBy = &at, "dev"
	}
	body, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			Subject: worklet.FileSubject(project, path), Op: worklet.OpPatch,
			CreatedAt: clock, Gen: 1, Writer: "n",
			Scope: worklet.ScopeSet{Subject: true, Container: project},
			OpID:  fmt.Sprintf("op-%s-%d", path, time.Now().UnixNano()),
		},
		Kind: worklet.ChangeFileWritten, Actor: "dev", ActorKind: worklet.AuthorAgent,
		Mutation: body,
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// appendFile appends a write of project/path naming o (nil: a removal) and
// answers its sequence.
func appendFile(t *testing.T, nc *nats.Conn, project, path string, o *stored) uint64 {
	t.Helper()
	return appendRecord(t, nc, fileRecord(t, project, path, o))
}

// windowService is a backup over db and nc whose objects are objs.
func windowService(t *testing.T, db *store.DB, nc *nats.Conn, objs *backup.Objects) *backup.Service {
	t.Helper()
	return windowServiceIn(t, jsapi.Embedded(), db, nc, objs)
}

// windowServiceIn is [windowService] asking its broker in api.
func windowServiceIn(t *testing.T, api jsapi.API, db *store.DB, nc *nats.Conn,
	objs *backup.Objects) *backup.Service {

	t.Helper()
	fleet := memory.NewFleet()
	return build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "n", Holds: fleet, Backups: fleet,
		Conn: nc, API: api, Now: func() time.Time { return clock }, Objects: objs,
	})
}

// noManifest fails the test if dir holds a manifest.
func noManifest(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, backup.ManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused backup wrote its manifest (stat: %v)", err)
	}
}

// A BACKUP CARRIES AN OBJECT NAMED AFTER ITS COPY (ADR-0075). A restore brings
// the store copy back at its position and replays the tracker's log from there
// to the last record the snapshot captured — so a file written while the
// backup ran, uploaded and named after the copy was taken, is a row the
// restored company has. The backup used to carry only what the copy names, and
// a restore into an empty bucket brought that file back with no bytes.
//
// AND IT OWES WHAT THE RESTORED ROWS NAME, NOT WHAT EVERY RECORD EVER NAMED: a
// file replaced again inside the window owes only its last object; one whose
// object the store has lost is lost; and a file the copy names that a record
// after the copy removed — whose object the collector then took — is owed by
// nothing a restore brings back, so it is not lost.
func TestABackupCarriesAnObjectNamedAfterItsCopy(t *testing.T) {
	t.Parallel()
	// fixture is a copy at sequence 3 of the tracker's log naming A as
	// ENG/file-0, and a store holding A and the bodies given.
	fixture := func(t *testing.T, bodies ...string) (*store.DB, *nats.Conn, *objstore.Store, stored, []stored) {
		t.Helper()
		db := openStore(t)
		nc := embeddedNATS(t)
		trackerLog(t, nc)
		appendBarriers(t, nc, 3)
		seedCursor(t, db, trackerLogStream, 1, 3)
		files, _ := filesStore(t)
		held := upload(t, files, append([]string{"the copy's file"}, bodies...)...)
		filesNaming(t, db, held[0])
		return db, nc, files, held[0], held[1:]
	}

	t.Run("named after the copy", func(t *testing.T) {
		t.Parallel()
		db, nc, files, a, rest := fixture(t, "written while the backup ran")
		b := rest[0]
		if seq := appendFile(t, nc, "ENG", "new.md", &b); seq != 4 {
			t.Fatalf("the window's record is at %d, want 4", seq)
		}
		dir := filepath.Join(t.TempDir(), "window")
		manifest, err := windowService(t, db, nc, &backup.Objects{Open: files.Open}).Take(t.Context(), dir)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		o := manifest.Objects
		if o == nil || o.Objects != 2 || o.Replayed != 1 || len(o.Lost) != 0 ||
			o.Bytes != a.object.Size+b.object.Size {
			t.Fatalf("manifest objects = %+v, want the copy's object and the window's, one replayed", o)
		}
		restoresWhole(t, filepath.Join(dir, o.Dir), []stored{a, b})
	})

	t.Run("replaced inside the window", func(t *testing.T) {
		t.Parallel()
		db, nc, files, _, rest := fixture(t, "the first version", "the second version")
		b, c := rest[0], rest[1]
		appendFile(t, nc, "ENG", "new.md", &b)
		appendFile(t, nc, "ENG", "new.md", &c)
		// THE COLLECTOR MAY TAKE B — nothing names it once C replaced it —
		// and a restore names C alone.
		if err := files.Delete(t.Context(), b.object.Key); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "replaced")
		manifest, err := windowService(t, db, nc, &backup.Objects{Open: files.Open}).Take(t.Context(), dir)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		if o := manifest.Objects; o == nil || o.Objects != 2 || o.Replayed != 1 || len(o.Lost) != 0 {
			t.Fatalf("manifest objects = %+v, want the copy's object and C, nothing lost", o)
		}
		restoresWhole(t, filepath.Join(dir, "objects"), []stored{c})
	})

	t.Run("lost inside the window", func(t *testing.T) {
		t.Parallel()
		db, nc, files, _, rest := fixture(t, "written and then lost")
		b := rest[0]
		appendFile(t, nc, "ENG", "new.md", &b)
		if err := files.Delete(t.Context(), b.object.Key); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "lost")
		manifest, err := windowService(t, db, nc, &backup.Objects{Open: files.Open}).Take(t.Context(), dir)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		if o := manifest.Objects; o == nil || len(o.Lost) != 1 ||
			o.Lost[0] != (backup.LostObject{Object: b.object.Key, NamedBy: "ENG/new.md"}) {
			t.Fatalf("manifest objects = %+v, want ENG/new.md lost", o)
		}
	})

	t.Run("removed during the copy is not lost", func(t *testing.T) {
		t.Parallel()
		db, nc, files, a, _ := fixture(t)
		appendFile(t, nc, "ENG", "file-0", nil)
		if err := files.Delete(t.Context(), a.object.Key); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "removed")
		manifest, err := windowService(t, db, nc, &backup.Objects{Open: files.Open}).Take(t.Context(), dir)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		if o := manifest.Objects; o != nil && len(o.Lost) != 0 {
			t.Fatalf("manifest objects = %+v: a file a record after the copy removed was "+
				"listed lost, and no restored row names it", o)
		}
	})

	t.Run("in a stream", func(t *testing.T) {
		t.Parallel()
		db, nc, files, _, rest := fixture(t, "written while the backup ran")
		b := rest[0]
		appendFile(t, nc, "ENG", "new.md", &b)
		dir := filepath.Join(t.TempDir(), "in-stream")
		manifest, err := windowService(t, db, nc, &backup.Objects{Stream: "OBJ_crewlet_files",
			Stat: files.Stat,
			Open: func(context.Context, objstore.Object) (io.ReadCloser, error) {
				t.Error("an object was read although the stream snapshot carries it")
				return nil, errors.New("not expected")
			}}).Take(t.Context(), dir)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		if o := manifest.Objects; o == nil || o.Stream != "OBJ_crewlet_files" || o.Objects != 2 ||
			o.Replayed != 1 || len(o.Lost) != 0 {
			t.Fatalf("manifest objects = %+v, want both asked after and held, one replayed", o)
		}
		if _, err := os.Stat(filepath.Join(dir, "objects")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("an object directory was written beside a stream that carries the objects")
		}
	})

	t.Run("deferred by the copy", func(t *testing.T) {
		t.Parallel()
		// A RECORD THE COPY'S APPLIER DEFERRED is one a restore applies
		// once a build that reads it runs: a readable file record held
		// back behind one it could not read names a file the copy's rows
		// do not.
		db, nc, files, _, rest := fixture(t, "held back by the copy's applier")
		b := rest[0]
		deferRecord(t, db, 2, fileRecord(t, "ENG", "held.md", &b))
		dir := filepath.Join(t.TempDir(), "deferred")
		manifest, err := windowService(t, db, nc, &backup.Objects{Open: files.Open}).Take(t.Context(), dir)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		if o := manifest.Objects; o == nil || o.Objects != 2 || o.Replayed != 1 {
			t.Fatalf("manifest objects = %+v, want the deferred record's object carried", o)
		}
		restoresWhole(t, filepath.Join(dir, "objects"), []stored{b})
	})

	t.Run("named by the copy too", func(t *testing.T) {
		// A RECORD PAST THE COPY RE-NAMING THE COPY'S OWN OBJECT — a
		// put that corrected a file's type — owes nothing the copy's
		// pass did not carry: the object is read once, counted once,
		// and not as replayed. Which window objects the copy names is
		// asked of the copy's rows, on both backends.
		t.Parallel()
		for _, stream := range []bool{false, true} {
			db, nc, files, a, _ := fixture(t)
			appendFile(t, nc, "ENG", "file-0", &a)
			var reads atomic.Int32
			objs := &backup.Objects{Open: func(ctx context.Context, o objstore.Object) (io.ReadCloser, error) {
				reads.Add(1)
				return files.Open(ctx, o)
			}}
			if stream {
				objs = &backup.Objects{Stream: "OBJ_crewlet_files",
					Stat: func(ctx context.Context, k objstore.Key) (objstore.Info, error) {
						reads.Add(1)
						return files.Stat(ctx, k)
					}}
			}
			manifest, err := windowService(t, db, nc, objs).Take(t.Context(),
				filepath.Join(t.TempDir(), fmt.Sprintf("renamed-%v", stream)))
			if err != nil {
				t.Fatalf("stream %v: Take: %v", stream, err)
			}
			if o := manifest.Objects; o == nil || o.Objects != 1 || o.Replayed != 0 || reads.Load() != 1 {
				t.Fatalf("stream %v: manifest objects = %+v after %d reads of the store; want the "+
					"copy's object carried once", stream, o, reads.Load())
			}
		}
	})

	t.Run("deferred past a page", func(t *testing.T) {
		t.Parallel()
		// A STALLED UPGRADE'S BACKLOG IS READ A PAGE AT A TIME — no read
		// answers more than a page — and EVERY PAGE IS FOLDED: a record
		// on the second page names a file as surely as one on the first.
		bodies := make([]string, backup.DeferredPage+1)
		for i := range bodies {
			bodies[i] = fmt.Sprintf("held back by the copy's applier, %d", i)
		}
		db, nc, files, _, rest := fixture(t, bodies...)
		for i, o := range rest {
			deferRecord(t, db, int64(i+1), fileRecord(t, "ENG", fmt.Sprintf("held-%d.md", i), &o))
		}
		svc := windowService(t, db, nc, &backup.Objects{Open: files.Open})
		var pages []int
		backup.SetDeferredPageObserver(svc, func(rows int) { pages = append(pages, rows) })
		dir := filepath.Join(t.TempDir(), "deferred-pages")
		manifest, err := svc.Take(t.Context(), dir)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		// THE TRACKER'S BACKLOG IN A WHOLE PAGE AND THE SHORT ONE, and
		// one empty page for each other declaring log, whose copy
		// deferred nothing.
		var held []int
		empty := 0
		for _, rows := range pages {
			if rows == 0 {
				empty++
				continue
			}
			held = append(held, rows)
		}
		_, logs := othersDeclared()
		if fmt.Sprint(held) != fmt.Sprint([]int{backup.DeferredPage, 1}) || empty != logs {
			t.Fatalf("the deferred records were read in pages of %v, want %d and then 1, "+
				"and one empty page for each of the %d other declaring logs",
				pages, backup.DeferredPage, logs)
		}
		if o := manifest.Objects; o == nil || o.Replayed != len(rest) || o.Objects != len(rest)+1 {
			t.Fatalf("manifest objects = %+v, want all %d deferred records' objects carried",
				o, len(rest))
		}
		restoresWhole(t, filepath.Join(dir, "objects"), rest)
	})
}

// deferRecord stores body as a record the copy's tracker applier deferred at
// seq, sealed under the suite's keyring.
func deferRecord(t *testing.T, db *store.DB, seq int64, body []byte) {
	t.Helper()
	deferFrame(t, db, seq, body, seal(t, suiteRing, body))
}

// deferFrame stores framed — body, as some writer framed it — as a record the
// copy's tracker applier deferred at seq: the bytes the broker stored, which
// is what the framework retains (statelog's deferred-record table).
func deferFrame(t *testing.T, db *store.DB, seq int64, body, framed []byte) {
	t.Helper()
	env, err := worklet.DecodeEnvelope(body)
	if err != nil {
		t.Fatal(err)
	}
	payload := framed
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO tracker_log_deferred
			(position, subject, subject_kind, subject_id, version, payload, stored_at)
			VALUES (?, ?, ?, ?, ?, ?, 0)`,
			int64(1)<<40|seq, env.Subject.String(), string(env.Subject.Kind), env.Subject.ID,
			env.V, payload)
		return err
	}); err != nil {
		t.Fatalf("defer the record: %v", err)
	}
}

// THE OBJECT STREAM IS SNAPSHOTTED AFTER EVERY OTHER STREAM. A stream backup
// counts an object the store holds once the snapshots are taken as one the
// object stream's snapshot carries — which holds for an object a log's record
// names only because the record was in a log snapshotted BEFORE the object
// stream: the bytes are uploaded before the record naming them is written. By
// name the bucket sorts after the logs only by accident, so the order is
// pinned here against a stream named to sort first.
func TestTheObjectStreamIsSnapshottedAfterEveryOtherStream(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	nc := embeddedNATS(t)
	seedStream(t, nc, "A_OBJECTS", "objects.>", 2)
	trackerLog(t, nc)
	appendBarriers(t, nc, 2)
	seedBucket(t, nc, "x", "k", "v")
	manifest, err := windowService(t, db, nc, &backup.Objects{Stream: "A_OBJECTS",
		Stat: func(context.Context, objstore.Key) (objstore.Info, error) {
			return objstore.Info{}, errors.New("not expected")
		}}).Take(t.Context(), filepath.Join(t.TempDir(), "ordered"))
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	var names []string
	for _, s := range manifest.Streams {
		names = append(names, s.Name)
	}
	want := []string{trackerLogStream, "KV_x", "A_OBJECTS"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("snapshotted %v, want %v — every other stream sorted, the object stream last", names, want)
	}
}

// A RECORD THIS BUILD CANNOT READ REFUSES THE BACKUP: a record a newer build
// wrote that may write a file — a file record, or one of a kind this build has
// never heard of, whose applier it cannot answer for — may name an object the
// restore needs, and nobody here can say which. Take the backup from a node
// running the build that wrote it. In the log past the copy and among the
// records the copy deferred alike, since a restore applies both.
func TestABackupRefusesALogRecordItCannotRead(t *testing.T) {
	t.Parallel()
	for name, rewrite := range map[string]func(map[string]any){
		"a file record a newer build wrote": func(map[string]any) {},
		"a kind this build does not know": func(doc map[string]any) {
			doc["subject"].(map[string]any)["kind"] = "filecopy"
		},
	} {
		for _, deferred := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, deferred %v", name, deferred), func(t *testing.T) {
				t.Parallel()
				db := openStore(t)
				nc := embeddedNATS(t)
				trackerLog(t, nc)
				appendBarriers(t, nc, 3)
				seedCursor(t, db, trackerLogStream, 1, 3)
				files, _ := filesStore(t)
				held := upload(t, files, "written by a newer build")
				var doc map[string]any
				if err := json.Unmarshal(fileRecord(t, "ENG", "new.md", &held[0]), &doc); err != nil {
					t.Fatal(err)
				}
				doc["v"] = worklet.RecordVersion + 1
				rewrite(doc)
				newer, err := json.Marshal(doc)
				if err != nil {
					t.Fatal(err)
				}
				if deferred {
					deferRecord(t, db, 2, newer)
				} else {
					appendRecord(t, nc, newer)
				}
				dir := filepath.Join(t.TempDir(), "unreadable")
				_, err = windowService(t, db, nc, &backup.Objects{Open: files.Open}).Take(t.Context(), dir)
				if !errors.Is(err, objstore.ErrRecordUnreadable) {
					t.Fatalf("Take = %v, want ErrRecordUnreadable", err)
				}
				noManifest(t, dir)
			})
		}
	}
}

// A RECORD THIS NODE CANNOT VERIFY REFUSES THE BACKUP. Every record on a log
// is a signed frame (ADR-0031), and what a declaration reads for the files a
// record names is the body inside it — so the backup opens each frame as the
// applier a restore replays it through does, and decides from a body only
// once its signature holds. One signed under a key this node does not hold is
// refused naming that verdict, and so is one no key of the fleet opens: read
// on trust, either could name a file the restore never does, or hide one it
// does. In the log past the copy and among the records the copy deferred
// alike, since a restore applies both.
func TestABackupRefusesARecordItCannotVerify(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		frame func(t *testing.T, body []byte) []byte
		want  error
	}{
		"signed under a key this node does not hold": {
			frame: func(t *testing.T, body []byte) []byte {
				return seal(t, statelog.OneKey("rotated-in", "material this node lacks"), body)
			},
			want: statelog.ErrRecordUnverified,
		},
		"signed by nothing": {
			frame: func(_ *testing.T, body []byte) []byte { return body },
			want:  statelog.ErrRecordTampered,
		},
		"changed after it was signed": {
			frame: func(t *testing.T, body []byte) []byte {
				framed := seal(t, suiteRing, body)
				// A BYTE OF THE PATH, so the body still decodes and
				// only the signature can tell.
				at := bytes.LastIndex(framed, []byte("new.md"))
				framed[at] = 'x'
				return framed
			},
			want: statelog.ErrRecordTampered,
		},
	} {
		for _, deferred := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, deferred %v", name, deferred), func(t *testing.T) {
				t.Parallel()
				db := openStore(t)
				nc := embeddedNATS(t)
				trackerLog(t, nc)
				appendBarriers(t, nc, 3)
				seedCursor(t, db, trackerLogStream, 1, 3)
				files, _ := filesStore(t)
				held := upload(t, files, "named by a record this node cannot verify")
				body := fileRecord(t, "ENG", "new.md", &held[0])
				if deferred {
					deferFrame(t, db, 2, body, tc.frame(t, body))
				} else {
					appendFrame(t, nc, tc.frame(t, body))
				}
				dir := filepath.Join(t.TempDir(), "unverified")
				_, err := windowService(t, db, nc, &backup.Objects{Open: files.Open}).Take(t.Context(), dir)
				if !errors.Is(err, tc.want) {
					t.Fatalf("Take = %v, want %v", err, tc.want)
				}
				noManifest(t, dir)
			})
		}
	}
}

// A WINDOW WITH A HOLE REFUSES THE BACKUP: a record the log no longer holds
// between the copy and the snapshot is one whose file nobody can say was
// carried, and a reader that stepped over it would leave the file uncounted.
func TestABackupRefusesAWindowWithAHole(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	nc := embeddedNATS(t)
	stream := trackerLog(t, nc)
	appendBarriers(t, nc, 8)
	seedCursor(t, db, trackerLogStream, 1, 3)
	if err := stream.DeleteMsg(t.Context(), 5); err != nil {
		t.Fatal(err)
	}
	files, _ := filesStore(t)
	dir := filepath.Join(t.TempDir(), "hole")
	_, err := windowService(t, db, nc, &backup.Objects{Open: files.Open}).Take(t.Context(), dir)
	if !errors.Is(err, backup.ErrLogGap) || !strings.Contains(err.Error(), " 5 ") {
		t.Fatalf("Take = %v; want ErrLogGap naming record 5", err)
	}
	noManifest(t, dir)
}

// A LOG LOOKUP A MEMBER DROPS IS ASKED AGAIN. Reading past the copy begins by
// reading the log's info — its identity, held against the copy's — which only
// the stream's leader answers, and a member that still believes in a leader
// its group has just lost drops the request without a word. Sent once on a
// backup with no deadline, it waited out the client's five seconds and refused
// the backup; asked again, the backup carries the file written after the copy.
func TestALogLookupAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ns, nc := natsBroker(t, "")
	trackerLog(t, nc)
	appendBarriers(t, nc, 3)
	seedCursor(t, db, trackerLogStream, 1, 3)
	files, _ := filesStore(t)
	held := upload(t, files, "written after the copy")
	appendFile(t, nc, "ENG", "after.md", &held[0])
	dropped := jsapitest.DropFirst(t, ns, jsapitest.StreamInfo(trackerLogStream), 1)
	dir := filepath.Join(t.TempDir(), "lookup")
	manifest, err := windowServiceIn(t, jsapi.Account(), db, nc,
		&backup.Objects{Open: files.Open}).Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("Take behind one dropped read of the log: %v", err)
	}
	if o := manifest.Objects; o == nil || o.Replayed != 1 || len(o.Lost) != 0 {
		t.Fatalf("manifest objects = %+v, want the file written after the copy carried", o)
	}
	if got := dropped.Count(); got != 1 {
		t.Errorf("the broker dropped %d reads of the log, want the 1 it was told to", got)
	}
	restoresWhole(t, filepath.Join(dir, "objects"), held)
}

// A LOG LOOKUP NOBODY ANSWERS, ON A BACKUP WITH NO DEADLINE, ENDS AT THE
// CLIENT'S OWN DEFAULT — asked again inside it, at none, two and four seconds,
// and never stretched to the lookup ceiling: it was one request the client
// held for its five seconds, and the backup's own cost was sized against that.
func TestALogLookupNobodyAnswersEndsAtTheClientsDefault(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ns, nc := natsBroker(t, "")
	trackerLog(t, nc)
	appendBarriers(t, nc, 3)
	seedCursor(t, db, trackerLogStream, 1, 3)
	files, _ := filesStore(t)
	held := upload(t, files, "written after the copy")
	appendFile(t, nc, "ENG", "after.md", &held[0])
	lookups := jsapitest.Hold(t, ns, jsapitest.StreamInfo(trackerLogStream))
	js, err := jsapi.Account().Client(nc)
	if err != nil {
		t.Fatal(err)
	}
	bound := js.Options().DefaultTimeout
	service := windowServiceIn(t, jsapi.Account(), db, nc, &backup.Objects{Open: files.Open})
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := service.Take(context.Background(), filepath.Join(t.TempDir(), "unanswered"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a backup whose log lookup nobody answered was taken")
		}
		if took := time.Since(start); took < bound {
			t.Errorf("the lookup gave up after %v, inside its client's %v", took, bound)
		}
	case <-time.After(jsprovision.LookupBudget / 2):
		t.Fatalf("a log lookup nobody answers was still being asked %v later, past its "+
			"client's %v default (%d requests)", jsprovision.LookupBudget/2, bound, lookups.Count())
	}
	// AT NONE, TWO AND FOUR SECONDS: the third a second inside the bound,
	// which a timer the load delays may miss, so it is the two the bound must
	// hold that are asked for.
	if n := lookups.Count(); n < 2 || n > 3 {
		t.Errorf("the lookup was asked %d times inside its client's %v, want it asked "+
			"again inside it — at none, two and four seconds", n, bound)
	}
}

// A RECORD AT THE TRANSPORT'S CEILING IS READ PAST. The broker admits a record
// whose payload reaches queue.MaxPayloadBytes, and counts the subject and the
// acknowledgement subject against a pull's byte bound besides — so a window
// reader whose pull was bounded by the ceiling alone was refused that record
// on every pull, and the backup waited out its bound and was refused, with the
// file written after it never read. The record here is a barrier padded to the
// ceiling, which names nothing; the file after it is what must be carried.
func TestABackupReadsPastARecordAtTheTransportCeiling(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	nc := embeddedNATS(t)
	trackerLog(t, nc)
	appendBarriers(t, nc, 3)
	seedCursor(t, db, trackerLogStream, 1, 3)
	files, _ := filesStore(t)
	held := upload(t, files, "written after the largest record a log holds")
	// THE FRAME IS WHAT REACHES THE CEILING, signature and all, since the
	// frame is what the broker stores and counts.
	frame := len(seal(t, suiteRing, nil))
	head := `{"v":1,"subject":{"kind":"barrier"},"scope":"s","pad":"`
	largest := seal(t, suiteRing,
		[]byte(head+strings.Repeat("x", queue.MaxPayloadBytes-frame-len(head)-2)+`"}`))
	if len(largest) != queue.MaxPayloadBytes {
		t.Fatalf("the padded record is %d bytes, want the ceiling %d", len(largest), queue.MaxPayloadBytes)
	}
	appendFrame(t, nc, largest)
	appendFile(t, nc, "ENG", "after.md", &held[0])
	dir := filepath.Join(t.TempDir(), "ceiling")
	manifest, err := windowService(t, db, nc, &backup.Objects{Open: files.Open}).Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if o := manifest.Objects; o == nil || o.Replayed != 1 || len(o.Lost) != 0 {
		t.Fatalf("manifest objects = %+v, want the file written after the largest record carried", o)
	}
	restoresWhole(t, filepath.Join(dir, "objects"), held)
}

// A COPY KEYED TO ANOTHER INSTANCE OF THE LOG REFUSES THE BACKUP: its position
// is a sequence on a stream that no longer exists, and read against the live
// one it names other records. The instant the copy recorded is the one the
// live stream was created at, or the window is somebody else's — and exactly
// that instant is accepted. For a log restored from a snapshot that instant is
// the one its creator stamped, which the restore kept, and never the broker's,
// which is the restore's own.
func TestABackupRefusesACopyKeyedToAnotherInstanceOfTheLog(t *testing.T) {
	t.Parallel()
	// stampedAt is when the restored log's original was created: a day
	// before the broker made the restored one.
	stampedAt := time.Now().Add(-24 * time.Hour).UTC()
	for name, tc := range map[string]struct {
		restored bool
		keyed    func(broker time.Time) time.Time
		refused  bool
	}{
		"another instance": {keyed: func(broker time.Time) time.Time { return broker.Add(-time.Hour) }, refused: true},
		"this instance":    {keyed: func(broker time.Time) time.Time { return broker }},
		"this instance, restored": {restored: true,
			keyed: func(time.Time) time.Time { return stampedAt }},
		"the restore's own instant": {restored: true,
			keyed: func(broker time.Time) time.Time { return broker }, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			db := openStore(t)
			nc := embeddedNATS(t)
			stream := trackerLog(t, nc)
			if tc.restored {
				stream = restoredTrackerLog(t, nc, stampedAt)
			}
			appendBarriers(t, nc, 5)
			seedCursorCreated(t, db, trackerLogStream, 1, 3, tc.keyed(stream.CachedInfo().Created))
			files, _ := filesStore(t)
			dir := filepath.Join(t.TempDir(), "instance")
			_, err := windowService(t, db, nc, &backup.Objects{Open: files.Open}).Take(t.Context(), dir)
			switch {
			case tc.refused && !errors.Is(err, statelog.ErrStreamRecreated):
				t.Fatalf("Take = %v, want ErrStreamRecreated", err)
			case tc.refused:
				noManifest(t, dir)
			case err != nil:
				t.Fatalf("a copy keyed to the live stream was refused: %v", err)
			}
		})
	}
}

// restoredTrackerLog makes the tracker's log on nc's broker again as a snapshot
// restore makes it: from a configuration carrying the instant its original was
// stamped with, while the broker dates the stream itself to now.
func restoredTrackerLog(t *testing.T, nc *nats.Conn, original time.Time) jetstream.Stream {
	t.Helper()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.DeleteStream(t.Context(), trackerLogStream); err != nil {
		t.Fatalf("delete the tracker's log: %v", err)
	}
	stream, err := js.CreateStream(t.Context(), jetstream.StreamConfig{
		Name: trackerLogStream, Subjects: worklet.Domain{}.Stream().Subjects,
		Storage:  jetstream.FileStorage,
		Metadata: statelog.StampCreatedAt(nil, original),
	})
	if err != nil {
		t.Fatalf("restore the tracker's log: %v", err)
	}
	return stream
}

// A KEY THE COPY NAMES AS ONE OBJECT AND A RECORD PAST IT AS ANOTHER REFUSES
// THE BACKUP, as two rows of the copy disagreeing does: a key is one upload's
// bytes, and the backup cannot verify the object against both.
func TestABackupRefusesAKeyTheLogNamesAsAnotherObject(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	nc := embeddedNATS(t)
	trackerLog(t, nc)
	appendBarriers(t, nc, 3)
	seedCursor(t, db, trackerLogStream, 1, 3)
	files, _ := filesStore(t)
	held := upload(t, files, "the copy's account")
	filesNaming(t, db, held...)
	other := held[0]
	other.object.Hash = objstore.HashOf([]byte("the record's account"))
	appendFile(t, nc, "ENG", "elsewhere.md", &other)
	dir := filepath.Join(t.TempDir(), "contradicted")
	_, err := windowService(t, db, nc, &backup.Objects{Open: files.Open}).Take(t.Context(), dir)
	if err == nil || !strings.Contains(err.Error(), "ENG/elsewhere.md") {
		t.Fatalf("Take = %v; want it refused naming the record's row", err)
	}
	noManifest(t, dir)
}

// A STREAM LISTING A MEMBER DROPS IS ASKED AGAIN. The backup snapshots every
// stream the broker holds, and only the metadata leader names them: a member
// that still believes in a leader the group has just lost drops the page
// without a word, and the client's lister held it for five seconds and
// refused the backup. Asked again, the backup carries every stream.
func TestAStreamListingAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ns, nc := natsBroker(t, "")
	trackerLog(t, nc)
	appendBarriers(t, nc, 3)
	seedCursor(t, db, trackerLogStream, 1, 3)
	files, _ := filesStore(t)
	dropped := jsapitest.DropFirst(t, ns, jsapitest.StreamNames(), 1)
	manifest, err := windowServiceIn(t, jsapi.Account(), db, nc,
		&backup.Objects{Open: files.Open}).Take(t.Context(), filepath.Join(t.TempDir(), "names"))
	if err != nil {
		t.Fatalf("Take behind one dropped page of the stream names: %v", err)
	}
	var carried []string
	for _, s := range manifest.Streams {
		carried = append(carried, s.Name)
	}
	if !slices.Contains(carried, trackerLogStream) {
		t.Errorf("the backup carried %v, want %s among them", carried, trackerLogStream)
	}
	if got := dropped.Count(); got != 1 {
		t.Errorf("the broker dropped %d pages of the stream names, want the 1 it was told to", got)
	}
}
