package main

import (
	"net/http"
	"strings"
	"testing"
)

// EVERY CLIENT RENDERS THE HINT, because the hint is the half that says what
// to do. A drain's refusal is `draining` plus a detail and a hint, and a
// client that decodes the code alone reports "this node cannot serve that:
// draining" — true, and useless to the operator holding it.
func TestANodeRefusalCarriesItsDetailAndHintToTheOperator(t *testing.T) {
	t.Parallel()
	body := []byte(`{"error":"draining",` +
		`"detail":"this node is draining for a shutdown: the turns already ` +
		`running finish, and nothing new is started here",` +
		`"hint":"retry against another node, or once this one has restarted; ` +
		`/ready answers 503 for as long as the drain lasts"}`)
	err := nodeError(http.StatusServiceUnavailable, body, true)
	if err == nil {
		t.Fatal("a 503 was not reported as an error")
	}
	for _, want := range []string{
		"draining",
		"nothing new is started here",
		"retry against another node",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, err)
		}
	}
	// AND NOT THE GUESS. The fallback names a build without a backend,
	// which is the other thing a 503 means here and the wrong one to
	// print when the body said which.
	if strings.Contains(err.Error(), "without the backend") {
		t.Errorf("the refusal guessed a reason the body had already given:\n%s", err)
	}
}

func TestARefusalWithNothingBesideTheCodeIsLeftAlone(t *testing.T) {
	t.Parallel()
	if got := withRefusalDetail("draining", "", ""); got != "draining" {
		t.Errorf("withRefusalDetail with no extras = %q, want the message unchanged", got)
	}
	if got := withRefusalDetail("draining", "", "retry elsewhere"); got != "draining\n  retry elsewhere" {
		t.Errorf("withRefusalDetail skipping an absent detail = %q", got)
	}
}
