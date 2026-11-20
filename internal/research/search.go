package research

import (
	"errors"
	"fmt"
)

// The bounds of a search (ADR-0151).
const (
	// MaxQuery is the longest query a search sends.
	MaxQuery = 200
)

// ErrSearch is a search the run could not answer, ErrSearchOff one on a
// company whose Ask Crewlet model's harness has no measured search-only run,
// and ErrQuery a query out of bounds.
var (
	ErrSearch    = errors.New("research: the search could not be made")
	ErrQuery     = fmt.Errorf("research: a query is 1 to %d characters", MaxQuery)
	ErrSearchOff = errors.New("research: web search is off: Ask Crewlet's model runs on a " +
		"harness with no measured search-only run")
)

// Result is one search result, as the search run's search tool returned it.
type Result struct {
	Title string `json:"title" untrusted:"name"`
	URL   string `json:"url"`
}

// Results are a search's results, and the tokens the search run spent, which
// the turn that asked is charged.
type Results struct {
	Query   string   `json:"query"`
	Results []Result `json:"results"`
	Tokens  int      `json:"tokens"`
}
