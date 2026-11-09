package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
)

func doctorNode(t *testing.T, answer advisor.Answer) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/advisor" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(server.Close)
	return bootstrapForURL(t, server.URL)
}

var doctorAnswer = advisor.Answer{Revision: "rev-7", Findings: []advisor.Finding{
	{Class: "durability.no_backup", Severity: advisor.SeverityBlocks, Actor: advisor.ActorOwner,
		Subject: advisor.Subject{Kind: advisor.SubjectCompany}, Title: "No verified backup.",
		Why: "One server, and nothing copied off it."},
	{Class: advisor.ClassPersonRequestsWithoutAsk, Severity: advisor.SeverityNote, Actor: advisor.ActorEngine,
		Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: "owner", Name: "The Owner"},
		Title:   "Agents named a person without asking them a question.", Why: "Named 6 times.",
		Evidence: []advisor.Fact{{Key: "named", Value: 6}}, Records: []string{"c-1"}},
}}

// THE DOCTOR PRINTS CLASSES, SUBJECTS AND COUNTS, never a finding's words unless
// asked, and exits non-zero while a finding blocks the company.
func TestTheDoctorPrintsCountsAndFailsOnAFindingThatBlocks(t *testing.T) {
	t.Parallel()
	config := doctorNode(t, doctorAnswer)
	out, _, err := cli(t, "doctor", config)
	if err == nil || !strings.Contains(err.Error(), "durability.no_backup") {
		t.Fatalf("a blocking finding exited %v, want an error naming its class", err)
	}
	for _, want := range []string{"SEVERITY", "blocks", "durability.no_backup", "company",
		"note", "engine", "person.requests_without_ask", "seat:owner", "named=6", "rev-7"} {
		if !strings.Contains(out, want) {
			t.Errorf("the doctor does not print %q:\n%s", want, out)
		}
	}
	for _, words := range []string{"No verified backup", "Named 6 times", "The Owner"} {
		if strings.Contains(out, words) {
			t.Errorf("the doctor printed %q without -words:\n%s", words, out)
		}
	}
	out, _, _ = cli(t, "doctor", "-words", config)
	if !strings.Contains(out, "No verified backup. One server, and nothing copied off it.") {
		t.Errorf("-words does not print the finding's title and why:\n%s", out)
	}
	out, _, err = cli(t, "doctor", "-json", config)
	var got advisor.Answer
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil || len(got.Findings) != 2 || err == nil {
		t.Errorf("-json printed %q (%v) and exited %v, want the node's answer and a non-zero exit", out, jerr, err)
	}
}

func TestTheDoctorExitsZeroWhenNothingBlocks(t *testing.T) {
	t.Parallel()
	config := doctorNode(t, advisor.Answer{Revision: "rev-8", Findings: doctorAnswer.Findings[1:]})
	if _, _, err := cli(t, "doctor", config); err != nil {
		t.Fatalf("a company with no blocking finding exited %v", err)
	}
	out, _, err := cli(t, "doctor", doctorNode(t, advisor.Answer{Revision: "rev-9", Findings: []advisor.Finding{}}))
	if err != nil || !strings.Contains(out, "No findings at revision rev-9.") {
		t.Fatalf("an empty answer printed %q and exited %v", out, err)
	}
}
