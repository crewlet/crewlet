package atlassian

import "context"

// FlyForTest runs one lookup of site outside the shared flight: the state a
// caller reaches when it read the map before another lookup stored a
// success, and asked after that lookup left the flight.
func FlyForTest(c *CloudIDs, ctx context.Context, site string) (string, error) {
	key, err := tenantInfoURL(site)
	if err != nil {
		return "", err
	}
	return c.fly(ctx, key)
}
