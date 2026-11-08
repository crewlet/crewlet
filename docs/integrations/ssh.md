# SSH

SSH lets agents reach your servers with **certificates Crewlet signs for minutes at a time**. Crewlet runs its own SSH certificate authority: it makes the authority when you connect, seals its private key, and never shows or exports it. Each server trusts the authority's public line once, so turning an agent on or off, or changing its servers, touches no server.

- **No key is stored on a server** for any agent, and none in an agent's box.
- **Every certificate names the agent and its turn** in its key ID, so each server's own log says which agent ran each command.
- **Each certificate reaches one level on the servers it names**, through a principal per server and level, so a certificate for one server is refused by every other.
- **Server host keys are pinned** in the company document. Crewlet accepts no other key from a server and never learns one on first use.

Nothing here receives events from a server; see [What is not supported yet](#what-is-not-supported-yet).

## What agents get

An agent is on at one level across every server it is granted.

| An agent at | Can | Cannot |
|---|---|---|
| `read` (the default) | Run each server's allowlisted read commands, such as uptime, disk and memory use, service status and journal reads, with no approval. The server forces every command through its allowlist. | Run anything off the allowlist, open a shell, or forward a port |
| `operate` | Everything `read` can, plus run any command a person approves first. Each approval is for one command, once, exactly as shown to the approver. | Run a command nobody approved, or run an approved command twice |

Granting `operate` asks for a fresh sign-in (step-up). Only administrators, and the people named in `integrations.ssh.approvers`, approve an operate command.

## Connecting Crewlet

Open **Company › Integrations › SSH** and press **Save**.

- Saving makes Crewlet's certificate authority, an Ed25519 key sealed as `SSH_CERTIFICATE_AUTHORITY`.
- The form then shows the authority's public line, for example `ssh-ed25519 AAAA... crewlet-ssh-ca-0123456789abcdef`. The name after the key is the first 16 hex digits of the key's SHA-256, so the line names its own authority.
- Servers trust that line. Its private key never leaves the engine.

## Add a server

Each server is an entry in `integrations.ssh.hosts`, with its pinned host keys:

1. **Read the server's host keys** on the server itself: `cat /etc/ssh/ssh_host_ed25519_key.pub`, and the ECDSA or RSA key beside it if the server uses one.
2. **Compare each fingerprint** with `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` on the server console, so the key you pin is the server's and not a key presented on the way.
3. **Write the entry** with an id, the address, the two login users and the key lines.

A host id is part of every certificate principal for the server (`crewlet.<host id>.read` and `crewlet.<host id>.operate`), so changing it means running the server's setup again. A server shared by two companies takes a distinct host id in each.

## Configuration

```yaml
integrations:
  ssh:
    enabled: true
    provisioning:
      certificate_authority: ${SSH_CERTIFICATE_AUTHORITY}             # minted on Save, sealed, never shown
      next_certificate_authority: ${SSH_CERTIFICATE_AUTHORITY_NEXT}   # only while a rotation is staged
      source_addresses: [203.0.113.7/32]                              # optional
    approvers: [ops-lead]          # optional: people who may approve operate commands, beside administrators
    commands:
      timeout_seconds: 120         # optional, 10 to 900
      max_output_bytes: 65536      # optional, 4096 to 1048576, per stream returned to the agent
    hosts:
      - id: api-1
        address: api-1.example.com
        port: 22                   # optional, default 22
        host_keys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleExampleExampleExampleExampleExample
        users:
          read: crewlet-read
          operate: crewlet-ops     # optional: absent means the server is read-only
        description: Production API server

roles:
  - name: Site Reliability Engineer
    handle: sre
    integrations:
      ssh:
        enabled: true
        level: read                # read (default) or operate
        hosts: [api-1]
```

| Field | Meaning |
|---|---|
| `provisioning.certificate_authority` | A reference to the sealed authority. Save mints it; a value written inline is refused. |
| `provisioning.next_certificate_authority` | A second authority, present only while a rotation is staged. Servers trust both lines; Crewlet never signs with this one until it is promoted. It must name a different value from `certificate_authority`. |
| `provisioning.source_addresses` | Up to 16 address ranges, such as the engine's egress address `203.0.113.7/32`, written into every certificate's `source-address`. A range is written in its network form: `203.0.113.0/24`, never `203.0.113.7/24`. |
| `approvers` | Handles of human seats who may approve an operate command, beside administrators. An agent's handle is refused: only people approve production commands. |
| `commands.timeout_seconds` | How long a command may run before Crewlet stops it: 10 to 900 seconds, 120 when empty. |
| `commands.max_output_bytes` | How much of each of a command's output streams reaches the agent, the rest cut: 4096 to 1048576 bytes, 65536 when empty. |
| `hosts[].id` | Up to 40 lowercase letters, digits and hyphens, unique, at most 200 servers. |
| `hosts[].address` | A host name or an IP address alone, with no scheme, user, port or path. A loopback, unspecified, link-local or multicast address, and `localhost`, are refused. |
| `hosts[].host_keys` | One to eight public key lines as `/etc/ssh/ssh_host_*_key.pub` holds them: `ssh-ed25519`, `ecdsa-sha2-nistp256`, `-nistp384` or `-nistp521`, or `ssh-rsa` of at least 3072 bits. A certificate, a line with options and a repeated key are refused. |
| `hosts[].users.read` | The login user for read access, whose every command the server forces through its allowlist. |
| `hosts[].users.operate` | The login user for approved commands. It must differ from the read user. Absent leaves the server read-only. |
| `hosts[].description` | One line of up to 200 characters, shown to agents and approvers. |
| `roles[].integrations.ssh.level` | `read` or `operate`, one level for every server the agent names. |
| `roles[].integrations.ssh.hosts` | The servers the agent may reach, each a host id above. Required when the agent is on. |

An agent at `operate` on a server with no operate user reaches that server at read, and the document says so as an advisory. A server an agent names cannot be removed or renamed until it is removed from that agent. A person's seat holds no SSH block.

## Turning access off

Turning `integrations.ssh` off, turning an agent off, or removing a server from an agent stops Crewlet signing for it at the next use. A certificate already issued lasts at most its few minutes of validity. Nothing is made on a server for an agent, so there is nothing to remove.

## What is not supported yet

- The server setup Crewlet renders for each server, **Test connection**, and the certificate authority's rotation routes.
- The agents' `ssh_read` and `ssh_operate` tools, and the audit of every certificate and command.
- Receiving events from a server.
