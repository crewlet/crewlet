//go:build scalewaylive

package scaleway_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/scaleway"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/sigv4"
)

// A LIVE PROBE OF A BUCKET GRANT (ADR-0158) on a real project, run by hand with
// SCW_ORGANIZATION_ID, SCW_PROJECT_ID and the provisioner key: it makes an
// application, policy, key and empty bucket, checks the grant changes the
// bucket's ACL and nothing else, and deletes all four.
func TestLiveBucketGrant(t *testing.T) {
	org, project := os.Getenv("SCW_ORGANIZATION_ID"), os.Getenv("SCW_PROJECT_ID")
	access, secret := os.Getenv("SCW_ACCESS_KEY"), os.Getenv("SCW_SECRET_KEY")
	if org == "" || project == "" || access == "" || secret == "" {
		t.Fatal("set SCW_ORGANIZATION_ID, SCW_PROJECT_ID, and SCW_ACCESS_KEY and SCW_SECRET_KEY to the provisioner key")
	}
	ctx := t.Context()
	client, err := scaleway.NewClient(scaleway.ClientOptions{OrganizationID: org, SecretKey: secret})
	if err != nil {
		t.Fatal(err)
	}
	catalogue, err := client.PermissionSets(ctx)
	if err != nil {
		t.Fatalf("read the permission sets: %v", err)
	}
	types := map[string]string{}
	for _, s := range catalogue {
		types[s.Name] = s.ScopeType
	}
	rules := func(sets ...string) []scaleway.RuleSpec {
		var grants []seataccess.Grant
		for _, s := range sets {
			grants = append(grants, seataccess.Grant{Permission: s})
		}
		specs, err := scaleway.RulesWith(config.ScalewayLevelRead, org, []string{project}, grants, types)
		if err != nil {
			t.Fatalf("write the policy: %v", err)
		}
		return specs
	}

	const name = "crewlet-live-probe-bucket-grant"
	app, err := client.CreateApplication(ctx, name, "Crewlet's live probe of a bucket grant; safe to delete")
	if err != nil {
		t.Fatalf("create the application: %v", err)
	}
	var policyID, keyID string
	var p s3Probe
	bucket := "crewlet-probe-" + strings.ToLower(rand.Text()[:12])
	made := false
	t.Cleanup(func() {
		ctx := context.WithoutCancel(ctx)
		if made && policyID != "" && keyID != "" {
			if err := client.SetRules(ctx, policyID, rules("ObjectStorageBucketsWrite", "ObjectStorageBucketsDelete")); err == nil {
				if !until(t, "delete the bucket", func() bool {
					status, _ := p.call(ctx, http.MethodDelete, bucket, "", nil, nil, true)
					return status == http.StatusNoContent || status == http.StatusNotFound
				}) {
					t.Errorf("the probe's bucket was NOT deleted: delete the crewlet-probe-* bucket in the project by hand")
				}
			}
		}
		if keyID != "" {
			if err := client.DeleteAPIKey(ctx, keyID); err != nil {
				t.Errorf("the probe's key was NOT deleted: %v", err)
			}
		}
		if policyID != "" {
			if err := client.DeletePolicy(ctx, policyID); err != nil {
				t.Errorf("the probe's policy was NOT deleted: %v", err)
			}
		}
		if err := client.DeleteApplication(ctx, app.ID); err != nil {
			t.Errorf("the probe's application was NOT deleted, delete %s by hand: %v", name, err)
		}
	})
	pol, err := client.CreatePolicy(ctx, scaleway.PolicySpec{Name: name, ApplicationID: app.ID,
		Rules: rules("ObjectStorageBucketsWrite")})
	if err != nil {
		t.Fatalf("create the policy: %v", err)
	}
	policyID = pol.ID
	key, err := client.CreateAPIKey(ctx, scaleway.KeySpec{ApplicationID: app.ID, Description: name,
		DefaultProjectID: project, ExpiresAt: time.Now().Add(24 * time.Hour)})
	if err != nil {
		t.Fatalf("create the key: %v", err)
	}
	keyID, p = key.AccessKey, s3Probe{access: key.AccessKey, secret: key.SecretKey}

	if !until(t, "create the bucket", func() bool {
		status, _ := p.call(ctx, http.MethodPut, bucket, "", nil, nil, true)
		made = made || status == http.StatusOK
		return made
	}) {
		t.Fatal("the grant never let the key create a bucket")
	}
	acl := func(canned string) int {
		status, _ := p.call(ctx, http.MethodPut, bucket, "acl", http.Header{"X-Amz-Acl": {canned}}, nil, true)
		return status
	}
	listed := func() int {
		status, _ := p.call(ctx, http.MethodGet, bucket, "", nil, nil, false)
		return status
	}
	if got := acl("public-read"); got != http.StatusOK {
		t.Fatalf("make the bucket public-read: %d", got)
	}
	t.Logf("public-read: an anonymous listing answers %d", listed())
	if got := acl("private"); got != http.StatusOK {
		t.Fatalf("make the bucket private: %d", got)
	}
	if !until(t, "the anonymous listing is refused", func() bool { return listed() == http.StatusForbidden }) {
		t.Error("the bucket's listing stayed open once its ACL was private")
	}
	if status, code := p.call(ctx, http.MethodPut, bucket+"/probe.txt", "", nil, []byte("probe"), true); status < 400 {
		t.Errorf("the grant wrote an object (%d %s); object contents are never granted", status, code)
	}
	doc := []byte(`{"Version":"2023-04-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:ListBucket"],"Resource":["` +
		bucket + `"]}]}`)
	if status, code := p.call(ctx, http.MethodPut, bucket, "policy", nil, doc, true); status < 400 {
		t.Errorf("the grant wrote a bucket policy (%d %s); bucket policies are never granted", status, code)
	}

	if err := client.SetRules(ctx, policyID, rules()); err != nil {
		t.Fatalf("take the grant off the policy: %v", err)
	}
	if !until(t, "the ACL is refused without the grant", func() bool { return acl("private") == http.StatusForbidden }) {
		t.Error("the key still changes the bucket's ACL once the grant is off its policy")
	}
	t.Log("the grant changed the ACL, wrote no object and no policy, and nothing once it was off")
}

// until asks check again, twenty times twenty seconds at most, since Scaleway
// documents up to five minutes for a policy to apply to Object Storage.
func until(t *testing.T, what string, check func() bool) bool {
	t.Helper()
	for attempt := range 20 {
		if check() {
			t.Logf("%s: after %d attempts", what, attempt+1)
			return true
		}
		time.Sleep(20 * time.Second) //fixedwait:liveness
	}
	return false
}

// s3Probe signs Object Storage calls in fr-par with the probe's key.
type s3Probe struct{ access, secret string }

// call is one path-style Object Storage call, signed or anonymous: its status
// and S3's error code.
func (p s3Probe) call(ctx context.Context, method, path, sub string, header http.Header, body []byte,
	signed bool) (int, string) {

	const region = "fr-par"
	host := scaleway.ObjectStorageHost(region)
	query := url.Values{}
	target := "https://" + host + "/" + path
	if sub != "" {
		query.Set(sub, "")
		target += "?" + sub
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	if signed {
		now := time.Now().UTC()
		hash := sigv4.PayloadHash(body)
		h := http.Header{}
		for k, v := range header {
			h[k] = v
		}
		h.Set("X-Amz-Date", now.Format(sigv4.DateFormat))
		h.Set("X-Amz-Content-Sha256", hash)
		for k, v := range h {
			req.Header[k] = v
		}
		req.Header.Set("Authorization", sigv4.Sign(sigv4.Request{Method: method, Host: host, Path: "/" + path,
			Query: query, Header: h, PayloadHash: hash, S3: true},
			sigv4.Credentials{AccessKeyID: p.access, SecretAccessKey: p.secret}, region, "s3", now))
	}
	res, err := httpx.Client(scaleway.ClientTimeout).Do(req)
	if err != nil {
		return 0, fmt.Sprintf("unreached: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	payload, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	var refusal struct {
		Code string `xml:"Code"`
	}
	_ = xml.Unmarshal(payload, &refusal)
	return res.StatusCode, refusal.Code
}
