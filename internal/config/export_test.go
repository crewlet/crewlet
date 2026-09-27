package config

import "strings"

// TestKeyMaterial is thirty-two zero bytes in base64: a key the keyring
// accepts, sealing nothing any case here reads back.
const TestKeyMaterial = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// TestKeyringYAML is the `secrets:` block a Tier A document in these suites
// carries, because every node requires one ([Bootstrap.validateTopology]).
const TestKeyringYAML = "secrets:\n  active_key_id: k1\n  keys:\n" +
	"    - {id: k1, material: \"" + TestKeyMaterial + "\"}\n"

// Keyed is a Tier A document with [TestKeyringYAML] appended, so that a case
// states what it is about and nothing else — a document that omitted the
// keyring would be refused for that before it was ever judged on its subject.
//
// A document that states its own `secrets:` block is the case whose subject
// IS the keyring, and it is returned as written.
func Keyed(doc string) string {
	if strings.HasPrefix(doc, "secrets:") || strings.Contains(doc, "\nsecrets:") {
		return doc
	}
	// THE EMPTY MAPPING WRITTEN IN FLOW STYLE, which is what an operator's
	// "empty" file looks like: a block key after it is not YAML at all.
	if strings.TrimSpace(doc) == "{}" {
		doc = ""
	}
	if doc != "" && !strings.HasSuffix(doc, "\n") {
		doc += "\n"
	}
	return doc + TestKeyringYAML
}

// KeyedBootstrap is [DefaultBootstrap] carrying the keyring every node needs.
func KeyedBootstrap() Bootstrap {
	b := DefaultBootstrap()
	b.Secrets = Secrets{
		ActiveKeyID: "k1",
		Keys:        []SecretKey{{ID: "k1", Material: TestKeyMaterial}},
	}
	return b
}

// ParseKeyedBootstrap is [ParseBootstrap] over [Keyed] of the document.
func ParseKeyedBootstrap(data []byte, r *Resolver) (*Bootstrap, error) {
	return ParseBootstrap([]byte(Keyed(string(data))), r)
}
