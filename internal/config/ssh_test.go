package config_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/config"
)

// sshKeyLine is a public key line of a made-up key, as a host's
// /etc/ssh/ssh_host_*_key.pub holds it.
func sshKeyLine(t *testing.T, pub any) string {
	t.Helper()
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))) + " root@api-1"
}

func ed25519Line(t *testing.T, seed byte) string {
	t.Helper()
	key := ed25519.NewKeyFromSeed(slices.Repeat([]byte{seed}, ed25519.SeedSize))
	return sshKeyLine(t, key.Public())
}

// sshDoc is a company with an SSH block holding hosts and one agent seat with
// seat, and one person seat, ops-lead.
func sshDoc(t *testing.T, block, seat string) string {
	t.Helper()
	return "name: Acme\nintegrations:\n  ssh:\n" + block + "\nroles:\n" +
		"  - {name: SRE, handle: sre, integrations: {ssh: {" + seat + "}}}\n" +
		"  - {name: Ops Lead, handle: ops-lead, kind: human}\n"
}

// sshBlock is an enabled block with one host api-1, whose fields can be
// replaced line by line.
func sshBlock(t *testing.T, host string) string {
	t.Helper()
	if host == "" {
		host = sshHost(t, "")
	}
	return "    enabled: true\n    provisioning:\n      certificate_authority: ${SSH_CERTIFICATE_AUTHORITY}\n" +
		"    hosts:\n" + host
}

// sshHost is api-1 with extra appended to its flow mapping.
func sshHost(t *testing.T, extra string) string {
	t.Helper()
	return fmt.Sprintf("      - {id: api-1, address: api-1.example.com, host_keys: [%q], "+
		"users: {read: crewlet-read, operate: crewlet-ops}%s}\n", ed25519Line(t, 1), extra)
}

// problemAt is the problem at path, or false.
func problemAt(err error, path string) (config.Problem, bool) {
	for _, p := range config.Problems(err) {
		if p.Path == path {
			return p, true
		}
	}
	return config.Problem{}, false
}

// A COMPLETE BLOCK VALIDATES: the contract's example, a seat at read and one at
// operate, a switched-off block, and a block whose CA a rotation stages.
func TestACompleteSSHBlockValidates(t *testing.T) {
	t.Parallel()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	full := "    enabled: true\n    provisioning:\n" +
		"      certificate_authority: ${SSH_CERTIFICATE_AUTHORITY}\n" +
		"      next_certificate_authority: ${SSH_CERTIFICATE_AUTHORITY_NEXT}\n" +
		"      source_addresses: [203.0.113.7/32, 2001:db8::/32]\n" +
		"    approvers: [ops-lead]\n" +
		"    commands: {timeout_seconds: 900, max_output_bytes: 4096}\n" +
		"    hosts:\n" +
		fmt.Sprintf("      - {id: api-1, address: api-1.example.com, port: 2222, host_keys: [%q, %q, %q], "+
			"users: {read: crewlet-read, operate: crewlet-ops}, description: Production API server}\n",
			ed25519Line(t, 1), sshKeyLine(t, &rsaKey.PublicKey), sshKeyLine(t, &ecKey.PublicKey)) +
		fmt.Sprintf("      - {id: db-2, address: 198.51.100.4, host_keys: [%q], users: {read: crewlet-read}}\n",
			ed25519Line(t, 2))
	for name, doc := range map[string]string{
		"read":      sshDoc(t, full, "enabled: true, hosts: [api-1, db-2]"),
		"operate":   sshDoc(t, full, "enabled: true, level: operate, hosts: [api-1]"),
		"seat off":  sshDoc(t, full, "enabled: false"),
		"block off": sshDoc(t, "    enabled: false", "enabled: false"),
	} {
		if _, err := config.ParseCompany([]byte(doc)); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	c, err := config.ParseCompany([]byte(sshDoc(t, full, "enabled: true, hosts: [api-1]")))
	if err != nil {
		t.Fatal(err)
	}
	b := c.Integrations.SSH
	if got := b.Commands.Timeout().Seconds(); got != 900 {
		t.Errorf("timeout = %vs, want 900", got)
	}
	if h, ok := b.Host("db-2"); !ok || h.PortOrDefault() != 22 {
		t.Errorf("db-2 = %+v, %v; want port 22", h, ok)
	}
	if got := c.Roles[0].Integrations.SSH.LevelOrDefault(); got != config.SSHLevelRead {
		t.Errorf("an unset level = %q, want read", got)
	}
	var none *config.SSHCommands
	if none.Timeout() != config.SSHDefaultTimeout || none.MaxOutput() != config.SSHDefaultMaxOutputBytes {
		t.Error("an absent commands block does not read as the defaults")
	}
}

// EVERY PROBLEM THE CONTRACT NAMES IS REPORTED AT ITS PATH WITH ITS CLASS. One
// case per rule; each case is a document this engine would otherwise run.
func TestEverySSHProblemIsReportedAtItsPath(t *testing.T) {
	t.Parallel()
	rsaShort, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(slices.Repeat([]byte{9}, ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	cert := &ssh.Certificate{Key: signer.PublicKey(), CertType: ssh.HostCert, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, signer); err != nil {
		t.Fatal(err)
	}
	certLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert)))
	key := ed25519Line(t, 1)
	ca := "      certificate_authority: ${SSH_CERTIFICATE_AUTHORITY}\n"
	host := func(id, address, keys, users string) string {
		return fmt.Sprintf("      - {id: %s, address: %q, host_keys: [%s], users: %s}\n", id, address, keys, users)
	}
	users := "{read: crewlet-read, operate: crewlet-ops}"
	ok := host("api-1", "api-1.example.com", fmt.Sprintf("%q", key), users)
	block := func(prov, rest string) string {
		return "    enabled: true\n    provisioning:\n" + prov + rest + "    hosts:\n" + ok
	}
	hostsBlock := func(hosts string) string {
		return "    enabled: true\n    provisioning:\n" + ca + "    hosts:\n" + hosts
	}
	on := "enabled: true, hosts: [api-1]"
	for _, tc := range []struct {
		name, block, seat, path, kind string
	}{
		{"no provisioning", "    enabled: true", "enabled: false",
			"integrations.ssh.provisioning", "missing"},
		{"no certificate authority", block("      source_addresses: []\n", ""), on,
			"integrations.ssh.provisioning.certificate_authority", "missing"},
		{"a pasted certificate authority", block("      certificate_authority: \"-----BEGIN OPENSSH PRIVATE KEY-----\"\n", ""), on,
			"integrations.ssh.provisioning.certificate_authority", "conflict"},
		{"a staged authority that is the current one",
			block(ca+"      next_certificate_authority: ${SSH_CERTIFICATE_AUTHORITY}\n", ""), on,
			"integrations.ssh.provisioning.next_certificate_authority", "conflict"},
		{"a staged authority written inline",
			block(ca+"      next_certificate_authority: inline\n", ""), on,
			"integrations.ssh.provisioning.next_certificate_authority", "conflict"},
		{"a source address that is no range", block(ca+"      source_addresses: [203.0.113.7]\n", ""), on,
			"integrations.ssh.provisioning.source_addresses[0]", "unknown_value"},
		{"a source range with host bits", block(ca+"      source_addresses: [203.0.113.7/24]\n", ""), on,
			"integrations.ssh.provisioning.source_addresses[0]", "unknown_value"},
		{"seventeen source ranges", block(ca+"      source_addresses: ["+
			strings.Repeat("203.0.113.7/32, ", 16)+"203.0.113.8/32]\n", ""), on,
			"integrations.ssh.provisioning.source_addresses", "out_of_range"},
		{"an agent as approver", block(ca, "    approvers: [sre]\n"), on,
			"integrations.ssh.approvers[0]", "unknown_value"},
		{"an approver nobody holds", block(ca, "    approvers: [nobody]\n"), on,
			"integrations.ssh.approvers[0]", "unknown_value"},
		{"an approver twice", block(ca, "    approvers: [ops-lead, ops-lead]\n"), on,
			"integrations.ssh.approvers[1]", "conflict"},
		{"a timeout under ten seconds", block(ca, "    commands: {timeout_seconds: 5}\n"), on,
			"integrations.ssh.commands.timeout_seconds", "out_of_range"},
		{"a timeout over fifteen minutes", block(ca, "    commands: {timeout_seconds: 901}\n"), on,
			"integrations.ssh.commands.timeout_seconds", "out_of_range"},
		{"an output cap under 4 KiB", block(ca, "    commands: {max_output_bytes: 4095}\n"), on,
			"integrations.ssh.commands.max_output_bytes", "out_of_range"},
		{"an output cap over 1 MiB", block(ca, "    commands: {max_output_bytes: 1048577}\n"), on,
			"integrations.ssh.commands.max_output_bytes", "out_of_range"},
		{"a host id twice", hostsBlock(ok + ok), on, "integrations.ssh.hosts[1].id", "conflict"},
		{"a host id with a dot", hostsBlock(host("api.1", "api-1.example.com", fmt.Sprintf("%q", key), users)),
			"enabled: false", "integrations.ssh.hosts[0].id", "unknown_value"},
		{"a host id too long", hostsBlock(host(strings.Repeat("a", 41), "api-1.example.com", fmt.Sprintf("%q", key), users)),
			"enabled: false", "integrations.ssh.hosts[0].id", "unknown_value"},
		{"an address with a user", hostsBlock(host("api-1", "root@api-1.example.com", fmt.Sprintf("%q", key), users)), on,
			"integrations.ssh.hosts[0].address", "unknown_value"},
		{"an address with a port", hostsBlock(host("api-1", "api-1.example.com:22", fmt.Sprintf("%q", key), users)), on,
			"integrations.ssh.hosts[0].address", "unknown_value"},
		{"an address with a scheme", hostsBlock(host("api-1", "ssh://api-1.example.com", fmt.Sprintf("%q", key), users)), on,
			"integrations.ssh.hosts[0].address", "unknown_value"},
		{"a loopback address", hostsBlock(host("api-1", "127.0.0.1", fmt.Sprintf("%q", key), users)), on,
			"integrations.ssh.hosts[0].address", "unknown_value"},
		{"an IPv6 loopback address", hostsBlock(host("api-1", "::1", fmt.Sprintf("%q", key), users)), on,
			"integrations.ssh.hosts[0].address", "unknown_value"},
		{"the unspecified address", hostsBlock(host("api-1", "0.0.0.0", fmt.Sprintf("%q", key), users)), on,
			"integrations.ssh.hosts[0].address", "unknown_value"},
		{"a link-local address", hostsBlock(host("api-1", "169.254.169.254", fmt.Sprintf("%q", key), users)), on,
			"integrations.ssh.hosts[0].address", "unknown_value"},
		{"a multicast address", hostsBlock(host("api-1", "224.0.0.1", fmt.Sprintf("%q", key), users)), on,
			"integrations.ssh.hosts[0].address", "unknown_value"},
		{"localhost by name", hostsBlock(host("api-1", "localhost", fmt.Sprintf("%q", key), users)), on,
			"integrations.ssh.hosts[0].address", "unknown_value"},
		{"no host key", hostsBlock(host("api-1", "api-1.example.com", "", users)), on,
			"integrations.ssh.hosts[0].host_keys", "missing"},
		{"nine host keys", hostsBlock(host("api-1", "api-1.example.com",
			strings.TrimSuffix(strings.Repeat(fmt.Sprintf("%q, ", key), 9), ", "), users)), on,
			"integrations.ssh.hosts[0].host_keys", "out_of_range"},
		{"a host key that is not a key", hostsBlock(host("api-1", "api-1.example.com", `"ssh-ed25519 AAAA"`, users)), on,
			"integrations.ssh.hosts[0].host_keys[0]", "unknown_value"},
		{"a short RSA host key", hostsBlock(host("api-1", "api-1.example.com",
			fmt.Sprintf("%q", sshKeyLine(t, &rsaShort.PublicKey)), users)), on,
			"integrations.ssh.hosts[0].host_keys[0]", "unknown_value"},
		{"a host certificate as a host key", hostsBlock(host("api-1", "api-1.example.com",
			fmt.Sprintf("%q", certLine), users)), on, "integrations.ssh.hosts[0].host_keys[0]", "unknown_value"},
		{"a host key with options", hostsBlock(host("api-1", "api-1.example.com",
			fmt.Sprintf("%q", "restrict "+key), users)), on, "integrations.ssh.hosts[0].host_keys[0]", "unknown_value"},
		{"a host key twice", hostsBlock(host("api-1", "api-1.example.com",
			fmt.Sprintf("%q, %q", key, key), users)), on, "integrations.ssh.hosts[0].host_keys[1]", "conflict"},
		{"no read user", hostsBlock(host("api-1", "api-1.example.com", fmt.Sprintf("%q", key), "{operate: crewlet-ops}")), on,
			"integrations.ssh.hosts[0].users.read", "missing"},
		{"a read user with a capital", hostsBlock(host("api-1", "api-1.example.com", fmt.Sprintf("%q", key), "{read: Crewlet}")), on,
			"integrations.ssh.hosts[0].users.read", "unknown_value"},
		{"an operate user with a space", hostsBlock(host("api-1", "api-1.example.com", fmt.Sprintf("%q", key),
			"{read: crewlet-read, operate: \"crewlet ops\"}")), on, "integrations.ssh.hosts[0].users.operate", "unknown_value"},
		{"one user for both levels", hostsBlock(host("api-1", "api-1.example.com", fmt.Sprintf("%q", key),
			"{read: crewlet-read, operate: crewlet-read}")), on, "integrations.ssh.hosts[0].users.operate", "conflict"},
		{"a port out of range", hostsBlock(fmt.Sprintf("      - {id: api-1, address: api-1.example.com, port: 65536, "+
			"host_keys: [%q], users: %s}\n", key, users)), on, "integrations.ssh.hosts[0].port", "out_of_range"},
		{"a long description", hostsBlock(fmt.Sprintf("      - {id: api-1, address: api-1.example.com, "+
			"host_keys: [%q], users: %s, description: %s}\n", key, users, strings.Repeat("a", 201))), on,
			"integrations.ssh.hosts[0].description", "out_of_range"},
		{"a description of two lines", hostsBlock(fmt.Sprintf("      - {id: api-1, address: api-1.example.com, "+
			"host_keys: [%q], users: %s, description: \"one\\ntwo\"}\n", key, users)), on,
			"integrations.ssh.hosts[0].description", "out_of_range"},
		{"an unknown level", sshBlock(t, ""), "enabled: true, level: administer, hosts: [api-1]",
			"roles[0].integrations.ssh.level", "unknown_value"},
		{"a host the company has not registered", sshBlock(t, ""), "enabled: true, hosts: [api-2]",
			"roles[0].integrations.ssh.hosts[0]", "unknown_value"},
		{"a seat's host twice", sshBlock(t, ""), "enabled: true, hosts: [api-1, api-1]",
			"roles[0].integrations.ssh.hosts[1]", "conflict"},
		{"an agent on at ssh in a company without it", "    enabled: false", "enabled: true, hosts: [api-1]",
			"roles[0].integrations.ssh.hosts[0]", "unknown_value"},
	} {
		_, err := config.ParseCompany([]byte(sshDoc(t, tc.block, tc.seat)))
		p, found := problemAt(err, tc.path)
		switch {
		case err == nil:
			t.Errorf("%s: accepted", tc.name)
		case !found:
			t.Errorf("%s: %v names no problem at %s", tc.name, err, tc.path)
		case p.Kind != tc.kind:
			t.Errorf("%s: %s is %s (%s), want %s", tc.name, tc.path, p.Kind, p.Message, tc.kind)
		}
	}
}

// AN AGENT ON AT SSH NAMES AT LEAST ONE HOST, which its certificate's
// principals are made of: one with none would reach nothing.
func TestAnSSHSeatNeedsAHost(t *testing.T) {
	t.Parallel()
	for _, seat := range []string{"enabled: true", "enabled: true, hosts: []", "enabled: true, level: operate"} {
		_, err := config.ParseCompany([]byte(sshDoc(t, sshBlock(t, ""), seat)))
		if p, ok := problemAt(err, "roles[0].integrations.ssh.hosts"); !ok || p.Kind != "missing" {
			t.Errorf("%s: %v, want a missing host at roles[0].integrations.ssh.hosts", seat, err)
		}
	}
	if _, err := config.ParseCompany([]byte(sshDoc(t, sshBlock(t, ""), "enabled: false"))); err != nil {
		t.Errorf("a seat that is off with no host was refused: %v", err)
	}
}

// A HOST A SEAT NAMES CANNOT BE REMOVED OR RENAMED: the revision that drops it
// is refused at the seat, and the host's grantees are the seats to change
// first, on or off.
func TestAHostASeatNamesCannotBeRemoved(t *testing.T) {
	t.Parallel()
	before := sshDoc(t, sshBlock(t, ""), "enabled: false, hosts: [api-1]")
	c, err := config.ParseCompany([]byte(before))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.SSHHostGrantees("api-1"); !slices.Equal(got, []string{"sre"}) {
		t.Errorf("api-1's grantees = %v, want [sre]", got)
	}
	renamed := strings.Replace(before, "id: api-1", "id: api-2", 1)
	removed := sshDoc(t, "    enabled: true\n    provisioning:\n      certificate_authority: ${SSH_CERTIFICATE_AUTHORITY}",
		"enabled: false, hosts: [api-1]")
	for name, doc := range map[string]string{"renamed": renamed, "removed": removed} {
		_, err := config.ParseCompany([]byte(doc))
		p, ok := problemAt(err, "roles[0].integrations.ssh.hosts[0]")
		if !ok || p.Kind != "unknown_value" || !strings.Contains(p.Message, `host "api-1"`) ||
			!strings.Contains(p.Message, `is granted to "sre"; remove it from those agents first`) {
			t.Errorf("%s: %v, want the seat's host refused naming the agents to change first", name, err)
		}
	}
}

// AN OPERATE SEAT ON A HOST WITH NO OPERATE USER IS ADVISED, not refused: it
// reaches that host at read.
func TestAnOperateSeatOnAReadOnlyHostIsAdvised(t *testing.T) {
	t.Parallel()
	readOnly := fmt.Sprintf("      - {id: api-1, address: api-1.example.com, host_keys: [%q], users: {read: crewlet-read}}\n",
		ed25519Line(t, 1))
	c, err := config.ParseCompany([]byte(sshDoc(t, sshBlock(t, readOnly), "enabled: true, level: operate, hosts: [api-1]")))
	if err != nil {
		t.Fatalf("an operate seat on a read-only host was refused: %v", err)
	}
	var found bool
	for _, w := range c.AdvisoryWarnings() {
		if w.Path == "roles[0].integrations.ssh.hosts[0]" && strings.Contains(w.Message, `host "api-1" has no operate user`) {
			found = true
		}
	}
	if !found {
		t.Errorf("advisories %+v say nothing of api-1's missing operate user", c.AdvisoryWarnings())
	}
}

// THE CERTIFICATE AUTHORITIES ARE CREDENTIALS: a read of the document masks a
// value written inline at either, as it masks every sealed field, and leaves
// the reference Save writes readable.
func TestTheSSHCertificateAuthoritiesAreMaskedOnRead(t *testing.T) {
	t.Parallel()
	c := &config.Company{Integrations: config.Integrations{SSH: &config.SSH{Provisioning: &config.SSHProvisioning{
		CertificateAuthority:     "-----BEGIN OPENSSH PRIVATE KEY-----",
		NextCertificateAuthority: "${SSH_CERTIFICATE_AUTHORITY_NEXT}",
	}}}}
	got := c.Redact().Integrations.SSH.Provisioning
	if strings.Contains(got.CertificateAuthority, "PRIVATE KEY") {
		t.Errorf("certificate_authority read as %q", got.CertificateAuthority)
	}
	if got.NextCertificateAuthority != "${SSH_CERTIFICATE_AUTHORITY_NEXT}" {
		t.Errorf("next_certificate_authority's reference read as %q", got.NextCertificateAuthority)
	}
}
