package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/secrets"
)

// nodeAt is a node client for a refusal test: an address to name, and a token
// so the 401 arm is the one a sent token takes.
func nodeAt(base string) *nodeClient {
	return &nodeClient{base: base, token: "ops-token", http: httpx.Client(nodeRequestTimeout)}
}

// THE NODE CLIENT RENDERS THE HINT, because the hint is the half that says
// what to do. A drain's refusal is `draining` plus a detail and a hint, and a
// client that decodes the code alone reports "draining" — true, and useless to
// the operator holding it.
func TestANodeRefusalCarriesItsDetailAndHintToTheOperator(t *testing.T) {
	t.Parallel()
	body := []byte(`{"error":"draining",` +
		`"detail":"this node is draining for a shutdown: the turns already ` +
		`running finish, and nothing new is started here",` +
		`"hint":"retry against another node, or once this one has restarted; ` +
		`/ready answers 503 for as long as the drain lasts"}`)
	err := nodeAt("http://node.example.com:8080").refusal(http.MethodGet, "/query/budgets",
		http.StatusServiceUnavailable, "application/json", body)
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
}

// A NODE'S REFUSAL NAMES THE ADDRESS AND THE ROUTE IT ANSWERED. The address
// was resolved from -url or the config's api block, so the operator may never
// have typed it, and asking that address again is how to see a refusal's body
// whole — which needs the method and the path as well as the host. Checked
// over every arm, because each builds its own sentence.
func TestANodeRefusalNamesTheAddressAndTheRouteItAsked(t *testing.T) {
	t.Parallel()
	const base = "http://node.example.com:8080"
	cases := map[string]struct {
		status      int
		contentType string
		body        string
		token       string
	}{
		"the node's 503":              {http.StatusServiceUnavailable, "application/json", `{"error":"draining"}`, "t"},
		"the node's 401, token sent":  {http.StatusUnauthorized, "application/json", `{"error":"invalid_token"}`, "t"},
		"the node's 401, no token":    {http.StatusUnauthorized, "application/json", `{"error":"invalid_token"}`, ""},
		"the node's 400":              {http.StatusBadRequest, "application/json", `{"error":"reason_required"}`, "t"},
		"a body that is not a node's": {http.StatusBadGateway, "text/plain", "upstream reset", "t"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := nodeAt(base)
			c.token = tc.token
			err := c.refusal(http.MethodPost, "/work/T-1/purge", tc.status, tc.contentType, []byte(tc.body))
			if err == nil {
				t.Fatal("a refusal was not reported as an error")
			}
			for _, want := range []string{base, "POST /work/T-1/purge"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q:\n%s", want, err)
				}
			}
		})
	}
}

// A 403 FROM THE NODE SAYS WHAT THE NODE SAID. The purge refuses a call with no
// operator identity with `operator_required` and a detail naming what is
// missing; a sentence about checking the token against api.auth.tokens over
// that would send the operator to a credential the node never objected to.
func TestTheNodesForbiddenIsItsOwnWordsRatherThanATokenGuess(t *testing.T) {
	t.Parallel()
	body := []byte(`{"error":"operator_required","detail":"a purge is an operator ` +
		`gesture and this request carries no operator identity"}`)
	err := nodeAt("http://node.example.com:8080").refusal(http.MethodPost, "/work/T-1/purge",
		http.StatusForbidden, "application/json", body)
	if err == nil {
		t.Fatal("a 403 was not reported as an error")
	}
	if strings.Contains(err.Error(), "api.auth.tokens") {
		t.Errorf("the node's operator_required was read as a refused token:\n%s", err)
	}
	for _, want := range []string{"operator_required", "carries no operator identity"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, err)
		}
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

// refusingNode is a server that answers every request with one refusal.
func refusingNode(t *testing.T, status int, contentType string, body []byte) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// everyClient is one call through each of the three clients, against base, with
// a token sent — so a 401 arm reached is the sent-token one.
func everyClient() map[string]func(t *testing.T, base string) error {
	return map[string]func(t *testing.T, base string) error{
		"config": func(t *testing.T, base string) error {
			client := &configClient{base: base, token: "ops-token", http: httpx.Client(apiTimeout)}
			_, _, err := client.Import(t.Context(), []byte("name: Acme\n"), "import")
			return err
		},
		"secrets": func(t *testing.T, base string) error {
			client := &secretsClient{base: base, token: "ops-token", http: httpx.Client(apiTimeout)}
			_, err := client.Get(t.Context(), "TOKEN")
			return err
		},
		"node": func(t *testing.T, base string) error {
			return nodeAt(base).get(t.Context(), "/query/budgets", nil)
		},
	}
}

// A STATUS IS INTERPRETED ONLY OVER THE NODE'S OWN CODE, on every client. What
// a 401 or a 503 means depends on who sent it: an auth wall in front of a node
// answers 401 with its sign-in page, and a load balancer with no healthy peer
// answers 503 with a page or a JSON object of its own. Read as the node's, the
// first sends the operator to api.auth.tokens on a node the request never
// reached and the second to a node that is fine. So a body with no `error`
// code is shown — the page's title, the JSON compacted — and gets no sentence
// about tokens, backends or missing secrets, whatever the status. A JSON
// object is the case a "did it decode" test gets wrong: any object decodes
// into the refusal's struct, unknown keys and all.
func TestAStatusIsInterpretedOnlyOverTheNodesOwnCode(t *testing.T) {
	t.Parallel()
	bodies := map[string]struct {
		contentType string
		body        string
		shows       string
	}{
		"an auth wall's page": {"text/html; charset=utf-8",
			"<html><head><title>Sign in to continue</title></head><body>...</body></html>",
			"Sign in to continue"},
		"a load balancer's JSON": {"application/json",
			`{"message":"failure to get a peer from the ring-balancer"}`,
			"failure to get a peer from the ring-balancer"},
		"an empty object": {"application/json", `{}`, "{}"},
	}
	statuses := []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusServiceUnavailable,
	}
	// What an interpretation would add. Each is a sentence one of the
	// clients writes only over a code the node sent.
	interpretations := []string{"api.auth.tokens", "no token was sent", "cannot serve"}

	for clientName, call := range everyClient() {
		for bodyName, b := range bodies {
			for _, status := range statuses {
				t.Run(fmt.Sprintf("%s/%s/%d", clientName, bodyName, status), func(t *testing.T) {
					t.Parallel()
					base := refusingNode(t, status, b.contentType, []byte(b.body))
					err := call(t, base)
					if err == nil {
						t.Fatal("a refusal was reported as an answer")
					}
					if errors.Is(err, secrets.ErrNotFound) {
						t.Errorf("a body with no code was read as the node's not_found: %v", err)
					}
					for _, guess := range interpretations {
						if strings.Contains(err.Error(), guess) {
							t.Errorf("the refusal interpreted a body the node did not send (%q):\n%s", guess, err)
						}
					}
					if !strings.Contains(err.Error(), b.shows) {
						t.Errorf("the refusal does not show what the body said (%q):\n%s", b.shows, err)
					}
					if !strings.Contains(err.Error(), fmt.Sprint(status)) {
						t.Errorf("the refusal does not name the status %d:\n%s", status, err)
					}
				})
			}
		}
	}
}

// AND THE NODE'S OWN 401 STILL NAMES THE TOKEN, on every client — the guard's
// `invalid_token` is the one refusal where api.auth.tokens is the place to go.
func TestTheNodesOwnRefusedTokenNamesTheTokenOnEveryClient(t *testing.T) {
	t.Parallel()
	for name, call := range everyClient() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := refusingNode(t, http.StatusUnauthorized, "application/json",
				[]byte(`{"error":"invalid_token"}`))
			err := call(t, base)
			if err == nil || !strings.Contains(err.Error(), "api.auth.tokens") {
				t.Errorf("the node's own 401 = %v, want it to name api.auth.tokens", err)
			}
		})
	}
}

// AN ANSWER THAT IS NOT THE ENGINE'S JSON IS DISTILLED, NEVER PASTED, AND A CUT
// IN IT IS MARKED — on all three clients, because each reads an answer up to a
// ceiling sized for the node's largest legitimate one (a megabyte, for the node
// client), and a proxy's page pasted whole is that page in the operator's
// terminal. The page's title is what a proxy puts its reason in; plain text is
// kept, on one line and inside [httpx.RefusalDetail], ending in the marker so a
// severed sentence cannot read as the whole answer; and an empty body is named
// rather than rendered as a message that ends at its colon.
func TestARefusalThatIsNotTheEnginesJSONIsDistilledOnEveryClient(t *testing.T) {
	t.Parallel()
	clients := everyClient()
	page := "<!doctype html><html><head><title>502 Bad Gateway</title>" +
		"<style>body{font-family:sans-serif}</style></head><body>" +
		strings.Repeat("<p>the upstream did not answer in time</p>", 1_000) + "</body></html>"
	text := "upstream refused: " + strings.Repeat("the connection was reset by peer ", 400)
	for name, answer := range clients {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			base := refusingNode(t, http.StatusBadGateway, "text/html; charset=utf-8", []byte(page))
			err := answer(t, base)
			if err == nil {
				t.Fatal("a gateway's page was reported as an answer")
			}
			if !strings.Contains(err.Error(), "502 Bad Gateway") {
				t.Errorf("the refusal does not carry the page's title:\n%s", err)
			}
			for _, markup := range []string{"<html", "<style", "<p>", "doctype"} {
				if strings.Contains(err.Error(), markup) {
					t.Errorf("the refusal pasted the page's markup (%q):\n%.300s", markup, err)
				}
			}

			base = refusingNode(t, http.StatusBadGateway, "text/plain", []byte(text))
			err = answer(t, base)
			if err == nil {
				t.Fatal("a gateway's text was reported as an answer")
			}
			// Bounded by the shared detail budget plus the client's own
			// framing, which names the address and the route.
			if limit := httpx.RefusalDetail + len(base) + 64; len(err.Error()) > limit {
				t.Errorf("the refusal is %d bytes, want at most %d:\n%.300s", len(err.Error()), limit, err)
			}
			if !strings.Contains(err.Error(), "upstream refused") {
				t.Errorf("the refusal does not quote the text:\n%.300s", err)
			}
			if !strings.HasSuffix(err.Error(), "…") {
				t.Errorf("the cut is unmarked: %q", err.Error()[max(0, len(err.Error())-24):])
			}
			if !utf8.ValidString(err.Error()) {
				t.Error("the refusal is not valid UTF-8")
			}

			base = refusingNode(t, http.StatusBadGateway, "", nil)
			err = answer(t, base)
			if err == nil || !strings.Contains(err.Error(), "an empty body") {
				t.Errorf("an empty refusal = %v, want it named as an empty body", err)
			}
		})
	}
}
