package executor

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// EVERY LISTED RUNNER IS REGISTERED, IN THE PROFILES' ORDER: a harness whose
// built-in profile has an executor block has a runner, and [Names],
// [Registered] and config.ExecutorHarnesses agree in that order whatever order
// Go ran the runner files' inits in.
//
// Mutation: drop a runner file's init, list a harness in profiles.yaml with no
// runner, or order the list by registration, and this goes red.
func TestEveryListedRunnerIsRegisteredInProfileOrder(t *testing.T) {
	t.Parallel()
	listed := harnessprofile.ExecutorRunners()
	if len(listed) == 0 {
		t.Fatal("no built-in profile lists a runner")
	}
	if got := Names(); !slices.Equal(got, listed) {
		t.Errorf("Names() = %v, the profiles list %v", got, listed)
	}
	var got []string
	for _, h := range Registered() {
		got = append(got, h.Name())
	}
	if !slices.Equal(got, listed) {
		t.Errorf("Registered() is %v, the profiles list %v", got, listed)
	}
	var cfg []string
	for _, h := range config.ExecutorHarnesses() {
		cfg = append(cfg, string(h))
	}
	if !slices.Equal(cfg, listed) {
		t.Errorf("config.ExecutorHarnesses() = %v, the profiles list %v", cfg, listed)
	}
}

// A RUNNER WITH NO PROFILE BLOCK, OR REGISTERED TWICE, IS REFUSED: either is a
// build running a runner nobody lists, or listing one twice.
//
// Mutation: drop either check, and a case goes red.
func TestARunnerWithNoProfileBlockOrRegisteredTwiceIsRefused(t *testing.T) {
	t.Parallel()
	byName := map[string]Harness{"claude-code": ClaudeCode{}, "a-runner-nobody-lists": OpenCode{}}
	if _, err := inProfileOrder(byName, harnessprofile.ExecutorRunners()); err == nil {
		t.Error("a runner with no executor block in its profile was listed")
	}
	defer func() {
		if recover() == nil {
			t.Error("a runner registered twice was taken")
		}
	}()
	Register(ClaudeCode{})
}
