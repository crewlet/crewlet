package executor

import (
	"encoding/json"
	"testing"
)

// OPENCODE'S POLICIES ARE READ AS OPENCODE 2.3.2 READS THEM, row by row as
// measured (a home, a repository's or a config directory's `policies` denying
// the plugin by `*`, its path, a glob or `?`, or `integration.use` on
// `plugin:<path>`, stops it; another path, another case, a prefix and a later
// allow in the same document do not; an earlier document's rule beats a later
// one's both ways). Mutations, each red: read the first matching rule; keep
// the documents' order; skip the integration.use mapping; match case-blind;
// match a prefix.
func TestOpenCodesPoliciesAreReadAsOpenCodeReadsThem(t *testing.T) {
	t.Parallel()
	const p = "/box/.crewlet/opencode-steer"
	doc := func(path, policies string) string {
		return `{"type":"document","path":"` + path + `","info":{"policies":` + policies + `}}`
	}
	rule := func(action, resource, effect string) string {
		return `{"action":"` + action + `","resource":"` + resource + `","effect":"` + effect + `"}`
	}
	const home, repo = "/box/.config/opencode/opencode.json", "/box/workspace/.opencode/opencode.json"
	for name, tc := range map[string]struct {
		docs   string
		denied bool
	}{
		"nothing":                           {`[]`, false},
		"h-pol-all":                         {`[` + doc(home, `[`+rule("plugin.use", "*", "deny")+`]`) + `]`, true},
		"h-pol-path":                        {`[` + doc(home, `[`+rule("plugin.use", p, "deny")+`]`) + `]`, true},
		"h-pol-int":                         {`[` + doc(home, `[`+rule("integration.use", "plugin:"+p, "deny")+`]`) + `]`, true},
		"h-pol-other":                       {`[` + doc(home, `[`+rule("plugin.use", "/box/elsewhere", "deny")+`]`) + `]`, false},
		"h-pol-then-allow":                  {`[` + doc(home, `[`+rule("plugin.use", "*", "deny")+`,`+rule("plugin.use", "*", "allow")+`]`) + `]`, false},
		"h-pol-glob":                        {`[` + doc(home, `[`+rule("plugin.use", "/box/.crewlet/*", "deny")+`]`) + `]`, true},
		"h-pol-q":                           {`[` + doc(home, `[`+rule("plugin.use", "/box/.crewlet/opencode-stee?", "deny")+`]`) + `]`, true},
		"h-pol-case":                        {`[` + doc(home, `[`+rule("plugin.use", "/BOX/.CREWLET/OPENCODE-STEER", "deny")+`]`) + `]`, false},
		"h-pol-prefix":                      {`[` + doc(home, `[`+rule("plugin.use", "/box/.crewlet/opencode", "deny")+`]`) + `]`, false},
		"h-pol-star-mid":                    {`[` + doc(home, `[`+rule("plugin.use", "/box/*/opencode-steer", "deny")+`]`) + `]`, true},
		"hd-wa (home deny, repo allow)":     {`[` + doc(home, `[`+rule("plugin.use", "*", "deny")+`]`) + `,` + doc(repo, `[`+rule("plugin.use", "*", "allow")+`]`) + `]`, true},
		"ha-wd (home allow, repo deny)":     {`[` + doc(home, `[`+rule("plugin.use", "*", "allow")+`]`) + `,` + doc(repo, `[`+rule("plugin.use", "*", "deny")+`]`) + `]`, false},
		"a directory entry decides nothing": {`[{"type":"directory","path":"/box/.config/opencode"}]`, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var docs []openCodePolicyDoc
			if err := json.Unmarshal([]byte(tc.docs), &docs); err != nil {
				t.Fatal(err)
			}
			if _, denied := openCodePolicyDenies(docs, "plugin.use", p); denied != tc.denied {
				t.Errorf("denied %t, want %t", denied, tc.denied)
			}
		})
	}
}
