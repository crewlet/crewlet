package config_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A REMOTE'S CREDENTIAL IS ONE code.remote_credentials DECLARES FOR ITS HOST,
// in a declared repository as in a person's request (internal/code's decide)
// and at the dial (internal/engine's mirrorDial), all three through
// [config.CodeRemoteCredentials.Refusal]: a variable the table does not
// declare, or a declared one toward another host, another port or a name
// beginning with the declared host, is refused at the credential as one conflict
// naming the table and the host its value would go to — never the credential
// itself. A declared variable toward its host, on the port declared, is
// accepted, and so is an anonymous remote, which sends nothing.
//
// Mutation: drop the remotes.Refusal arm from validateCodeRemote — every
// refusal below is accepted, red. Compare hosts without their port in
// Refusal — "another port" is accepted, red. Match a host by its prefix — "a name
// beginning with its host" is accepted, red.
func TestARemoteCredentialIsOneDeclaredForItsHost(t *testing.T) {
	t.Parallel()
	table := "  remote_credentials:\n" +
		"    FORGE_TOKEN: {hosts: [forge.example.com]}\n" +
		"    SELF_TOKEN: {hosts: ['git.example.com:8443']}\n"
	doc := func(remote string) string {
		return codeChart + "code:\n  backend: gitlet\n" + table + "  repos:\n" +
			"    - name: web\n      owner: {unit: Engineering}\n      protect: [{branch: main}]\n" + remote
	}
	for name, tc := range map[string]struct {
		remote, path string
		says         []string
	}{
		"an undeclared variable": {
			remote: "      import: {url: 'https://forge.example.com/a/web.git', credential: '${AWS_SECRET_ACCESS_KEY}'}\n",
			path:   "code.repos[0].import.credential",
			says:   []string{"code.remote_credentials", "forge.example.com", "FORGE_TOKEN"},
		},
		"a declared variable toward another host": {
			remote: "      mirror_from: {url: 'https://attacker.example.com/x.git', credential: '${FORGE_TOKEN}'}\n",
			path:   "code.repos[0].mirror_from.credential",
			says:   []string{"attacker.example.com", "declares no variable for it"},
		},
		"a declared variable toward a name beginning with its host": {
			remote: "      mirror_from: {url: 'https://forge.example.com.attacker.example/x.git', " +
				"credential: '${FORGE_TOKEN}'}\n",
			path: "code.repos[0].mirror_from.credential",
			says: []string{"forge.example.com.attacker.example"},
		},
		"a declared variable toward another port": {
			remote: "      mirror_to: {url: 'https://git.example.com/web.git', credential: '${SELF_TOKEN}', as: swe}\n",
			path:   "code.repos[0].mirror_to.credential",
			says:   []string{"git.example.com", "declares no variable for it"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseCompany([]byte(doc(tc.remote)))
			problems := config.Problems(err)
			if len(problems) != 1 {
				t.Fatalf("problems = %+v, want the one at %s", problems, tc.path)
			}
			p := problems[0]
			if p.Path != tc.path || p.Kind != "conflict" {
				t.Errorf("refused at %s (%s), want %s (conflict): %s", p.Path, p.Kind, tc.path, p.Message)
			}
			for _, want := range tc.says {
				if !strings.Contains(p.Message, want) {
					t.Errorf("message = %q, want it to name %q", p.Message, want)
				}
			}
			if strings.Contains(p.Message, "${") {
				t.Errorf("message = %q quotes the credential", p.Message)
			}
		})
	}
	for name, remote := range map[string]string{
		"a declared variable toward its host": "      import: {url: 'https://forge.example.com/a/web.git', " +
			"credential: '${FORGE_TOKEN}'}\n",
		"a declared variable toward its host's declared port": "      mirror_to: {url: " +
			"'https://git.example.com:8443/web.git', credential: '${SELF_TOKEN}', as: swe}\n",
		"an anonymous remote anywhere": "      mirror_from: {url: 'https://elsewhere.example.org/lib.git'}\n",
	} {
		if _, err := config.ParseCompany([]byte(doc(remote))); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

// code.remote_credentials REFUSES BY FIELD, AS ONE PROBLEM EACH: a variable
// declared for no host, a host written as a URL, with a user or with a path, the same host
// twice, a name no ${NAME} can spell, and a table on a company that hosts no
// code — each a declaration that could never match, or that nothing reads.
//
// Mutation: drop the empty-hosts arm, the host's shape arm, the duplicate arm
// or the name arm of CodeRemoteCredentials.validate, or the remote_credentials
// arm of Code.validate's not-native list — its case is accepted, red.
func TestTheRemoteCredentialsAreRefusedByField(t *testing.T) {
	t.Parallel()
	native := func(table string) string {
		return codeChart + "code:\n  backend: gitlet\n  remote_credentials:\n" + table
	}
	for name, tc := range map[string]struct {
		doc, path, kind, says string
	}{
		"a variable sent nowhere": {
			doc: native("    FORGE_TOKEN: {hosts: []}\n"), path: "code.remote_credentials.FORGE_TOKEN.hosts",
			kind: "missing", says: "sent nowhere",
		},
		"a host written as a URL": {
			doc:  native("    FORGE_TOKEN: {hosts: ['https://forge.example.com']}\n"),
			path: "code.remote_credentials.FORGE_TOKEN.hosts[0]", kind: "shape", says: "no scheme",
		},
		"a host with a user": {
			doc:  native("    FORGE_TOKEN: {hosts: ['bot@forge.example.com']}\n"),
			path: "code.remote_credentials.FORGE_TOKEN.hosts[0]", kind: "shape", says: "no scheme, user or path",
		},
		// A PATH IS NO HOST: read as a URL's, it would parse and match its
		// host alone, so the writer of forge.example.com/acme would have
		// declared every repository of the forge.
		"a host with a path": {
			doc:  native("    FORGE_TOKEN: {hosts: ['forge.example.com/acme']}\n"),
			path: "code.remote_credentials.FORGE_TOKEN.hosts[0]", kind: "shape", says: "no scheme, user or path",
		},
		"one host twice, once with https' port": {
			doc:  native("    FORGE_TOKEN: {hosts: [forge.example.com, 'FORGE.example.com:443']}\n"),
			path: "code.remote_credentials.FORGE_TOKEN.hosts[1]", kind: "conflict", says: "hosts[0] too",
		},
		"a name no reference spells": {
			doc:  native("    1FORGE: {hosts: [forge.example.com]}\n"),
			path: "code.remote_credentials.1FORGE", kind: "shape", says: "a letter or an underscore",
		},
		"a table with nothing hosting code": {
			doc:  codeChart + "code:\n  remote_credentials:\n    FORGE_TOKEN: {hosts: [forge.example.com]}\n",
			path: "code.remote_credentials", kind: "conflict", says: "code.backend: gitlet",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := config.ParseCompany([]byte(tc.doc))
			problems := config.Problems(err)
			if len(problems) != 1 {
				t.Fatalf("problems = %+v, want the one at %s", problems, tc.path)
			}
			if p := problems[0]; p.Path != tc.path || p.Kind != tc.kind || !strings.Contains(p.Message, tc.says) {
				t.Errorf("refused at %s (%s) %q, want %s (%s) naming %q", p.Path, p.Kind, p.Message, tc.path,
					tc.kind, tc.says)
			}
		})
	}
}

// code.remote_credentials' BOUNDS ARE ADMISSION RULES: a document declaring
// more than CodeMaxRemoteCredentials variables, or a variable with more than
// CodeMaxRemoteCredentialHosts hosts, is refused at the field — and a revision
// being APPLIED that breaks either still runs, since a decide and a dial read
// a declared credential in a table of any size.
//
// Mutation: move either check into CodeRemoteCredentials.validate and
// ValidateRunnable refuses the stored revision, red; drop either and
// ValidateAdmission answers one problem, red.
func TestTheRemoteCredentialsBoundsAreAdmissionRules(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString(codeChart + "code:\n  backend: gitlet\n  remote_credentials:\n")
	hosts := make([]string, config.CodeMaxRemoteCredentialHosts+1)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("h%d.example.com", i)
	}
	fmt.Fprintf(&b, "    WIDE: {hosts: [%s]}\n", strings.Join(hosts, ", "))
	for i := range config.CodeMaxRemoteCredentials {
		fmt.Fprintf(&b, "    TOKEN_%d: {hosts: [forge.example.com]}\n", i)
	}
	c, err := config.ParseCompanyDocument([]byte(b.String()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := c.ValidateRunnable(); err != nil {
		t.Fatalf("ValidateRunnable() = %v: an applied revision past a policy bound still runs", err)
	}
	want := map[string]string{
		"code.remote_credentials":            fmt.Sprint(config.CodeMaxRemoteCredentials),
		"code.remote_credentials.WIDE.hosts": fmt.Sprint(config.CodeMaxRemoteCredentialHosts),
	}
	problems := config.Problems(c.ValidateAdmission())
	if len(problems) != len(want) {
		t.Fatalf("ValidateAdmission() = %+v, want one problem each at %v", problems, want)
	}
	for _, p := range problems {
		if bound, ok := want[p.Path]; !ok || p.Kind != "out_of_range" || !strings.Contains(p.Message, bound) {
			t.Errorf("%s (%s) %q, want out_of_range at one of %v naming its bound", p.Path, p.Kind, p.Message, want)
		}
	}
}
