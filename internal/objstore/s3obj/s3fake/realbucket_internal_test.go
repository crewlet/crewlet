package s3fake

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore/s3obj"
)

// WHAT THE BENCH VARIABLES MEAN, held where they are read.
//
// Every benchmark that measures a real bucket reads its configuration through
// [RealBucket], so these rules are what each of them gets: the BUCKET alone
// turns a real arm on; a bucket without a region is refused naming
// CREWLET_BENCH_S3_REGION, the variable to set, rather than left to
// [s3obj.Open]'s own refusal, which names no variable; a prefix left out is
// `crewlet-bench/`, never the bucket's root; a path style is a boolean or a
// refusal, never quietly false; and no credential is ever taken from a
// CREWLET_ variable — the SDK's own chain reads the AWS ones, exactly as on a
// node holding none.
func TestTheBenchVariablesNameARealBucket(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		env  map[string]string
		want s3obj.Config
		ok   bool
		// refused, when set, is the variable the refusal must name.
		refused string
	}{
		{
			name: "nothing set measures no real bucket",
			env:  map[string]string{},
		},
		{
			name: "a region without a bucket measures no real bucket",
			env: map[string]string{
				"CREWLET_BENCH_S3_REGION":   "eu-west-1",
				"CREWLET_BENCH_S3_ENDPOINT": "https://s3.example.com",
			},
		},
		{
			name: "a bucket and its region, the prefix the benchmark's own",
			env: map[string]string{
				"CREWLET_BENCH_S3_BUCKET": "acme-bench",
				"CREWLET_BENCH_S3_REGION": "eu-west-1",
			},
			want: s3obj.Config{Bucket: "acme-bench", Region: "eu-west-1", Prefix: "crewlet-bench/"},
			ok:   true,
		},
		{
			name: "every variable set",
			env: map[string]string{
				"CREWLET_BENCH_S3_BUCKET":     "acme-bench",
				"CREWLET_BENCH_S3_REGION":     "us-east-1",
				"CREWLET_BENCH_S3_ENDPOINT":   "https://minio.example.com",
				"CREWLET_BENCH_S3_PREFIX":     "runs/42/",
				"CREWLET_BENCH_S3_PATH_STYLE": "true",
			},
			want: s3obj.Config{
				Bucket: "acme-bench", Region: "us-east-1", Endpoint: "https://minio.example.com",
				Prefix: "runs/42/", PathStyle: true,
			},
			ok: true,
		},
		{
			name: "a path style of false is virtual-hosted",
			env: map[string]string{
				"CREWLET_BENCH_S3_BUCKET":     "acme-bench",
				"CREWLET_BENCH_S3_REGION":     "auto",
				"CREWLET_BENCH_S3_PATH_STYLE": "false",
			},
			want: s3obj.Config{Bucket: "acme-bench", Region: "auto", Prefix: "crewlet-bench/"},
			ok:   true,
		},
		{
			name: "the SDK's own credentials stay the SDK's to read",
			env: map[string]string{
				"CREWLET_BENCH_S3_BUCKET": "acme-bench",
				"CREWLET_BENCH_S3_REGION": "eu-west-1",
				"AWS_ACCESS_KEY_ID":       "example-key-id",
				"AWS_SECRET_ACCESS_KEY":   "example-secret",
			},
			want: s3obj.Config{Bucket: "acme-bench", Region: "eu-west-1", Prefix: "crewlet-bench/"},
			ok:   true,
		},
		{
			name: "a bucket without a region is refused",
			env: map[string]string{
				"CREWLET_BENCH_S3_BUCKET": "acme-bench",
			},
			refused: "CREWLET_BENCH_S3_REGION",
		},
		{
			name: "a path style that is not a boolean is refused",
			env: map[string]string{
				"CREWLET_BENCH_S3_BUCKET":     "acme-bench",
				"CREWLET_BENCH_S3_REGION":     "eu-west-1",
				"CREWLET_BENCH_S3_PATH_STYLE": "yes please",
			},
			refused: "CREWLET_BENCH_S3_PATH_STYLE",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok, err := parseRealBucket(func(name string) string { return tc.env[name] })
			if tc.refused != "" {
				if err == nil {
					t.Fatalf("accepted %v as %+v; want a refusal naming %s", tc.env, got, tc.refused)
				}
				if !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("refusal %q does not name %s, the variable to change", err, tc.refused)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused %v: %v", tc.env, err)
			}
			if ok != tc.ok || got != tc.want {
				t.Fatalf("read %v as %+v, %v; want %+v, %v", tc.env, got, ok, tc.want, tc.ok)
			}
		})
	}
}
