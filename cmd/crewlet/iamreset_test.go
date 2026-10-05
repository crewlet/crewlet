package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE INVITATION AND RESET COMMANDS REACH THEIR ROUTES, AND SAY WHAT THEY GOT.
//
// `invitations -all` lists every invitation held; `cancel-invite` withdraws
// one under the reason given; `reset-password` names its person by id or by
// login — a login resolved to the one row holding it EXACTLY — and prints the
// link once with its expiry. The usage names all three. Mutation: send no
// `all`, resolve a login by the first row the substring search answers, or
// drop the link from the printout, and a row fails.
func TestTheInvitationAndResetCommandsReachTheirRoutes(t *testing.T) {
	const person = "0192f00d-0000-7000-8000-0000000000bb"
	var asked []string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/iam/invitations":
			_, _ = w.Write([]byte(`{"invitations":[{"id":"inv-1","state":"expired",` +
				`"email":"sam@example.com","grants":["state:read"]}],"position":"x"}`))
		case r.URL.Path == "/iam/people" && r.URL.Query().Get("after") == "":
			// THE SUBSTRING SEARCH ANSWERS A NEAR MISS FIRST, which a
			// resolution by the first row would take — and the exact
			// holder only on the NEXT page, which a resolution by the
			// first page would never read.
			_, _ = w.Write([]byte(`{"people":[` +
				`{"id":"0192f00d-0000-7000-8000-0000000000aa","login":"jane.doe2"}],` +
				`"next":"0192f00d-0000-7000-8000-0000000000aa"}`))
		case r.URL.Path == "/iam/people":
			_, _ = w.Write([]byte(`{"people":[` +
				`{"id":"` + person + `","login":"jane.doe"}],"next":""}`))
		case strings.HasSuffix(r.URL.Path, "/password-reset"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"` + person + `","outcome":"applied",` +
				`"url":"https://crewlet.example.com/dashboard#/reset/c-1.s3cr3t",` +
				`"expires_at":"2026-06-15T12:00:00Z"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"inv-1","outcome":"applied","position":"p"}`))
		}
	}))
	defer node.Close()
	t.Setenv(apiTokenEnv, "a-tier-a-token")
	cfg := bootstrapWithKeyring(t, "k1")
	cli := func(args ...string) (string, error) {
		var out, errs bytes.Buffer
		err := run(append(append([]string{"iam"}, args...), "-config", cfg,
			"-api", node.URL), &out, &errs)
		return out.String(), err
	}

	listed, err := cli("invitations", "-all")
	if err != nil || !strings.Contains(listed, "expired") || len(asked) != 1 ||
		asked[0] != "GET /iam/invitations?all=true" {
		t.Fatalf("invitations -all asked %v and printed %q (%v)", asked, listed, err)
	}
	if _, err = cli("cancel-invite", "inv-1", "-reason", "wrong address"); err != nil ||
		asked[len(asked)-1] != "DELETE /iam/invitations/inv-1?reason=wrong+address" {
		t.Errorf("cancel-invite asked %v (%v)", asked, err)
	}
	printed, err := cli("reset-password", "jane.doe")
	if err != nil {
		t.Fatalf("reset-password by login: %v", err)
	}
	if want := "POST /iam/people/" + person + "/password-reset?"; asked[len(asked)-1] != want {
		t.Errorf("reset-password by login asked %v, want %s", asked, want)
	}
	if !strings.Contains(printed, "#/reset/c-1.s3cr3t") {
		t.Errorf("the link was not printed:\n%s", printed)
	}
	before := len(asked)
	if _, err = cli("reset-password", person); err != nil || len(asked) != before+1 {
		t.Errorf("reset-password by id looked it up first: %v (%v)",
			asked[before:], err)
	}

	for _, refused := range [][]string{
		{"people", "-all"},
		{"reset-password", person, "-idempotency-key",
			statelog.NewOpID(time.Now(), "x")},
	} {
		if _, err := cli(refused...); err == nil {
			t.Errorf("iam %v was accepted", refused)
		}
	}
	var usage bytes.Buffer
	_ = run([]string{"iam"}, &usage, &bytes.Buffer{})
	for _, sub := range []string{"iam invitations", "iam cancel-invite",
		"iam reset-password"} {
		if !strings.Contains(usage.String(), sub) {
			t.Errorf("the usage does not name %q", sub)
		}
	}
}
