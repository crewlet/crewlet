package config

import (
	"strings"

	"github.com/crewlet/crewlet/internal/iam"
)

// TestKeyMaterial is thirty-two zero bytes in base64: a key the keyring
// accepts, sealing nothing any case here reads back.
const TestKeyMaterial = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// TestKeyringYAML is the `secrets:` block a Tier A document in these suites
// carries, because every node requires one ([Bootstrap.validateTopology]).
const TestKeyringYAML = "secrets:\n  active_key_id: k1\n  keys:\n" +
	"    - {id: k1, material: \"" + TestKeyMaterial + "\"}\n"

// TestStoreDir is where [Runnable] and [RunnableBootstrap] say an embedded
// stream persists. Never opened: validation judges that one is named, which
// is all a case about something else needs.
const TestStoreDir = "/var/lib/crewlet/stream"

// TestStreamYAML is the `stream:` block a Tier A document in these suites
// carries unless it states its own, because an embedded stream held in memory
// is refused on every node ([Stream.Durable]).
const TestStreamYAML = "stream:\n  store_dir: " + TestStoreDir + "\n"

// Runnable is a Tier A document carrying the two things every node requires
// whatever else it says — [TestKeyringYAML] and a stream that survives a
// restart ([TestStreamYAML]) — so that a case states what it is about and
// nothing else: a document missing either would be refused for that before it
// was ever judged on its subject.
//
// A document that states its own `secrets:` block is the case whose subject IS
// the keyring, and one that states its own `stream:` block is about the
// stream: each such block is left as written.
func Runnable(doc string) string {
	// THE EMPTY MAPPING WRITTEN IN FLOW STYLE, which is what an operator's
	// "empty" file looks like: a block key after it is not YAML at all.
	if strings.TrimSpace(doc) == "{}" {
		doc = ""
	}
	// A NULL DOCUMENT is no mapping at all, so there is nothing to add a
	// block to without making it a different document: it is left as
	// written, and refused for the keyring it cannot carry.
	switch strings.TrimSpace(doc) {
	case "~", "null":
		return doc
	}
	if doc != "" && !strings.HasSuffix(doc, "\n") {
		doc += "\n"
	}
	if !states(doc, "secrets") {
		doc += TestKeyringYAML
	}
	if !states(doc, "stream") {
		doc += TestStreamYAML
	}
	return doc
}

// states reports whether a document names top-level key.
func states(doc, key string) bool {
	return strings.HasPrefix(doc, key+":") || strings.Contains(doc, "\n"+key+":")
}

// RunnableBootstrap is [DefaultBootstrap] carrying the keyring and the durable
// stream every node needs.
func RunnableBootstrap() Bootstrap {
	b := DefaultBootstrap()
	b.Secrets = Secrets{
		ActiveKeyID: "k1",
		Keys:        []SecretKey{{ID: "k1", Material: TestKeyMaterial}},
	}
	b.Stream.StoreDir = TestStoreDir
	return b
}

// ParseRunnableBootstrap is [ParseBootstrap] over [Runnable] of the document.
func ParseRunnableBootstrap(data []byte, r *Resolver) (*Bootstrap, error) {
	return ParseBootstrap([]byte(Runnable(string(data))), r)
}

// testTokenValue is a Tier A token value above the entropy floor.
const testTokenValue = "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"

// TestServedAPIYAML is what a node SERVING the API must state beside its port
// ([Bootstrap.ServesAPI]) — an external URL, the grant ceiling and a token —
// as lines under an `api:` key, for a document whose subject is something else
// to carry. The https address has its front end named, so it raises no
// warning either.
const TestServedAPIYAML = "  external_url: https://crewlet.example.com\n" +
	"  trusted_proxies: [10.0.0.0/8]\n" +
	"  auth:\n    max_grants: [config:write]\n    tokens:\n" +
	"      - {id: founder, token: \"" + testTokenValue + "\", grants: [config:write]}\n"

// Served is a Tier A document whose `api:` block carries [TestServedAPIYAML],
// for a case about a node that serves the API and about something else. A
// document with no `api:` key gains one.
func Served(doc string) string {
	if doc != "" && !strings.HasSuffix(doc, "\n") {
		doc += "\n"
	}
	switch {
	case strings.HasPrefix(doc, "api:\n"):
		return "api:\n" + TestServedAPIYAML + strings.TrimPrefix(doc, "api:\n")
	case strings.Contains(doc, "\napi:\n"):
		return strings.Replace(doc, "\napi:\n", "\napi:\n"+TestServedAPIYAML, 1)
	}
	return doc + "api:\n" + TestServedAPIYAML
}

// ServeAPI gives b what a node serving the API must state beside its port, the
// values [TestServedAPIYAML] writes.
func ServeAPI(b *Bootstrap) {
	b.API.ExternalURL = "https://crewlet.example.com"
	b.API.TrustedProxies = []string{"10.0.0.0/8"}
	b.API.Auth.MaxGrants = []iam.Grant{iam.GrantConfigWrite}
	b.API.Auth.Tokens = []APIToken{{
		ID: "founder", Token: testTokenValue, Grants: []iam.Grant{iam.GrantConfigWrite},
	}}
}
