package secrets

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// KEY MATERIAL IS DECODED ONE WAY, and what is not a key says so without
// saying what it was.
//
// The Tier A loader and `crewlet validate` both read material through this, so
// a key one of them accepts is a key the other accepts too. A newline around
// the material is a paste, not a different key.
func TestKeyMaterialIsDecodedOneWay(t *testing.T) {
	t.Parallel()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeKey("  " + EncodeKey(key) + "\n")
	if err != nil || !bytes.Equal(decoded, key) {
		t.Fatalf("DecodeKey(EncodeKey(key)) = (%x, %v), want the key", decoded, err)
	}
	for name, material := range map[string]string{
		"not base64":       "not-base64-at-all",
		"three bytes":      "AAAA",
		"a corrupted tail": EncodeKey(key) + "!!!",
		"empty":            "",
	} {
		got, err := DecodeKey(material)
		if !errors.Is(err, ErrKeyMaterial) || got != nil {
			t.Errorf("%s: DecodeKey = (%x, %v), want ErrKeyMaterial", name, got, err)
			continue
		}
		if material != "" && strings.Contains(err.Error(), material) {
			t.Errorf("%s: the refusal carries the material: %v", name, err)
		}
	}
}
