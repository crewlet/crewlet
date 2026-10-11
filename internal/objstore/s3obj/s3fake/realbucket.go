package s3fake

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore/s3obj"
)

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
