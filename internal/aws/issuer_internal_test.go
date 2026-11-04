package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// AN AGENT'S SESSION IS ISSUED ONCE AND KEPT WHILE IT LASTS what a caller asks:
// the lives the engine asks of AWS (10 minutes at a launch, 5 at a tool) are
// served from one issue, and a session that would run short is issued again.
func TestTheIssuerKeepsASessionWhileItLasts(t *testing.T) {
	t.Parallel()
	var issued atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exp := time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339)
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/sessions" {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"credentialSet": []any{map[string]any{
				"credentials": map[string]any{"accessKeyId": "ASIABROKER", "secretAccessKey": "s",
					"sessionToken": "broker", "expiration": exp}}}})
			return
		}
		form, _ := url.ParseQuery(string(body))
		if form.Get("RoleArn") != "arn:aws:iam::123456789012:role/crewlet/acme/seat/crewlet-acme-"+SeatSlug("sre") {
			t.Errorf("assumed %q", form.Get("RoleArn"))
		}
		n := issued.Add(1)
		_, _ = fmt.Fprintf(w, `<AssumeRoleResponse><AssumeRoleResult><Credentials><AccessKeyId>ASIASEAT%d</AccessKeyId>`+
			`<SecretAccessKey>x</SecretAccessKey><SessionToken>t</SessionToken><Expiration>%s</Expiration>`+
			`</Credentials></AssumeRoleResult></AssumeRoleResponse>`, n, exp)
	}))
	defer srv.Close()
	client := newClient(mustConnection(t), srv.Client(), srv.URL)
	clock := time.Now()
	i := &Issuer{now: func() time.Time { return clock }}
	ctx := context.Background()
	first, err := i.session(ctx, client, "sre", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := i.session(ctx, client, "sre", 5*time.Minute); again.AccessKeyID != first.AccessKeyID || issued.Load() != 1 {
		t.Errorf("a held session was issued again: %d issues", issued.Load())
	}
	clock = clock.Add(11 * time.Minute)
	if renewed, _ := i.session(ctx, client, "sre", 5*time.Minute); renewed.AccessKeyID == first.AccessKeyID || issued.Load() != 2 {
		t.Errorf("a session running short was kept: %d issues", issued.Load())
	}
	if _, err := i.session(ctx, client, "sre", 50*time.Minute); err == nil {
		t.Error("a life longer than a session was promised")
	}
}
