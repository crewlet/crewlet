package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

// offDoc is a company with a server turned off: a per-seat template whose
// seat and unit still declare blocks for it, and which a box lists. The
// header block written by the unit is the shape [Company.validateHeaderSpellings]
// judges, so a test can show the rules still read a server turned off.
func offDoc(serverLines, seatBlock string) string {
	return `
name: Acme
mcp_servers:
  - name: docs
    transport: http
    shared: false
    url: https://docs.example.com/mcp
` + serverLines + `
units:
  - name: Engineering
    mcp_env:
      docs:
        Authorization: "Bearer ${ENG_DOCS}"
    roles:
      - name: SRE
        handle: sre
        sandbox:
          mcp:
            servers: [docs]
        mcp_env:
          docs:
` + seatBlock
}

// A SERVER TURNED OFF IS GRANTED TO NOBODY, shared or per seat, whatever its
// seats declare for it; the default is on, so every document written before
// the field existed runs exactly as it did.
func TestAServerTurnedOffIsGrantedToNobody(t *testing.T) {
	t.Parallel()
	declares := &org.Role{Name: "SRE", MCPEnv: org.MCPEnv{"docs": {"TOKEN": "${SRE_DOCS}"}}}
	for _, tc := range []struct {
		name   string
		server MCPServer
		want   bool
	}{
		{"shared, on by default", MCPServer{Name: "docs"}, true},
		{"shared, on", MCPServer{Name: "docs", Enabled: org.On()}, true},
		{"shared, off", MCPServer{Name: "docs", Enabled: org.Off()}, false},
		{"per seat, on by default", MCPServer{Name: "docs", Shared: org.Off()}, true},
		{"per seat, off", MCPServer{Name: "docs", Shared: org.Off(), Enabled: org.Off()}, false},
	} {
		if got := tc.server.Grants(declares); got != tc.want {
			t.Errorf("%s: Grants = %v, want %v", tc.name, got, tc.want)
		}
		if got := tc.server.IsEnabled(); got != tc.server.Enabled.Or(true) {
			t.Errorf("%s: IsEnabled = %v, want the field's value with the true default", tc.name, got)
		}
	}
}

// A SERVER TURNED OFF IS STILL DECLARED, so every block that names it stays
// admissible and stays held to its rules: its seat and unit mcp_env blocks name
// a declared server, a box may list it, and a header its unit and seat spell
// two ways is refused exactly as it is for a server that runs. Off is the
// gesture that keeps what somebody wrote, and a block it kept unchecked would
// be the one that fails the day the server is turned back on.
func TestAServerTurnedOffKeepsItsBlocksAndTheirRules(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, offDoc("    enabled: false\n", "            Authorization: \"Bearer ${SRE_DOCS}\"\n"))
	if cfg.MCPServers[0].IsEnabled() {
		t.Fatal("enabled: false parsed as on")
	}

	err := rejects(t, offDoc("    enabled: false\n", "            authorization: \"Bearer ${SRE_DOCS}\"\n"),
		"mcp_env.docs.authorization")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("a header spelled two ways for a server turned off was refused as %v, want ErrConflict", err)
	}
}

// A DESCRIPTION IS ONE LINE OF AT MOST 200 CHARACTERS, counted as the schema
// counts them (code points, not bytes), because it is one line of every
// agent's prompt. A line break or a tab is refused rather than folded, so what
// the operator reads back is what the prompt says.
func TestADescriptionIsOneLineOfAtMost200Characters(t *testing.T) {
	t.Parallel()
	described := func(text string) string {
		return offDoc("    description: \""+text+"\"\n", "            Authorization: \"Bearer ${SRE_DOCS}\"\n")
	}
	for _, ok := range []string{
		"",
		"Reads and searches the engineering handbook",
		strings.Repeat("a", MaxMCPDescription),
		// 200 characters and 400 bytes: the bound is characters.
		strings.Repeat("é", MaxMCPDescription),
	} {
		cfg := mustCompany(t, described(ok))
		if cfg.MCPServers[0].Description != ok {
			t.Errorf("description %q read back as %q", ok, cfg.MCPServers[0].Description)
		}
	}
	for _, tc := range []struct {
		name, text string
		want       error
	}{
		{"201 characters", strings.Repeat("a", MaxMCPDescription+1), ErrOutOfRange},
		{"201 two-byte characters", strings.Repeat("é", MaxMCPDescription+1), ErrOutOfRange},
		{"a line break", `Reads the handbook\nand files pages`, ErrUnknownValue},
		{"a tab", `Reads the handbook\tquickly`, ErrUnknownValue},
	} {
		err := rejects(t, described(tc.text), "mcp_servers[0].description")
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: refused as %v, want %v", tc.name, err, tc.want)
		}
	}
}
