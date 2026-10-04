package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// THE BLOCK IS VALIDATED WHERE IT WAS WRITTEN: an unknown backend, an s3
// backend with no bucket or region or with half a key, and a bucket named
// under a backend that would not use it are refused naming the field; the
// zero block is the nats backend, and a complete bucket is accepted.
func TestTheObjectsBlockIsValidated(t *testing.T) {
	t.Parallel()
	bucket := config.ObjectsS3{Region: "us-east-1", Bucket: "files"}
	for name, tc := range map[string]struct {
		objects config.StoreObjects
		refuse  string // "" = accepted
	}{
		"the zero block": {config.StoreObjects{}, ""},
		"nats named":     {config.StoreObjects{Backend: config.ObjectBackendNATS}, ""},
		"a bucket":       {config.StoreObjects{Backend: config.ObjectBackendS3, S3: bucket}, ""},
		"a bucket with keys": {config.StoreObjects{Backend: config.ObjectBackendS3, S3: config.ObjectsS3{
			Region: "auto", Bucket: "files", Endpoint: "https://example.com",
			AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example"}}, ""},
		"an unknown backend": {config.StoreObjects{Backend: "disk"}, "store.objects.backend"},
		"s3 and no bucket": {config.StoreObjects{Backend: config.ObjectBackendS3,
			S3: config.ObjectsS3{Region: "us-east-1"}}, "store.objects.s3.bucket"},
		"s3 and no region": {config.StoreObjects{Backend: config.ObjectBackendS3,
			S3: config.ObjectsS3{Bucket: "files"}}, "store.objects.s3.region"},
		"half a key": {config.StoreObjects{Backend: config.ObjectBackendS3, S3: config.ObjectsS3{
			Region: "us-east-1", Bucket: "files", AccessKeyID: "AKIAEXAMPLE"}}, "store.objects.s3"},
		"a bucket nats would ignore": {config.StoreObjects{S3: bucket}, "store.objects.s3"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := config.DefaultBootstrap()
			b.Stream.StoreDir = t.TempDir()
			b.Store.Objects = tc.objects
			err := b.Validate()
			if tc.refuse == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %+v", tc.objects)
			}
			if !strings.Contains(err.Error(), tc.refuse) {
				t.Errorf("refusal %q does not name %s", err, tc.refuse)
			}
		})
	}
}

// THE IDENTITY NAMES WHERE THE FILES ARE, and nothing else: two nodes reaching
// one bucket with different keys are one store, and two buckets — or one
// bucket under two prefixes — are two.
func TestTheObjectsIdentityNamesWhereTheFilesAre(t *testing.T) {
	t.Parallel()
	nats := config.StoreObjects{}
	if got := nats.Identity(); got != "nats" {
		t.Fatalf("the default identity is %q", got)
	}
	a := config.StoreObjects{Backend: config.ObjectBackendS3, S3: config.ObjectsS3{
		Endpoint: "https://s3.example.com/", Region: "us-east-1", Bucket: "files", Prefix: "acme/",
		AccessKeyID: "one", SecretAccessKey: "x"}}
	b := a
	b.S3.AccessKeyID, b.S3.Endpoint = "another", "https://s3.example.com"
	if a.Identity() != b.Identity() {
		t.Errorf("one bucket reached with two keys is two identities: %q, %q", a.Identity(), b.Identity())
	}
	c := a
	c.S3.Prefix = "other/"
	d := a
	d.S3.Bucket = "files2"
	for _, other := range []config.StoreObjects{c, d, nats} {
		if other.Identity() == a.Identity() {
			t.Errorf("%q is the same identity as %q", other.Identity(), a.Identity())
		}
	}
}

// STREAM REPLICAS ARE BOUNDED BY WHAT THE BROKER CAN KEEP: JetStream's ceiling
// of five everywhere, and on an embedded cluster the members it names — past
// either, every stream and bucket the node provisions is refused at boot.
func TestStreamReplicasAreBoundedByWhatTheBrokerCanKeep(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		replicas int
		peers    []string
		refuse   string
	}{
		"three on three members": {3, []string{"nats://b:6222", "nats://c:6222"}, ""},
		"five on five members": {5, []string{"nats://b:6222", "nats://c:6222",
			"nats://d:6222", "nats://e:6222"}, ""},
		"three on a two-peer list": {3, []string{"nats://b:6222", "nats://c:6222"}, ""},
		"four on three members":    {4, []string{"nats://b:6222", "nats://c:6222"}, "names 3"},
		"six on six members": {6, []string{"nats://b:6222", "nats://c:6222",
			"nats://d:6222", "nats://e:6222", "nats://f:6222"}, "1..5"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := fleetBootstrap(t)
			b.Stream.Replicas = tc.replicas
			b.Stream.Cluster.Peers = tc.peers
			err := b.Validate()
			if tc.refuse == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%d replicas on %d members accepted", tc.replicas, len(tc.peers)+1)
			}
			if !strings.Contains(err.Error(), "stream.replicas") ||
				!strings.Contains(err.Error(), tc.refuse) {
				t.Errorf("refusal %q, want it to name stream.replicas and say %q", err, tc.refuse)
			}
		})
	}
}
