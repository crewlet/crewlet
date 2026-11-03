package aws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Issuer issues each agent's session at use and keeps it, process-wide,
// until it runs short of what a caller asks. Nothing it holds is sealed.
type Issuer struct {
	// HTTP reaches AWS; nil is the engine's transport. A test hands one that
	// reaches a fake.
	HTTP *http.Client

	mu   sync.Mutex
	held map[string]awssdk.Credentials
	// now is the clock; nil is the wall clock.
	now func() time.Time
}

// ErrSettling is a seat whose role STS will not let the broker assume yet: a
// role just created, still propagating through IAM. A wait clears it; a role
// that stays refused is the access sync's to report.
var ErrSettling = errors.New("aws: the agent's role is not assumable yet")

// SeatVariable is the variable an agent's session is issued under, and the
// one its box's credentials file holds.
func SeatVariable(handle string) string {
	return seataccess.SinkName(integration.KindAWS, CredentialsField, handle)
}

// SeatOn reports an agent seat with handle that is on at AWS in c.
func SeatOn(c *config.Company, handle string) bool {
	if c == nil || c.Integrations.AWS == nil || !c.Integrations.AWS.Enabled {
		return false
	}
	for role := range c.EachRole() {
		if role.IdentityKey() == handle {
			in := role.Integrations.AWS
			return in != nil && config.SeatAccessOn(in.Enabled)
		}
	}
	return false
}

// Session is the session of the agent with handle, good for at least life
// after the call: the one held while it is, else a new one, issued through
// the broker as the seat's own role.
func (i *Issuer) Session(ctx context.Context, c *config.Company, env *config.Resolver, handle string,
	life time.Duration) (awssdk.Credentials, error) {
	client, err := ClientFor(c, env, i.HTTP)
	if err != nil {
		return awssdk.Credentials{}, err
	}
	return i.session(ctx, client, handle, life)
}

// session is [Issuer.Session] through client.
func (i *Issuer) session(ctx context.Context, client *Client, handle string, life time.Duration) (awssdk.Credentials, error) {
	conn := client.conn
	slug := SeatSlug(handle)
	key := conn.Account + "/" + string(conn.Company) + "/" + slug
	now := time.Now
	if i.now != nil {
		now = i.now
	}
	i.mu.Lock()
	held, ok := i.held[key]
	i.mu.Unlock()
	if ok && held.Expires.After(now().Add(life)) {
		return held, nil
	}
	if life >= time.Duration(SeatSeconds)*time.Second {
		return awssdk.Credentials{}, fmt.Errorf("aws: a session lasts %s, shorter than the %s asked",
			time.Duration(SeatSeconds)*time.Second, life)
	}
	role := conn.resolve("arn:{partition}:iam::{account}:role"+seatPath) + conn.Company.SeatRoleName(slug)
	creds, _, err := client.SeatSession(ctx, slug, role, SeatSeconds)
	if hasCode(err, "AccessDenied") {
		return awssdk.Credentials{}, fmt.Errorf("%w: %w", ErrSettling, err)
	}
	if err != nil {
		return awssdk.Credentials{}, err
	}
	i.mu.Lock()
	if i.held == nil {
		i.held = map[string]awssdk.Credentials{}
	}
	i.held[key] = creds
	i.mu.Unlock()
	return creds, nil
}
