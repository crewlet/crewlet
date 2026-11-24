package sandbox

import (
	"strconv"
	"strings"
	"testing"
)

// gitConfigOf is the configuration env hands git through the environment, by
// key: each key's values in order.
func gitConfigOf(t *testing.T, env map[string]string) map[string][]string {
	t.Helper()
	n, err := strconv.Atoi(env[gitConfigCount])
	if err != nil {
		t.Fatalf("%s = %q: %v", gitConfigCount, env[gitConfigCount], err)
	}
	out := map[string][]string{}
	for i := range n {
		key := env[gitConfigKey+strconv.Itoa(i)]
		out[key] = append(out[key], env[gitConfigValue+strconv.Itoa(i)])
	}
	return out
}

// A CHECK'S BOX IS GIVEN NO COMMIT IDENTITY: it acts as nobody, so its git gets
// Gitlet's credential and protocol but no user.name or user.email, and a commit
// its command makes fails rather than naming somebody; a coding box keeps its
// seat's; and half an identity is refused.
//
// Mutation: write user.name and user.email whatever the box is given, and the
// check's box names an empty identity, red.
func TestACheckBoxGitNamesNobody(t *testing.T) {
	t.Parallel()
	base := "https://git.example.com/box/git"
	check, err := BoxGit{Base: base, User: "check", Token: "chk-token"}.Env(nil)
	if err != nil {
		t.Fatalf("a box with no identity: %v", err)
	}
	cfg := gitConfigOf(t, check)
	for _, key := range []string{"user.name", "user.email"} {
		if v, ok := cfg[key]; ok {
			t.Errorf("a check's box is configured %s = %q; it acts as nobody", key, v)
		}
	}
	if got := cfg["protocol.version"]; len(got) != 1 || got[0] != "2" {
		t.Errorf("protocol.version = %v, want Gitlet's 2", got)
	}
	if check[GitURLVar] != base {
		t.Errorf("the base is %q, want %q", check[GitURLVar], base)
	}
	for name, value := range check {
		if strings.Contains(value, "chk-token") {
			t.Errorf("the box's variable %s holds its credential, which only its file holds", name)
		}
	}
	seat, err := BoxGit{Base: base, User: "run", Token: "t", Name: "Alice", Email: "alice@example.com"}.Env(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := gitConfigOf(t, seat)["user.email"]; len(got) != 1 || got[0] != "alice@example.com" {
		t.Errorf("a coding box's user.email = %v, want its seat's", got)
	}
	for _, half := range []BoxGit{
		{Base: base, User: "run", Token: "t", Name: "Alice"},
		{Base: base, User: "run", Token: "t", Email: "alice@example.com"},
	} {
		if _, err := half.Env(nil); err == nil {
			t.Errorf("an identity of name %q and email %q was given", half.Name, half.Email)
		}
	}
}

// A BOX'S GITLET CREDENTIAL RIDES GIT_CONFIG_PARAMETERS, AFTER THE RUN'S OWN:
// the helper's reset and the helper, which reads the file the box's
// environment names, are its last two items, quoted as git reads them, the
// run environment's items ahead of them; no count entry names a credential
// helper; and a base holding '=', which an item cannot name, is refused
// naming CREWLET_BOX_GIT_URL.
//
// Mutations, each red: keep the helper in GIT_CONFIG_COUNT; replace the run's
// items; drop the '=' refusal.
func TestABoxsGitletCredentialRidesGitConfigParameters(t *testing.T) {
	t.Parallel()
	const base = "https://git.example.com/box/git"
	env, err := BoxGit{Base: base, User: "run", Token: "t"}.Env(map[string]string{
		GitConfigParametersVar: "'http.lowSpeedLimit=1'"})
	if err != nil {
		t.Fatal(err)
	}
	scope := "credential." + base + "/.helper"
	want := "'http.lowSpeedLimit=1' '" + scope + "=' '" + scope +
		`='\!'f() { test "$1" = get || return 0; t=$(cat "$CREWLET_GIT_ACCESS_FILE") || return 1; ` +
		`printf '\''username=run\npassword=%s\n'\'' "$t"; }; f'`
	if got := env[GitConfigParametersVar]; got != want {
		t.Errorf("%s=%q\nwant %q", GitConfigParametersVar, got, want)
	}
	if helpers, ok := gitConfigOf(t, env)[scope]; ok {
		t.Errorf("a count entry still names the helper: %q", helpers)
	}
	if _, err := (BoxGit{Base: "https://git.example.com/box=git", User: "run", Token: "t"}).Env(nil); err == nil ||
		!strings.Contains(err.Error(), BoxGitURLVar) {
		t.Errorf("a base holding '=' answered %v, want it refused naming %s", err, BoxGitURLVar)
	}
}

// A BOX'S GIT DIALS GITLET AS THE BOX REACHES THE ENGINE HOST, AND ITS HELPER
// IS SCOPED TO THAT URL: a loopback base, the documented single-host value, is
// the box's own loopback inside a container, so a box behind the runtime's
// alias is handed the base under the alias, and the credential helper's scope
// names the URL git dials, which git matches the scope against. A box on the
// host's network keeps the base as written, and a base naming any other host
// reaches every box as written.
//
// Mutations, each red: spell the scope from the base as written (the
// container's helper answers no clone); hand GitURLVar as written.
func TestABoxsGitDialsGitletAsTheBoxReachesTheEngineHost(t *testing.T) {
	t.Parallel()
	const alias = EngineHost("host.docker.internal")
	for _, tc := range []struct {
		name string
		base string
		host EngineHost
		want string
	}{
		{"a loopback base in a container", "http://localhost:8000/box/git", alias,
			"http://host.docker.internal:8000/box/git"},
		{"an IPv4 loopback base in a container", "http://127.0.0.1:8000/box/git", alias,
			"http://host.docker.internal:8000/box/git"},
		{"a loopback base on the host's network", "http://localhost:8000/box/git", "",
			"http://localhost:8000/box/git"},
		{"a public base in a container", "https://git.example.com/box/git", alias,
			"https://git.example.com/box/git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, err := BoxGit{Base: tc.base, EngineHost: tc.host, User: "check", Token: "t"}.Env(nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := env[GitURLVar]; got != tc.want {
				t.Errorf("%s=%q, want %q", GitURLVar, got, tc.want)
			}
			scope := "credential." + tc.want + "/.helper"
			// The reset and the helper, both under the dialled URL's scope.
			if got := env[GitConfigParametersVar]; !strings.HasPrefix(got, GitConfigParameter(scope, "")+" ") ||
				strings.Count(got, "'"+scope+"=") != 2 {
				t.Errorf("%s=%q, want the helper reset and set under %s, the URL the box dials",
					GitConfigParametersVar, got, scope)
			}
		})
	}
}
