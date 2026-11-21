package natsobj_test

import (
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	"github.com/crewlet/crewlet/internal/objstore/objstoretest"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// BenchmarkBackend measures the broker's object store through the suite every
// backend runs, two ways.
//
// AT `stream.sync: always`, production's default, not the [js.Config] zero
// value the contract's cases use: a put's figure is the fsync per message.
// ON A MEMBER'S OWN CLIENT (`via=member`), as a data node reaches the bucket,
// and THROUGH A LEAF (`via=leaf`), as a node without `data` does: pieces
// share the one window a link is given (jsapi.LinkWindow).
// ONE MEMBER, never a cluster, which would make this package one of the solo
// half's (`go doc ./internal/solo`); natsobj/replicated measures three replicas.
func BenchmarkBackend(b *testing.B) {
	member := js.Config{SyncAlways: true}
	for _, via := range []struct {
		name   string
		client func(testing.TB, js.Config) jetstream.JetStream
		cfg    natsobj.Config
	}{
		{"member", memberClientAt, natsobj.Config{Replicas: 1}},
		{"leaf", leafClientAt, acrossLink()},
	} {
		cfg := via.cfg
		cfg.MaxBytes = benchCeiling
		b.Run("via="+via.name, func(b *testing.B) {
			objstoretest.Bench(b, func(b *testing.B) objstoretest.BenchBackend {
				return objstoretest.BenchBackend{Backend: openWith(b, via.client(b, member), cfg)}
			})
		})
	}
}

// benchCeiling is the reservation every benchmark bucket is opened with: four
// gibibytes, since the suite holds the stat case's ten thousand objects of up
// to 256 KiB at once (some 840 MiB with their metadata) and a 128 MiB object
// beside the ranged case, where the contract's cases' [testCeiling] holds a
// few pieces. A put past it is refused as the bucket filling, which a figure
// must never be made of.
const benchCeiling = 4 << 30
