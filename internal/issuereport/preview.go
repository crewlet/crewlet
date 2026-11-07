package issuereport

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Preview is what a class's report would say: built as a real one is, with this
// engine's own build and topology and the catalogue's example of each fact.
type Preview struct {
	Class   Class           `json:"class"`
	Title   string          `json:"title"`
	Summary string          `json:"summary"`
	Payload json.RawMessage `json:"payload"`
}

// Previews is every class's preview, in the catalogue's order.
func (r *Reporter) Previews(ctx context.Context) ([]Preview, error) {
	now := r.cfg.Now()
	out := make([]Preview, 0, len(catalogue))
	for _, entry := range catalogue {
		facts := make(map[FactKey]FactValue, len(entry.Facts))
		for _, fact := range entry.Facts {
			facts[fact.Key] = fact.example
		}
		var harness *Harness
		if entry.Harness {
			harness = harnessOf("claude-code", "2.1.310")
		}
		report, err := r.reportOf(ctx, pending{entry: entry, site: entry.Site, harness: harness, facts: facts,
			count: entry.Threshold.Count, first: now, last: now})
		if err != nil {
			return nil, fmt.Errorf("issuereport: preview %s: %w", entry.ID, err)
		}
		payload, err := report.Encode()
		if err != nil {
			return nil, err
		}
		out = append(out, Preview{Class: entry.ID, Title: entry.Title, Summary: entry.Summary, Payload: payload})
	}
	return out, nil
}

// Row is one report as a list shows it.
type Row struct {
	Class       string          `json:"class"`
	Title       string          `json:"title"`
	Fingerprint string          `json:"fingerprint"`
	FirstSent   time.Time       `json:"first_sent"`
	LastSent    time.Time       `json:"last_sent"`
	Status      string          `json:"status"`
	Issue       string          `json:"issue"`
	Payload     json.RawMessage `json:"payload"`
}

// List is the company's record of what it reported.
func (r *Reporter) List(ctx context.Context) ([]Row, error) {
	all, err := r.cfg.Store.ListIssueReports(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(all))
	for _, rec := range all {
		title := string(rec.Class)
		if entry, ok := entryOf(Class(rec.Class)); ok {
			title = entry.Title
		}
		out = append(out, Row{Class: rec.Class, Title: title, Fingerprint: rec.Fingerprint,
			FirstSent: rec.FirstSent, LastSent: rec.ClaimedAt, Status: string(rec.Status), Issue: rec.Issue,
			Payload: rec.Payload})
	}
	return out, nil
}
