package statelog_test

// streamInstantFixture is a tree whose verdict is known, for
// TestTheStreamInstantWalkJudgesAKnownTree: paths relative to the tree's root,
// read as this module's own packages, against the real NATS packages.
var streamInstantFixture = map[string]string{
	// THE ONE READER, recognised and allowed.
	"internal/statelog/identity.go": `package statelog

import (
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func StreamCreatedAt(info *jetstream.StreamInfo) time.Time {
	if _, stamped := info.Config.Metadata["crewlet.created_at"]; stamped {
		return time.Time{}
	}
	return info.Created
}
`,

	// EVERY SHAPE A READER TAKES, the three this rule was written after first,
	// each reverted to the field.
	"internal/readers/readers.go": `package readers

import (
	"context"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/helper"
)

// domainLog is a state log's Stats, reverted.
type domainLog struct {
	state jetstream.Stream
}

func (l *domainLog) stats(ctx context.Context) time.Time {
	info, err := l.state.Info(ctx)
	if err != nil {
		return time.Time{}
	}
	return info.Created
}

// syncer is the memory changelog's hydration, reverted.
type syncer struct{ js jetstream.JetStream }

func (s *syncer) checkIdentity(ctx context.Context) time.Time {
	info, _ := s.js.Stream(ctx, "CREWLET_MEMORY")
	return info.CachedInfo().Created
}

// readLog is the backup's read past its copy, reverted.
func readLog(ctx context.Context, js jetstream.JetStream, name string) time.Time {
	stream, err := js.Stream(ctx, name)
	if err != nil {
		return time.Time{}
	}
	return stream.CachedInfo().Created
}

func listed(ctx context.Context, js jetstream.JetStream) {
	for info := range js.ListStreams(ctx).Info() {
		_ = info.Created
	}
}

func legacy(js nats.JetStreamContext) time.Time {
	info, _ := js.StreamInfo("CREWLET_TRACKER_LOG")
	return info.Created
}

func decoded(resp server.JSApiStreamInfoResponse) time.Time { return resp.Created }

func detail(d *server.StreamDetail) time.Time { return d.Created }

func bucket(st jetstream.KeyValueStatus) time.Time {
	return st.(*jetstream.KeyValueBucketStatus).StreamInfo().Created
}

type wrapped struct{ *jetstream.StreamInfo }

func promoted(w wrapped) time.Time { return w.Created }

type defined jetstream.StreamInfo

func redefined(d *defined) time.Time { return d.Created }

func otherFile() time.Time { return produced().Created }

func otherPackage() time.Time { return helper.Info().Created }

var converted = func() time.Time { return (*jetstream.StreamInfo)(nil).Created }

func indexed(infos []*jetstream.StreamInfo, byName map[string]*jetstream.StreamInfo) (time.Time, time.Time) {
	held, ok := byName["CREWLET_PAGES_LOG"]
	if !ok {
		return time.Time{}, time.Time{}
	}
	return infos[0].Created, held.Created
}

func anonymous(x struct{ info *jetstream.StreamInfo }) time.Time { return x.info.Created }

func methodValue(s jetstream.Stream) time.Time {
	read := s.CachedInfo
	return read().Created
}

func first[T any](vs []T) T { return vs[0] }

func generic(infos []*jetstream.StreamInfo) time.Time { return first(infos).Created }

// op, define and call are the shape the estate router's seams take: a generic
// type, and two generic functions whose type arguments are inferred.
type op[A, R any] struct{ serve func(A) R }

func define[A, R any](serve func(A) R) op[A, R] { return op[A, R]{serve} }

func call[A, R any](o op[A, R], a A) R { return o.serve(a) }

var opInfo = define(func(name string) *jetstream.StreamInfo { return nil })

func inferred() time.Time { return call(opInfo, "CREWLET_TRACKER_LOG").Created }

type box[T any] struct{ v T }

func (b box[T]) get() T { return b.v }

func boxed(b box[*jetstream.StreamInfo]) (time.Time, time.Time) {
	return b.get().Created, b.v.Created
}
`,

	// A producer in another file of the package, which holds no Created of its
	// own and so is read only for its declarations.
	"internal/readers/other.go": `package readers

import "github.com/nats-io/nats.go/jetstream"

func produced() *jetstream.StreamInfo { return nil }
`,

	// A producer in another package of the module.
	"internal/helper/helper.go": `package helper

import "github.com/nats-io/nats.go/jetstream"

func Info() *jetstream.StreamInfo { return nil }
`,

	// WHAT IS NEVER A STREAM'S INSTANT: a consumer's, found through a method of
	// the package as the queue's own rebuild finds it; a field of this module's
	// own, shallower than a promoted one; a call; and a method value.
	"internal/readers/fine.go": `package readers

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

type consumerHandle struct{ held jetstream.Consumer }

func (c *consumerHandle) lookup(ctx context.Context) (jetstream.Consumer, error) {
	return c.held, nil
}

func (c *consumerHandle) rebuilt(ctx context.Context, judged *jetstream.ConsumerInfo) bool {
	held, err := c.lookup(ctx)
	if err != nil {
		return false
	}
	info := held.CachedInfo()
	fresh, _ := held.Info(ctx)
	return !info.Created.Equal(judged.Created) && fresh.Created.IsZero()
}

type shadowed struct {
	*jetstream.StreamInfo
	Created time.Time
}

func ownField(s shadowed) time.Time { return s.Created }

func entry(e jetstream.KeyValueEntry) time.Time { return e.Created() }

func entryValue(e jetstream.KeyValueEntry) func() time.Time { return e.Created }

type result struct{ Created []string }

func unrelated(r result) []string { return r.Created }

type ensured struct{ Created []string }

var opEnsure = define(func(name string) ensured { return ensured{} })

func ensuredTags() []string { return call(opEnsure, "CREWLET_TRACKER_LOG").Created }
`,

	// WHAT THE WALK CANNOT TYPE, reported as such: a type switch's variable,
	// and a value a function of a package it does not read returns.
	"internal/readers/untyped.go": `package readers

import (
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func switched(v any) time.Time {
	switch x := v.(type) {
	case *jetstream.StreamInfo:
		return x.Created
	}
	return time.Time{}
}

var once = sync.OnceValue(func() *jetstream.StreamInfo { return nil })

func viaStd() time.Time { return once().Created }
`,

	// cmd/ is walked too.
	"cmd/tool/main.go": `package main

import "github.com/nats-io/nats.go/jetstream"

func main() {
	var info *jetstream.StreamInfo
	_ = info.Created
}
`,

	// NEVER READ: a test file and testdata.
	"internal/readers/readers_test.go": `package readers

import "github.com/nats-io/nats.go/jetstream"

func inATest(info *jetstream.StreamInfo) { _ = info.Created }
`,
	"internal/readers/testdata/copy.go": `package readers

import "github.com/nats-io/nats.go/jetstream"

func inTestdata(info *jetstream.StreamInfo) { _ = info.Created }
`,
}

// streamInstantFixtureHits is every read of a stream's instant the fixture
// holds outside the one reader, as "package.declaration: owner".
var streamInstantFixtureHits = []string{
	"internal/readers.domainLog.stats: jetstream.StreamInfo",
	"internal/readers.syncer.checkIdentity: jetstream.StreamInfo",
	"internal/readers.readLog: jetstream.StreamInfo",
	"internal/readers.listed: jetstream.StreamInfo",
	"internal/readers.legacy: nats.StreamInfo",
	"internal/readers.decoded: server.StreamInfo",
	"internal/readers.detail: server.StreamDetail",
	"internal/readers.bucket: jetstream.StreamInfo",
	"internal/readers.promoted: jetstream.StreamInfo",
	"internal/readers.redefined: jetstream.StreamInfo",
	"internal/readers.otherFile: jetstream.StreamInfo",
	"internal/readers.otherPackage: jetstream.StreamInfo",
	"internal/readers.converted: jetstream.StreamInfo",
	"internal/readers.indexed: jetstream.StreamInfo",
	"internal/readers.indexed: jetstream.StreamInfo",
	"internal/readers.anonymous: jetstream.StreamInfo",
	"internal/readers.methodValue: jetstream.StreamInfo",
	"internal/readers.generic: jetstream.StreamInfo",
	"internal/readers.inferred: jetstream.StreamInfo",
	"internal/readers.boxed: jetstream.StreamInfo",
	"internal/readers.boxed: jetstream.StreamInfo",
	"cmd/tool.main: jetstream.StreamInfo",
}

// streamInstantFixtureUntyped is every Created selector the walk cannot type,
// as "package.declaration: expression".
var streamInstantFixtureUntyped = []string{
	"internal/readers.switched: x",
	"internal/readers.viaStd: once(…)",
}
