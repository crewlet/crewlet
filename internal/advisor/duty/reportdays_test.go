package duty_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
)

// dayReader is a reader that reads a week day by day as well.
type dayReader struct {
	*fakeReader
	days  []advisor.Day
	err   error
	asked [][2]time.Time
}

func (r *dayReader) Days(_ context.Context, from, to time.Time) ([]advisor.Day, error) {
	r.asked = append(r.asked, [2]time.Time{from, to})
	return r.days, r.err
}

// A WEEK'S REPORT KEEPS ITS DAYS, read over the week it covers, so the page
// draws the week across its days; a reader that cannot read them still has
// the report written, with none. Mutation: read the days over another week,
// or fail the report when they cannot be read, and a check fails.
func TestAWeeksReportKeepsItsDays(t *testing.T) {
	t.Parallel()
	monday := time.Date(2030, 1, 14, 0, 0, 0, 0, time.UTC)
	days := []advisor.Day{{Day: "2030-01-07", Turns: 4, TurnsNothing: 1, Tokens: 900}}
	for _, c := range []struct {
		name string
		err  error
		want []advisor.Day
	}{
		{"read", nil, days},
		{"unread", errors.New("the ledger is down"), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			l := &fakeLog{reports: map[string]advisor.Report{}}
			r := &dayReader{fakeReader: &fakeReader{signature: "r1"}, days: days, err: c.err}
			d := newDuty(l, r.fakeReader, &fakeModel{}, &clock{now: monday.Add(6*time.Hour + time.Minute)})
			d.Read = r
			step(t, d)
			report, written := l.reports["2030-W02"]
			if !written {
				t.Fatalf("no report; wrote %v", l.order)
			}
			if !slices.Equal(report.Days, c.want) {
				t.Errorf("days = %+v, want %+v", report.Days, c.want)
			}
			if len(r.asked) != 1 || !r.asked[0][0].Equal(report.From) || !r.asked[0][1].Equal(report.To) {
				t.Errorf("read the days of %v, want the report's week %s to %s", r.asked, report.From, report.To)
			}
		})
	}
}
