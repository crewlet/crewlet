package metaads

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// usageMeta answers every call with one business use case's usage, and the
// first call of each method with a throttle where throttle is set.
type usageMeta struct {
	mu       sync.Mutex
	percent  int
	regain   int
	throttle bool
	served   map[string]int
}

func (u *usageMeta) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.served[r.Method]++
	w.Header().Set("X-Business-Use-Case-Usage", fmt.Sprintf(`{"111":[{"type":"ads_management","call_count":%d,`+
		`"total_cputime":1,"total_time":1,"estimated_time_to_regain_access":%d}]}`, u.percent, u.regain))
	w.Header().Set("Content-Type", "application/json")
	if u.throttle && u.served[r.Method] == 1 {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":17,"message":"User request limit reached"}}`))
		return
	}
	_, _ = w.Write([]byte(`{"id":"1"}`))
}

func newUsageClient(t *testing.T, u *usageMeta) *Client {
	t.Helper()
	u.served = map[string]int{}
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "secret", srv.Client())
	c.now = func() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) }
	c.limit.Wait = func(context.Context, time.Duration) error { return nil }
	return c
}

// Agents' reads stop at nine tenths of a business use case's limit, which is
// kept for approved acts and the pass; a write and a reserved read still go.
// While Meta asks for time to regain access, no call an agent makes goes.
//
// Mutation: drop the reserve or the regain check, and an agent's read spends
// what approved acts need.
func TestReadsStopAtTheReserveAndWhileMetaAsksToWait(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	u := &usageMeta{percent: 95}
	c := newUsageClient(t, u)
	read := Request{Method: http.MethodGet, Path: "/act_111/campaigns", Object: "111", Read: true}
	if _, err := c.Do(ctx, "EAAtoken", read); err != nil {
		t.Fatalf("the first read, before any usage was heard, answered %v", err)
	}
	if _, err := c.Do(ctx, "EAAtoken", read); !errors.Is(err, ErrReserve) {
		t.Fatalf("a read at 95%% answered %v, want the reserve", err)
	}
	reserved := read
	reserved.Reserved = true
	if _, err := c.Do(ctx, "EAAtoken", reserved); err != nil {
		t.Errorf("an approved act's read at the reserve answered %v", err)
	}
	if _, err := c.Do(ctx, "EAAtoken", Request{Method: http.MethodPost, Path: "/501", Object: "111"}); err != nil {
		t.Errorf("a write at the reserve answered %v", err)
	}
	if got := c.Usage("111"); got != 95 {
		t.Errorf("the usage read %d, want 95", got)
	}
	wait := &usageMeta{percent: 10, regain: 3}
	c = newUsageClient(t, wait)
	if _, err := c.Do(ctx, "EAAtoken", read); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(ctx, "EAAtoken", read); !errors.Is(err, ErrQuota) {
		t.Errorf("a read while Meta asks for 3 minutes answered %v, want the quota", err)
	}
}

// Only a read is retried after Meta's throttle; a write is never sent twice.
//
// Mutation: retry every throttled call, and the write is sent twice.
func TestOnlyAReadIsRetriedAfterAThrottle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	u := &usageMeta{throttle: true}
	c := newUsageClient(t, u)
	if _, err := c.Do(ctx, "EAAtoken", Request{Method: http.MethodGet, Path: "/501", Read: true}); err != nil {
		t.Errorf("a throttled read was not retried: %v", err)
	}
	_, err := c.Do(ctx, "EAAtoken", Request{Method: http.MethodPost, Path: "/501"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.Throttled() {
		t.Errorf("a throttled write answered %v, want Meta's throttle", err)
	}
	if u.served["GET"] != 2 || u.served["POST"] != 1 {
		t.Errorf("Meta served %v: the read twice, the write once", u.served)
	}
}
