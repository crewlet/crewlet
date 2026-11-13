package datadog_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/integration"
)

// limiter is a Datadog that answers 429 to the first `limited` requests, with
// the given headers, and hands the rest to the region's own routes. It
// records every wait the client takes and fails a request that arrives before
// the client has waited what the 429s before it asked for.
type limiter struct {
	reg     *region
	limited int
	headers map[string]string

	mu     sync.Mutex
	sent   int           // 429s answered
	owed   time.Duration // what those 429s asked the client to wait
	waited []time.Duration
	early  []string // requests that arrived before the client waited
	bodies []string
}

func newLimiter(t *testing.T, limited int, headers map[string]string) (*limiter, *datadog.Client) {
	t.Helper()
	l := &limiter{reg: newRegion(t), limited: limited, headers: headers}
	inner := l.reg.Config.Handler
	l.reg.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		l.bodies = append(l.bodies, string(body))
		if total(l.waited) < l.owed {
			l.early = append(l.early, r.Method+" "+r.URL.Path)
		}
		limit := l.sent < l.limited
		if limit {
			l.reg.calls = append(l.reg.calls, r.Method+" "+r.URL.Path)
			l.sent++
			l.owed += wantWait(headers)
		}
		l.mu.Unlock()
		if !limit {
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			inner.ServeHTTP(w, r)
			return
		}
		for name, value := range headers {
			w.Header().Set(name, value)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errors":["Rate limit of 300 requests in 60 seconds reached"]}`))
	})
	c := l.reg.client(t)
	datadog.SetWait(c, func(_ context.Context, d time.Duration) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.waited = append(l.waited, d)
		return nil
	})
	return l, c
}

// wantWait is the wait a 429 carrying headers asks for, as Datadog documents
// it: the reset, where the test sends one.
func wantWait(headers map[string]string) time.Duration {
	switch {
	case headers["Retry-After"] != "":
		return seconds(headers["Retry-After"])
	case headers["X-RateLimit-Reset"] != "":
		return seconds(headers["X-RateLimit-Reset"])
	}
	return 0
}

func seconds(raw string) time.Duration {
	d, err := time.ParseDuration(raw + "s")
	if err != nil {
		return 0
	}
	return d
}

func total(waits []time.Duration) time.Duration {
	var sum time.Duration
	for _, d := range waits {
		sum += d
	}
	return sum
}

func (l *limiter) record() (waited []time.Duration, early []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.waited), slices.Clone(l.early)
}

func org(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte(`{"orgs":[{"name":"Acme","public_id":"abc123"}]}`))
}

// A 429 IS WAITED OUT FOR AS LONG AS DATADOG SAYS, BEFORE THE CALL IS SENT
// AGAIN, and the call then answers as if it had not happened.
//
// The fake region refuses a request that arrives before the client has waited
// what every 429 before it asked for, so a client that retries at once goes
// red here even when its retry succeeds. Mutation: drop the wait from do's
// loop (continue without WaitOut) and the retries arrive early.
func TestARateLimitIsWaitedOutBeforeTheCallIsSentAgain(t *testing.T) {
	t.Parallel()
	l, c := newLimiter(t, 2, map[string]string{
		"X-RateLimit-Reset": "7", "X-RateLimit-Name": "org", "X-RateLimit-Period": "60",
	})
	l.reg.handle["/api/v1/org"] = org
	got, err := c.VerifyCredentials(t.Context(), pair)
	if err != nil || got.Name != "Acme" {
		t.Fatalf("VerifyCredentials after two 429s = %+v, %v; want the organization", got, err)
	}
	waited, early := l.record()
	if len(early) > 0 {
		t.Errorf("sent again before waiting what Datadog asked: %v", early)
	}
	if !slices.Equal(waited, []time.Duration{7 * time.Second, 7 * time.Second}) {
		t.Errorf("waited %v, want Datadog's reset of 7s before each try", waited)
	}
	if len(l.reg.calls) != 3 {
		t.Errorf("sent %d requests, want the two refused and the one answered: %v",
			len(l.reg.calls), l.reg.calls)
	}
}

// THE WAIT IS THE ONE DATADOG NAMED: a Retry-After where one is sent, else
// the limit's reset, else its whole window, else the default.
func TestTheWaitIsReadFromWhatDatadogSaid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    time.Duration
	}{
		{"a Retry-After before the reset", map[string]string{
			"Retry-After": "3", "X-RateLimit-Reset": "9", "X-RateLimit-Period": "60"}, 3 * time.Second},
		{"the reset", map[string]string{
			"X-RateLimit-Reset": "9", "X-RateLimit-Period": "60"}, 9 * time.Second},
		{"the window, where no reset is said", map[string]string{
			"X-RateLimit-Period": "10"}, 10 * time.Second},
		{"an unreadable reset falls through to the window", map[string]string{
			"X-RateLimit-Reset": "soon", "X-RateLimit-Period": "10"}, 10 * time.Second},
		{"nothing said", map[string]string{}, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, c := newLimiter(t, 1, tc.headers)
			l.reg.handle["/api/v1/org"] = org
			if _, err := c.VerifyCredentials(t.Context(), pair); err != nil {
				t.Fatal(err)
			}
			if waited, _ := l.record(); !slices.Equal(waited, []time.Duration{tc.want}) {
				t.Errorf("waited %v, want %v", waited, tc.want)
			}
		})
	}
}

// ONCE THE BOUND IS SPENT, THE 429 IS HANDED BACK SAYING WHICH LIMIT, WHEN IT
// RESETS AND WHY THE CALL STOPPED, and it stays a rate limit rather than a
// rejected credential: three waits, a wait past the cap, or one past the
// caller's deadline each end the call without another request.
func TestASpentBoundIsHandedBackSayingWhenTheLimitResets(t *testing.T) {
	t.Parallel()
	soon, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		reset    string
		requests int
		waits    int
		because  string
	}{
		{"three waits taken", context.Background(), "2", 4, 3, "it already waited 3 times"},
		{"a wait past the cap", context.Background(), "120", 1, 0,
			"the wait is longer than the 30s one call waits"},
		{"a wait past the deadline", soon, "25", 1, 0, "the wait ends after this call's deadline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, c := newLimiter(t, 100, map[string]string{
				"X-RateLimit-Reset": tc.reset, "X-RateLimit-Name": "users_list",
			})
			err := c.CanReadUsers(tc.ctx, pair)
			api, ok := errors.AsType[*datadog.APIError](err)
			if !ok || api.Status != http.StatusTooManyRequests {
				t.Fatalf("a limit that never lifts = %v, want the 429 handed back", err)
			}
			if api.RateLimit != "users_list" || api.RetryAfter != seconds(tc.reset) {
				t.Errorf("handed back limit %q resetting in %s, want users_list in %ss",
					api.RateLimit, api.RetryAfter, tc.reset)
			}
			for _, want := range []string{`the "users_list" rate limit is spent and resets in`,
				tc.because, "Try again after the reset", "Rate limit of 300 requests"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error %q does not say %q", err, want)
				}
			}
			if waited, _ := l.record(); len(waited) != tc.waits || len(l.reg.calls) != tc.requests {
				t.Errorf("waited %v over %d requests, want %d waits and %d requests",
					waited, len(l.reg.calls), tc.waits, tc.requests)
			}
			if errors.Is(integration.Reject(err, datadog.Status(err)), integration.ErrCredentialRejected) {
				t.Error("a rate limit was classified as a rejected credential")
			}
		})
	}
}

// A CALL SENT AGAIN CARRIES THE SAME BODY: the request is rebuilt from the
// encoded body on every try, so a retried write is not an empty one.
func TestARetriedWriteCarriesItsBodyAgain(t *testing.T) {
	t.Parallel()
	l, c := newLimiter(t, 1, map[string]string{"X-RateLimit-Reset": "1"})
	l.reg.handle["/api/v1/integration/webhooks/configuration/webhooks"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}
	hook := datadog.Webhook{Name: "crewlet", URL: "https://engine.example.com/webhooks/datadog"}
	if err := c.CreateWebhook(t.Context(), pair, hook); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.bodies) != 2 || l.bodies[0] == "" || l.bodies[0] != l.bodies[1] {
		t.Errorf("bodies sent = %q, want the same webhook twice", l.bodies)
	}
}

// EVERY CALL THE CLIENT MAKES WAITS OUT A 429. Each method that reaches
// Datadog is driven against a region that refuses everything, and must wait
// before sending its first request again. The table is held to the client's
// methods by reflection, so a method added later without a row fails here.
func TestEveryCallWaitsOutARateLimit(t *testing.T) {
	t.Parallel()
	account := datadog.Account{ID: "u-1", RoleIDs: []string{"r-old"},
		Keys: []datadog.AppKey{{ID: "k-new", Name: "crewlet"}, {ID: "k-old", Name: "crewlet"}}}
	hook := datadog.Webhook{Name: "crewlet", URL: "https://engine.example.com/webhooks/datadog"}
	calls := map[string]func(ctx context.Context, c *datadog.Client) error{
		"VerifyCredentials": func(ctx context.Context, c *datadog.Client) error {
			_, err := c.VerifyCredentials(ctx, pair)
			return err
		},
		"ValidateAPIKey": func(ctx context.Context, c *datadog.Client) error { return c.ValidateAPIKey(ctx, "api-key") },
		"CanReadUsers":   func(ctx context.Context, c *datadog.Client) error { return c.CanReadUsers(ctx, pair) },
		"CanReadRoles":   func(ctx context.Context, c *datadog.Client) error { return c.CanReadRoles(ctx, pair) },
		"CanReadWebhook": func(ctx context.Context, c *datadog.Client) error { return c.CanReadWebhook(ctx, pair, "crewlet") },
		"User": func(ctx context.Context, c *datadog.Client) error {
			_, _, err := c.User(ctx, pair, "u-1")
			return err
		},
		"ListServiceAccounts": func(ctx context.Context, c *datadog.Client) error {
			_, err := c.ListServiceAccounts(ctx, pair, "")
			return err
		},
		"ListRoles": func(ctx context.Context, c *datadog.Client) error {
			_, err := c.ListRoles(ctx, pair, "")
			return err
		},
		"Roles": func(ctx context.Context, c *datadog.Client) error {
			_, err := c.Roles(ctx, pair)
			return err
		},
		"Role": func(ctx context.Context, c *datadog.Client) error {
			_, _, err := c.Role(ctx, pair, "r-1")
			return err
		},
		"AddRoleMember":    func(ctx context.Context, c *datadog.Client) error { return c.AddRoleMember(ctx, pair, "r-1", "u-1") },
		"RemoveRoleMember": func(ctx context.Context, c *datadog.Client) error { return c.RemoveRoleMember(ctx, pair, "r-1", "u-1") },
		"CreateServiceAccount": func(ctx context.Context, c *datadog.Client) error {
			_, err := c.CreateServiceAccount(ctx, pair, "agent@example.com", "agent", "r-1")
			return err
		},
		"CreateAppKey": func(ctx context.Context, c *datadog.Client) error {
			_, err := c.CreateAppKey(ctx, pair, "u-1", "crewlet")
			return err
		},
		"DeleteAppKey": func(ctx context.Context, c *datadog.Client) error { return c.DeleteAppKey(ctx, pair, "u-1", "k-1") },
		"ListAppKeys": func(ctx context.Context, c *datadog.Client) error {
			_, err := c.ListAppKeys(ctx, pair, "u-1")
			return err
		},
		"DisableUser":       func(ctx context.Context, c *datadog.Client) error { return c.DisableUser(ctx, pair, "u-1") },
		"MarkDisconnected":  func(ctx context.Context, c *datadog.Client) error { return c.MarkDisconnected(ctx, pair, "u-1") },
		"ClearDisconnected": func(ctx context.Context, c *datadog.Client) error { return c.ClearDisconnected(ctx, pair, "u-1") },
		"EnableUser":        func(ctx context.Context, c *datadog.Client) error { return c.EnableUser(ctx, pair, "u-1") },
		"Webhook": func(ctx context.Context, c *datadog.Client) error {
			_, _, err := c.Webhook(ctx, pair, "crewlet")
			return err
		},
		"CreateWebhook": func(ctx context.Context, c *datadog.Client) error { return c.CreateWebhook(ctx, pair, hook) },
		"UpdateWebhook": func(ctx context.Context, c *datadog.Client) error { return c.UpdateWebhook(ctx, pair, hook) },
		"DeleteWebhook": func(ctx context.Context, c *datadog.Client) error { return c.DeleteWebhook(ctx, pair, "crewlet") },
		"SetRole": func(ctx context.Context, c *datadog.Client) error {
			_, err := c.SetRole(ctx, pair, account, "r-1")
			return err
		},
		"RevokeKeys": func(ctx context.Context, c *datadog.Client) error { return c.RevokeKeys(ctx, pair, account) },
		"Disable":    func(ctx context.Context, c *datadog.Client) error { return c.Disable(ctx, pair, account) },
		"TurnOff":    func(ctx context.Context, c *datadog.Client) error { return c.TurnOff(ctx, pair, account) },
		"RetireKeys": func(ctx context.Context, c *datadog.Client) error {
			_, err := c.RetireKeys(ctx, pair, account, "k-new")
			return err
		},
		"CanList": func(ctx context.Context, c *datadog.Client) error { return c.CanList(ctx, pair, datadog.ListHosts) },
		"Monitors": func(ctx context.Context, c *datadog.Client) error {
			_, err := c.Monitors(ctx, pair)
			return err
		},
		"ServiceDefinitions": func(ctx context.Context, c *datadog.Client) error {
			_, err := c.ServiceDefinitions(ctx, pair)
			return err
		},
		"Hosts": func(ctx context.Context, c *datadog.Client) error {
			_, err := c.Hosts(ctx, pair)
			return err
		},
		"SyntheticTests": func(ctx context.Context, c *datadog.Client) error {
			_, err := c.SyntheticTests(ctx, pair)
			return err
		},
	}
	contextType := reflect.TypeFor[context.Context]()
	client := reflect.TypeFor[*datadog.Client]()
	for i := range client.NumMethod() {
		m := client.Method(i)
		if m.Type.NumIn() > 1 && m.Type.In(1) == contextType {
			if _, ok := calls[m.Name]; !ok {
				t.Errorf("(*datadog.Client).%s reaches Datadog and has no row here, so "+
					"nothing shows it waits out a rate limit", m.Name)
			}
		}
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l, c := newLimiter(t, 1000, map[string]string{"X-RateLimit-Reset": "1"})
			err := call(t.Context(), c)
			waited, early := l.record()
			if datadog.Status(err) != http.StatusTooManyRequests || len(waited) == 0 || len(early) > 0 {
				t.Fatalf("against a spent limit: %v after waits %v (early %v), want the 429 "+
					"handed back after waiting", err, waited, early)
			}
			if len(l.reg.calls) < 2 || l.reg.calls[0] != l.reg.calls[1] {
				t.Errorf("requests %v, want the first one sent again after the wait", l.reg.calls)
			}
		})
	}
}
