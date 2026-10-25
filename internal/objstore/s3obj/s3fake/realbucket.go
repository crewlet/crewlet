package s3fake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/objstoretest"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
)

// NoRealBucket is what a benchmark's real-bucket arm says when it skips
// because CREWLET_BENCH_S3_BUCKET names no bucket — one sentence, so every arm
// names the same remedy.
const NoRealBucket = "CREWLET_BENCH_S3_BUCKET names no bucket, so no real bucket is measured; " +
	"internal/objstore/s3obj's package doc lists the variables that name one"

// realClear bounds clearing a real bucket's prefix once its benchmark ends:
// the largest holds a hundred thousand objects, deleted objstoretest.Clear's
// workers at a time — about half a minute at a 20 ms round trip — so ten
// minutes is a hang detector rather than a pace.
const realClear = 10 * time.Minute

// RealBackend is the bucket [RealBucket] names, opened through the
// production backend under a PREFIX OF ITS OWN — a key minted now, below the
// variables' prefix — and cleared when b ends; it skips b with
// [NoRealBucket] when no bucket is named.
//
// A prefix a call mints rather than the variables' own, because a real bucket
// outlives the run: a benchmark starts from an empty store whatever an earlier
// one — or a run killed part way — left, its listing counts its own objects
// alone, and what a killed run leaves sits under one prefix a person or a
// lifecycle rule can clear. One opener, because every benchmark that measures
// a real bucket needs exactly this, and a copy is one that forgets the clear.
func RealBackend(b *testing.B) objstore.Backend {
	b.Helper()
	cfg, ok := RealBucket(b)
	if !ok {
		b.Skip(NoRealBucket)
	}
	cfg.Prefix += objstore.KeyAt(time.Now()).String() + "/"
	backend, err := s3obj.Open(b.Context(), cfg)
	if err != nil {
		b.Fatalf("open bucket %s: %v", cfg.Bucket, err)
	}
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(b.Context()), realClear)
		defer cancel()
		if err := objstoretest.Clear(ctx, backend); err != nil {
			b.Errorf("clear %s in bucket %s: %v", cfg.Prefix, cfg.Bucket, err)
		}
	})
	return backend
}

// benchPrefix is what every key a real bucket's benchmark writes is put
// under when CREWLET_BENCH_S3_PREFIX names nothing: a prefix of the
// benchmarks' own rather than the bucket's root, so what a run killed part way
// leaves — objects, an unfinished upload — sits under one prefix a person or
// a lifecycle rule can clear, never among the bucket's other keys.
const benchPrefix = "crewlet-bench/"

// RealBucket is the bucket the CREWLET_BENCH_S3_* variables name — for the
// benchmarks that measure a real bucket beside this fake — or false when
// CREWLET_BENCH_S3_BUCKET names none. A bucket named without what a request
// to it needs, or a variable holding what it cannot mean, fails b naming the
// variable to change: a region left out, a path style that is neither true
// nor false. internal/objstore/s3obj's package doc lists the variables.
//
// BENCHMARKS ONLY, which is why it takes a [*testing.B] rather than a
// [testing.TB]: a test that reached a real bucket would pass or fail on
// somebody's network. And it is the ONE place the variables are read, so the
// object store's own benchmark and every later one that measures a real
// bucket agree on what they mean rather than each parsing them in a copy of
// its own (s3fake's tests hold that no other package spells their names, or
// the stem one is built from).
//
// NO CREDENTIAL IS A CREWLET_ VARIABLE: the access key and secret are left
// empty, so [s3obj.Open] takes the SDK's own chain — AWS_ACCESS_KEY_ID and
// AWS_SECRET_ACCESS_KEY, AWS_PROFILE, a web identity, the instance's role —
// exactly as a node holding none does.
func RealBucket(b *testing.B) (s3obj.Config, bool) {
	b.Helper()
	cfg, ok, err := parseRealBucket(os.Getenv)
	if err != nil {
		b.Fatal(err)
	}
	return cfg, ok
}

// parseRealBucket is [RealBucket] over getenv, so the rules are tested on a
// set of variables the case writes rather than on the process's own.
func parseRealBucket(getenv func(string) string) (s3obj.Config, bool, error) {
	cfg := s3obj.Config{
		Bucket:   getenv("CREWLET_BENCH_S3_BUCKET"),
		Region:   getenv("CREWLET_BENCH_S3_REGION"),
		Endpoint: getenv("CREWLET_BENCH_S3_ENDPOINT"),
		Prefix:   getenv("CREWLET_BENCH_S3_PREFIX"),
	}
	if cfg.Bucket == "" {
		return s3obj.Config{}, false, nil
	}
	if cfg.Region == "" {
		return s3obj.Config{}, false, errors.New("CREWLET_BENCH_S3_BUCKET names a bucket and " +
			"CREWLET_BENCH_S3_REGION no region; the signature needs one even where the " +
			"provider ignores it (R2 takes auto, MinIO us-east-1)")
	}
	if cfg.Prefix == "" {
		cfg.Prefix = benchPrefix
	}
	if v := getenv("CREWLET_BENCH_S3_PATH_STYLE"); v != "" {
		style, err := strconv.ParseBool(v)
		if err != nil {
			return s3obj.Config{}, false, fmt.Errorf("CREWLET_BENCH_S3_PATH_STYLE=%q is not true or false: %w", v, err)
		}
		cfg.PathStyle = style
	}
	return cfg, true, nil
}
