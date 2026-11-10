package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// BoxBasis is the fingerprint of what seat's box in cell is made from: the
// cell, its backend's settings (image, runtime, network and run arguments, or
// template and domain) and the setup steps the box is given, in order. A run's
// reading of a box is kept under it, so a changed image, size or recipe is
// read again rather than told from an older box. Empty where the company runs
// no sandbox or cell names no backend.
//
// No secret is in it: a step's files are named by path and its environment by
// key, never by value.
func (c *Company) BoxBasis(seat *Role, cell Placement) string {
	catalogue := c.Providers.Sandbox
	if !catalogue.Enabled() || !slices.Contains(Placements, cell) {
		return ""
	}
	type stepBasis struct {
		Name     string   `json:"name"`
		Commands []string `json:"commands,omitempty"`
		Files    []string `json:"files,omitempty"`
		Env      []string `json:"env,omitempty"`
		Brief    string   `json:"brief,omitempty"`
	}
	basis := struct {
		Cell     Placement   `json:"cell"`
		Image    string      `json:"image,omitempty"`
		Runtime  string      `json:"runtime,omitempty"`
		Network  string      `json:"network,omitempty"`
		RunArgs  []string    `json:"run_args,omitempty"`
		Template string      `json:"template,omitempty"`
		Domain   string      `json:"domain,omitempty"`
		Steps    []stepBasis `json:"steps,omitempty"`
	}{Cell: cell}
	switch {
	case cell == PlacementE2B && catalogue.E2B != nil:
		basis.Template, basis.Domain = catalogue.E2B.Template, catalogue.E2B.Domain
	case cell == PlacementContainer && catalogue.Local != nil:
		l := catalogue.Local
		basis.Image, basis.Runtime, basis.Network, basis.RunArgs = l.Image, string(l.Runtime), l.Network, l.RunArgs
	}
	add := func(steps []SandboxSetupStep) {
		for _, s := range steps {
			basis.Steps = append(basis.Steps, stepBasis{Name: s.Name, Commands: s.Commands,
				Files: slices.Sorted(maps.Keys(s.Files)), Env: slices.Sorted(maps.Keys(s.Env)), Brief: s.Brief})
		}
	}
	add(catalogue.Setup)
	if seat != nil && seat.Sandbox != nil {
		add(seat.Sandbox.Setup)
	}
	raw, err := json.Marshal(basis)
	if err != nil {
		// Every field is a string or a list of strings.
		panic("config: encode a box basis: " + err.Error())
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SeatBoxesOf is the boxes of the agent seat whose handle is handle
// ([Company.SeatBoxes]), each in its own cell.
func (c *Company) SeatBoxesOf(handle string) []SeatBox {
	handle = strings.TrimSpace(handle)
	var out []SeatBox
	for box := range c.SeatBoxes() {
		if handle != "" && strings.TrimSpace(box.Seat.IdentityKey()) == handle {
			out = append(out, box)
		}
	}
	return out
}

// MemoryLimit is the memory a container box is held to by run_args, as the
// container runtime reads `--memory`, `--memory=`, `-m` and `-m=` (the last one
// written wins), in bytes; false where none is written or it does not parse.
func (l *LocalSandbox) MemoryLimit() (int64, bool) {
	if l == nil {
		return 0, false
	}
	var value string
	for i := 0; i < len(l.RunArgs); i++ {
		arg := strings.TrimSpace(l.RunArgs[i])
		switch {
		case arg == "--memory" || arg == "-m":
			if i+1 < len(l.RunArgs) {
				i++
				value = l.RunArgs[i]
			}
		case strings.HasPrefix(arg, "--memory="):
			value = strings.TrimPrefix(arg, "--memory=")
		case strings.HasPrefix(arg, "-m="):
			value = strings.TrimPrefix(arg, "-m=")
		}
	}
	return ramInBytes(value)
}

// ramSize is a container runtime's memory size: a number and a binary unit.
var ramSize = regexp.MustCompile(`^(\d+(?:\.\d+)?) ?([kKmMgGtT])?[iI]?[bB]?$`)

// ramInBytes reads a memory size as Docker and Podman do: binary units, no
// unit for bytes; false for a size that does not parse or is not positive.
func ramInBytes(s string) (int64, bool) {
	m := ramSize.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	shift := strings.Index("kmgt", strings.ToLower(m[2])) + 1
	if m[2] == "" {
		shift = 0
	}
	bytes := n * math.Exp2(float64(10*shift))
	if bytes < 1 || bytes > math.MaxInt64 {
		return 0, false
	}
	return int64(bytes), true
}
