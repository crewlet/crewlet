package modelpass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// A WEEK'S REPORT IS SUMMARISED BY THE SAME MODEL, from the report alone: it
// is handed the report's numbers and named findings, reads nothing else, and
// submits a few sentences the engine keeps only where every number they say is
// one the report says ([advisor.GroundSummary]).

// SummaryTool is the call a report's summary is submitted by.
const SummaryTool = "submit_summary"

// SummaryCeiling is the most tokens a report's summary may spend: the report
// is a few thousand, and the summary a few sentences.
const SummaryCeiling = 30_000

// summaryRounds bounds the summary's model requests: the submission, and one
// more after the engine said what was wrong with it.
const summaryRounds = 3

// summarySystem is what the summary is told.
const summarySystem = `You summarise one week's report of the Advisor of a company of AI agents and the people who
direct them, for the company's administrator. You are given the report as JSON. Call submit_summary once with at
most five plain sentences: what the week cost and how that moved from the week before, what became of the
findings, and which open finding to look at first. Say only numbers the report says, spelled as it spells them;
a summary that says any other number, or names a record the report does not, is not kept. No links or markup.`

// Summarize is the model's summary of r, checked against r's numbers; nil
// where the company assigns no model, or where the model wrote none the
// engine could keep.
func (s *Service) Summarize(ctx context.Context, now time.Time, r advisor.Report) (*string, error) {
	if !s.Models.Assigned() {
		return nil, nil
	}
	deps, err := s.Models.Deps(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("modelpass: the Advisor's model could not start: %w", err)
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return now }
	}
	text, err := Summary(ctx, deps, r, SummaryCeiling)
	if err != nil {
		return nil, err
	}
	return &text, nil
}

// ErrNoSummary is a summary the model did not submit, or one the engine did not keep.
var ErrNoSummary = errors.New("modelpass: the model wrote no summary the engine could keep")

// Summary is one summary of r, run on deps under ceiling.
func Summary(ctx context.Context, deps Deps, r advisor.Report, ceiling int) (string, error) {
	report, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	surf := &summarySurface{}
	_, err = toolloop.Run(ctx, toolloop.Config{
		Provider: deps.Provider, ProviderKey: deps.ProviderKey, Surface: surf, Host: deps.Host,
		ServerTitle: "advisor", MaxRounds: summaryRounds, OwnPrompt: true,
		Messages: []llm.Message{{Role: llm.RoleSystem, Content: summarySystem},
			{Role: llm.RoleUser, Content: "The report: " + string(report)}},
		TerminateAfter: []string{SummaryTool}, Budget: &ceilingMeter{inner: deps.Budget, ceiling: ceiling},
		Now: deps.Now,
		// NO SEAT TO FENCE: it reads nothing but the report it was handed.
		Fence: nil,
	})
	if err != nil {
		return "", fmt.Errorf("modelpass: the report's summary: %w", err)
	}
	text := surf.result()
	if text == "" {
		return "", ErrNoSummary
	}
	if err := advisor.GroundSummary(text, r); err != nil {
		return "", errors.Join(ErrNoSummary, err)
	}
	return strings.TrimSpace(text), nil
}

// summarySurface takes the summary and offers nothing to read.
type summarySurface struct {
	mu   sync.Mutex
	text string
}

var _ toolloop.Surface = (*summarySurface)(nil)

func (*summarySurface) ToolDefs() []llm.ToolDef {
	return []llm.ToolDef{{Name: SummaryTool, Description: "Submit the report's summary.",
		Parameters: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"summary"},
			"properties": map[string]any{"summary": map[string]any{"type": "string",
				"maxLength": advisor.MaxSummaryBytes}}}}}
}

func (*summarySurface) Revision() uint64                     { return 0 }
func (*summarySurface) AnnotationsOf(string) mcp.Annotations { return mcp.Annotations{} }
func (*summarySurface) Phase() string                        { return "advisor_model" }

func (s *summarySurface) Admit(name string, _ map[string]any) toolloop.Admitted {
	return func(_ context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
		text, ok := call.Arguments["summary"].(string)
		if name != SummaryTool || !ok || strings.TrimSpace(text) == "" {
			return toolloop.ToolResult{Failed: true, Cause: mcp.RefusalInvalid,
				Output: "call submit_summary with {\"summary\": \"...\"}"}, nil
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.text = text
		return toolloop.ToolResult{Output: "received"}, nil
	}
}

func (s *summarySurface) result() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text
}
