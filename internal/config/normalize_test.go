package config_test

import (
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// TIER A IS TRIMMED BEFORE ANY RULE READS IT, so the value a rule checked is
// the value the engine runs.
//
// Whitespace arrives because ${VAR} references resolve first and what they
// carry comes out of files, secrets and captured command output, all of
// which keep a trailing newline. Every block is represented, and three of
// the entries are not plain struct fields — a slice of strings, a slice of
// structs, and a map's KEY — because those are what a hand-written list of
// fields misses.
func TestTierAIsTrimmedBeforeAnyRuleReadsIt(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Node.ID = " node-a\n"
	b.Node.Labels = map[string]string{" zone\t": " eu \n"}
	b.Store.Path = " /var/lib/crewlet/node.db\n"
	b.Stream.URL = " nats://broker.example.com:4222 \n"
	b.API.Host = " 127.0.0.1 "
	b.API.Auth.Tokens = []config.APIToken{{ID: " ops \n", Token: " tok_example \n"}}
	b.API.Auth.AllowedOrigins = []string{" https://ops.example.com \n"}
	b.Secrets = config.Secrets{
		ActiveKeyID: " k1 \n",
		Keys:        []config.SecretKey{{ID: " k1 \n", Material: " bWF0ZXJpYWw= \n"}},
	}

	_ = b.Validate() // the trim is what is under test, not the verdict

	for name, got := range map[string]string{
		"node.id":                     b.Node.ID,
		"store.path":                  b.Store.Path,
		"stream.url":                  b.Stream.URL,
		"api.host":                    b.API.Host,
		"api.auth.tokens[0].id":       b.API.Auth.Tokens[0].ID,
		"api.auth.tokens[0].token":    b.API.Auth.Tokens[0].Token,
		"api.auth.allowed_origins[0]": b.API.Auth.AllowedOrigins[0],
		"secrets.active_key_id":       b.Secrets.ActiveKeyID,
		"secrets.keys[0].id":          b.Secrets.Keys[0].ID,
		"secrets.keys[0].material":    b.Secrets.Keys[0].Material,
	} {
		if got != strings.TrimSpace(got) {
			t.Errorf("%s = %q, still carries the whitespace the runtime would read", name, got)
		}
	}
	if _, ok := b.Node.Labels["zone"]; !ok {
		t.Errorf("node.labels = %v, want the key trimmed to \"zone\": a placement "+
			"selector matches it EXACTLY, so an untrimmed key matches nothing", b.Node.Labels)
	}
	if got := b.Node.Labels["zone"]; got != "eu" {
		t.Errorf("node.labels[zone] = %q, want %q", got, "eu")
	}
}

// AND THE WALK REACHES A FIELD NOBODY LISTED.
//
// The guard against this being rewritten as an enumeration of the fields
// somebody thought of — the shape the cluster block's own IsZero comment
// records going wrong, where a list named two of four and the other two went
// unvalidated. The padding here is applied by a walk this test owns, so a
// production walk that stopped reaching a field would leave that field
// padded and fail, while a list that was merely COMPLETE TODAY would pass
// only until the next field lands.
func TestTheTrimReachesEveryStringTierAHas(t *testing.T) {
	t.Parallel()
	b := populatedBootstrap(t)

	padded := padStrings(t, reflect.ValueOf(&b))
	if padded < 20 {
		t.Fatalf("padded %d strings, want the whole of Tier A: the fixture stopped "+
			"reaching most of it, so this test no longer proves anything", padded)
	}

	_ = b.Validate()

	var left []string
	findPadded(reflect.ValueOf(&b), "", func(path, s string) {
		left = append(left, path+" = "+s)
	})
	if len(left) > 0 {
		t.Errorf("after Validate these strings still carry their whitespace:\n\t%s",
			strings.Join(left, "\n\t"))
	}
}

// A WHITESPACE-ONLY VALUE IS UNSET, not a value made of whitespace.
//
// `advertise: " "` was the sharpest case: validation trimmed it to empty and
// skipped the address check ENTIRELY, then the engine handed the field raw to
// server.ClusterOpts.Advertise, where nats-server refuses it while STARTING —
// a node that boots, dies, and leaves the operator reading broker logs for a
// typo in their own file.
func TestAWhitespaceOnlyValueIsUnsetRatherThanRunAsWhitespace(t *testing.T) {
	t.Parallel()
	b := fleetBootstrap(t)
	b.Stream.Cluster.Advertise = " \t\n"

	if err := b.Validate(); err != nil {
		t.Fatalf("refused a cluster that sets nothing but whitespace: %v", err)
	}
	if b.Stream.Cluster.Advertise != "" {
		t.Errorf("cluster.advertise = %q, want it unset: the broker is given this "+
			"string verbatim and refuses to start on it",
			b.Stream.Cluster.Advertise)
	}
}

// AND A PADDED ONE IS VALIDATED AS THE ADDRESS IT WILL RUN AS.
//
// The other half of the same drift: a padded address passed validation,
// because the check trimmed its own copy, and then reached the broker with
// the padding still on it.
func TestAPaddedAddressIsValidatedAsWhatTheBrokerWillGet(t *testing.T) {
	t.Parallel()
	b := fleetBootstrap(t)
	b.Stream.Cluster.Advertise = " node-a.internal:6222\n"

	if err := b.Validate(); err != nil {
		t.Fatalf("refused a well-formed advertise address: %v", err)
	}
	if got, want := b.Stream.Cluster.Advertise, "node-a.internal:6222"; got != want {
		t.Errorf("cluster.advertise = %q, want %q", got, want)
	}
}

// TWO KEYS THAT ARE ONE KEY ONCE TRIMMED ARE REFUSED, not resolved.
//
// Collapsing them would pick a winner by Go's randomized range order, so the
// same file would mean two different things on two runs of one binary. The
// map is left intact, because what the refusal describes has to still be in
// front of the operator.
func TestATrimCollisionIsRefusedRatherThanDecidedByMapOrder(t *testing.T) {
	t.Parallel()
	for name, labels := range map[string]map[string]string{
		// One key survives the trim unchanged, so the collision is with an
		// entry that was never a candidate to be rewritten.
		"against a key already correct": {"zone": "eu", "zone ": "us"},

		// NEITHER survives unchanged, which is the case that decides
		// whether the collisions are all found BEFORE any rewrite lands.
		// Rewriting as they are found renames whichever the range produced
		// first and reports the second — so the map that comes back
		// differs between two runs of one binary, and the error describes
		// a file the operator can no longer see.
		"against another rewrite": {"zone ": "eu", "zone  ": "us"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := config.DefaultBootstrap()
			b.Node.Labels = maps.Clone(labels)

			err := b.Validate()
			if err == nil {
				t.Fatal("accepted two label keys that are one key once trimmed")
			}
			if !strings.Contains(err.Error(), "node.labels") {
				t.Errorf("refusal = %q, want it to name node.labels", err)
			}
			if !maps.Equal(b.Node.Labels, labels) {
				t.Errorf("labels = %v, want %v: a refused collision must not "+
					"half-apply, or the error describes a file the operator "+
					"can no longer see", b.Node.Labels, labels)
			}
		})
	}
}

// THE REFERENCE INDEX READS THE SAME WALK AND WRITES NOTHING.
//
// One walk serves both questions, which is what stops the trim and the
// reference index disagreeing about which fields a payload has. It also means
// a read-only caller is one closure away from a rewriting one, so this pins
// that it does not rewrite: Tier B carries the company's PROSE, and trimming
// a prompt fragment there would be this pass reaching a tier it has no
// business in.
func TestTheReferenceIndexDoesNotRewriteWhatItWalks(t *testing.T) {
	t.Parallel()
	c := &config.Company{Name: "  Nimbus  "}

	config.References(c)
	config.ReferencedNames(c)

	if got, want := c.Name, "  Nimbus  "; got != want {
		t.Errorf("name = %q after a read-only walk, want %q unchanged", got, want)
	}
}

// ---- helpers --------------------------------------------------------- //

const pad = " \t"

// populatedBootstrap fills the collections Tier A holds, so the walk below
// has a slice element, a slice of structs and a map entry to reach rather
// than only plain fields.
func populatedBootstrap(t *testing.T) config.Bootstrap {
	t.Helper()
	b := config.DefaultBootstrap()
	b.Node.ID = "node-a"
	b.Node.Roles = []string{"seats"}
	b.Node.Labels = map[string]string{"zone": "eu"}
	b.Stream = config.Stream{
		Type: config.StreamEmbedded, Replicas: 3, StoreDir: t.TempDir(),
		Credentials: "/etc/crewlet/nats.creds", Token: "tok_example",
		TLS: config.NATSTLS{CA: "/etc/ssl/ca.pem", Cert: "/etc/ssl/c.pem", Key: "/etc/ssl/k.pem"},
		Cluster: config.StreamCluster{
			Name: "crewlet", Port: 6222, Host: "10.0.0.11",
			Advertise: "node-a.internal:6222",
			Peers:     []string{"nats://node-b.internal:6222"},
		},
	}
	b.Store.ReplicatedPath = "/var/lib/crewlet/replicated.db"
	b.Store.SnapshotDir = "/var/lib/crewlet/snapshots"
	b.API.Auth.Tokens = []config.APIToken{{ID: "ops", Token: "tok_example"}}
	b.API.Auth.AllowedOrigins = []string{"https://ops.example.com"}
	b.Secrets = config.Secrets{
		ActiveKeyID: "k1",
		Keys:        []config.SecretKey{{ID: "k1", Material: "bWF0ZXJpYWw="}},
	}
	return b
}

// padStrings puts whitespace on both ends of every non-empty string reachable
// from v, and reports how many it changed. A walk of this test's own, so that
// what it covers is not whatever the production walk covers.
func padStrings(t *testing.T, v reflect.Value) int {
	t.Helper()
	n := 0
	switch v.Kind() {
	case reflect.String:
		if s := v.String(); s != "" && v.CanSet() {
			v.SetString(pad + s + pad)
			n++
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			n += padStrings(t, v.Elem())
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			n += padStrings(t, v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			val := reflect.New(v.Type().Elem()).Elem()
			val.Set(v.MapIndex(k))
			n += padStrings(t, val)
			key := reflect.New(v.Type().Key()).Elem()
			key.Set(k)
			n += padStrings(t, key)
			v.SetMapIndex(k, reflect.Value{})
			v.SetMapIndex(key, val)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).PkgPath == "" {
				n += padStrings(t, v.Field(i))
			}
		}
	}
	return n
}

// findPadded reports every string still carrying whitespace on either end.
func findPadded(v reflect.Value, path string, report func(path, s string)) {
	switch v.Kind() {
	case reflect.String:
		if s := v.String(); s != strings.TrimSpace(s) {
			report(path, s)
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			findPadded(v.Elem(), path, report)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			findPadded(v.Index(i), path, report)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			findPadded(k, path+"[key]", report)
			findPadded(v.MapIndex(k), path+"["+k.String()+"]", report)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if f.PkgPath == "" {
				findPadded(v.Field(i), path+"."+f.Name, report)
			}
		}
	}
}
