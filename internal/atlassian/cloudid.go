package atlassian

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/crewlet/crewlet/internal/httpx"
)

const (
	// TenantInfoTimeout bounds one cloud id lookup: jira.ClientTimeout, the
	// budget the engine already gives one read of the same site, restated
	// because jira imports this package (a jira test holds them equal).
	TenantInfoTimeout = 10 * time.Second

	// TenantInfoMaxBytes caps the answer. It is one line,
	// {"cloudId":"<36-character id>"}; 4 KiB is room for any envelope
	// Atlassian adds and still nothing a caller pays to read.
	TenantInfoMaxBytes = 4 << 10

	// TenantInfoRetryFloor is the least time between two failed lookups of
	// one site. Forge retries a refused event a minute apart, so a floor
	// under that gives each retry a fresh lookup while a burst of events
	// inside one minute shares a single request to the site.
	TenantInfoRetryFloor = 30 * time.Second

	tenantInfoPath = "/_edge/tenant_info"
)

// cloudIDShape is a cloud id as Atlassian issues them: a UUID.
var cloudIDShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// TenantInfoError is a lookup that did not yield a cloud id: the address
// asked and a short cause, which every surface names.
type TenantInfoError struct {
	URL   string
	Cause string
}

func (e *TenantInfoError) Error() string {
	return "the site's cloud id could not be read from " + e.URL + " (" + e.Cause + ")"
}

// CloudIDs reads the cloud id of an Atlassian Cloud site named by its url,
// from the site's own public /_edge/tenant_info (Atlassian Support, "How to
// Find Your Atlassian Cloud Site's Cloud ID"). One per process, shared by
// the node's Forge route, its passes and setup.
//
// A site's cloud id never changes, so a success is kept for the process's
// life. A failure is kept only as the last result, retried after
// [TenantInfoRetryFloor]; concurrent asks for one site share one request,
// which belongs to none of them.
type CloudIDs struct {
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	sites map[string]lookup
	group singleflight.Group
}

type lookup struct {
	id  string
	err error
	at  time.Time
}

// CloudIDOptions configures [NewCloudIDs]. Zero values are the production
// ones: httpx's client at [TenantInfoTimeout], and the wall clock. Whatever
// the client, a redirect is refused: a site's cloud id must come from that
// site itself, so a 3xx is answered like any other non-200.
type CloudIDOptions struct {
	Client *http.Client
	Now    func() time.Time
}

// NewCloudIDs builds the process's resolver.
func NewCloudIDs(opts CloudIDOptions) *CloudIDs {
	client := opts.Client
	if client == nil {
		client = httpx.Client(TenantInfoTimeout)
	}
	refusing := *client
	refusing.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c := &CloudIDs{client: &refusing, now: opts.Now, sites: map[string]lookup{}}
	if c.now == nil {
		c.now = func() time.Time { return time.Now().UTC() }
	}
	return c
}

// Resolve is site's cloud id: kept from an earlier success, the last error
// while inside the retry floor, or else read from the site now.
func (c *CloudIDs) Resolve(ctx context.Context, site string) (string, error) {
	key, err := tenantInfoURL(site)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	last, tried := c.sites[key]
	c.mu.Unlock()
	switch {
	case tried && last.err == nil:
		return last.id, nil
	case tried && c.now().Sub(last.at) < TenantInfoRetryFloor:
		return "", last.err
	}
	flight := c.group.DoChan(key, func() (any, error) { return c.fly(ctx, key) })
	select {
	case res := <-flight:
		if res.Err != nil {
			return "", res.Err
		}
		return res.Val.(string), nil
	case <-ctx.Done():
		return "", &TenantInfoError{URL: key, Cause: "no answer before the request ended"}
	}
}

// fly is one shared lookup of key, which belongs to no one caller. A success
// kept since the caller read the map is answered without asking, and a
// failure never replaces one: a site's cloud id never changes.
func (c *CloudIDs) fly(ctx context.Context, key string) (string, error) {
	c.mu.Lock()
	last, tried := c.sites[key]
	c.mu.Unlock()
	if tried && last.err == nil {
		return last.id, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), TenantInfoTimeout)
	defer cancel()
	id, err := c.read(ctx, key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if kept, ok := c.sites[key]; ok && kept.err == nil {
		return kept.id, nil
	}
	c.sites[key] = lookup{id: id, err: err, at: c.now()}
	return id, err
}

// read asks address once and validates the answer.
func (c *CloudIDs) read(ctx context.Context, address string) (string, error) {
	fail := func(cause string) error { return &TenantInfoError{URL: address, Cause: cause} }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", fail(err.Error())
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		var timeout interface{ Timeout() bool }
		if errors.As(err, &timeout) && timeout.Timeout() {
			return "", fail(fmt.Sprintf("no answer within %s", TenantInfoTimeout))
		}
		var request *url.Error
		if errors.As(err, &request) {
			err = request.Err
		}
		return "", fail("unreachable: " + err.Error())
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fail(fmt.Sprintf("HTTP %d", res.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, TenantInfoMaxBytes+1))
	if err != nil {
		return "", fail("the answer could not be read: " + err.Error())
	}
	if len(body) > TenantInfoMaxBytes {
		return "", fail(fmt.Sprintf("the answer is over %d bytes", TenantInfoMaxBytes))
	}
	var answer struct {
		CloudID *string `json:"cloudId"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", fail("the answer is not a JSON object")
	}
	if answer.CloudID == nil || strings.TrimSpace(*answer.CloudID) == "" {
		return "", fail("the answer names no cloudId")
	}
	id := strings.ToLower(strings.TrimSpace(*answer.CloudID))
	if !cloudIDShape.MatchString(id) {
		return "", fail("the answer's cloudId is not a cloud id")
	}
	return id, nil
}

// tenantInfoURL is site's lookup address: https and its host, whatever path
// the configured url carries (a Confluence site's /wiki). Atlassian Cloud
// serves only https, so a url naming another scheme is refused unasked.
func tenantInfoURL(site string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(site))
	if err != nil || u.Host == "" {
		return "", &TenantInfoError{URL: site + tenantInfoPath, Cause: "the url is not a site address"}
	}
	address := "https://" + strings.ToLower(u.Host) + tenantInfoPath
	if u.Scheme != "https" {
		return "", &TenantInfoError{URL: address, Cause: "Atlassian Cloud serves only https; " +
			"the url must start with https://"}
	}
	return address, nil
}
