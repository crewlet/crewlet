package jetstream

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
)

// NO SENTENCE NAMING A SERVER CARRIES THE CREDENTIAL ITS URL HOLDS.
//
// stream.url goes to the client verbatim, and a NATS URL carries a credential
// in either half of its userinfo: `user:password@` signs in as a user, and a
// bare `token@` is a bearer token — the client reads a username with no
// password as one. Every sentence the backend writes about a server is an
// operator's to read on a boot's stderr or in a journal, so none may repeat
// either. Three of them did: a refusal at boot named the server through the
// client's ConnectedUrlRedacted, which masks a password and prints a token; a
// dial that failed named stream.url as it was written; and a URL that did not
// parse came back as the parse's own error, which quotes the whole entry. The
// reconnect watch's line named the server the first way too.
//
// Each row presents a credential and asserts two things of what comes back:
// that it does not carry the credential, and that it still names the server —
// the control that keeps an answer naming nothing at all from passing.
//
// Mutation: name the server through nc.ConnectedUrlRedacted in
// connectedServer and the refusal and reconnect rows go red; name cfg.URL in
// dial's error and the failed-dial and parse rows do; drop unquoted and the
// parse rows do; drop its escape branch and the escape row does.
func TestNoSentenceNamingAServerCarriesTheCredentialItsURLHolds(t *testing.T) {
	t.Parallel()
	// Placeholders, not credentials: long enough that a match is never an
	// accident of the rest of the sentence.
	const (
		token    = "placeholder-token-0f3a9c"
		user     = "crewlet"
		password = "placeholder-password-7d21e4"
	)

	t.Run("a refusal at boot", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name, secret string
			auth         func(*server.Options)
			userinfo     string
		}{
			{"a token", token, func(o *server.Options) { o.Authorization = token },
				token + "@"},
			{"a user and a password", password, func(o *server.Options) {
				o.Username, o.Password = user, password
			}, user + ":" + password + "@"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				// nats-server's own 1 MiB default, which is refused.
				host := credentialedServer(t, tc.auth)
				err := openRefused(t, "nats://"+tc.userinfo+host)
				assertNamesWithoutSecret(t, err.Error(), "nats://"+host, tc.secret)
			})
		}
	})

	t.Run("a dial that fails", func(t *testing.T) {
		t.Parallel()
		// A port nothing listens on: bound, read and released.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("find a free port: %v", err)
		}
		host := l.Addr().String()
		_ = l.Close()
		for _, tc := range []struct{ name, secret, userinfo string }{
			{"a token", token, token + "@"},
			{"a user and a password", password, user + ":" + password + "@"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				// Two members, the second without a scheme, as a list
				// stream.url may hold: both are named, and neither
				// credential is.
				err := openRefused(t, "nats://"+tc.userinfo+host+", "+tc.userinfo+host)
				assertNamesWithoutSecret(t, err.Error(),
					"nats://"+host+",nats://"+host, tc.secret)
			})
		}
	})

	t.Run("an address that does not parse", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct{ name, url, secret, says string }{
			{"its port", "nats://" + user + ":" + password + "@127.0.0.1:no-port",
				password, "invalid port"},
			// A malformed %-escape quotes the three characters it failed
			// on, wherever they were: here, inside the password.
			{"a %-escape in its password", "nats://" + user + ":pa%zz" + password +
				"@127.0.0.1:4222", "%zz", "malformed %-escape"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				err := openRefused(t, tc.url)
				assertNamesWithoutSecret(t, err.Error(), tc.says, tc.secret)
				if strings.Contains(err.Error(), password) {
					t.Errorf("the refusal %q carries the password", err)
				}
			})
		}
	})

	t.Run("the reconnect watch's line", func(t *testing.T) {
		t.Parallel()
		host := credentialedServer(t, func(o *server.Options) { o.Authorization = token })
		nc, err := nats.Connect("nats://" + token + "@" + host)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(nc.Close)
		var logged bytes.Buffer
		reconnectWatch{log: slog.New(slog.NewTextHandler(&logged, nil))}.reconnected(nc)
		if !strings.Contains(logged.String(), "jetstream_max_payload_below_contract") {
			t.Fatalf("a reconnect to a server below the contract logged %q, "+
				"so this row certifies nothing", logged.String())
		}
		assertNamesWithoutSecret(t, logged.String(), "nats://"+host, token)
	})
}

// A LIST IS NAMED ENTRY BY ENTRY, AS THE CLIENT SPLITS IT, by scheme, host and
// port alone.
//
// The client splits stream.url on commas, trims each entry and reads a bare
// host:port as a `nats://` one — so the list a sentence names is the list it
// dialled only if the rendering follows the same rules. A bare
// `token@host:port` in particular does not parse as a URL at all without the
// scheme, and was a member the client dialled named as one that does not
// parse.
//
// Mutation: drop the scheme serverAddress puts on a bare entry and the bare
// rows go red; keep a path or a query and the last row does.
func TestAServerListIsNamedAsTheClientReadsIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ raw, want string }{
		{"nats://a:4222", "nats://a:4222"},
		{"nats://tok@a:4222, b:4222/ ,nats://u:p@c:4222", "nats://a:4222,nats://b:4222,nats://c:4222"},
		{"tok@a:4222", "nats://a:4222"},
		{"u:p@a:4222", "nats://a:4222"},
		{"tls://u:p@a:4443", "tls://a:4443"},
		{"nats://a:4222,,", "nats://a:4222"},
		{"nats://a:4222/path?token=x", "nats://a:4222"},
	} {
		if got := serverAddresses(tc.raw); got != tc.want {
			t.Errorf("serverAddresses(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// openRefused opens a queue on url and answers the refusal, failing the case
// where it opens.
func openRefused(t *testing.T, url string) error {
	t.Helper()
	q, err := Open(t.Context(), Config{URL: url})
	if err == nil {
		_ = q.Stop(context.WithoutCancel(t.Context()))
		t.Fatalf("a queue opened on %q, which every row here refuses", url)
	}
	return err
}

// credentialedServer starts an external NATS server at nats-server's own
// default max_payload, which the queue refuses, authenticating as auth
// configures it, and answers its host and port.
func credentialedServer(t *testing.T, auth func(*server.Options)) string {
	t.Helper()
	return externaltest.Start(t, 0, auth).HostPort()
}

// assertNamesWithoutSecret fails where said carries secret, or does not carry
// names — what the sentence is about.
func assertNamesWithoutSecret(t *testing.T, said, names, secret string) {
	t.Helper()
	if strings.Contains(said, secret) {
		t.Errorf("%q carries the credential %q", said, secret)
	}
	if !strings.Contains(said, names) {
		t.Errorf("%q does not name %q", said, names)
	}
}
