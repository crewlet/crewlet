package config

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/words"
)

// StoreObjects is where the company's files are kept: which backend every node
// reads and writes their objects through (ADR-0026).
//
// It is Tier A on every node: an endpoint, a bucket and credentials are the
// operator's, and a node needs them before it can serve an upload. The fleet
// records the first node's choice in the coordination store, so a node
// configured with another backend refuses to boot rather than split the files
// between two stores (coord.ObjectStores).
//
// The backend keeps its own copies: the NATS bucket is a stream replicated at
// `stream.replicas`, and any other keeps what its provider promises. The engine
// places nothing, so there is no failure domain to spread.
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
	// each name theirs here. One no request can reach is refused naming
	// what is wrong with it ([checkS3Endpoint]).
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty" js:"maxlen=2048" desc:"The S3 API's base URL: http or https, a host, and a port of 1..65535 if it has one; empty is Amazon S3's own for the region. At most 2048 characters."`
	// Region is the bucket's region — required by the signature even where
	// the provider ignores it (R2 takes auto, MinIO us-east-1).
	Region string `yaml:"region,omitempty" json:"region,omitempty" desc:"The bucket's region; required (R2: auto, MinIO: us-east-1)."`
	// Bucket is the bucket's name.
	Bucket string `yaml:"bucket,omitempty" json:"bucket,omitempty" js:"maxlen=255" desc:"The bucket's name; required. At most 255 characters."`
	// Prefix is prepended to every key: an object is `<prefix>files/<key>`.
	// It keeps keys from colliding where companies share a bucket, and
	// nothing more: what keeps one company out of another's files is a
	// credential scoped to its own prefix (see docs/concepts/object-store.md).
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty" js:"maxlen=1024" desc:"Prepended to every object's key, e.g. acme/. Companies sharing a bucket are isolated only by credentials scoped to their own prefix. At most 1024 characters."`
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
		p.wrap(checkS3Endpoint(at(s3, "endpoint"), o.S3.Endpoint))
		p.wrap(checkObjectIdentity(s3, o.S3))
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

// MaxS3BucketLength and MaxS3PrefixLength bound an S3 store's bucket and
// prefix in characters as written, and [MaxExternalURLLength] its endpoint:
// the three spell the store's identity ([StoreObjects.Identity]), which the
// fleet records as a small value read at the term (internal/coord/kv's
// objects bucket), and these keep its widest, every character four bytes,
// at 13,313 bytes, within it. 255 is the longest bucket name any
// S3-compatible store takes (Amazon's are 63, Ceph's gateway's 255); 1024 is
// S3's bound on a whole key, which every key the store writes starts its
// prefix in.
const (
	MaxS3BucketLength = 255
	MaxS3PrefixLength = 1024
)

// checkObjectIdentity refuses an S3 store whose endpoint, bucket or prefix is
// past its bound, naming the field.
func checkObjectIdentity(s3 Path, o ObjectsS3) error {
	var p problems
	for _, f := range []struct {
		name, value string
		max         int
	}{
		{"endpoint", o.Endpoint, MaxExternalURLLength},
		{"bucket", o.Bucket, MaxS3BucketLength},
		{"prefix", o.Prefix, MaxS3PrefixLength},
	} {
		if n := utf8.RuneCountInString(f.value); n > f.max {
			p.add(at(s3, f.name), ErrOutOfRange, "is %d characters; it can be at most %d, so the "+
				"fleet's record of its object store stays a small value every node reads", n, f.max)
		}
	}
	return p.err()
}

// checkS3Endpoint refuses an endpoint no request to the bucket can be sent to,
// naming what is wrong. Empty is Amazon S3's own endpoint, and passes.
//
// Only what can never work refuses: not an http(s) URL with a host and no query
// (the client refuses it at start; with no host a path-style request goes
// nowhere); a port outside 1 to 65535; an IPv6 zone holding `]`, `%` or a space
// (it splits the address where the dialer looks for a port). Each used to
// surface 77 to 86 s into boot.
//
// EVERY FORM THE CLIENT REACHES PASSES: a scheme's own port, leading zeros or
// none after the colon, an upper-case scheme, a base path, a non-ASCII host, a
// bracketed IPv6 with a named or numbered zone, `http://:9000`.
func checkS3Endpoint(path Path, raw string) error {
	var p problems
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		if parse, ok := errors.AsType[*url.Error](err); ok {
			err = parse.Err
		}
		p.add(path, ErrShape, "%q is not a URL the S3 client can parse (%v): write the S3 API's "+
			"base URL, e.g. https://minio.example:9000", raw, err)
	case u.Scheme != "http" && u.Scheme != "https":
		hint := "https://" + raw
		if u.Host != "" {
			hint = "https://" + u.Host + u.EscapedPath()
		}
		p.add(path, ErrShape, "%q is not an http or https URL, which the S3 client refuses: "+
			"write %s", raw, hint)
	case u.Host == "":
		p.add(path, ErrShape, "%q names no host, so no request reaches a bucket: write the S3 "+
			"API's host, e.g. %s://minio.example:9000", raw, u.Scheme)
	case u.RawQuery != "":
		p.add(path, ErrShape, "%q has a query, which the S3 client refuses in an endpoint: "+
			"remove it", raw)
	default:
		p.wrap(checkPort(path, raw, u.Port()))
		host := u.Hostname()
		if i := strings.IndexByte(host, '%'); i >= 0 {
			if zone := host[i+1:]; strings.ContainsAny(zone, "]%") ||
				strings.IndexFunc(zone, unicode.IsSpace) >= 0 {
				p.add(path, ErrShape, "%q has the IPv6 zone %q, which no dial can use: name the "+
					"interface as it is, by its name or its number, e.g. https://[fe80::1%%25eth0]:9000",
					raw, zone)
			}
		}
	}
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
