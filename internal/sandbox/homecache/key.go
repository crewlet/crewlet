package homecache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/envref"
)

// formatVersion is the layout a tree is built and kept in. Raising it gives
// every key a new value, so a build reading an older layout never takes it.
const formatVersion = 1

// Key names one provisioned home: [KeyOf] of what built it.
type Key string

// Step is one setup step as the cache reads it: what keys it, and what
// decides whether it may be cached ([CachedPrefix]).
type Step struct {
	Name           string            `json:"name"`
	Files          map[string]string `json:"files,omitempty"`
	Commands       []string          `json:"commands,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds float64           `json:"timeout_seconds,omitempty"`
	// EveryBox runs the step in each box, never in a build: for a step that
	// needs a credential none of [CachedPrefix]'s other rules see.
	EveryBox bool `json:"every_box,omitempty"`
}

// Input is what a box's home is provisioned from.
type Input struct {
	// Owner is the seat the home is for: [Cache.Collect] keeps what its
	// owners hold. Not keyed (the seat's identity facts in Env are).
	Owner string
	// Runtime is the container runtime that runs the build and the box.
	Runtime string
	// ImageID is the box image's ID, never its tag: a tag moves under a pull.
	ImageID string
	// Env is every variable the build is handed: no credential, by the
	// caller's construction.
	Env map[string]string
	// Steps is the box's whole setup, in the order it runs.
	Steps []Step
	// Withheld is every variable a launch's environment carries and the build
	// is not handed: a step whose commands name one is not cached.
	Withheld []string
	// Secrets is every value a launch hands the box from a credential
	// position: a step with a file holding one is not cached, and a tree
	// holding one is not kept. Not keyed.
	Secrets []string
}

// keyed is what a key is the digest of: everything that changes what a build
// leaves, and nothing else (Withheld and Secrets only decide the prefix).
type keyed struct {
	Format  int               `json:"format"`
	Runtime string            `json:"runtime"`
	ImageID string            `json:"image_id"`
	Env     map[string]string `json:"env"`
	Steps   []Step            `json:"steps"`
}

// KeyOf is the key of the home the first prefix steps of in leave.
func KeyOf(in Input, prefix int) Key {
	env := in.Env
	if len(env) == 0 {
		env = nil // no environment is one key, however the caller spelt it
	}
	raw, err := json.Marshal(keyed{
		Format: formatVersion, Runtime: in.Runtime, ImageID: in.ImageID,
		Env: env, Steps: in.Steps[:prefix],
	})
	if err != nil {
		// Strings, ints and maps of strings: json.Marshal cannot refuse them.
		panic("homecache: encoding a key: " + err.Error())
	}
	sum := sha256.Sum256(raw)
	return Key(hex.EncodeToString(sum[:]))
}

// shellVariable is a variable a command names, braced or not, with or without
// a parameter expansion after its name: wider than [envref]'s ${VAR}, since a
// shell reads all of them, and a wider match only caches less. Built at first
// use, not at init, which every process linking the engine pays.
var shellVariable = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\$\{?(` + envref.NameGrammar + `)`)
})

// CachedPrefix is how many of steps, from the first, a build may run: those
// before the first step that needs what a build is not handed (see the package
// doc). withheld and secrets are [Input.Withheld] and [Input.Secrets].
func CachedPrefix(steps []Step, withheld, secrets []string) int {
	needles := needlesOf(secrets)
	for i, step := range steps {
		if step.EveryBox || referencesCredential(step, needles) || namesWithheld(step, withheld) {
			return i
		}
	}
	return len(steps)
}

// referencesCredential is whether a step's env carries a ${VAR} reference, or
// one of its files is exactly one or holds a credential value (the launch
// resolves a whole reference before the box sees it): what a build is not
// handed. A reference inside a longer file is text the box's own tools read
// (npm expands `${NPM_TOKEN}` in an .npmrc), the same in every box.
func referencesCredential(step Step, needles [][]byte) bool {
	for _, value := range step.Env {
		if envref.Has(value) {
			return true
		}
	}
	for _, body := range step.Files {
		if _, whole := envref.Whole(body); whole || containsAny([]byte(body), needles) {
			return true
		}
	}
	return false
}

// namesWithheld is whether one of a step's commands names a withheld variable.
func namesWithheld(step Step, withheld []string) bool {
	for _, command := range step.Commands {
		for _, m := range shellVariable().FindAllStringSubmatch(command, -1) {
			if slices.Contains(withheld, m[1]) {
				return true
			}
		}
	}
	return false
}
