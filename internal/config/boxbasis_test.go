package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A CONTAINER BOX'S MEMORY IS READ AS ITS RUNTIME READS IT: binary units, the
// last flag written wins, and a size that does not parse is no ceiling rather
// than a guess.
//
// Mutation: read decimal units, take the first flag, or answer a bad size with
// a number, and this goes red.
func TestAContainerBoxMemoryIsReadAsItsRuntimeReadsIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want int64
		ok   bool
	}{
		{args: []string{"--cpus", "2", "--memory", "4g"}, want: 4 << 30, ok: true},
		{args: []string{"--memory=512m"}, want: 512 << 20, ok: true},
		{args: []string{"-m", "2GiB"}, want: 2 << 30, ok: true},
		{args: []string{"-m=1.5g"}, want: 3 << 29, ok: true},
		{args: []string{"--memory", "1g", "--memory", "8g"}, want: 8 << 30, ok: true},
		{args: []string{"--memory", "1048576"}, want: 1 << 20, ok: true},
		{args: []string{"--memory", "lots"}},
		{args: []string{"--memory"}},
		{args: []string{"--memory-swap", "4g"}},
		{},
	} {
		got, ok := (&config.LocalSandbox{RunArgs: tc.args}).MemoryLimit()
		if got != tc.want || ok != tc.ok {
			t.Errorf("MemoryLimit(%q) = %d, %v, want %d, %v", tc.args, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := (*config.LocalSandbox)(nil).MemoryLimit(); ok {
		t.Error("a company with no local block holds a container box to a memory")
	}
}

// boxCompany is a company whose one agent seat runs in a container box.
func boxCompany(t *testing.T, edit func(string) string) *config.Company {
	t.Helper()
	doc := `name: Acme
providers:
  sandbox:
    local:
      image: registry.example.com/coding:1
      run_args: ["--memory", "4g"]
    default_run_in: container
    setup:
      - name: tools
        commands: ["apt-get install -y ripgrep"]
        env: {TOKEN: "${TOKEN}"}
  llm:
    main:
      model: sonnet
roles:
  - name: SWE
`
	if edit != nil {
		doc = edit(doc)
	}
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	return c
}

// A BOX'S BASIS MOVES WITH WHAT MAKES THE BOX AND NOTHING ELSE: its image, its
// run arguments and its setup recipe move it, so an older reading is not told
// of a box made another way; a value behind a step's environment key, which
// the basis never holds, and anything outside the box do not.
//
// Mutation: leave the image, the run arguments or a step's commands out of the
// basis, or put an environment value in it, and this goes red.
func TestABoxBasisMovesWithWhatMakesTheBox(t *testing.T) {
	t.Parallel()
	basis := func(c *config.Company) string {
		boxes := c.SeatBoxesOf("swe")
		if len(boxes) != 1 || boxes[0].Cell != config.PlacementContainer {
			t.Fatalf("the seat's boxes are %+v, want one container box", boxes)
		}
		return c.BoxBasis(boxes[0].Seat, boxes[0].Cell)
	}
	base := basis(boxCompany(t, nil))
	if base == "" {
		t.Fatal("a container box has no basis")
	}
	for name, edit := range map[string]func(string) string{
		"the image":     func(d string) string { return strings.Replace(d, "coding:1", "coding:2", 1) },
		"the run args":  func(d string) string { return strings.Replace(d, `"4g"`, `"8g"`, 1) },
		"a step's line": func(d string) string { return strings.Replace(d, "ripgrep", "chromium", 1) },
	} {
		if basis(boxCompany(t, edit)) == base {
			t.Errorf("changing %s kept the box's basis", name)
		}
	}
	for name, edit := range map[string]func(string) string{
		"an env value":       func(d string) string { return strings.Replace(d, "${TOKEN}", "${OTHER}", 1) },
		"the company's name": func(d string) string { return strings.Replace(d, "name: Acme", "name: Acme Two", 1) },
	} {
		if basis(boxCompany(t, edit)) != base {
			t.Errorf("changing %s moved the box's basis", name)
		}
	}
	if got := boxCompany(t, nil).SeatBoxesOf("nobody"); len(got) != 0 {
		t.Errorf("a handle no seat has holds boxes %+v", got)
	}
}
