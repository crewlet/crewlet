package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
type poller struct {
	// route is the job's notes route, which holds the run's token in its
	// path: never printed ([scrub]).
	route  string
	client *http.Client
}

// newPoller is a poller of route, refused where route is no http(s) address.
func newPoller(route string) (*poller, error) {
	u, err := url.Parse(route)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// The parse error quotes the route, so it is not wrapped.
		return nil, errors.New("the notes route is not an http or https address")
	}
	return &poller{
		route: strings.TrimSuffix(route, "/"),
		client: &http.Client{
			// THE ROUTE NEVER REDIRECTS, and one followed would carry the
			// token's path to wherever it pointed: a 3xx is a failed poll.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// poll is the notes past after, oldest first, asked for within bound.
func (p *poller) poll(ctx context.Context, after int, bound time.Duration) ([]note, error) {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.route+"/notes?after="+strconv.Itoa(after), nil)
	if err != nil {
		return nil, p.scrub(err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, p.scrub(err)
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
		return nil, p.scrub(err)
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

// scrub is err with the route taken out: net/http's errors quote the whole
// address, token and all, and the run's error stream is no place for it.
func (p *poller) scrub(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		err = u.Err
	}
	return errors.New("polling the notes route: " + strings.ReplaceAll(err.Error(), p.route, "the notes route"))
}
