package artifactspike

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"testing"
	"testing/fstest"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PUBLISH PIPELINE'S BYTE LEG, as S3 and S7 drive it: the shipped upload
// service reserving in the coordination twin and streaming into a real object
// store backend through [artifactupload.Service.Transfer], the leg every
// sandbox publish takes (agent sandboxes never get a presigned URL).
//
// The spikes measure the bytes, so the artifacts domain behind the service is
// reduced to what an upload for a NEW artifact asks of it before its publish:
// the company's usage, which is empty because the spike's company holds
// nothing. [createReads] answers that and refuses every other question by
// name, so a spike that started asking one would fail rather than measure a
// path it does not model.

// publishBytes is the publish S3 and S7 stream: the per-publish ceiling
// ([artifacts.MaxPublishBytes]), as four text files at the per-file ceiling.
const (
	publishFiles    = 4
	publishFileSize = artifacts.MaxTextFileBytes
	publishBytes    = publishFiles * publishFileSize
)

// sandboxChunk is how much one read of a sandbox's stream answers: a network
// body's size, not the object store's message size, so the leg's own filling
// is exercised.
const sandboxChunk = 32 << 10

// createReads is the artifacts domain's read side as an upload for a new
// artifact uses it.
type createReads struct{}

var errSpikeReads = errors.New("artifactspike: the byte-leg spikes upload for a new " +
	"artifact and read no existing one")

func (createReads) Artifact(context.Context, artifacts.ID, statelog.Freshness) (artifactsdomain.Head, statelog.Position, error) {
	return artifactsdomain.Head{}, statelog.Position{}, errSpikeReads
}

func (createReads) Version(context.Context, artifacts.ID, int) (artifactsdomain.Version, error) {
	return artifactsdomain.Version{}, errSpikeReads
}

func (createReads) HeldObjects(context.Context, artifacts.ID, []objstore.Hash) (map[objstore.Hash]objstore.Object, error) {
	return nil, errSpikeReads
}

func (createReads) Usage(context.Context) (artifactsdomain.Usage, error) {
	return artifactsdomain.Usage{Complete: true}, nil
}

// noWrites is the write side: the byte leg decides nothing.
type noWrites struct{}

var errSpikeWrites = errors.New("artifactspike: the byte-leg spikes stream bytes and " +
	"decide no version")

func (noWrites) Create(context.Context, artifactsdomain.Call, artifactsdomain.CreateInput) (artifactsdomain.Published, error) {
	return artifactsdomain.Published{}, errSpikeWrites
}

func (noWrites) Publish(context.Context, artifactsdomain.Call, artifactsdomain.PublishInput) (artifactsdomain.Published, error) {
	return artifactsdomain.Published{}, errSpikeWrites
}

func (noWrites) AddAsset(context.Context, artifactsdomain.Call, artifactsdomain.AssetInput) (artifactsdomain.Asset, artifactsdomain.Written, error) {
	return artifactsdomain.Asset{}, artifactsdomain.Written{}, errSpikeWrites
}

// uploadRig is the upload service over one object store backend.
type uploadRig struct {
	svc     *artifactupload.Service
	objects *objstore.Store
}

func newUploadRig(tb testing.TB, backend objstore.Backend) *uploadRig {
	tb.Helper()
	objects, err := objstore.NewStore(backend)
	if err != nil {
		tb.Fatalf("build the object store: %v", err)
	}
	runtimes, err := artifactupload.ReleasesIn(fstest.MapFS{"1.0.0/runtime.js": {Data: []byte("//")}})
	if err != nil {
		tb.Fatalf("build the runtime set: %v", err)
	}
	kits, err := artifactupload.ReleasesIn(fstest.MapFS{"0.20.0/kit.css": {Data: []byte(":root{}")}})
	if err != nil {
		tb.Fatalf("build the kit set: %v", err)
	}
	svc, err := artifactupload.New(artifactupload.Deps{
		Objects: objects, Reservations: coordmemory.NewFleet(),
		Writes: noWrites{}, Reads: createReads{},
		Limits: spikeLimits, Hosts: lint.DefaultAllowlist(), Runtimes: runtimes, Kits: kits,
	})
	if err != nil {
		tb.Fatalf("build the upload service: %v", err)
	}
	return &uploadRig{svc: svc, objects: objects}
}

// openBucket opens the nats object bucket on q at a reservation of maxBytes,
// replicated as the queue's streams are.
func openBucket(tb testing.TB, q *js.Queue, replicas int, maxBytes int64) *natsobj.Backend {
	tb.Helper()
	backend, err := natsobj.Open(tb.Context(), q.JetStream(), natsobj.Config{
		Replicas: replicas, Clustered: replicas > 1, MaxBytes: maxBytes, Explicit: true})
	if err != nil {
		tb.Fatalf("open the nats object bucket: %v", err)
	}
	return backend
}

// sandboxFile is one file of a sandbox's output: size bytes from a seeded
// generator, read sandboxChunk at a time, so the same bytes can be hashed and
// then streamed without ever being held whole.
type sandboxFile struct {
	seed uint64
	size int64
}

func (f sandboxFile) reader() io.Reader {
	var key [32]byte
	key[0], key[1] = byte(f.seed), byte(f.seed>>8)
	return &chunked{src: rand.NewChaCha8(key), left: f.size}
}

func (f sandboxFile) digest(tb testing.TB) objstore.Hash {
	tb.Helper()
	h := sha256.New()
	if _, err := io.Copy(h, f.reader()); err != nil {
		tb.Fatalf("hash a sandbox file: %v", err)
	}
	return objstore.Hash(hex.EncodeToString(h.Sum(nil)))
}

// chunked answers at most sandboxChunk bytes a read, left bytes in all.
type chunked struct {
	src  *rand.ChaCha8
	left int64
}

func (c *chunked) Read(p []byte) (int, error) {
	if c.left == 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), sandboxChunk, c.left))
	if _, err := c.src.Read(p[:n]); err != nil {
		return 0, err
	}
	c.left -= int64(n)
	return n, nil
}

// publisher is the agent seat a sandbox publish streams as.
func publisher() artifactsdomain.Call {
	return artifactsdomain.Call{Authority: artifacts.Authority{
		Actor: iam.Actor{Name: "member-10", Kind: iam.ActorAgent, OperatorID: "member-10"},
		Role:  artifacts.RoleOwner, Limits: spikeLimits(), Origin: artifacts.OriginTool,
	}, TurnID: "turn-spike"}
}

// streamPublish declares a publishBytes upload for a new artifact and streams
// its files one after another, as a sandbox's directory is published, and
// answers how long the bytes took from the intent to the last file stored, or
// the transfer that failed. A declaration that fails is the spike's own fault
// and fails it.
func (u *uploadRig) streamPublish(tb testing.TB, round int) (time.Duration, error) {
	tb.Helper()
	files := make([]sandboxFile, publishFiles)
	entries := make([]artifacts.UploadEntry, publishFiles)
	for i := range files {
		files[i] = sandboxFile{seed: uint64(round*publishFiles + i + 1), size: publishFileSize}
		entries[i] = artifacts.UploadEntry{Path: fmt.Sprintf("data/part-%d.json", i),
			SHA256: files[i].digest(tb), Size: publishFileSize}
	}
	// A FRESH TURN per round: the per-turn byte guardrail
	// ([artifacts.TurnBytesWritten]) is two publishes, and a spike that
	// streams more than two would otherwise measure that refusal.
	call := publisher()
	call.TurnID = fmt.Sprintf("turn-spike-%d", round)
	start := time.Now()
	intent, err := u.svc.Intend(tb.Context(), call, artifacts.ID{},
		artifacts.UploadRequest{Entries: entries})
	if err != nil {
		tb.Fatalf("declare the upload: %v", err)
	}
	for i, f := range files {
		if err := u.svc.Transfer(tb.Context(), call, intent.Upload, entries[i].Path,
			f.reader(), f.size); err != nil {
			return time.Since(start), fmt.Errorf("stream %s: %w", entries[i].Path, err)
		}
	}
	return time.Since(start), nil
}
