package tracker_test

import "testing"

func TestZZVerifyAbsentVsZero(t *testing.T) {
	r := newRoundTrip(t)
	// Three tasks, NOTHING estimated, NO points, NO due dates.
	for i := 0; i < 3; i++ {
		task := newTask("t-" + itoa(i))
		if _, err := r.writer.CreateTask(t.Context(), "op-"+itoa(i), task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}
	a := r.ask(map[string]any{
		"container": "project:ENG",
		"totals": "estimate_min:sum,estimate_min:avg,estimate_min:min,points:sum," +
			"estimate_min:count,due_at:min,tasks:count,spend_tokens:sum",
	})
	for _, tot := range a.Totals {
		v := "ABSENT"
		if tot.Value != nil {
			v = "present " + ftoa(*tot.Value)
		}
		if tot.At != nil {
			v = "at " + tot.At.String()
		}
		t.Logf("%-22s -> %s", tot.Key, v)
	}
	// Empty set for comparison.
	e := r.ask(map[string]any{
		"container": "project:ENG", "assignee": "nobody-at-all",
		"totals": "estimate_min:sum,estimate_min:avg,tasks:count",
	})
	t.Logf("--- empty matched set ---")
	for _, tot := range e.Totals {
		v := "ABSENT"
		if tot.Value != nil {
			v = "present " + ftoa(*tot.Value)
		}
		t.Logf("%-22s -> %s", tot.Key, v)
	}
}

func ftoa(f float64) string {
	if f == float64(int64(f)) {
		return itoa(int(f))
	}
	return "frac"
}
