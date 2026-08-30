package atlassian_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
)

func TestTheAuthSchemeIsChosenByTheAddress(t *testing.T) {
	t.Parallel()
	// Cloud rejects a bare bearer API token and Data Center rejects Basic
	// with an empty user — the same credential, refused purely on which
	// header carried it. The presence of an email is the discriminator.
	if got := atlassian.AuthHeader("", "pat"); got != "Bearer pat" {
		t.Errorf("no address = %q, want a bearer token", got)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("a@example.com:tok"))
	if got := atlassian.AuthHeader("a@example.com", "tok"); got != want {
		t.Errorf("with an address = %q, want %q", got, want)
	}
}

func TestTheCloudHostListCoversTheGatewayAsWellAsASite(t *testing.T) {
	t.Parallel()
	// THE DRIFT THIS LIST EXISTS TO END. The tracker's copy carried
	// .atlassian.com and the wiki's did not, so an api.atlassian.com
	// gateway address was Cloud to one product and Data Center to the
	// other — which selects a different REST version and a different
	// identity field, on one product only.
	for _, cloud := range []string{
		"https://acme.atlassian.net",
		"https://api.atlassian.com/ex/jira/abc-123",
		"https://api.atlassian.com/ex/confluence/abc-123",
		"https://acme.jira.com",
	} {
		if !atlassian.IsCloud(cloud) {
			t.Errorf("IsCloud(%q) = false, want true", cloud)
		}
	}
	for _, dc := range []string{
		"https://jira.internal.example.com",
		"https://wiki.example.com",
		"http://localhost:8080",
	} {
		if atlassian.IsCloud(dc) {
			t.Errorf("IsCloud(%q) = true, want false", dc)
		}
	}
	// An unparseable address answers Data Center, which is the SAFE
	// direction everywhere it is consulted: it selects the older REST
	// version, which fails loudly with the version in the path, and it
	// refuses provisioning rather than calling an admin API that is not
	// there.
	if atlassian.IsCloud("://not a url") {
		t.Error("an unparseable address was read as Cloud")
	}
}

func TestARefusalIsTypedAndCarriesItsSurface(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	tr, err := atlassian.NewTransport("jira", srv.URL, "Bearer x", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = tr.Do(context.Background(), http.MethodGet, "/myself", nil, nil, nil)

	var api *atlassian.APIError
	if !errors.As(err, &api) {
		t.Fatalf("err = %v, want *atlassian.APIError", err)
	}
	if api.Status != http.StatusForbidden || api.Surface != "jira" || api.Detail != "nope" {
		t.Fatalf("error = %+v", api)
	}
	// The surface is in the message because one error type now serves
	// three planes, and an operator reading a bare status has no way to
	// tell which of them refused them.
	if msg := api.Error(); !strings.HasPrefix(msg, "jira: GET /myself: 403") {
		t.Fatalf("message = %q", msg)
	}
	if got := atlassian.StatusOf(err); got != http.StatusForbidden {
		t.Errorf("StatusOf = %d", got)
	}
	if got := atlassian.StatusOf(errors.New("plain")); got != 0 {
		t.Errorf("StatusOf(plain error) = %d, want 0", got)
	}
}

func TestAnErrorBodyIsCappedSoAReportStaysReadable(t *testing.T) {
	t.Parallel()
	// Atlassian answers some refusals with a whole HTML page, and this
	// detail goes into a log line and an operator's terminal.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 8192)))
	}))
	defer srv.Close()

	tr, _ := atlassian.NewTransport("atlassian", srv.URL, "Bearer x", srv.Client())
	err := tr.Do(context.Background(), http.MethodGet, "/anything", nil, nil, nil)
	var api *atlassian.APIError
	if !errors.As(err, &api) {
		t.Fatalf("err = %v", err)
	}
	if len(api.Detail) != 2048 {
		t.Fatalf("detail is %d bytes, want it capped at 2048", len(api.Detail))
	}
}

func TestATransportWithNothingToSayIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()
	// Refused HERE rather than at the first request: a client built with
	// no address or no credential fails only once a run is already under
	// way, which for a provisioner is after it has created something.
	if _, err := atlassian.NewTransport("jira", "  ", "Bearer x", nil); err == nil {
		t.Error("a transport with no address was accepted")
	}
	if _, err := atlassian.NewTransport("jira", "https://x.example.com", " ", nil); err == nil {
		t.Error("a transport with no credential was accepted")
	}
}

func TestSilenceAndARefusalAreDifferentFacts(t *testing.T) {
	t.Parallel()
	// They lead to OPPOSITE reports. A refusal is proof the server
	// processed the request and declined it, so nothing was created. No
	// answer at all means the request may have landed and had its response
	// lost — so whatever it would have created may exist, with no id
	// anywhere to reach it by. A caller that could only ask "did it fail"
	// had to guess which.
	refuses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer refuses.Close()
	tr, _ := atlassian.NewTransport("atlassian", refuses.URL, "Bearer x", refuses.Client())

	var lost *atlassian.TransportError
	err := tr.Do(context.Background(), http.MethodPost, "/create", nil, map[string]any{}, nil)
	if errors.As(err, &lost) {
		t.Fatalf("a refusal was reported as silence: %v", err)
	}
	if atlassian.StatusOf(err) != http.StatusConflict {
		t.Fatalf("err = %v, want a typed refusal", err)
	}

	// The answer is aborted mid-flight, which is what a real one lost in
	// transit looks like to the client.
	drops := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer drops.Close()
	tr, _ = atlassian.NewTransport("atlassian", drops.URL, "Bearer x", drops.Client())
	err = tr.Do(context.Background(), http.MethodPost, "/create", nil, map[string]any{}, nil)
	if !errors.As(err, &lost) {
		t.Fatalf("err = %v, want *atlassian.TransportError", err)
	}
	if lost.Method != http.MethodPost || lost.Path != "/create" {
		t.Errorf("error = %+v, want it to name the request that went unanswered", lost)
	}

	// AN UNREADABLE ANSWER IS THE SAME FACT: the server accepted the
	// request and this process holds nothing to address the result with.
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer garbage.Close()
	tr, _ = atlassian.NewTransport("atlassian", garbage.URL, "Bearer x", garbage.Client())
	var out struct {
		ID string `json:"id"`
	}
	if err := tr.Do(context.Background(), http.MethodPost, "/create", nil, map[string]any{}, &out); !errors.As(err, &lost) {
		t.Fatalf("err = %v, want an unreadable answer to be silence too", err)
	}
}

func TestACredentialThatIsAlreadyAHeaderIsSentAsItIs(t *testing.T) {
	t.Parallel()
	// THE SHAPE THAT WAS SILENTLY BROKEN. A seat running an HTTP MCP server
	// declares its credential as the header itself, and the grammar keeps
	// that value whole on purpose — the payload of a Basic header is already
	// base64(email:token), so there is nothing left to encode. Wrapping it
	// again produced "Bearer Basic …", which every Atlassian surface refuses:
	// the seat's identity never resolved and it received no Jira and no
	// Confluence events at all, with nothing failing anywhere.
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("a@example.com:tok"))
	if got := atlassian.AuthHeader("", basic); got != basic {
		t.Errorf("header = %q, want it passed through unchanged", got)
	}
	// An address beside it changes nothing: the header already carries one,
	// and re-encoding would authenticate as nobody.
	if got := atlassian.AuthHeader("someone@example.com", basic); got != basic {
		t.Errorf("header = %q, want the whole header to win", got)
	}
	// A bearer value the grammar did not strip is the same case.
	if got := atlassian.AuthHeader("", "Bearer dc-pat"); got != "Bearer dc-pat" {
		t.Errorf("header = %q", got)
	}
	// The scheme name is case-insensitive per RFC 9110, and this value is
	// typed by hand into a YAML file.
	if got := atlassian.AuthHeader("", "basic "+base64.StdEncoding.EncodeToString([]byte("a:b"))); !strings.HasPrefix(got, "basic ") {
		t.Errorf("header = %q, want a lowercase scheme recognised too", got)
	}
}

func TestNoCredentialIsRefusedAtConstructionRatherThanAtTheFirstRequest(t *testing.T) {
	t.Parallel()
	// "Bearer " is not a credential, and it is not blank either — so it
	// walked straight past the guard that exists to fail a client while
	// there is still a caller to tell.
	if got := atlassian.AuthHeader("", "   "); got != "" {
		t.Fatalf("AuthHeader with no token = %q, want empty", got)
	}
	if _, err := atlassian.NewTransport("jira", "https://acme.atlassian.net",
		atlassian.AuthHeader("someone@example.com", ""), nil); err == nil {
		t.Error("a client with no credential was built")
	}
}

func TestALostAnswerNamesTheVerbAsWellAsThePath(t *testing.T) {
	t.Parallel()
	// Three of these routes serve three verbs on one path, and "the account
	// listing did not answer" and "the account CREATE did not answer" are
	// the two facts furthest apart in what they cost.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()
	tr, _ := atlassian.NewTransport("atlassian", srv.URL, "Bearer x", srv.Client())
	err := tr.Do(context.Background(), http.MethodPost, "/service-accounts", nil, map[string]any{}, nil)
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), "POST /service-accounts") {
		t.Fatalf("message = %q, want it to name the verb", err.Error())
	}
}
