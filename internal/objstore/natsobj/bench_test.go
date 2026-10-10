package natsobj_test

import (
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/objstore/objstoretest"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// BenchmarkBackend measures the broker's object store through the suite every
// backend runs, two ways.
//
// AT `stream.sync: always`, production's default, rather than at the
// [js.Config] zero value the contract's cases start their member with — a
// member that never fsyncs is one no operator runs, and on a put it is the
// fsync per message the figure is made of.
//
// ON A MEMBER'S OWN CLIENT (`via=member`), as a data node reaches the bucket,
// and THROUGH A LEAF (`via=leaf`), as a node without `data` does: its broker
// runs no JetStream and every piece crosses the leaf link.
//
// ONE MEMBER, never a cluster: standing a cluster up here would make this
// package one of the solo half's (`go doc ./internal/solo`), and its whole
// suite with it. The bucket at three replicas is measured by
// internal/objstore/natsobj/replicated, a package of its own for exactly that
// reason.
func BenchmarkBackend(b *testing.B) {
	member := js.Config{SyncAlways: true}
	for _, via := range []struct {
		name   string
		client func(testing.TB, js.Config) jetstream.JetStream
	}{
		{"member", memberClientAt},
		{"leaf", leafClientAt},
	} {
		b.Run("via="+via.name, func(b *testing.B) {
			objstoretest.Bench(b, func(b *testing.B) objstoretest.BenchBackend {
				return objstoretest.BenchBackend{Backend: open(b, via.client(b, member))}
			})
		})
	}
}
