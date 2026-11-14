package ssh

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// The servers' half of the atlas (ADR-0134): on each server that opts in
// (`inventory: true`), the ports it listens on become facts of the server and
// its running containers become workloads that run on it. A server that does
// not opt in is never contacted.
//
// SSH SAYS A WORKLOAD EXISTS AND ONLY NAMES ITS SERVER: the server is the
// settings' entry, so opting it out retires its workloads and no server.
//
// Each server is two parts of the read, its ports and its containers. A part
// the engine could not read (the server is down, its setup was not run again,
// the command failed or printed past the output limit) is a finding on the
// source's standing, and what an earlier read of it recorded stands.

// InventoryRunner runs one inventory command and records it ([Runner]).
type InventoryRunner interface {
	Inventory(ctx context.Context, call InventoryCall) (Result, error)
}

// AtlasSource is the SSH integration's [collect.Source]. It is built with the
// node's runner rather than registered from init, since every command it runs
// is recorded on the access log the runner writes to.
type AtlasSource struct{ run InventoryRunner }

// NewAtlasSource is the source over run.
func NewAtlasSource(run InventoryRunner) AtlasSource { return AtlasSource{run: run} }

// Kind is the SSH integration.
func (AtlasSource) Kind() integration.Kind { return integration.KindSSH }

// Collect runs the inventory's commands on every server that opts in and
// reports what they showed as claims in the integration's voice.
func (s AtlasSource) Collect(ctx context.Context, in collect.Input) (collect.Read, error) {
	block := in.Config.Integrations.SSH
	if block == nil || !block.Enabled || block.Provisioning == nil ||
		strings.TrimSpace(block.Provisioning.CertificateAuthority) == "" {
		return collect.Read{}, integration.ErrNotConfigured
	}
	b := collect.NewBuilder(integration.KindSSH, in.Now)
	use := Use{Limits: block.Commands, SourceAddresses: block.Provisioning.SourceAddresses}
	for i, h := range block.Hosts {
		// WHAT A SERVER OPTS IN TO IS ONE LIST: nothing where its inventory
		// is off, so such a server is skipped before anything is resolved.
		commands := h.InventoryCommands()
		if len(commands) == 0 {
			continue
		}
		if use.CA == nil {
			// THE AUTHORITY IS RESOLVED ONLY ONCE A SERVER OPTS IN: a
			// company with none never has its key opened for this duty.
			ca, err := setup.ParseSSHCertificateAuthority(
				strings.TrimSpace(in.Env.Value(block.Provisioning.CertificateAuthority)))
			if err != nil {
				return collect.Read{}, fmt.Errorf("ssh: the certificate authority is not usable, so no "+
					"server is read: %w", err)
			}
			use.CA = ca
		}
		where := "integrations.ssh.hosts[" + strconv.Itoa(i) + "]"
		host, ok := b.Names(where, atlas.KindHost, atlas.HostSSHPrefix+"/"+h.ID)
		if !ok {
			continue
		}
		b.Scope(h.ID)
		for _, command := range commands {
			if err := ctx.Err(); err != nil {
				return collect.Read{}, err
			}
			b.Call()
			res, err := s.run.Inventory(ctx, InventoryCall{Use: use, Host: h, Command: command})
			if FailureOf(err) == accessdomain.FailureEngineError && err != nil {
				// THE ENGINE'S OWN FAULT FAILS THE READ WHOLE: a command
				// whose record did not land is not read into the atlas.
				return collect.Read{}, fmt.Errorf("ssh: inventory of %s: %w", h.ID, err)
			}
			part := inventoryParts[command]
			detail := unreadDetail(res, err)
			if detail == "" {
				detail = part.read(b, where, host, h.ID, res.Stdout)
			}
			if detail != "" {
				b.Unread(collect.Unread{Part: h.ID + " " + part.name, Scope: h.ID, Detail: detail, Holds: part.holds(host, h.ID)})
			}
		}
	}
	return b.Read(), nil
}

// inventoryPart is one inventory command's part of a server's read: its
// name, how its output becomes claims, and which claims those are.
type inventoryPart struct {
	name string
	// read claims what stdout shows and answers "" or why it could not.
	read  func(b *collect.Builder, where string, host atlas.Ref, id string, stdout []byte) string
	holds func(host atlas.Ref, id string) func(atlas.Claim) bool
}

// inventoryParts is each inventory command's part.
var inventoryParts = map[string]inventoryPart{
	config.SSHInventoryPorts: {
		name: "ports",
		read: func(b *collect.Builder, where string, host atlas.Ref, _ string, stdout []byte) string {
			ports, err := ParseOpenPorts(stdout)
			if err != nil {
				return unreadTable
			}
			for _, p := range ports {
				b.Says(where+".ports", host, atlas.FactOpenPort, strconv.Itoa(p))
			}
			return ""
		},
		holds: func(host atlas.Ref, _ string) func(atlas.Claim) bool {
			return func(c atlas.Claim) bool {
				return c.Subject == host && c.Facet.Kind == atlas.FacetFact && c.Facet.Fact == atlas.FactOpenPort
			}
		},
	},
	config.SSHInventoryContainers: {
		name: "containers",
		read: func(b *collect.Builder, where string, host atlas.Ref, id string, stdout []byte) string {
			containers, skipped, err := ParseContainers(stdout)
			if err != nil {
				return unreadTable
			}
			for range skipped {
				b.Withhold(where+".containers", errMisaligned)
			}
			for _, c := range containers {
				at := where + ".containers[" + c.Name + "]"
				w, ok := b.Ref(at, atlas.KindWorkload, workloadPrefix(id)+c.Name)
				if !ok {
					continue
				}
				b.Says(at+".name", w, atlas.FactName, c.Name)
				b.Says(at+".image", w, atlas.FactImage, c.Image)
				b.Relates(at, w, atlas.RelationRunsOn, host)
			}
			return ""
		},
		holds: func(_ atlas.Ref, id string) func(atlas.Claim) bool {
			return func(c atlas.Claim) bool {
				return c.Subject.Kind == atlas.KindWorkload && strings.HasPrefix(c.Subject.Key, workloadPrefix(id))
			}
		},
	},
}

// workloadPrefix opens the key of every workload on the server id.
func workloadPrefix(id string) string { return atlas.HostSSHPrefix + "/" + id + "/" }

// errMisaligned is a container's row that does not sit under the table's
// header, which is skipped whole rather than cut.
var errMisaligned = errors.New("a row that does not sit under the table's header")

// unreadTable is the finding for output that is not its command's table.
const unreadTable = "The command printed something other than its table, so none of it is read."

// unreadDetail is why a command's output is not read, in the source's own
// words and never the server's, or "" for output to parse.
func unreadDetail(res Result, err error) string {
	if err != nil {
		switch FailureOf(err) {
		case accessdomain.FailureUnreachable:
			return "The engine could not reach the server. Let the engine's address through its firewall."
		case accessdomain.FailureHostKeyRefused:
			return "The server presented a host key that is not pinned for it. Compare its fingerprint on " +
				"the server's console, then update the server's keys."
		case accessdomain.FailureAuthenticationRefused:
			return "The server refused the inventory's certificate. Run the server setup again, so the " +
				"server accepts the inventory."
		case accessdomain.FailureTimedOut:
			return "The command did not finish within the company's command timeout."
		}
		return "The server closed the session before the command finished."
	}
	switch {
	case res.TimedOut:
		return "The command did not finish within the company's command timeout."
	case res.ExitCode == GateRefused:
		return "The server's gate refused the command. Run the server setup again, so its inventory " +
			"allowlist holds the command."
	case res.ExitCode != 0:
		return "The command exited with status " + strconv.Itoa(res.ExitCode) + " on the server."
	case res.Truncated:
		return "The command printed more than the company's output limit, so none of it is read."
	}
	return ""
}
