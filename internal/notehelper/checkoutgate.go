package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
)

// THE GATE: a harness whose own code decides what a checkout brings, which no
// reimplementation keeps exact (Hermes Agent 0.23.3 scans every project file
// for prompt injection and blocks a hit, and loads a checkout's skills only
// where its configuration trusts the root, each passing its own scan), hands
// the core that code's answer for each candidate before a firing claims one
// ([checkoutGate]). The core then claims, copies and logs as for any harness,
// never reading the instructions or walking the skill directories of a gated
// checkout itself. The package doc's "Checkouts below the working directory"
// has the contract.

// checkoutGate is the harness's own answer for the checkouts a firing may
// claim, keyed by root as the firing names one (the working directory joined
// with its name): Checkouts, each one's; Taken, every name the home's skills
// are found by, with the skill that holds it, which no copy takes.
type checkoutGate struct {
	Checkouts map[string]gatedCheckout `json:"checkouts"`
	Taken     map[string]string        `json:"taken"`
}

// gatedCheckout is what the harness's own code makes of one checkout: Text,
// its instructions as a session started there reads them ("" for none);
// Files, the files they were read from; Skills, the skill directories such a
// session loads, each copied; Said, what it leaves out, a line each, said once
// the checkout is claimed.
type gatedCheckout struct {
	Text   string   `json:"text"`
	Files  []string `json:"files"`
	Skills []string `json:"skills"`
	Said   []string `json:"said"`
}

// maxGate bounds a gate: every candidate's instructions at the most a harness
// keeps of them (Hermes: 500,000 characters, some 2 MB) and their skills'
// paths, for more checkouts than a run clones at once.
const maxGate = 64 << 20

// readGate is the gate on r, refused where it passes [maxGate] or is no JSON
// object of a gate's fields.
func readGate(r io.Reader) (*checkoutGate, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxGate+1))
	if err != nil {
		return nil, fmt.Errorf("the gate could not be read: %w", err)
	}
	if len(raw) > maxGate {
		return nil, fmt.Errorf("the gate passes its bound of %d bytes", maxGate)
	}
	var g checkoutGate
	if err = json.Unmarshal(raw, &g); err != nil {
		return nil, errors.New("the gate is not a JSON object of its checkouts and the names the home takes")
	}
	return &g, nil
}

// gatedSkillDirs is the skill directories the gate hands a checkout, each
// named by its directory, the first of a name winning it as a walk's would.
func gatedSkillDirs(g gatedCheckout) []skillDir {
	dirs := make([]skillDir, 0, len(g.Skills))
	for _, src := range g.Skills {
		dirs = append(dirs, skillDir{src: src, name: filepath.Base(src)})
	}
	return dirs
}

// claimRoom is what an adapter's answer has left for the checkouts a firing
// claims, nil where every checkout is claimed whatever its size: take reserves
// what handing h costs, saying how where it is not whole, false where even
// that does not fit, and h then waits for a later firing ([handCheckouts]).
type claimRoom interface {
	take(h handedCheckout, said *skips) bool
}

// taken is the skill of the home's found by name, where g answers (nil for
// none) and one is.
func (g *checkoutGate) taken(name string) (string, bool) {
	if g == nil {
		return "", false
	}
	held, ok := g.Taken[name]
	return held, ok
}

// holds reports whether g answers for the checkout at root.
func (g *checkoutGate) holds(root string) bool {
	_, ok := g.Checkouts[root]
	return ok
}
