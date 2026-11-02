package configapi_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// valueDoc is a company whose `linear` server sends a header written as a
// literal, beside a description another write may change.
const valueDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    default:
      model: claude-opus-5-5
mcp_servers:
  - name: linear
    transport: http
    url: https://mcp.linear.example/mcp
    description: Issues and projects.
    headers: {Authorization: "Bearer literal-not-a-key", X-Team: "eng"}
`

var valueWriter = iam.Actor{Name: "jane.doe", Kind: iam.ActorHuman, OperatorID: "session:0f3c"}

// slotOf is the slot for linear's Authorization in the active document.
func slotOf(t *testing.T, s *surface) config.MCPValueSlot {
	t.Helper()
	company, _, err := s.service().ActiveCompany(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	slot, err := company.MCPValueSlot("linear", "", "Authorization")
	if err != nil {
		t.Fatal(err)
	}
	return slot
}

// A pointer is one write of the document as it stands: a write meanwhile is
// kept, and a key pointed elsewhere meanwhile is refused.
func TestAValuesPointerIsOneWriteOfTheDocumentAsItStands(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	first := s.seed(t, valueDoc)
	slot := slotOf(t, s)
	if !slot.Repoint || slot.Variable != "MCP_LINEAR_AUTHORIZATION" {
		t.Fatalf("slot = %+v, want a literal pointed at a variable of its own", slot)
	}

	checked, err := s.service().CheckMCPValue(t.Context(), slot, valueWriter)
	if err != nil {
		t.Fatalf("CheckMCPValue: %v", err)
	}
	if got := checked.MCPServers[0].Headers["Authorization"]; got != "${MCP_LINEAR_AUTHORIZATION}" {
		t.Fatalf("the dry run's company reads %q", got)
	}
	if revision, _, _ := s.active(t.Context()); revision.ID != first {
		t.Fatalf("the dry run stored a revision: %s", revision.ID)
	}

	// A WRITE THAT LANDS WHILE THE VALUE IS TESTED is kept.
	if _, err = s.service().ApplyEntity(t.Context(), configapi.ApplyEntityRequest{Kind: configapi.EntityMCPServers,
		ID: "linear", Body: []byte(`{"name":"linear","transport":"http","url":"https://mcp.linear.example/mcp",` +
			`"description":"Issues, projects and cycles.","headers":{"Authorization":"Bearer literal-not-a-key",` +
			`"X-Team":"eng"}}`), Summary: "describe linear", By: valueWriter}); err != nil {
		t.Fatalf("the write meanwhile: %v", err)
	}
	applied, err := s.service().PointMCPValue(t.Context(), slot, "store Authorization of MCP server linear", valueWriter)
	if err != nil {
		t.Fatalf("PointMCPValue: %v", err)
	}
	document := s.activeDocument(t)
	for _, want := range []string{`"Authorization":"${MCP_LINEAR_AUTHORIZATION}"`, `"X-Team":"eng"`,
		"Issues, projects and cycles."} {
		if !strings.Contains(document, want) {
			t.Errorf("the pointed document lacks %s: %s", want, document)
		}
	}
	if strings.Contains(document, "literal-not-a-key") || applied.RevisionID == first {
		t.Errorf("the write = %+v over %s", applied, document)
	}

	if _, err = s.service().ApplyEntity(t.Context(), configapi.ApplyEntityRequest{Kind: configapi.EntityMCPServers,
		ID: "linear", Body: []byte(`{"name":"linear","transport":"http","url":"https://mcp.linear.example/mcp",` +
			`"headers":{"Authorization":"${LINEAR_OWN}"}}`), Summary: "point linear", By: valueWriter}); err != nil {
		t.Fatal(err)
	}
	var moved *configapi.MCPValueMovedError
	if _, err = s.service().PointMCPValue(t.Context(), slot, "again", valueWriter); !errors.As(err, &moved) ||
		moved.Now != "LINEAR_OWN" {
		t.Errorf("a key pointed elsewhere meanwhile = %v, want it refused naming the variable it reads", err)
	}
}

// A managed document refuses a value's pointer, its dry run included.
func TestAManagedDocumentRefusesAValuesPointer(t *testing.T) {
	t.Parallel()
	s := newSurfaceWith(t, func(o *configapi.Options) { o.CompanyWriters = []string{"gitops"} })
	s.seed(t, valueDoc)
	slot := slotOf(t, s)
	var managed *configapi.ManagedError
	if _, err := s.service().CheckMCPValue(t.Context(), slot, valueWriter); !errors.As(err, &managed) {
		t.Errorf("the dry run = %v, want config_managed", err)
	}
	if _, err := s.service().PointMCPValue(t.Context(), slot, "store", valueWriter); !errors.As(err, &managed) {
		t.Errorf("the write = %v, want config_managed", err)
	}
	if err := s.service().Authorize(valueWriter); !errors.As(err, &managed) {
		t.Errorf("Authorize = %v, want config_managed", err)
	}
}
