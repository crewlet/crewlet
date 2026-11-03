package config

import (
	"errors"
	"strings"
	"testing"
)

func registryDoc(lines string) string {
	return "name: Acme\n" + lines
}

// A COMPANY'S MCP REGISTRY IS AN HTTPS ADDRESS, or absent, which is no
// community search at all: what the registry answers is drafted into launch
// lines, so an address a network peer could answer for is refused, and so is
// one carrying a credential, which an address in a document is never the
// place for. The address is read back as written, so a search asks exactly
// what the document names.
func TestACompanysMCPRegistryIsAnHTTPSAddress(t *testing.T) {
	t.Parallel()
	if cfg := mustCompany(t, registryDoc("")); !cfg.MCPRegistry.IsZero() {
		t.Errorf("a company naming no registry reads as naming %q", cfg.MCPRegistry.URL)
	}
	for _, ok := range []string{
		"https://registry.modelcontextprotocol.io",
		"https://mcp-mirror.internal.example.com/registry/",
	} {
		cfg := mustCompany(t, registryDoc("mcp_registry:\n  url: "+ok+"\n"))
		if cfg.MCPRegistry.URL != ok {
			t.Errorf("%q read back as %q", ok, cfg.MCPRegistry.URL)
		}
	}
	for _, refused := range []struct{ url, says string }{
		{"http://registry.example.com", "https"},
		{"https://reader:secret@registry.example.com", "credentials"},
		{"https://registry.example.com/?token=secret", "query"},
		{"registry.example.com", "https"},
	} {
		err := rejects(t, registryDoc("mcp_registry:\n  url: \""+refused.url+"\"\n"), "mcp_registry.url")
		if !errors.Is(err, ErrUnknownValue) || !strings.Contains(err.Error(), refused.says) {
			t.Errorf("%q was refused as %v, want ErrUnknownValue saying %q", refused.url, err, refused.says)
		}
	}
}

// AN ADMISSION RULE: a revision a newer build admitted with an address this
// one refuses still runs, because nothing running reads it; the next write
// that keeps it is refused.
func TestARegistryAddressIsJudgedOnAdmissionAlone(t *testing.T) {
	t.Parallel()
	cfg, err := ParseCompanyDocument([]byte(registryDoc("mcp_registry:\n  url: http://registry.example.com\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateRunnable(); err != nil {
		t.Errorf("a registry address stopped a revision from running: %v", err)
	}
	if err := cfg.ValidateAdmission(); err == nil || !strings.Contains(err.Error(), "mcp_registry.url") {
		t.Errorf("a plain http registry was admitted: %v", err)
	}
}
