package mcpapi_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/coord"
)

// THE SUMMARY A REVIEW WRITES FITS THE ACTIVATION POINTER AT ITS WORST: a
// thousand tools and a long server name, every byte escaping to six. A summary
// past coord.MaxSummaryBytes is refused, so one this composes for its caller
// would turn a review into a refusal for words the caller never wrote.
//
// Mutation: drop either cut and the summary is past the bound.
func TestTheSummaryAReviewWritesFitsThePointer(t *testing.T) {
	t.Parallel()
	tools := make([]string, 1000)
	for i := range tools {
		tools[i] = fmt.Sprintf("tool_\x01%04d", i)
	}
	summary := mcpapi.ReviewedSummary(strings.Repeat("\x01", 2000), tools)
	if err := coord.HoldSummary(summary); err != nil {
		t.Errorf("the summary a review of %d tools writes: %v", len(tools), err)
	}
}

// THE SUMMARY AN ALWAYS ALLOW WRITES FITS THE ACTIVATION POINTER AT ITS WORST:
// a tool name an MCP server supplied, a server name and a handle, each long and
// every byte escaping to six. Past coord.MaxSummaryBytes the allow is refused
// with summary_too_long for words its caller never wrote.
//
// Mutation: drop any of the three cuts and the summary is past the bound.
func TestTheSummaryAnAlwaysAllowWritesFitsThePointer(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("\x01", 2000)
	if err := coord.HoldSummary(mcpapi.AllowedSummary(long, long, long)); err != nil {
		t.Errorf("the summary an Always allow writes: %v", err)
	}
}
