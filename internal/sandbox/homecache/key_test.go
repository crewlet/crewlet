package homecache

import "testing"

// The cached prefix ends at the first step that needs what a build is not
// handed; everything before it, and only that, is built once.
func TestTheCachedPrefixEndsAtTheFirstStepNeedingACredential(t *testing.T) {
	plain := Step{Name: "tools", Commands: []string{`npm install -g x@1 --prefix "$HOME/.npm-global"`}}
	withheld := []string{"GITHUB_TOKEN", "NPM_TOKEN"}
	for _, tc := range []struct {
		name string
		step Step
		want int
	}{
		{"a step naming nothing withheld", plain, 2},
		{"a step marked for every box", Step{Name: "x", EveryBox: true}, 1},
		{"a reference in its env", Step{Name: "x", Env: map[string]string{"T": "Bearer ${TOKEN}"}}, 1},
		{"a literal env", Step{Name: "x", Env: map[string]string{"GIT_TERMINAL_PROMPT": "0"}}, 2},
		{"a file that is one reference", Step{Name: "x", Files: map[string]string{".npmrc": "${NPMRC}"}}, 1},
		{"a file holding a credential value", Step{Name: "x",
			Files: map[string]string{".config/tool/token": "ghs_resolvedvalue"}}, 1},
		{"a file holding a value too short to tell", Step{Name: "x",
			Files: map[string]string{".config/tool/port": "5432"}}, 2},
		{"a reference inside a file, for the box's tools", Step{Name: "x",
			Files: map[string]string{".npmrc": "//registry.npmjs.org/:_authToken=${NPM_TOKEN}\n"}}, 2},
		{"a command naming a withheld variable", Step{Name: "x",
			Commands: []string{`echo "$GITHUB_TOKEN" > ~/.token`}}, 1},
		{"a command naming it braced", Step{Name: "x", Commands: []string{"echo ${NPM_TOKEN}"}}, 1},
		{"a command expanding it", Step{Name: "x", Commands: []string{"echo ${NPM_TOKEN:-none}"}}, 1},
		{"a command naming a variable that only begins like one", Step{Name: "x",
			Commands: []string{"echo $GITHUB_TOKENS"}}, 2},
		{"a command naming the build's own", Step{Name: "x",
			Commands: []string{`git config --global user.name "$CREWLET_AGENT_HANDLE"`}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps := []Step{plain, tc.step, plain}
			got := CachedPrefix(steps, withheld, []string{"ghs_resolvedvalue", "5432"})
			if tc.want == 2 {
				tc.want = len(steps)
			}
			if got != tc.want {
				t.Errorf("CachedPrefix = %d, want %d", got, tc.want)
			}
		})
	}
	if got := CachedPrefix(nil, withheld, nil); got != 0 {
		t.Errorf("no steps: %d", got)
	}
}
