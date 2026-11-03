package aws

import (
	"context"
	"fmt"
	"net/http"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const (
	// sessionSeconds is how long the provisioner's and the broker's sessions
	// last: the profile's own cap, an hour.
	sessionSeconds int32 = 3600
	// SeatSeconds is a seat session's life, STS's minimum: a pre-off session
	// that survives an off and an on lasts a quarter of an hour at most.
	SeatSeconds int32 = 900
	// renewBefore renews a cached session this long before it expires.
	renewBefore = 10 * time.Minute
)

// Client is the company's reach into its account: IAM as the provisioner,
// and seat sessions through the broker.
type Client struct {
	conn *Connection
	ra   rolesAnywhere
	http *http.Client
	// base replaces every AWS endpoint in tests; empty in production.
	base string
	// IAM is the SDK's IAM client, signing as the provisioner.
	IAM *iam.Client
}

// NewClient builds the clients over the engine's transport (internal/httpx).
// The SDK's standard retryer, raised to the same eight attempts as the Roles
// Anywhere path, retries throttling and server errors on every IAM and STS call.
func NewClient(conn *Connection, hc *http.Client) *Client { return newClient(conn, hc, "") }

func newClient(conn *Connection, hc *http.Client, base string) *Client {
	c := &Client{conn: conn, ra: newRolesAnywhere(hc, conn.Region), http: hc, base: base}
	if base != "" {
		c.ra.endpoint = func(string) string { return base }
	}
	provisioner := awssdk.NewCredentialsCache(awssdk.CredentialsProviderFunc(func(ctx context.Context) (awssdk.Credentials, error) {
		s, err := c.ra.createSession(ctx, conn.CA, named(provisionerCN, conn.Company), LeafECDSA,
			conn.ProvisionerProfileARN, conn.ProvisionerRoleARN, conn.TrustAnchorARN, sessionSeconds)
		return s.creds, err
	}), func(o *awssdk.CredentialsCacheOptions) { o.ExpiryWindow = renewBefore })
	c.IAM = iam.New(iam.Options{
		Region: iamRegion(conn.Partition), Credentials: provisioner, HTTPClient: hc, Retryer: retryer(),
		BaseEndpoint: c.baseEndpoint(),
	})
	return c
}

func (c *Client) baseEndpoint() *string {
	if c.base == "" {
		return nil
	}
	return awssdk.String(c.base)
}

func retryer() awssdk.Retryer {
	return retry.NewStandard(func(o *retry.StandardOptions) {
		o.MaxAttempts = maxAttempts
		o.MaxBackoff = retryCeiling
	})
}

// SeatSession is one seat's credentials, issued the one way a seat is ever
// reached: a leaf named for the seat opens the broker through the seats'
// profile, and the broker assumes the seat role with that name as its session
// name, so the session's source identity is the seat's own and its trust,
// which pins it, admits nothing else.
func (c *Client) SeatSession(ctx context.Context, slug, seatRoleARN string, seconds int32) (awssdk.Credentials, string, error) {
	cn := seatCN(c.conn.Company, slug)
	broker, err := c.ra.createSession(ctx, c.conn.CA, cn, LeafECDSA,
		c.conn.SeatProfileARN, c.conn.BrokerRoleARN, c.conn.TrustAnchorARN, sessionSeconds)
	if err != nil {
		return awssdk.Credentials{}, "", fmt.Errorf("aws: the broker for %s: %w", slug, err)
	}
	client := sts.New(sts.Options{
		Region: c.conn.Region, HTTPClient: c.http, Retryer: retryer(), BaseEndpoint: c.baseEndpoint(),
		Credentials: credentials.NewStaticCredentialsProvider(broker.creds.AccessKeyID, broker.creds.SecretAccessKey, broker.creds.SessionToken),
	})
	out, err := client.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: awssdk.String(seatRoleARN), RoleSessionName: awssdk.String(cn), DurationSeconds: awssdk.Int32(seconds),
	})
	if err != nil {
		return awssdk.Credentials{}, "", fmt.Errorf("aws: assume %s: %w", slug, err)
	}
	return awssdk.Credentials{
		AccessKeyID: awssdk.ToString(out.Credentials.AccessKeyId), SecretAccessKey: awssdk.ToString(out.Credentials.SecretAccessKey),
		SessionToken: awssdk.ToString(out.Credentials.SessionToken), CanExpire: true,
		Expires: awssdk.ToTime(out.Credentials.Expiration), Source: "crewlet-broker",
	}, awssdk.ToString(out.SourceIdentity), nil
}
