package insights

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// The run's cadence: a send pass every ten minutes, so a record leaves close to
// its random time, and an evaluation every hour, since a day is what counts.
const (
	sendEvery     = 10 * time.Minute
	evaluateEvery = time.Hour
)

// Run evaluates and sends on the run's cadence until ctx ends. A failed pass is
// logged and tried again on the next tick: nothing is lost by waiting.
func (s *Sharer) Run(ctx context.Context) {
	send := time.NewTicker(sendEvery)
	defer send.Stop()
	var evaluated time.Time
	for {
		if now := s.Now(); now.Sub(evaluated) >= evaluateEvery {
			evaluated = now
			if err := s.Evaluate(ctx); err != nil {
				log.WarnContext(ctx, "insights_not_evaluated", "error", err.Error())
			}
		}
		if err := s.SendDue(ctx); err != nil {
			log.WarnContext(ctx, "insights_not_sent", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-send.C:
		}
	}
}

// maxAnswerBytes bounds what is read of the relay's answer, which says nothing
// the engine keeps.
const maxAnswerBytes = 4 << 10

// NewSender posts a record's bytes to url on client. NO HEADER SAYS ANYTHING:
// the body is the whole record, and no credential or engine address goes with it.
func NewSender(client *http.Client, url string) func(ctx context.Context, body []byte) (int, error) {
	return func(ctx context.Context, body []byte) (int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return 0, fmt.Errorf("insights: build the request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "crewlet-engine")
		resp, err := client.Do(req)
		if err != nil {
			return 0, fmt.Errorf("insights: the relay did not answer: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		// DRAINED so the connection is reused; the answer carries nothing kept.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswerBytes))
		switch resp.StatusCode {
		case http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent:
			return resp.StatusCode, nil
		}
		return resp.StatusCode, fmt.Errorf("insights: the relay answered %d", resp.StatusCode)
	}
}
