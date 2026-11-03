package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// AN EXTERNAL BROKER NEEDS A CREDENTIAL.
//
// A node dialling `stream.type: nats` with nothing to present reaches only a
// server that authenticates nobody, and anything that reaches such a server
// reads and writes this company. So Tier A refuses it at stream.credentials,
// and accepts any one thing the client presents: a credentials file, a token,
// a client certificate, or a userinfo in ANY entry of the comma-separated url
// — `user:pass@` or a bare `token@`, written with or without a scheme, as the
// client reads them.
//
// Mutations: drop the rule and the bare row is accepted; ignore a userinfo
// and the url rows are refused; read only the first url entry and the rows
// whose credential is in a later entry are refused.
func TestAnExternalBrokerNeedsACredential(t *testing.T) {
	t.Parallel()
	const plain = "nats://nats-1.example.com:4222"
	for _, tc := range []struct {
		name    string
		stream  func(*config.Stream)
		refused bool
	}{
		{"nothing to present", func(*config.Stream) {}, true},
		{"an empty userinfo presents nothing", func(s *config.Stream) {
			s.URL = "nats://@nats-1.example.com:4222"
		}, true},
		{"a credentials file", func(s *config.Stream) { s.Credentials = "/etc/crewlet/engine.creds" }, false},
		{"a token", func(s *config.Stream) { s.Token = "fixture-token" }, false},
		{"a client certificate", func(s *config.Stream) {
			s.TLS = config.NATSTLS{Cert: "/etc/crewlet/client.pem", Key: "/etc/crewlet/client.key"}
		}, false},
		{"a user and password in the second url entry", func(s *config.Stream) {
			s.URL = plain + ", nats://engine:fixture-password@nats-2.example.com:4222"
		}, false},
		{"a bearer token in the last url entry", func(s *config.Stream) {
			s.URL = plain + ",nats://nats-2.example.com:4222,nats://fixture-token@nats-3.example.com:4222"
		}, false},
		{"a bearer token on an entry with no scheme", func(s *config.Stream) {
			s.URL = plain + ",fixture-token@nats-2.example.com:4222"
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := config.RunnableBootstrap()
			b.Coordination = config.Coordination{Type: config.CoordinationEmbeddedKV}
			b.Stream = config.Stream{Type: config.StreamNATS, URL: plain}
			tc.stream(&b.Stream)
			err := b.Validate()
			if !tc.refused {
				if err != nil {
					t.Fatalf("refused a node that presents a credential: %v", err)
				}
				return
			}
			if !errors.Is(err, config.ErrMissing) {
				t.Fatalf("err = %v, want ErrMissing", err)
			}
			var at *config.Problem
			for _, p := range config.Problems(err) {
				if p.Path == "stream.credentials" {
					at = &p
				}
			}
			if at == nil {
				t.Fatalf("no problem at stream.credentials in %v", err)
			}
			for _, says := range []string{"presents no credential", "authenticates nobody",
				"stream.credentials", "stream.token", "stream.tls.cert"} {
				if !strings.Contains(at.Message, says) {
					t.Errorf("the refusal does not say %q: %q", says, at.Message)
				}
			}
		})
	}
}
