package envref

import (
	"os"
	"slices"
	"testing"
)

// TestGrammarRejectsShellExpansions is the security case. Two real bugs came
// from looser patterns: a redaction regex that treated a literal secret
// containing "${line#host=}" as an inert pointer and displayed it unmasked,
// and a provisioning regex that accepted "${1}" and would mint a live
// credential into a variable nothing reads.
func TestGrammarRejectsShellExpansions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		has   bool
		why   string
	}{
		{"${TOKEN}", true, "a plain reference"},
		{"${A_B9}", true, "letters, digits and underscores after the first char"},
		{"Bearer ${TOKEN}", true, "embedded in a larger string"},
		{"${1}", false, "a positional parameter is not a variable name"},
		{"${line#host=}", false, "shell parameter expansion, not a reference"},
		{"${A:-default}", false, "shell default expansion"},
		{"$TOKEN", false, "unbraced is not the grammar"},
		{"${}", false, "empty name"},
		{"${9LIVES}", false, "a name may not start with a digit"},
		{"${a-b}", false, "hyphens are not name characters"},
		{"plain text", false, "no reference at all"},
	} {
		if got := Has(tc.value); got != tc.has {
			t.Errorf("Has(%q) = %v, want %v — %s", tc.value, got, tc.has, tc.why)
		}
	}
}

// TestWholeIsTheCaptureContract pins what a provisioner may mint into: a
// slot that is exactly one reference, so a single variable owns the value.
func TestWholeIsTheCaptureContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		name  string
		ok    bool
	}{
		{"${TOKEN}", "TOKEN", true},
		{"  ${TOKEN}  ", "TOKEN", true},
		{"Bearer ${TOKEN}", "", false},
		{"${A}${B}", "", false},
		{"${TOKEN}x", "", false},
		{"literal-secret", "", false},
		{"", "", false},
	} {
		name, ok := Whole(tc.value)
		if name != tc.name || ok != tc.ok {
			t.Errorf("Whole(%q) = (%q, %v), want (%q, %v)", tc.value, name, ok, tc.name, tc.ok)
		}
	}
}

func TestNamesDeduplicatesInOrder(t *testing.T) {
	t.Parallel()
	got := Names("${B} then ${A} then ${B} again")
	if want := []string{"B", "A", "B"}; slices.Equal(got, want) {
		t.Errorf("Names kept a duplicate: %v", got)
	}
	if want := []string{"B", "A"}; !slices.Equal(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}
	if got := Names("no refs here"); got != nil {
		t.Errorf("Names on a literal = %v, want nil", got)
	}
}

// TestExpandReportsUnresolvedByName is why Expand returns a list rather than
// an error: "Bearer ${TOKEN}" with TOKEN unset resolves to "Bearer ", which
// is truthy-but-broken. A caller that only checked for emptiness would hand
// that to an API and get a 401 hours later.
func TestExpandReportsUnresolvedByName(t *testing.T) {
	t.Parallel()
	env := map[string]string{"KNOWN": "value"}
	lookup := func(n string) (string, bool) { v, ok := env[n]; return v, ok }

	got, unresolved := Expand("Bearer ${MISSING}", lookup)
	if got != "Bearer " {
		t.Errorf("Expand = %q, want %q", got, "Bearer ")
	}
	if !slices.Equal(unresolved, []string{"MISSING"}) {
		t.Errorf("unresolved = %v, want [MISSING]", unresolved)
	}

	got, unresolved = Expand("${KNOWN}/${MISSING}", lookup)
	if got != "value/" {
		t.Errorf("Expand = %q, want %q", got, "value/")
	}
	if !slices.Equal(unresolved, []string{"MISSING"}) {
		t.Errorf("unresolved = %v, want [MISSING]", unresolved)
	}
}

// TestExpandLeavesShellContentAlone guards the sandbox setup step: an
// operator's helper script is config-authored content full of shell syntax,
// and it must survive resolution untouched.
func TestExpandLeavesShellContentAlone(t *testing.T) {
	t.Parallel()
	script := `while read -r line; do echo "${line#host=}"; done < "${1:-/dev/stdin}"`
	got, unresolved := Expand(script, func(string) (string, bool) {
		t.Error("lookup called for shell syntax that is not a reference")
		return "", false
	})
	if got != script {
		t.Errorf("Expand mangled shell content:\n got %q\nwant %q", got, script)
	}
	if unresolved != nil {
		t.Errorf("unresolved = %v, want nil", unresolved)
	}
}

// Not parallel: t.Setenv mutates process state.
func TestExpandFromEnviron(t *testing.T) {
	t.Setenv("CREWLET_ENVREF_TEST", "resolved")
	got, unresolved := Expand("${CREWLET_ENVREF_TEST}", func(n string) (string, bool) {
		return os.LookupEnv(n)
	})
	if got != "resolved" || unresolved != nil {
		t.Errorf("Expand = (%q, %v), want (resolved, nil)", got, unresolved)
	}
}

// AN UNSET REFERENCE RESOLVES TO EMPTY, never to its own literal text.
//
// This is the rule the two chat transports each wrote out by hand: a raw
// "${SLACK_BOT_TOKEN_CEO}" matches nothing any vendor accepts, so passing it
// through turns a missing variable into a seat that authenticates as nobody
// — diagnosed far from the config that named it.
func TestResolveYieldsEmptyForAnUnsetReference(t *testing.T) {
	t.Parallel()
	got := Resolve("${MISSING_TOKEN}", func(string) (string, bool) {
		return "", false
	})
	if got != "" {
		t.Errorf("Resolve = %q, want empty rather than the reference itself", got)
	}
}

// A LITERAL IS RETURNED AS IT IS, trimmed. Most config values are literals
// and must not be looked up.
func TestResolveLeavesALiteralAlone(t *testing.T) {
	t.Parallel()
	looked := false
	got := Resolve("  xoxb-a-real-token  ", func(string) (string, bool) {
		looked = true
		return "substituted", true
	})
	if got != "xoxb-a-real-token" {
		t.Errorf("Resolve = %q, want the trimmed literal", got)
	}
	if looked {
		t.Error("a literal was looked up in the environment")
	}
}

// A WHOLE REFERENCE RESOLVES, and its value is trimmed too: an environment
// variable set from a file routinely carries a trailing newline, and a token
// with one is a token every vendor rejects.
func TestResolveSubstitutesAndTrims(t *testing.T) {
	t.Parallel()
	got := Resolve("${TOKEN}", func(name string) (string, bool) {
		if name != "TOKEN" {
			return "", false
		}
		return "  xoxb-value\n", true
	})
	if got != "xoxb-value" {
		t.Errorf("Resolve = %q, want the trimmed value", got)
	}
}

// A PARTIAL reference is a literal, not a lookup: the capture contract is
// whole-value only, so a URL with a variable embedded in it has no single
// variable that owns the value.
func TestResolveIgnoresAPartialReference(t *testing.T) {
	t.Parallel()
	const embedded = "https://example.com/${PATH_PART}/hook"
	got := Resolve(embedded, func(string) (string, bool) {
		return "substituted", true
	})
	if got != embedded {
		t.Errorf("Resolve = %q, want the value untouched", got)
	}
}
