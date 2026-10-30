package sandbox

import (
	"strconv"
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

// A CHECK'S BOX IS GIVEN NO COMMIT IDENTITY: it acts as nobody, so its git is
// configured with the code host's credential and protocol and no user.name or
// user.email — a commit its command makes fails for want of one rather than
// naming somebody — while a coding box keeps its seat's; and an identity of a
// name without an email, or the reverse, is refused rather than half given.
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
		t.Errorf("protocol.version = %v, want the code host's 2", got)
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
