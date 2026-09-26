package config_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// A SERVER IS GRANTED TO THE SEATS THAT CAN ACT THROUGH IT, and to nobody
// else. A shared server serves every agent seat; a per-seat template serves a
// seat that declares credentials for it and nobody else, because a template
// with nobody's identity in it is a server nobody can act through; a human
// seat runs no tools. The engine starts a seat's children by this rule and the
// org projection publishes it, so a change here moves both.
func TestAServerIsGrantedToTheSeatsThatCanActThroughIt(t *testing.T) {
	t.Parallel()
	shared := config.MCPServer{Name: "search"}
	template := config.MCPServer{Name: "github", Shared: org.Off()}
	declares := &org.Role{Name: "SRE", MCPEnv: org.MCPEnv{"github": {"GITHUB_TOKEN": "${SRE_GITHUB}"}}}
	silent := &org.Role{Name: "CTO"}
	human := &org.Role{Name: "Founder", Kind: org.KindHuman}
	for _, tc := range []struct {
		server config.MCPServer
		seat   *org.Role
		want   bool
	}{
		{shared, silent, true},
		{shared, declares, true},
		{shared, human, false},
		{template, declares, true},
		{template, silent, false},
		{template, &org.Role{Name: "Founder", Kind: org.KindHuman, MCPEnv: declares.MCPEnv}, false},
		{shared, nil, false},
	} {
		name := "<nil>"
		if tc.seat != nil {
			name = tc.seat.Name
		}
		if got := tc.server.Grants(tc.seat); got != tc.want {
			t.Errorf("%s.Grants(%s) = %v, want %v", tc.server.Name, name, got, tc.want)
		}
	}
}
