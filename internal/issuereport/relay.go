package issuereport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/httpx"
)

const (
	// RelayURL is where a report goes. A constant: the document has no say in
	// it, and the engine carries no credential for it or for anything behind
	// it.
	RelayURL = "https://reports.crewlet.ai/v1/reports"
	// requestTimeout bounds one attempt: the relay asks GitHub a search and a
	// create, each answered in seconds at its worst.
	requestTimeout = 20 * time.Second
	// maxAnswerBytes bounds what is read of an answer, which is a status and
	// an address.
	maxAnswerBytes = 4 << 10
)

// issueAddress is the one address a relay's answer may name: an issue on the
// engine's own repository. An administrator is offered it as a link.
var issueAddress = regexp.MustCompile(`^https://github\.com/crewlet/crewlet/issues/[0-9]+$`)

// NewClient is the client a report goes out on: held to public addresses, and
// sending nothing on to wherever a redirect points.
func NewClient() *http.Client {
	client := httpx.Guarded(requestTimeout)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

// answer is what the relay said of a report it took.
type answer struct {
	status coord.IssueReportStatus
	issue  string
}

// refusedError is a relay's answer other than taking a report.
type refusedError struct {
	status int
	retry  bool
	after  time.Duration
}

func (e *refusedError) Error() string { return "the relay answered " + strconv.Itoa(e.status) }

// deliver is one attempt. NO HEADER SAYS ANYTHING: the body is the whole report,
// and no credential, cookie or engine address goes with it.
func (r *Reporter) deliver(ctx context.Context, body []byte) (answer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return answer{}, &refusedError{}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "crewlet-engine")
	resp, err := r.cfg.Client.Do(req)
	if err != nil {
		return answer{}, fmt.Errorf("the relay did not answer: %w", errors.Unwrap(err))
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return readAnswer(resp)
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		http.StatusInternalServerError:
		return answer{}, &refusedError{status: resp.StatusCode, retry: true, after: retryAfter(resp)}
	}
	return answer{}, &refusedError{status: resp.StatusCode}
}

// readAnswer reads a taken report's answer. An address of any other shape is
// kept as none, and a status this build does not name is a failed send.
func readAnswer(resp *http.Response) (answer, error) {
	var said struct {
		Status string `json:"status"`
		Issue  string `json:"issue"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswerBytes)).Decode(&said); err != nil {
		return answer{}, &refusedError{status: resp.StatusCode}
	}
	out := answer{}
	switch said.Status {
	case "filed":
		out.status = coord.IssueReportFiled
	case "duplicate":
		out.status = coord.IssueReportDuplicate
	default:
		return answer{}, &refusedError{status: resp.StatusCode}
	}
	if issueAddress.MatchString(said.Issue) {
		out.issue = said.Issue
	}
	return out, nil
}

// retryAfter reads a seconds Retry-After, zero for none or a date.
func retryAfter(resp *http.Response) time.Duration {
	seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
