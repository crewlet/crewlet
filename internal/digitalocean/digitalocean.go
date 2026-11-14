// Package digitalocean gives each agent seat its own DigitalOcean personal
// access token in the company's team, at the seat's level.
//
// # A person creates each token
//
// DigitalOcean has no API that creates a token, a team member, a role or a
// service account, and its OAuth server offers no client-credentials grant. So
// a person creates one custom-scoped personal access token per agent in the
// control panel and pastes it ([seataccess.HandsIn]). The token acts as the
// person who created it: DigitalOcean records the agent's actions under that
// person, and Crewlet records which agent took each one. Revocation is per
// agent: Crewlet revokes a seat's token itself through DigitalOcean's OAuth
// revoke endpoint, which accepts a personal access token.
//
// # A level is proven, not read
//
// A token cannot read its own scopes, so the level is proven by calls that
// cannot have an effect ([checks]): a list read per scope, and for writes a
// request DigitalOcean refuses with 403 before it reads the body or finds the
// resource (a malformed or empty body, an id that cannot exist). Each was
// proven live in both directions with a token that holds the scope and one
// that does not. A custom-scoped token's scopes cannot be edited and never
// grow, so a proof at paste holds for the token's life; only a narrower team
// role takes scopes away, which each pass proves again.
//
// # The token reaches the agent by reference
//
// The seat's token is sealed as DIGITALOCEAN_TOKEN_<HANDLE>. doctl reads it as
// DIGITALOCEAN_ACCESS_TOKEN from the seat's `role.sandbox.env`, and the
// curated DigitalOcean MCP server reads it as a bearer header.
//
// # The atlas reads with a token of its own
//
// The company's half holds no credential, so the atlas reads the team's
// resources only where a person made one more token for it alone
// (`integrations.digitalocean.atlas.token`, ADR-0146): eight read scopes,
// proven before every read to change nothing and to be no agent's
// ([CheckAtlas]). Each listing is decoded into closed fields, so a database's
// connection and an app's environment are dropped with the answer.
package digitalocean

// Backend is the integration's name: the config block's key, the integration
// kind and the seat access lane.
const Backend = "digitalocean"
