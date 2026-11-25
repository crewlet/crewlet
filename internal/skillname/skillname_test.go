package skillname

import (
	"strings"
	"testing"
)

// GROK BUILD 1.0.92'S NAME IS THE MEASURED ONE, case by case (each named by its
// measurement's row, nn, nn2, x1 or x7, and its input): letters lowered,
// every run of anything else one '-', trimmed, non-ASCII a separator, the cap
// on the result; a failing name falls back to the directory's, and a failing
// directory lists nothing. Mutations, each red: keep non-ASCII characters;
// trim no leading '-'; cap the raw name; skip the fallback.
func TestGrokNamesASkillAsGrokBuildListsIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		name  string
		named bool
		dir   string
		want  string
		ok    bool
	}{
		"nn UPPER":             {"UPPER", true, "t00", "upper", true},
		"nn MiXeD":             {"MiXeD", true, "t01", "mixed", true},
		"nn Ünïcode":           {"Ünïcode", true, "t02", "n-code", true},
		"nn 日本":                {"日本", true, "t03", "t03", true},
		"nn a:b":               {"a:b", true, "t", "a-b", true},
		"nn a_b":               {"a_b", true, "t", "a-b", true},
		"nn a b":               {"a b", true, "t", "a-b", true},
		"nn a.b":               {"a.b", true, "t", "a-b", true},
		"nn a/b":               {"a/b", true, "t", "a-b", true},
		"nn a@b":               {"a@b", true, "t", "a-b", true},
		"nn a+b":               {"a+b", true, "t", "a-b", true},
		"nn a=b":               {"a=b", true, "t", "a-b", true},
		"nn a,b":               {"a,b", true, "t", "a-b", true},
		"nn a'b":               {"a'b", true, "t", "a-b", true},
		"nn a~b":               {"a~b", true, "t", "a-b", true},
		"nn a-b":               {"a-b", true, "t", "a-b", true},
		"nn a(b)":              {"a(b)", true, "t", "a-b", true},
		"nn a::b":              {"a::b", true, "t", "a-b", true},
		"nn a_:_b":             {"a_:_b", true, "t", "a-b", true},
		"nn a  b":              {"a  b", true, "t", "a-b", true},
		"nn a--b":              {"a--b", true, "t", "a-b", true},
		"nn -a":                {"-a", true, "t", "a", true},
		"nn a-":                {"a-", true, "t", "a", true},
		"nn :a":                {":a", true, "t", "a", true},
		"nn a:":                {"a:", true, "t", "a", true},
		"nn spaces around a":   {"  a  ", true, "t", "a", true},
		"nn 1a":                {"1a", true, "t", "1a", true},
		"nn a1":                {"a1", true, "t", "a1", true},
		"nn empty name":        {"", true, "t09", "t09", true},
		"nn colons":            {":::", true, "t10", "t10", true},
		"nn2 a":                {"a", true, "t", "a", true},
		"nn2 Z":                {"Z", true, "t", "z", true},
		"nn2 7":                {"7", true, "t", "7", true},
		"nn2 b_":               {"b_", true, "t", "b", true},
		"nn2 123":              {"123", true, "t", "123", true},
		"nn2 dir Dir_X:y":      {"", true, "Dir_X:y", "dir-x-y", true},
		"nn2 dir ÜDir":         {"", true, "ÜDir", "dir", true},
		"nn2 dir too long":     {"", true, strings.Repeat("g", 65), "", false},
		"nn cap b31::b32":      {strings.Repeat("b", 31) + "::" + strings.Repeat("b", 32), true, "t", strings.Repeat("b", 31) + "-" + strings.Repeat("b", 32), true},
		"nn cap space c64":     {" " + strings.Repeat("c", 64), true, "t", strings.Repeat("c", 64), true},
		"nn cap d64 colon":     {strings.Repeat("d", 64) + ":", true, "t", strings.Repeat("d", 64), true},
		"nn cap e64 é":         {strings.Repeat("e", 64) + "é", true, "t", strings.Repeat("e", 64), true},
		"nn cap é a63":         {"é" + strings.Repeat("a", 63), true, "t", strings.Repeat("a", 63), true},
		"nn cap f65":           {strings.Repeat("f", 65), true, "t37", "t37", true},
		"nn cap é64":           {strings.Repeat("é", 64), true, "t", "t", true},
		"nn cap 日22":           {strings.Repeat("日", 22), true, "t", "t", true},
		"nn cap dir h64 colon": {"", false, strings.Repeat("h", 64) + ":", strings.Repeat("h", 64), true},
		"x7 no name key":       {"x", false, "late:qux", "late-qux", true},
		"x1 copy":              {"late:foo", true, "late:foo", "late-foo", true},
		"x1 home":              {"late-foo", true, "late-foo", "late-foo", true},
		"x7 copy":              {"late:bar", true, "late:bar", "late-bar", true},
		"x7 home":              {"late-bar", true, "late-bar-x", "late-bar", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, ok := GrokListed(tc.name, tc.named, tc.dir); got != tc.want || ok != tc.ok {
				t.Errorf("GrokListed(%q, %t, %q) = %q, %t; want %q, %t", tc.name, tc.named, tc.dir, got, ok,
					tc.want, tc.ok)
			}
		})
	}
}
