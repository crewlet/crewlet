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
	if check[GitTokenVar] != "chk-token" || check[GitURLVar] != base {
		t.Errorf("the credential and base are %q and %q", check[GitTokenVar], check[GitURLVar])
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
// the helper's reset and the helper are its last two items, quoted as git
// reads them, the run environment's items ahead of them; no count entry names
// a credential helper; and a base holding '=', which an item cannot name, is
// refused naming CREWLET_BOX_GIT_URL.
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
		`='\!'f() { test "$1" = get || return 0; printf '\''username=run\npassword=%s\n'\'' "$CREWLET_GIT_TOKEN"; }; f'`
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
