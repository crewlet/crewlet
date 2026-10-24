package config

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/words"
)

// StoreObjects is where the company's files are kept: which backend every node
// reads and writes their objects through (ADR-0026).
//
// # Why Tier A, and why every node
//
// The backend is infrastructure, like the stream: an endpoint, a bucket and
// the credentials to reach it are the operator's, and a node needs them before
// it can serve a single upload. Every node carries the same block — a node
// without `data` uploads and downloads files exactly as a data node does — and
// the fleet records the first node's choice in the coordination store, so a
// node configured with another backend refuses to boot rather than splitting
// the company's files between two stores (coord.ObjectStores).
//
// # Why no copies or failure domain here
//
// The backend keeps its own: the NATS backend's bucket is a stream replicated
// at `stream.replicas` across the broker's members like every other, and a
// bucket keeps whatever copies its provider promises. The engine places
// nothing, so there is nothing for it to spread.
type StoreObjects struct {
	// Backend is where the files' objects live. Empty is `nats`.
	Backend ObjectBackend `yaml:"backend,omitempty" json:"backend,omitempty" desc:"Where the company's files are kept: nats (the default — a bucket on the fleet's own broker, replicated at stream.replicas) or s3 (an S3-compatible bucket, named under s3)."`

	// S3 is the bucket, for the `s3` backend; refused for any other.
	S3 ObjectsS3 `yaml:"s3,omitempty" json:"s3,omitzero"`

	// NatsMaxBytes is the byte RESERVATION of the `nats` backend's bucket on
	// the fleet's broker.
	//
	// A RESERVATION, NOT A HOPE. The state logs reserve their ceilings
	// against stream.store_max_bytes, and the leases, ledgers and budget
	// counters the fleet coordinates through share whatever is left. A
	// bucket with no ceiling grows into that remainder, and a full remainder
	// refuses a lease heartbeat: a fleet outage, not a file outage. Unset
	// derives a share of the broker's limit; refused for the `s3` backend,
	// whose bucket is not on the broker at all.
	NatsMaxBytes int64 `yaml:"nats_max_bytes,omitempty" json:"nats_max_bytes,omitempty" js:"min=1073741824;max=70368744177664" desc:"Byte reservation of the nats backend's bucket on the broker; unset derives a share of stream.store_max_bytes, 1 GiB..64 TiB. Refused with backend s3."`
}

// ObjectBackend is a backend the object store can keep objects in.
type ObjectBackend string

const (
	// ObjectBackendNATS keeps objects in the fleet's own JetStream object
	// store. The default.
	ObjectBackendNATS ObjectBackend = "nats"
	// ObjectBackendS3 keeps objects in an S3-compatible bucket.
	ObjectBackendS3 ObjectBackend = "s3"
)

// ObjectBackends is every backend in the order the docs name them.
var ObjectBackends = []ObjectBackend{ObjectBackendNATS, ObjectBackendS3}

// Valid reports whether b is a backend this build speaks.
func (b ObjectBackend) Valid() bool { return slices.Contains(ObjectBackends, b) }

// ObjectsS3 is an S3-compatible bucket.
type ObjectsS3 struct {
	// Endpoint is the S3 API's base URL; empty is Amazon's own for the
	// region. R2, MinIO, GCS's interoperability endpoint and Ceph's gateway
	// each name theirs here.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty" desc:"The S3 API's base URL; empty is Amazon S3's own for the region."`
	// Region is the bucket's region — required by the signature even where
	// the provider ignores it (R2 takes auto, MinIO us-east-1).
	Region string `yaml:"region,omitempty" json:"region,omitempty" desc:"The bucket's region; required (R2: auto, MinIO: us-east-1)."`
	// Bucket is the bucket's name.
	Bucket string `yaml:"bucket,omitempty" json:"bucket,omitempty" desc:"The bucket's name; required."`
	// Prefix is prepended to every key: an object is `<prefix>files/<key>`.
	// It keeps keys from colliding where companies share a bucket, and
	// nothing more: what keeps one company out of another's files is a
	// credential scoped to its own prefix (see docs/concepts/object-store.md).
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty" desc:"Prepended to every object's key, e.g. acme/. Companies sharing a bucket are isolated only by credentials scoped to their own prefix."`
	// PathStyle addresses the bucket in the URL's path rather than its host
	// name, which MinIO and most self-hosted gateways need.
	PathStyle bool `yaml:"path_style,omitempty" json:"path_style,omitempty" desc:"Address the bucket in the path rather than the host name; MinIO and most self-hosted gateways need it."`
	// AccessKeyID and SecretAccessKey are the credentials, as ${VAR}
	// references. Both empty takes the SDK's own chain — the environment,
	// a shared profile, a web identity or the instance's role.
	AccessKeyID     string `yaml:"access_key_id,omitempty" json:"access_key_id,omitempty" desc:"Access key id, as a ${VAR} reference; with secret_access_key, or neither to use the environment or the instance's role."`
	SecretAccessKey string `secret:"true" yaml:"secret_access_key,omitempty" json:"secret_access_key,omitempty" desc:"Secret access key, as a ${VAR} reference."`
}

// BackendOrDefault is the backend with the default applied.
func (o StoreObjects) BackendOrDefault() ObjectBackend {
	if o.Backend == "" {
		return ObjectBackendNATS
	}
	return o.Backend
}

// Identity names the store the objects are in, for the fleet's record of it:
// two nodes whose identities differ would write the company's files into two
// different places. The credentials are not part of it — two nodes may reach
// one bucket with different keys.
func (o StoreObjects) Identity() string {
	if o.BackendOrDefault() != ObjectBackendS3 {
		return string(ObjectBackendNATS)
	}
	return "s3:" + strings.TrimSuffix(o.S3.Endpoint, "/") + "/" + o.S3.Bucket + "/" + o.S3.Prefix
}

func (o StoreObjects) validate(path Path) error {
	var p problems
	if o.Backend != "" && !o.Backend.Valid() {
		p.add(at(path, "backend"), ErrUnknownValue,
			"must be one of %s, got %q", words.Or(ObjectBackends), o.Backend)
	}
	s3 := at(path, "s3")
	switch {
	case o.BackendOrDefault() == ObjectBackendS3:
		if strings.TrimSpace(o.S3.Bucket) == "" {
			p.add(at(s3, "bucket"), ErrMissing, "the s3 backend keeps the files in a bucket; name it")
		}
		if strings.TrimSpace(o.S3.Region) == "" {
			p.add(at(s3, "region"), ErrMissing,
				"every S3 request is signed for a region, even where the provider "+
					"ignores it; name the bucket's (R2: auto, MinIO: us-east-1)")
		}
		if (o.S3.AccessKeyID == "") != (o.S3.SecretAccessKey == "") {
			p.add(s3, ErrConflict,
				"access_key_id and secret_access_key are given together, or neither "+
					"to take the environment's or the instance's credentials")
		}
	case o.S3 != (ObjectsS3{}):
		p.add(s3, ErrConflict,
			"names a bucket, and store.objects.backend is %q; set backend: s3 to use it",
			o.BackendOrDefault())
	}
	if o.NatsMaxBytes != 0 && o.BackendOrDefault() != ObjectBackendNATS {
		p.add(at(path, "nats_max_bytes"), ErrConflict,
			"reserves room for the nats bucket on the broker, and store.objects."+
				"backend is %q, whose objects are not on the broker. Remove it",
			o.BackendOrDefault())
	}
	bytesInRange(&p, path, "nats_max_bytes", o.NatsMaxBytes,
		NatsObjectsMaxBytesFloor, NatsObjectsMaxBytesCeiling)
	return p.err()
}

// validateObjectReservation refuses a bucket reservation the broker cannot
// back beside the state-log ceilings an operator wrote: the broker grants a
// stream its whole ceiling when it creates it, so the sum must fit inside
// stream.store_max_bytes. NECESSARY AND NOT SUFFICIENT, for the stream block's
// own reason: every other stream on the broker grows inside the same limit.
func (b *Bootstrap) validateObjectReservation() error {
	var p problems
	bucket := b.Store.Objects.NatsMaxBytes
	limit := b.Stream.StoreMaxBytes
	if bucket == 0 || limit == 0 || b.Store.Objects.BackendOrDefault() != ObjectBackendNATS {
		return nil
	}
	if logs := b.Stream.DeclaredLogCeilings(); bucket+logs > limit {
		p.add(field("store.objects.nats_max_bytes"), ErrConflict,
			"%d bytes for the object bucket plus %d bytes of declared log "+
				"ceilings is more than stream.store_max_bytes %d: the broker "+
				"would refuse to create one of them. Lower the reservation or "+
				"raise store_max_bytes, and leave headroom for the streams that "+
				"reserve nothing",
			bucket, logs, limit)
	}
	return p.err()
}
