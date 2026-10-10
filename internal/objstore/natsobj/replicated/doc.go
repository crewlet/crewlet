// Package replicated measures the broker's object store
// (internal/objstore/natsobj) where every clustered fleet runs it: a bucket
// held at three replicas on a three-member cluster, each member at
// `stream.sync: always`.
//
// It has no exported surface and no code of its own; its tests are the whole
// of it.
//
// # Why a package of its own
//
// A put to a replicated bucket is paid for by a QUORUM: every 128 KiB message
// is proposed to the bucket's raft group, written and fsynced on each member
// holding a copy, and acknowledged once a majority has it — a cost one
// member's figures (natsobj's own BenchmarkBackend) cannot show. Standing up
// three members forms a quorum, and a package that does runs in the solo half
// (`go doc ./internal/solo`); this package is that arm alone, so natsobj's own
// suite keeps sharing a runner.
package replicated
