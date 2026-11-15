package configapi

import "testing"

// TWO DOCUMENTS ARE THE SAME WHEN THEY SAY THE SAME THING, and only then: key
// order and spacing are no change, while a field only a newer build knows and
// an integer past float64's reach are. A write judged unchanged stores
// nothing ([writeUnchanged]), so the wrong answer in that direction loses the
// write.
func TestSameDocumentComparesWhatTheDocumentSays(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		a, b string
		same bool
	}{
		{"key order and spacing", `{"name":"Acme","mission":"x"}`, `{ "mission": "x", "name": "Acme" }`, true},
		{"a value moved", `{"name":"Acme"}`, `{"name":"Acme Inc"}`, false},
		{"a field a newer build wrote", `{"name":"Acme"}`, `{"name":"Acme","from_a_newer_build":1}`, false},
		{"integers past 2^53", `{"n":9007199254740993}`, `{"n":9007199254740992}`, false},
		{"a list reordered", `{"l":[1,2]}`, `{"l":[2,1]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			same, err := sameDocument([]byte(tc.a), []byte(tc.b))
			if err != nil {
				t.Fatal(err)
			}
			if same != tc.same {
				t.Errorf("sameDocument(%s, %s) = %v, want %v", tc.a, tc.b, same, tc.same)
			}
		})
	}
}
