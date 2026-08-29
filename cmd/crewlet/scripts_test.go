package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// THE BOOTSTRAP SCRIPTS DRIVE THIS BINARY, and nothing else checks that.
//
// The scripts in scripts/ stand up the local vendor loops and then print,
// and run, `crewlet <vendor> <sub>` commands. A flag one of them names that
// this CLI does not define fails at the moment an operator runs it — after
// a Mattermost, a Plane or a GitLab has already been provisioned, which is
// minutes in — and the scripts are shell, so no compiler and no linter has
// an opinion.
//
// That is not hypothetical: both the Plane and GitLab loops shipped
// `--webhook-url <full endpoint>`, which this CLI answers with "flag
// provided but not defined". It takes `-public-url <base>` and derives the
// path itself, because the engine owns its seven webhook routes and an
// operator typing one can get it wrong.

// scriptInvocation finds `crewlet <vendor> <sub>` and everything up to the
// end of its command line, INCLUDING backslash continuations — which is
// where the flags actually are. The continuation branch is written first
// because alternation here is leftmost-first: with `[^\n]` first, the
// backslash is consumed as an ordinary character, the newline then ends the
// match, and every flag on a continued line goes unread. That is not a
// hypothetical either — it made the first version of this test pass
// vacuously on the two invocations it was written for.
var scriptInvocation = regexp.MustCompile(
	`crewlet (gitlab|plane|mattermost) (provision|import|resync|doctor)((?:\\\n|[^\n])*)`)

// envPrefixedInvocation is the same call with the `NAME=value` assignments a
// script puts in front of it — which is how every one of them passes the
// operator credential.
// The continuation allows MORE THAN ONE backslash: inside a heredoc — which
// is where every one of these loops prints its "run this next" instructions
// — a line continuation is written `\\`, and a pattern accepting only one
// stops matching exactly where the operator-facing copy lives.
var envPrefixedInvocation = regexp.MustCompile(
	`((?:[A-Z][A-Z0-9_]*=(?:"[^"\n]*"|[^\s\\]*)[ \t]*(?:\\+\n\s*)?)+)crewlet (gitlab|plane|mattermost) (provision|import|resync|doctor)`)

// envAssignmentName is one such assignment's variable.
var envAssignmentName = regexp.MustCompile(`([A-Z][A-Z0-9_]*)=`)

// scriptFlag is a flag token as a script writes one.
var scriptFlag = regexp.MustCompile(`(^|\s)--?([a-z][a-z0-9-]*)`)

func TestTheBootstrapScriptsNameOnlyFlagsThisCLIDefines(t *testing.T) {
	t.Parallel()
	for _, name := range bootstrapScripts(t) {
		body := readRepoFile(t, filepath.Join("scripts", name))
		for _, call := range scriptInvocation.FindAllStringSubmatch(body, -1) {
			vendor, sub, tail := call[1], call[2], call[3]
			defined := definedFlags(t, vendor, sub)
			for _, tok := range scriptFlag.FindAllStringSubmatch(tail, -1) {
				flagName := tok[2]
				if _, ok := defined[flagName]; !ok {
					t.Errorf("%s runs `crewlet %s %s` with -%s, which this "+
						"CLI does not define (it has: %s)",
						name, vendor, sub, flagName, strings.Join(sorted(defined), " "))
				}
			}
		}
	}
}

// THE SCRIPTS DO NOT HAND OVER A WEBHOOK PATH.
//
// A separate assertion from the one above because the failure is different
// in kind: `-public-url https://host/webhooks/plane` parses perfectly and
// registers a hook at `https://host/webhooks/plane/webhooks/plane`, which
// nothing serves and nothing reports. The vendor's own settings page then
// shows a healthy webhook that delivers nowhere.
func TestTheScriptsPassABaseURLNotAnEndpoint(t *testing.T) {
	t.Parallel()
	for _, name := range bootstrapScripts(t) {
		body := readRepoFile(t, filepath.Join("scripts", name))
		for _, call := range scriptInvocation.FindAllStringSubmatch(body, -1) {
			tail := call[3]
			idx := strings.Index(tail, "-public-url")
			if idx < 0 {
				continue
			}
			rest := strings.Fields(strings.NewReplacer("\\", " ", "\n", " ").
				Replace(tail[idx+len("-public-url"):]))
			if len(rest) == 0 {
				continue
			}
			// Resolved one level through the script's own assignments,
			// because the slip that shipped was not a literal path — it
			// was ${WEBHOOK_URL}, a variable the script sets to the
			// endpoint two dozen lines above.
			arg := expandOnce(rest[0], shellAssignments(body))
			if strings.Contains(arg, "/webhooks/") {
				t.Errorf("%s passes %s to -public-url, which resolves to %q; "+
					"that flag takes the engine's BASE address and derives "+
					"the path itself", name, rest[0], arg)
			}
		}
	}
}

// shellAssignment is a plain `NAME="value"` line.
var shellAssignment = regexp.MustCompile(`(?m)^([A-Z_][A-Z0-9_]*)="([^"\n]*)"`)

// shellVar is a ${NAME}, ${NAME%...} or $NAME reference.
var shellVar = regexp.MustCompile(`\$\{?([A-Z_][A-Z0-9_]*)[^}\s]*\}?`)

// shellAssignments reads a script's top-level string assignments. Last one
// wins, which is what a shell would do.
func shellAssignments(body string) map[string]string {
	out := map[string]string{}
	for _, m := range shellAssignment.FindAllStringSubmatch(body, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// expandOnce substitutes one level of variable reference.
//
// One level, not a shell: the question here is only whether a value the
// script builds ends in a webhook path, and that path is written literally
// wherever it is built. A real expander would be a shell interpreter, and
// this test would then be testing it.
func expandOnce(arg string, vars map[string]string) string {
	return shellVar.ReplaceAllStringFunc(arg, func(ref string) string {
		m := shellVar.FindStringSubmatch(ref)
		if value, ok := vars[m[1]]; ok {
			return value
		}
		return ref
	})
}

// THE SCRIPTS PASS THE CREDENTIAL UNDER THE NAME THE CLI READS.
//
// The operator credential does not arrive as a flag — every loop exports it
// in front of the command — so the flag check above cannot see it, and the
// failure is worse than an unknown flag: an unread variable is not an error.
// The command runs, finds nothing, and stops with "no administrator token",
// which reads as a missing credential rather than a misspelt one.
//
// Both the Plane and GitLab loops shipped `*_PROVISION_TOKEN`, which no
// command here reads.
func TestTheScriptsExportTheCredentialUnderTheNameTheCLIReads(t *testing.T) {
	t.Parallel()
	for _, name := range bootstrapScripts(t) {
		body := readRepoFile(t, filepath.Join("scripts", name))
		for _, call := range envPrefixedInvocation.FindAllStringSubmatch(body, -1) {
			assignments, vendor, sub := call[1], call[2], call[3]
			known := documentedNames(t, vendor, sub)
			for _, m := range envAssignmentName.FindAllStringSubmatch(assignments, -1) {
				varName := m[1]
				// CREDENTIALS ONLY. A loop also exports the instance
				// URL, and that reaches the command through the
				// company config's own ${VAR} resolution rather than
				// being read here — so a usage string has no reason to
				// name it, and requiring one would fail every script
				// for doing the right thing.
				if !strings.HasPrefix(varName, strings.ToUpper(vendor)+"_") ||
					!credentialShaped(varName) {
					continue
				}
				if _, ok := known[varName]; !ok {
					t.Errorf("%s runs `crewlet %s %s` with %s=…, which that "+
						"command never reads — it would stop with \"no "+
						"administrator token\" and look like a missing credential",
						name, vendor, sub, varName)
				}
			}
		}
	}
}

// credentialShaped reports whether a variable carries a secret, by the
// suffix every one of them uses.
func credentialShaped(name string) bool {
	for _, suffix := range []string{"_TOKEN", "_KEY", "_SECRET", "_PASSWORD"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// documentedNames is every ALL-CAPS identifier the command's own usage
// mentions — which is where it says which variables it consults.
func documentedNames(t *testing.T, vendor, sub string) map[string]struct{} {
	t.Helper()
	var usage bytes.Buffer
	_ = runIntegration(vendor, []string{sub, "-h"}, io.Discard, &usage)
	names := map[string]struct{}{}
	for _, m := range allCaps.FindAllString(usage.String(), -1) {
		names[m] = struct{}{}
	}
	if len(names) == 0 {
		t.Fatalf("crewlet %s %s names no environment variable in its usage",
			vendor, sub)
	}
	return names
}

// allCaps matches an environment variable as a usage string writes one.
var allCaps = regexp.MustCompile(`\b[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+\b`)

// bootstrapScripts is every shell script in scripts/.
func bootstrapScripts(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", "scripts"))
	if err != nil {
		t.Fatalf("reading scripts/: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sh") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("no shell scripts found, so this test proves nothing")
	}
	return names
}

// definedFlags asks the command itself what it takes, by running its help
// and reading the usage the flag package prints.
//
// The COMMAND is the source of truth, not a list kept here: a list would be
// a second place to update, and the whole defect this test exists for is
// two places disagreeing about one flag.
func definedFlags(t *testing.T, vendor, sub string) map[string]struct{} {
	t.Helper()
	var usage bytes.Buffer
	// ErrHelp, and the usage text, is the point. Any other error still
	// leaves the usage printed.
	_ = runIntegration(vendor, []string{sub, "-h"}, io.Discard, &usage)

	defined := map[string]struct{}{}
	for _, line := range strings.Split(usage.String(), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "-") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(trimmed, "-"), " ")
		defined[name] = struct{}{}
	}
	if len(defined) == 0 {
		t.Fatalf("crewlet %s %s printed no flags, so this test proves nothing",
			vendor, sub)
	}
	return defined
}

func sorted(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, "-"+k)
	}
	sort.Strings(out)
	return out
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(raw)
}

// --- the local loop's own wiring ------------------------------------------

// A VENDOR THAT POSTS INTO THE ENGINE MUST BE ABLE TO RESOLVE THE HOST.
//
// The bootstrap scripts register webhooks at `host.docker.internal`, which
// Docker Desktop provides natively and a Linux host does not — there it
// needs `extra_hosts: host.docker.internal:host-gateway`, or the name
// resolves to nothing and every delivery fails a lookup.
//
// plane-api and plane-worker carry it. `gitlab` did not, for the length of
// the rewrite, while the compose file's own mattermost comment said "gitlab
// and plane both need it" — the file asserted an invariant it broke. Nothing
// noticed because the only guard was a Python test that named the plane
// services and stopped there.
//
// Mattermost is deliberately absent: it never calls the engine at all — the
// engine opens an outbound websocket per seat, because Mattermost has no
// usable inbound webhook — which is also why that stack works behind NAT
// with no tunnel.
func TestEveryInboundVendorCanResolveTheEngineHost(t *testing.T) {
	t.Parallel()
	compose := readRepoFile(t, "docker-compose.yml")
	const mapping = "host.docker.internal:host-gateway"

	for _, service := range []string{"gitlab", "plane-api", "plane-worker"} {
		block := composeService(t, compose, service)
		if !strings.Contains(block, mapping) {
			t.Errorf("the %s service has no %q, so on a Linux host every "+
				"webhook it sends the engine fails a name lookup",
				service, mapping)
		}
	}

	// And the one that must NOT have it, so the exclusion stays a
	// decision: a mapping here would imply an inbound path Mattermost
	// does not have, and the next reader would go looking for it.
	if strings.Contains(composeService(t, compose, "mattermost"), mapping) {
		t.Error("the mattermost service carries the host mapping; it never " +
			"calls the engine, so that says an inbound path exists")
	}
}

// composeService returns one service's block, from its key to the next
// top-level service key.
//
// A text slice rather than a YAML parse: the compose file carries anchors
// and merge keys that a naive decode flattens, and what is under test is a
// line a person wrote under a specific service.
func composeService(t *testing.T, compose, name string) string {
	t.Helper()
	start := strings.Index(compose, "\n  "+name+":\n")
	if start < 0 {
		t.Fatalf("no %s service in docker-compose.yml", name)
	}
	rest := compose[start+1:]
	for offset := 1; ; {
		next := strings.Index(rest[offset:], "\n  ")
		if next < 0 {
			return rest
		}
		offset += next + 1
		// A sibling key, not a nested one: two spaces then a
		// non-space, then a colon before the end of the line.
		line := rest[offset:]
		if end := strings.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
		}
		trimmed := strings.TrimPrefix(line, "  ")
		if strings.HasPrefix(trimmed, " ") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasSuffix(strings.TrimSpace(trimmed), ":") {
			return rest[:offset]
		}
	}
}
