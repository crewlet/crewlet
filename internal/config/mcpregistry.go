package config

import (
	"errors"

	"github.com/crewlet/crewlet/internal/mcp/catalogue"
)

// MCPRegistry names the community registry Settings › MCP & tools searches
// beside the curated catalogue (internal/mcp/catalogue). Absent, there is no
// community search: which registry a company trusts to suggest servers is the
// company's decision, so none is assumed.
//
// A registry speaks the MCP Registry's v0.1 API. What it answers is only ever
// a draft for the same add flow an address typed by hand goes through,
// labelled as not reviewed, and the engine dials nothing a listing names.
type MCPRegistry struct {
	// URL is the registry's address; the API's /v0.1/servers path is
	// appended to it.
	URL string `yaml:"url,omitempty" json:"url,omitempty" desc:"The registry's https address; /v0.1/servers is appended to it. Absent: no community search."`
}

// IsZero is an unset registry, which drops out of a round trip.
func (r MCPRegistry) IsZero() bool { return r.URL == "" }

// validateMCPRegistry holds the registry's address to one the catalogue
// searches ([catalogue.CheckRegistryAddress], the one rule, which the search
// asks again before it dials).
//
// AN ADMISSION RULE: nothing a running company does depends on the address,
// since a search that cannot run says why to the person who asked and the
// curated catalogue answers beside it.
func (c *Company) validateMCPRegistry() error {
	if c.MCPRegistry.IsZero() {
		return nil
	}
	var p problems
	var refused *catalogue.AddressError
	if err := catalogue.CheckRegistryAddress(c.MCPRegistry.URL); errors.As(err, &refused) {
		p.add(field("mcp_registry.url"), ErrUnknownValue, "%q is not a registry address: %s",
			c.MCPRegistry.URL, refused.Reason)
	}
	return p.err()
}
