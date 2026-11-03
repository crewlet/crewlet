package seataccess

import (
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// A level the connection cannot grant yet matters only to a seat on it: one
// [LevelGap] is the blocking finding while a seat that is on holds the level,
// an advisory while none does, its offer's reason and a seat's refusal.

// LevelGap is one level the company's connection cannot grant yet.
type LevelGap struct {
	// Level is the level's identifier, and Name it as a person reads it.
	Level, Name string

	// Reason is the sentence saying what the level waits on, which its offer
	// carries ([Level.Unavailable]) and its advisory says: "The Configure
	// level is unavailable until the provisioner token holds Zone Settings:
	// Edit and Response Compression: Edit".
	Reason string

	// Remedy is what makes the level available, and ActionURL where.
	Remedy, ActionURL string

	// Missing is what the connection lacks, as a person adds it.
	Missing []string

	// Blocking is the vendor's finding while a seat that is on holds the
	// level: a grant the connection cannot make for an agent that needs it.
	Blocking integration.Finding
}

// GapFindings is each gap as the integration reports it: its blocking finding
// where a level in inUse has it, and the [integration.FindingLevelUnavailable]
// advisory otherwise, on the same subject.
func GapFindings(gaps []LevelGap, inUse []string) []integration.Finding {
	out := make([]integration.Finding, 0, len(gaps))
	for _, g := range gaps {
		if slices.Contains(inUse, g.Level) {
			out = append(out, g.Blocking)
			continue
		}
		out = append(out, integration.Finding{
			Kind: integration.FindingLevelUnavailable, Subject: g.Blocking.Subject,
			Title: g.Name + " level unavailable", Detail: g.Reason, Remedy: g.Remedy,
			ActionURL: g.ActionURL, Subjects: slices.Clone(g.Missing),
		})
	}
	return out
}

// OfferGaps is levels with each gap's reason on the level it names.
func OfferGaps(levels []Level, gaps []LevelGap) []Level {
	out := slices.Clone(levels)
	for i := range out {
		if j := slices.IndexFunc(gaps, func(g LevelGap) bool { return g.Level == out[i].ID }); j >= 0 {
			out[i].Unavailable = gaps[j].Reason
		}
	}
	return out
}

// LevelsInUse is every level a seat that is on at lane holds, as the document
// states it, each once and sorted.
func LevelsInUse(c *config.Company, lane Lane) []string {
	if c == nil {
		return nil
	}
	var out []string
	for role := range c.EachRole() {
		if d, ok := DesiredOf(role, lane); ok && d.Enabled && !slices.Contains(out, d.Level) {
			out = append(out, d.Level)
		}
	}
	slices.Sort(out)
	return out
}
