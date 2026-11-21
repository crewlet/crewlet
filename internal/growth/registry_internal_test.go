package growth

import (
	"strings"
	"testing"
)

// A TOOL NAME IS ONE PRODUCT'S, and every catalog keeps its rules: the
// registry's first read refuses a tool two catalogs list, though each name
// starts with its own kind's (one kind may begin another's), and an invalid
// catalog. Two catalogs with their own names pass.
//
// Mutation: drop the walk over the names already listed, or the validation,
// and its case passes.
func TestTheRegistryRefusesASharedToolAndAnInvalidCatalog(t *testing.T) {
	t.Parallel()
	object := map[string]any{"type": "object"}
	catalog := func(kind, tool string) Catalog {
		return Catalog{Kind: kind, API: "Shared API", Version: "v1",
			Operations: []Operation{{ID: "list", Method: "GET", Path: "/list", Class: ClassRead}},
			Tools: []Tool{{Name: tool, Class: ClassRead, Operations: []string{"list"},
				Description: "List.", Parameters: object}}}
	}
	if err := check([]Catalog{catalog("shared", "shared_list"), catalog("shared_ads", "shared_ads_list")}); err != nil {
		t.Errorf("two catalogs with their own tool names were refused: %v", err)
	}
	err := check([]Catalog{catalog("shared", "shared_ads_list"), catalog("shared_ads", "shared_ads_list")})
	if err == nil || !strings.Contains(err.Error(), "shared_ads_list") {
		t.Errorf("a tool two catalogs list answered %v", err)
	}
	if err := check([]Catalog{catalog("shared", "other_list")}); err == nil {
		t.Error("a catalog whose tool is not named after its kind passed")
	}
}
