package ssh

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// The server setup: one idempotent POSIX sh script per host that a person runs
// as root once, and again after a rotation or an allowlist change. It trusts
// the engine's authority for the two Crewlet users only, writes the principal
// each accepts, installs the gate every certificate runs through, and changes
// nothing for any other user of the server.

// SetupVersion is the version of the setup a script renders; the gate answers
// its own version, which Test connection compares.
const SetupVersion = 1

// GatePath is where the setup installs the gate.
const GatePath = "/usr/local/sbin/crewlet-gate"

// gateScript is the gate, verbatim: the only program a certificate runs.
//
//go:embed gate.sh
var gateScript string

// GateVersionLine is what the gate answers to `crewlet-gate version`.
func GateVersionLine() string { return fmt.Sprintf("crewlet-gate %d", SetupVersion) }

// DefaultReadAllowlist is the read rules rendered for a server that writes
// none of its own, with the container reads where it turns them on. Paths are
// Debian's and Ubuntu's.
func DefaultReadAllowlist(containers bool) []string {
	rules := []string{
		"/usr/bin/uptime",
		"/usr/bin/free -m",
		"/bin/df -h",
		"/usr/bin/systemctl status *",
		"/usr/bin/systemctl is-active *",
		"/usr/bin/systemctl list-units --type=service --state=failed --no-pager",
		"/usr/bin/journalctl --no-pager -u * -n *",
		"/usr/bin/journalctl --no-pager -u * --since=*",
		"/usr/bin/ss -tln",
	}
	if containers {
		rules = append(rules, "/usr/bin/sudo -n /usr/bin/docker ps", "/usr/bin/sudo -n /usr/bin/docker logs --tail * *")
	}
	return rules
}

// ReadAllowlist is the rules a host's read user runs under: its own, or the
// default.
func ReadAllowlist(h config.SSHHost) []string {
	if len(h.ReadAllowlist) > 0 {
		return h.ReadAllowlist
	}
	return DefaultReadAllowlist(h.Containers)
}

// allowlistHeader opens the rendered allowlist file.
const allowlistHeader = `# Crewlet read allowlist, one command per line. The first word is an absolute
# path. "*" is any one word that does not start with "-". A word ending in "=*"
# is that option with any value. A final "..." allows any further words. Never
# put "*" where a file path goes.
`

// ErrUnrenderable is a host or authority the setup cannot be rendered for.
var ErrUnrenderable = errors.New("ssh: the server setup cannot be rendered")

// RenderSetup is the setup script for host, trusting each of caLines (the
// current authority's public line, and a staged one's during a rotation).
// Every value it writes was held to its shape by config validation; it checks
// them again here, since a script is run as root.
func RenderSetup(h config.SSHHost, caLines []string) (string, error) {
	switch {
	case !config.SSHHostID(h.ID) || !config.SSHUser(h.Users.Read):
		return "", fmt.Errorf("%w: host %q or its read user is not one config admits", ErrUnrenderable, h.ID)
	case h.Users.Operate != "" && (!config.SSHUser(h.Users.Operate) || h.Users.Operate == h.Users.Read):
		return "", fmt.Errorf("%w: host %q's operate user is not one config admits", ErrUnrenderable, h.ID)
	case len(caLines) == 0:
		return "", fmt.Errorf("%w: no certificate authority to trust", ErrUnrenderable)
	}
	for _, line := range caLines {
		if _, err := ParsePublicLine(line); err != nil {
			return "", fmt.Errorf("%w: %w", ErrUnrenderable, err)
		}
	}
	rules := ReadAllowlist(h)
	for _, rule := range rules {
		if sentence := config.SSHAllowlistRule(rule); sentence != "" {
			return "", fmt.Errorf("%w: %s", ErrUnrenderable, sentence)
		}
	}
	ops := h.Users.Operate
	users := h.Users.Read
	if ops != "" {
		users += "," + ops
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("#!/bin/sh\n# Crewlet SSH setup %d for host %s. Safe to run again.\nset -eu\numask 022\n", SetupVersion, h.ID)
	w("HOST_ID=%s\nREAD_USER=%s\n", h.ID, h.Users.Read)
	if ops != "" {
		w("OPS_USER=%s\n", ops)
	}
	w("DIR=/etc/ssh/crewlet\n\n")
	w("install -d -m 0755 \"$DIR\" \"$DIR/principals\"\n\n")
	w("# 1. Trust the engine's certificate authority. Two lines while a new one is staged.\n")
	w("cat >\"$DIR/user_ca.pub\" <<'CREWLET_CA'\n%s\nCREWLET_CA\nchmod 0644 \"$DIR/user_ca.pub\"\n\n", strings.Join(caLines, "\n"))
	w("# 2. The login users. Neither has a password.\n")
	w("id \"$READ_USER\" >/dev/null 2>&1 || useradd --create-home --shell /bin/sh \"$READ_USER\"\n")
	if ops != "" {
		w("id \"$OPS_USER\" >/dev/null 2>&1 || useradd --create-home --shell /bin/sh \"$OPS_USER\"\n")
	}
	w("passwd -l \"$READ_USER\" >/dev/null\n")
	if ops != "" {
		w("passwd -l \"$OPS_USER\" >/dev/null\n")
	}
	w("if getent group systemd-journal >/dev/null; then usermod -aG systemd-journal \"$READ_USER\"; fi\n\n")
	w("# 3. The certificate principal each user accepts, and no other user's.\n")
	w("find \"$DIR/principals\" -type f -delete\n")
	w("printf 'restrict crewlet.%%s.read\\n' \"$HOST_ID\" >\"$DIR/principals/$READ_USER\"\n")
	w("chmod 0644 \"$DIR/principals/$READ_USER\"\n")
	if ops != "" {
		w("printf 'restrict crewlet.%%s.operate\\n' \"$HOST_ID\" >\"$DIR/principals/$OPS_USER\"\n")
		w("chmod 0644 \"$DIR/principals/$OPS_USER\"\n")
	}
	w("\n# 4. The gate every Crewlet certificate runs through, and the read allowlist.\n")
	w("cat >%s <<'CREWLET_GATE'\n%sCREWLET_GATE\nchmod 0755 %s\n", GatePath, gateScript, GatePath)
	w("cat >\"$DIR/read-allowlist\" <<'CREWLET_ALLOW'\n%s%s\nCREWLET_ALLOW\nchmod 0644 \"$DIR/read-allowlist\"\n\n",
		allowlistHeader, strings.Join(rules, "\n"))
	w("# 5. Privileges.\n")
	if ops != "" {
		w("# Operate is full sudo, as decided per server; narrow it here if wanted.\n")
		w("printf '%%s ALL=(ALL) NOPASSWD: ALL\\n' \"$OPS_USER\" >/etc/sudoers.d/crewlet-ops\n")
		w("chmod 0440 /etc/sudoers.d/crewlet-ops\nvisudo -cf /etc/sudoers.d/crewlet-ops >/dev/null\n")
	} else {
		w("rm -f /etc/sudoers.d/crewlet-ops\n")
	}
	if h.Containers {
		w("printf '%%s ALL=(root) NOPASSWD: /usr/bin/docker ps, /usr/bin/docker logs --tail *\\n' \"$READ_USER\" >/etc/sudoers.d/crewlet-read\n")
		w("chmod 0440 /etc/sudoers.d/crewlet-read\nvisudo -cf /etc/sudoers.d/crewlet-read >/dev/null\n")
	} else {
		w("rm -f /etc/sudoers.d/crewlet-read\n")
	}
	w("\n# 6. sshd, for the Crewlet users only.\n")
	w("cat >/etc/ssh/sshd_config.d/60-crewlet.conf <<'CREWLET_SSHD'\n")
	w("# Crewlet SSH setup %d. Applies only to the Crewlet users.\n", SetupVersion)
	w("Match User %s\n", users)
	for _, line := range []string{
		"TrustedUserCAKeys /etc/ssh/crewlet/user_ca.pub",
		"AuthorizedPrincipalsFile /etc/ssh/crewlet/principals/%u",
		"AuthorizedKeysFile none",
		"AuthenticationMethods publickey",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"ExposeAuthInfo yes",
		"LogLevel VERBOSE",
		"DisableForwarding yes",
		"PermitTTY no",
		"PermitUserRC no",
		"PermitTunnel no",
	} {
		w("    %s\n", line)
	}
	w("Match User %s\n    ForceCommand %s read\nMatch all\nCREWLET_SSHD\n", h.Users.Read, GatePath)
	w("chmod 0644 /etc/ssh/sshd_config.d/60-crewlet.conf\n")
	w("grep -Eq '^[[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config\\.d/\\*\\.conf' /etc/ssh/sshd_config ||\n")
	w("  { echo \"This sshd does not include /etc/ssh/sshd_config.d; add the block above to sshd_config by hand.\" >&2; exit 1; }\n")
	w("sshd -t\nsystemctl reload ssh 2>/dev/null || systemctl reload sshd\n")
	w("echo \"Crewlet SSH setup %d applied for $HOST_ID.\"\n", SetupVersion)
	return b.String(), nil
}

// HostDigest is the SHA-256, in hex, of everything about a host its setup and
// a Test connection depend on: all of its fields but its description. A test
// recorded under one digest says nothing about the host once it changes.
func HostDigest(h config.SSHHost) string {
	h.Description = ""
	keys := slices.Clone(h.HostKeys)
	slices.Sort(keys)
	h.HostKeys = keys
	data, err := json.Marshal(h)
	if err != nil {
		panic(err) // a struct of strings, ints and bools always encodes
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
