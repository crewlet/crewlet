package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// artifactListener is a served node's api block with an artifact listener at
// address, for a case about that address.
func artifactListener(address string) string {
	return Served("api:\n  port: 8080\n  artifacts:\n    port: 8001\n    external_url: " +
		address + "\n")
}

// THE ARTIFACT ORIGIN LOADS in each shape the serving guide writes, and the
// mode it is served in is decided by the SITE as well as the address: a
// template on the dashboard's own registrable domain is served opaque, because
// a real origin there would be same-site to the dashboard and carry its
// cookie on every fetch an artifact starts.
func TestTheArtifactOriginModeFollowsTheSite(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		address string
		mode    artifacts.OriginMode
		warned  bool
	}{
		"an origin per artifact on a site of its own": {
			"https://{artifact}.artifacts.example.net", artifacts.OriginPerArtifact, false},
		"the hosted form, one DNS level with a company suffix": {
			"https://{artifact}--acme.crewletusercontent.com", artifacts.OriginPerArtifact, false},
		"a template on the dashboard's own site is served opaque": {
			"https://{artifact}.art.example.com", artifacts.OriginShared, true},
		"one shared origin on another site": {
			"https://artifacts.example.org", artifacts.OriginShared, false},
		"one shared origin on the dashboard's site": {
			"https://artifacts.example.com", artifacts.OriginShared, true},
		"a second port on the dashboard's host": {
			"https://crewlet.example.com:8443", artifacts.OriginShared, true},
		"loopback development with a per-artifact template": {
			"http://{artifact}.localhost:8001", artifacts.OriginPerArtifact, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, err := ParseRunnableBootstrap([]byte(artifactListener(tc.address)), EnvOnly())
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if got := b.ArtifactOriginMode(); got != tc.mode {
				t.Fatalf("mode %q, want %q", got, tc.mode)
			}
			warned := false
			for _, w := range b.Warnings() {
				if w.Path == "api.artifacts.external_url" {
					warned = true
				}
			}
			if warned != tc.warned {
				t.Fatalf("same-site warning given %v, want %v", warned, tc.warned)
			}
		})
	}
}

// EVERY ARTIFACT ADDRESS THAT CANNOT BE SAFE OR CANNOT WORK IS REFUSED, naming
// the field: each would otherwise run artifact script where the API's session
// runs, carry it in the clear, or mint hosts no DNS can hold.
func TestAnArtifactAddressThatCannotWorkIsRefused(t *testing.T) {
	t.Parallel()
	const field = "api.artifacts.external_url"
	cases := []struct {
		name, address string
		kind          error
	}{
		{"the placeholder twice", "https://{artifact}.{artifact}.example.net", ErrShape},
		{"the placeholder inside a label", "https://art-{artifact}.example.net", ErrShape},
		{"the placeholder as the whole host", "https://{artifact}", ErrShape},
		{"a label past 63 bytes",
			"https://{artifact}" + strings.Repeat("x", 40) + ".example.net", ErrShape},
		{"a label suffix no host may carry", "https://{artifact}_Acme.example.net", ErrShape},
		{"plain http on a routable host", "http://artifacts.example.org", ErrShape},
		{"a path", "https://artifacts.example.org/pages", ErrShape},
		{"the dashboard's own origin", "https://crewlet.example.com", ErrConflict},
		{"a parent of the dashboard's host", "https://example.com", ErrConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, artifactListener(tc.address), field)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
		})
	}
}

// AN ORIGIN THE API ADMITS CROSS-SITE IS NEVER AN ARTIFACT ORIGIN: listed in
// api.auth.allowed_origins, every page an artifact serves could write and
// open sockets as the signed-in person.
func TestAnArtifactOriginIsNeverOneTheAPIAdmits(t *testing.T) {
	t.Parallel()
	b, err := ParseRunnableBootstrap([]byte(artifactListener("https://artifacts.example.org")), EnvOnly())
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	b.API.Auth.AllowedOrigins = []string{"https://artifacts.example.org"}
	err = b.Validate()
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "allowed_origins") {
		t.Fatalf("an allow-listed artifact origin was accepted: %v", err)
	}
}

// THE LISTENER'S OWN RULES: a port needs an address on an ingress node, an
// address needs a port, and the port is a socket like any other.
func TestAnArtifactListenerThatCannotOpenIsRefused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, doc, path string
		kind            error
	}{
		{"a port and no address",
			"api:\n  port: 8080\n  artifacts:\n    port: 8001\n", "api.artifacts.external_url", ErrMissing},
		{"an address and no port",
			"api:\n  port: 8080\n  artifacts:\n    external_url: https://artifacts.example.org\n",
			"api.artifacts.port", ErrMissing},
		{"the API's own port",
			"api:\n  port: 8080\n  artifacts:\n    port: 8080\n    external_url: https://artifacts.example.org\n",
			"api.artifacts.port", ErrConflict},
		{"out of range",
			"api:\n  port: 8080\n  artifacts:\n    port: 70000\n    external_url: https://artifacts.example.org\n",
			"api.artifacts.port", ErrOutOfRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, Served(tc.doc), tc.path)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
		})
	}
}

// AN UNSET QUOTA TAKES ITS DOCUMENTED DEFAULT, and a set one is stamped as
// written: the value a deciding node puts into every record.
func TestTheArtifactQuotasResolveToTheirDefaults(t *testing.T) {
	t.Parallel()
	want := artifacts.Limits{
		CompanyBytes:   artifacts.DefaultCompanyBytes,
		Artifacts:      artifacts.DefaultArtifacts,
		DataBytes:      artifacts.DefaultDataBytes,
		DataStoreBytes: artifacts.DefaultDataStoreBytes,
		PendingBytes:   artifacts.DefaultPendingBytes,
	}
	if got := (Artifacts{}).Stamp(); got != want {
		t.Fatalf("unset quotas stamp %+v, want %+v", got, want)
	}
	set := Artifacts{Limits: ArtifactLimits{Artifacts: 500}}
	if got := set.Stamp().Artifacts; got != 500 {
		t.Fatalf("a written quota stamps %d, want 500", got)
	}
}

// A QUOTA OUTSIDE ITS BOUNDS, OR ONE STORE ALLOWED MORE THAN EVERY STORE, IS
// REFUSED naming the field.
func TestAnArtifactQuotaThatCannotHoldIsRefused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, doc, path string
		kind            error
	}{
		{"company bytes under a gibibyte",
			"artifacts:\n  limits:\n    company_bytes: 1048576\n",
			"artifacts.limits.company_bytes", ErrOutOfRange},
		{"too few artifacts",
			"artifacts:\n  limits:\n    artifacts: 5\n", "artifacts.limits.artifacts", ErrOutOfRange},
		{"one store above every store",
			"artifacts:\n  limits:\n    data_bytes: 67108864\n    data_store_bytes: 134217728\n",
			"artifacts.limits.data_store_bytes", ErrConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, Served(tc.doc), tc.path)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
		})
	}
}

// THE NATS BUCKET'S RESERVATION IS THE NATS BACKEND'S ALONE, and it must fit
// beside the log ceilings an operator wrote inside the broker's limit.
func TestTheObjectReservationIsHeldToTheBroker(t *testing.T) {
	t.Parallel()
	err := rejectsBootstrap(t, Served("store:\n  objects:\n    backend: s3\n"+
		"    nats_max_bytes: 2147483648\n    s3:\n      bucket: b\n      region: auto\n"),
		"store.objects.nats_max_bytes")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want a conflict, got %v", err)
	}
	// 48 GiB of bucket beside a 32 GiB mutation log is 80 GiB inside a
	// 64 GiB limit, and nothing else in the document is wrong: a case with a
	// second problem passes on whichever of the two fires.
	err = rejectsBootstrap(t, Served("store:\n  objects:\n    nats_max_bytes: 51539607552\n"+
		"stream:\n  store_dir: /var/js\n  store_max_bytes: 68719476736\n"+
		"  tracker_log_max_bytes: 34359738368\n"),
		"store.objects.nats_max_bytes")
	if problems := Problems(err); len(problems) != 1 {
		t.Fatalf("want the one refusal at the bucket, got %+v", problems)
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want a conflict, got %v", err)
	}
}

// AN UNSET ARTIFACTS LOG CEILING DERIVES FROM THE MUTATION LOG'S, inside the
// bounds the field accepts, and a written one is taken as written.
func TestTheArtifactLogCeilingsDeriveFromTheMutationLog(t *testing.T) {
	t.Parallel()
	s := Stream{TrackerLogMaxBytes: 64 << 30}
	if got, derived := s.ArtifactsMaxBytes(0); got != 16<<30 || !derived {
		t.Fatalf("artifacts log %d (derived %v), want a quarter of 64 GiB", got, derived)
	}
	if got, derived := s.ArtifactDataMaxBytes(0); got != 16<<30 || !derived {
		t.Fatalf("datastore log %d (derived %v), want a quarter of 64 GiB", got, derived)
	}
	small := Stream{TrackerLogMaxBytes: 1 << 30}
	if got, _ := small.ArtifactsMaxBytes(0); got != ArtifactsLogMaxBytesFloor {
		t.Fatalf("artifacts log %d under its floor", got)
	}
	set := Stream{ArtifactDataLogMaxBytes: 3 << 30}
	if got, derived := set.ArtifactDataMaxBytes(0); got != 3<<30 || derived {
		t.Fatalf("a written ceiling read as %d (derived %v)", got, derived)
	}
}

// THE COMPANY'S SEEN-BY POLICY reads unset as editors and refuses a value it
// cannot enforce.
func TestTheSeenByPolicyIsAKnownValue(t *testing.T) {
	t.Parallel()
	c, err := ParseCompany([]byte("name: Acme\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Artifacts.SeenByOrDefault(); got != SeenByEditors {
		t.Fatalf("unset seen_by reads %q, want editors", got)
	}
	if _, err := ParseCompany([]byte("name: Acme\nartifacts:\n  seen_by: everyone\n")); err == nil ||
		!strings.Contains(err.Error(), "artifacts.seen_by") {
		t.Fatalf("an unknown seen_by was accepted: %v", err)
	}
}
