package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// skillQueue bounds the loads waiting to be reported. A report takes
// milliseconds and a model loads a skill a few times a run, so 16 is never
// reached; one past it is dropped, which fails closed: the bridge's guard
// refuses the skill's tool and names the skill to load again.
const skillQueue = 16

// reportBound bounds one report, as OpenCode's skill plugin's does
// (opencode_skills.js, reportAbortMs): one write the bridge answers in
// milliseconds.
const reportBound = 5 * time.Second

// reporter reports each skill the harness loaded to the skill route
// (mcpbridge.SkillPattern), as OpenCode's plugin does: one POST of
// {"skill": name}, never made again; the route's 401 or 404 ends the
// reports for the rest of the run.
type reporter struct {
	*route
	queue chan string
}

func newReporter(r *route) *reporter {
	return &reporter{route: r, queue: make(chan string, skillQueue)}
}

// loaded queues name's load, never blocking: the adapter calls it from the
// harness's reader.
func (r *reporter) loaded(name string) {
	select {
	case r.queue <- name:
	default:
	}
}

// run reports each queued load until ctx ends or the route refuses the
// helper.
func (r *reporter) run(ctx context.Context, logger *log.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case name := <-r.queue:
			refused, err := r.report(ctx, name)
			if err != nil && ctx.Err() == nil {
				logger.Print(err)
			}
			if refused {
				return
			}
		}
	}
}

// report posts one load: refused where the route ends the reports.
func (r *reporter) report(ctx context.Context, name string) (refused bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, reportBound)
	defer cancel()
	body, err := json.Marshal(map[string]string{"skill": name})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return false, r.scrub("reporting skill "+name+" to", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return false, r.scrub("reporting skill "+name+" to", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound:
		return true, &statusError{op: "reporting skill " + name + " to " + r.name, status: resp.Status,
			then: "; no more loads are reported this run"}
	case resp.StatusCode/100 != 2:
		return false, &statusError{op: "reporting skill " + name + " to " + r.name, status: resp.Status}
	}
	return false, nil
}

// statusError is a route's answer that is no success.
type statusError struct{ op, status, then string }

func (e *statusError) Error() string { return e.op + ": it answered " + e.status + e.then }
