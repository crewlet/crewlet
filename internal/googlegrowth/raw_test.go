package googlegrowth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// A REPORT GOOGLE SENDS AS BYTES IS READ AS TEXT, AND ONE TOO LARGE SAYS SO:
// a method answering a CSV is handed to the agent as its type and text, never
// a decode failure, and one past the bound is a refusal naming what to ask
// for, recorded as an outcome not known.
//
// Mutation: send a raw request down the JSON path, and the CSV fails to decode.
func TestAReportSentAsBytesIsReadAsText(t *testing.T) {
	t.Parallel()
	var huge atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		if huge.Load() {
			_, _ = w.Write([]byte(strings.Repeat("x,", 5<<20)))
			return
		}
		_, _ = w.Write([]byte("DATE,CLICKS\n2026-11-20,3\n"))
	}))
	t.Cleanup(srv.Close)
	c := &googlegrowth.Client{Kind: "x", Name: "X", Grant: &renewingGrant{},
		Company: func() *config.Company { return &config.Company{Name: "Acme"} },
		Env:     func() *config.Resolver { return nil }, HTTP: &http.Client{Transport: httpxtest.Rewrite(t, srv)}}
	op := growth.Operation{ID: "x.reports.generateCsv", Method: "GET", Resource: "reports", Class: growth.ClassRead,
		Path: "https://x.googleapis.com/v1/accounts/{accountsId}/reports:generateCsv"}
	req, err := googlegrowth.NewRequest(op, map[string]string{"accountsId": "1"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Raw = true
	out, err := c.SendAndRead(context.Background(), "finance", req, nil)
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ ContentType, Data string }
	if err := json.Unmarshal([]byte(out.Answer), &body); err != nil || body.ContentType != "text/csv" ||
		body.Data != "DATE,CLICKS\n2026-11-20,3\n" {
		t.Errorf("the CSV was answered as %s (%v)", out.Answer, err)
	}
	huge.Store(true)
	_, err = c.SendAndRead(context.Background(), "finance", req, nil)
	var ce *googlegrowth.CallError
	var r *growth.Refusal
	if !errors.As(err, &ce) || ce.Failure != growth.FailureUnknownOutcome || !errors.As(err, &r) ||
		!strings.Contains(r.Said, "narrow it") {
		t.Errorf("a CSV past the bound answered %v, want a refusal saying to narrow it", err)
	}
}
