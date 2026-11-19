package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/api/queries"
)

// doctorNode is a node whose Advisor keeps answer's findings open, its latest
// run unable to read answer's unread sources.
func doctorNode(t *testing.T, answer advisor.Answer) string {
	t.Helper()
	ended := time.Date(2030, 1, 7, 9, 0, 0, 0, time.UTC)
	list := queries.AdvisorFindingList{Findings: []queries.AdvisorFinding{},
		LastRun: &queries.AdvisorRunSummary{RunID: "run-" + answer.Revision, Status: "done", EndedAt: &ended,
			Unread: append([]string{}, answer.Unread...)}}
	for i, f := range answer.Findings {
		g := queries.AdvisorFinding{ID: fmt.Sprintf("f-%d", i), Class: string(f.Class), Severity: f.Severity,
			Actor: f.Actor, State: advisor.StateOpen, Title: f.Title, Why: f.Why, Item: f.Item, Names: f.Names,
			Subject: queries.AdvisorSubject{Kind: f.Subject.Kind, Seat: f.Subject.Seat, Unit: f.Subject.Unit,
				Schedule: f.Subject.Schedule, Name: f.Subject.Name}}
		for _, fact := range f.Evidence {
			g.Evidence.Facts = append(g.Evidence.Facts, queries.AdvisorFact{Key: fact.Key, Value: fact.Value})
		}
		list.Findings = append(list.Findings, g)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/advisor/findings" || r.URL.Query().Get("state") != "open" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(server.Close)
	return bootstrapForURL(t, server.URL)
}

var doctorAnswer = advisor.Answer{Revision: "rev-7", Findings: []advisor.Finding{
	{Class: advisor.ClassNoVerifiedBackup, Severity: advisor.SeverityBlocks, Actor: advisor.ActorOwner,
		Subject: advisor.Subject{Kind: advisor.SubjectCompany}, Title: "No verified backup.",
		Why: "One server, and nothing copied off it.", Evidence: []advisor.Fact{{Key: "logs", Value: 12}}},
	{Class: advisor.ClassScheduleFrequent, Severity: advisor.SeverityNote, Actor: advisor.ActorOwner,
		Subject: advisor.Subject{Kind: advisor.SubjectSchedule, Seat: "sre", Schedule: "invoice-sync",
			Name: "invoice-sync"}, Title: "It runs 24 times a day.", Why: "Each run starts a turn."},
	{Class: advisor.ClassPersonRequestsWithoutAsk, Severity: advisor.SeverityNote, Actor: advisor.ActorEngine,
		Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: "jordan", Name: "Jordan"},
		Title:   "Agents named a person without asking them a question.", Why: "Named 6 times.",
		Evidence: []advisor.Fact{{Key: "named", Value: 6}}, Records: []advisor.Record{{Kind: advisor.RecordComment, ID: "c-1"}}},
	{Class: advisor.ClassAuxiliaryModel, Severity: advisor.SeverityNote, Actor: advisor.ActorOwner,
		Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: "jordan", Name: "Jordan"},
		Title:   "It learns on its own model.", Why: "Learning costs its spend.", Item: "sonnet-large"},
}}

// THE DOCTOR NAMES NOTHING THE COMPANY CHOSE BY DEFAULT: subjects are a kind
// and an ordinal, the same subject the same ordinal; handles and words only
// with -words, below the table.
func TestTheDoctorPrintsKindsOrdinalsAndCountsUnlessAskedForWords(t *testing.T) {
	t.Parallel()
	config := doctorNode(t, doctorAnswer)
	out, _, _ := cli(t, "doctor", config)
	for _, want := range []string{"SEVERITY", "blocks", "durability.no_verified_backup", "company",
		"logs=12", "schedule#1", "seat#1", "named=6", "Last checked at 2030-01-07T09:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("the doctor does not print %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "seat#1") != 2 || strings.Contains(out, "seat#2") {
		t.Errorf("one subject named twice is not one ordinal:\n%s", out)
	}
	for _, chosen := range []string{"sre", "invoice-sync", "jordan", "Jordan", "No verified backup", "Named 6 times", "sonnet-large"} {
		if strings.Contains(out, chosen) {
			t.Errorf("the doctor printed %q without -words:\n%s", chosen, out)
		}
	}
	out, _, _ = cli(t, "doctor", "-words", config)
	for _, want := range []string{"schedule:sre:invoice-sync", "seat:jordan",
		"1. No verified backup. One server, and nothing copied off it.",
		"   seat:jordan: sonnet-large"} {
		if !strings.Contains(out, want) {
			t.Errorf("-words does not print %q:\n%s", want, out)
		}
	}
	header := strings.SplitN(out, "\n", 2)[0]
	if len(header) > 100 {
		t.Errorf("-words widened the table to %d columns:\n%s", len(header), out)
	}
}

// A CLASS SAID FOR SEVERAL SUBJECTS IS ONE ROW naming them all, with each one's evidence, and
// with -words its words once and each subject's item under it. Mutation: print a row per
// finding, and the class is listed twice.
func TestTheDoctorSaysAClassOnceForEverySubject(t *testing.T) {
	t.Parallel()
	learns := func(seat, item string) advisor.Finding {
		return advisor.Finding{Class: advisor.ClassAuxiliaryModel, Severity: advisor.SeverityNote,
			Actor: advisor.ActorOwner, Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: seat},
			Title: "Agents that learn on their main model.", Why: "Learning costs its spend.", Item: item,
			Evidence: []advisor.Fact{{Key: "seats", Value: 1}}}
	}
	config := doctorNode(t, advisor.Answer{Revision: "rev-1", Findings: []advisor.Finding{
		learns("ada", "opus"), learns("bo", "sonnet")}})
	out, _, _ := cli(t, "doctor", config)
	if strings.Count(out, string(advisor.ClassAuxiliaryModel)) != 1 || !strings.Contains(out, "seat#1,seat#2") ||
		!strings.Contains(out, "seats=1; seats=1") {
		t.Errorf("two seats of one class are not one row:\n%s", out)
	}
	out, _, _ = cli(t, "doctor", "-words", config)
	if strings.Count(out, "Learning costs its spend.") != 1 || !strings.Contains(out, "   seat:ada: opus") ||
		!strings.Contains(out, "   seat:bo: sonnet") {
		t.Errorf("-words does not say the class once with each seat under it:\n%s", out)
	}
}

// A FINDING THAT BLOCKS EXITS 2, apart from 1 for a node the command could not
// ask, with -json as without it.
func TestTheDoctorExitsTwoOnABlockAndOneWhenItCannotAsk(t *testing.T) {
	t.Parallel()
	config := doctorNode(t, doctorAnswer)
	for _, args := range [][]string{{"doctor", config}, {"doctor", "-json", config}} {
		out, _, err := cli(t, args...)
		var exit *exitError
		if !errors.As(err, &exit) || exit.code != exitBlocked {
			t.Errorf("%v exited %v, want status %d", args, err, exitBlocked)
		}
		if args[1] == "-json" {
			var got doctorReading
			if jerr := json.Unmarshal([]byte(out), &got); jerr != nil || len(got.Findings) != 4 {
				t.Errorf("-json printed %q (%v), want the node's answer", out, jerr)
			}
		}
	}
	if !strings.Contains(func() string { _, _, err := cli(t, "doctor", config); return err.Error() }(),
		"1 finding says the company cannot work as configured: \"durability.no_verified_backup\"") {
		t.Error("the blocking error does not name its class once")
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	_, _, err := cli(t, "doctor", bootstrapForURL(t, down.URL))
	var exit *exitError
	if err == nil || errors.As(err, &exit) {
		t.Errorf("a node that could not answer exited %v, want an ordinary failure (status 1)", err)
	}
}

func TestTheDoctorExitsZeroWhenNothingBlocks(t *testing.T) {
	t.Parallel()
	config := doctorNode(t, advisor.Answer{Revision: "rev-8", Findings: doctorAnswer.Findings[1:]})
	if _, _, err := cli(t, "doctor", config); err != nil {
		t.Fatalf("a company with no blocking finding exited %v", err)
	}
	out, _, err := cli(t, "doctor", doctorNode(t, advisor.Answer{Revision: "rev-9", Findings: []advisor.Finding{}}))
	if err != nil || !strings.Contains(out, "No open findings.") {
		t.Fatalf("an empty answer printed %q and exited %v", out, err)
	}
}

// WHAT THE NODE COULD NOT READ IS SAID AND FAILS CLOSED: an empty or short
// list never reads as all clear, and exits 1 as a failure to ask, with -json
// as without it, unless a listed finding blocks; two integrations are two
// subjects.
func TestTheDoctorSaysWhatItCouldNotReadAndNamesEachIntegrationApart(t *testing.T) {
	t.Parallel()
	unread := doctorNode(t, advisor.Answer{Revision: "rev-10",
		Findings: []advisor.Finding{}, Unread: []string{advisor.UnreadIntegrations}})
	out, _, err := cli(t, "doctor", unread)
	var exit *exitError
	if err == nil || errors.As(err, &exit) || !strings.Contains(out, "No open findings.") ||
		!strings.Contains(out, `Not read: "integrations".`) {
		t.Fatalf("an answer with integrations unread printed %q and exited %v, want status 1", out, err)
	}
	if _, _, err := cli(t, "doctor", "-json", unread); err == nil || errors.As(err, &exit) {
		t.Errorf("-json with integrations unread exited %v, want status 1", err)
	}
	blocked := doctorNode(t, advisor.Answer{Revision: "rev-12",
		Findings: doctorAnswer.Findings[:1], Unread: []string{advisor.UnreadIntegrations}})
	if _, _, err := cli(t, "doctor", blocked); !errors.As(err, &exit) || exit.code != exitBlocked {
		t.Errorf("a listed block with integrations unread exited %v, want status %d", err, exitBlocked)
	}
	integration := func(name string) advisor.Finding {
		return advisor.Finding{Class: "integration.credential_rejected", Severity: advisor.SeverityNote,
			Actor: advisor.ActorOwner, Subject: advisor.Subject{Kind: advisor.SubjectIntegration, Name: name}}
	}
	config := doctorNode(t, advisor.Answer{Revision: "rev-11",
		Findings: []advisor.Finding{integration("GitHub"), integration("Linear")}, Unread: []string{}})
	out, _, _ = cli(t, "doctor", config)
	if !strings.Contains(out, "integration#1") || !strings.Contains(out, "integration#2") ||
		strings.Contains(out, "Not read") {
		t.Errorf("two integrations are not two ordinals, or an empty unread list was said:\n%s", out)
	}
	out, _, _ = cli(t, "doctor", "-words", config)
	if !strings.Contains(out, "integration:GitHub") || !strings.Contains(out, "integration:Linear") {
		t.Errorf("-words does not name each integration by its product:\n%s", out)
	}
}

// AN ADVISOR THAT HAS NOT RUN FAILS CLOSED: an empty list before its first run
// says nothing about what blocks, so it exits 1 as a failure to ask.
func TestTheDoctorFailsClosedBeforeTheAdvisorsFirstRun(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(queries.AdvisorFindingList{Findings: []queries.AdvisorFinding{}})
	}))
	t.Cleanup(server.Close)
	out, _, err := cli(t, "doctor", bootstrapForURL(t, server.URL))
	var exit *exitError
	if err == nil || errors.As(err, &exit) || !strings.Contains(out, "The Advisor has not run yet.") {
		t.Fatalf("before the first run the doctor printed %q and exited %v, want status 1", out, err)
	}
}
