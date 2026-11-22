package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// note is one note as the notes route serves it (mcpbridge.SteerNote): the
// fields the helper reads.
type note struct {
	Seq     int    `json:"seq"`
	ID      string `json:"id"`
	Message string `json:"message"`
}

// errRefused is the route refusing the helper for the rest of the run: its
// token no longer resolves (401), or the run has no notes route (404).
var errRefused = errors.New("the notes route refused the helper; its notes wait for the run's review")

// maxAnswer bounds one poll's answer: a poll names at most the notes one job
// can be owed (24, mcpbridge's steerReadIDs), each an item of at most 4,505
// UTF-16 units (internal/engine's maxLateItemUnits), at most six bytes a unit
// escaped: 649 KB, under 1 MiB with every field and bracket around them.
const maxAnswer = 1 << 20

// poller asks the notes route for the notes past a cursor.
type poller struct{ *route }

// poll is the notes past after, oldest first, asked for within bound.
func (p *poller) poll(ctx context.Context, after int, bound time.Duration) ([]note, error) {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url+"/notes?after="+strconv.Itoa(after), nil)
	if err != nil {
		return nil, p.scrub("polling", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, p.scrub("polling", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w (%s)", errRefused, resp.Status)
	case resp.StatusCode != http.StatusOK:
		// A 503 among them: an opening ending or a node stopping, paused on.
		return nil, fmt.Errorf("polling the notes route: it answered %s", resp.Status)
	}
	var answer struct {
		Notes []note `json:"notes"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return nil, p.scrub("polling", err)
	}
	if len(body) > maxAnswer {
		return nil, fmt.Errorf("polling the notes route: its answer is past %d bytes", maxAnswer)
	}
	if err = json.Unmarshal(body, &answer); err != nil || answer.Notes == nil {
		return nil, errors.New("polling the notes route: its answer is not a list of notes")
	}
	for _, n := range answer.Notes {
		if n.Seq <= 0 || n.ID == "" || n.Message == "" {
			return nil, errors.New("polling the notes route: it answered a note with no place, id or text")
		}
	}
	return answer.Notes, nil
}
