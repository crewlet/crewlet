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

1. **Read the server's host keys.** Crewlet can read what the server presents (`POST /setup/integrations/ssh/hosts/scan`), without signing in, one connection per key type. Or read them on the server itself: `cat /etc/ssh/ssh_host_ed25519_key.pub`, and the ECDSA or RSA key beside it.
2. **Compare each fingerprint** with `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` on the server's console, so the key you pin is the server's and not a key presented on the way.
3. **Save the server** with an id, the address, the two login users and the key lines (`PUT /setup/integrations/ssh/hosts/{id}`). It is a revision of the company document like any other.
4. **Run the server setup** Crewlet renders for it (`GET /setup/integrations/ssh/hosts/{id}/setup`) as root on the server.

A host id is part of every certificate principal for the server (`crewlet.<host id>.read` and `crewlet.<host id>.operate`), so changing it means running the server's setup again. A server shared by two companies takes a distinct host id in each.

## The server setup

The setup is one POSIX shell script, safe to run again: after a rotation, an allowlist change or a new operate user, run it again and it rewrites the same files. It:

- trusts Crewlet's authority line (and a staged one during a rotation) in `/etc/ssh/crewlet/user_ca.pub`;
- creates the read and operate users with no password, and adds the read user to `systemd-journal` where the group exists;
- writes the one principal each user accepts;
- installs the gate, `/usr/local/sbin/crewlet-gate`, the only program a Crewlet certificate runs, and the read allowlist, `/etc/ssh/crewlet/read-allowlist`;
- gives the operate user full sudo (edit `/etc/sudoers.d/crewlet-ops` to narrow it) and, with `containers`, gives the read user `sudo -n` for `docker ps` and `docker logs --tail` alone;
- adds `/etc/ssh/sshd_config.d/60-crewlet.conf`, which applies to the two Crewlet users only: certificates from Crewlet's authority, no `authorized_keys`, no password, no PTY, no forwarding of any kind, `LogLevel VERBOSE`, and the read user's every command forced through the gate;
- checks the result with `sshd -t` and reloads sshd. It changes nothing for any other user.

The gate refuses an empty command (an interactive shell), any quote or shell character, an option where a rule takes a word, and anything off the allowlist, each with exit status 126, and logs each command it runs or refuses with the certificate's key ID. sshd's own log names the key ID and serial of every certificate it accepts.

The gate matches a command against a rule word for word, with two places where a program's bare name stands for the absolute path the rule names: the first word, which the gate runs by that path, and the word after `sudo -n`, which sudo resolves through its `secure_path` and runs only if the sudoers line names that same path. So `sudo -n docker ps` matches `/usr/bin/sudo -n /usr/bin/docker ps`, while `sudo -n ./docker ps`, `sudo -n /tmp/docker ps` and `sudo -n docker ps -a` are refused. Every other word is exact, and a bare name never stands for a file path. This is gate version 2; a server set up with an earlier version keeps refusing the bare name after `sudo -n` until its setup runs again, and Test connection fails its gate version step until then.

The default read allowlist:

```
/usr/bin/uptime
/usr/bin/free -m
/bin/df -h
/usr/bin/systemctl status *
/usr/bin/systemctl is-active *
/usr/bin/systemctl list-units --type=service --state=failed --no-pager
/usr/bin/journalctl --no-pager -u * -n *
/usr/bin/journalctl --no-pager -u * --since=*
/usr/bin/ss -tln
```

With `containers`, it adds `/usr/bin/sudo -n /usr/bin/docker ps` and `/usr/bin/sudo -n /usr/bin/docker logs --tail * *`. A server's own `read_allowlist` replaces the list. In a rule, `*` is any one word that does not start with `-`, a word ending in `=*` is that option with any value, and a final `...` allows any further words. Never put `*` where a file path goes.

An agent is told each of its servers' read commands as it types them, in the "Your access" section of every turn: programs by their bare names, and a placeholder for each word a rule leaves open (`<unit>`, `<n>`, `<container>`, `<value>`, or `<arg>` for any other), such as `systemctl status <unit>` and `sudo -n docker logs --tail <n> <container>`. The section lists at most 16 per server and counts the rest, and it changes on the next turn after a server's allowlist does.

## The agents' tools

An agent on at SSH has two tools; an agent at `read` has only the first.

- **`ssh_read`** runs one command on one of the agent's servers as its read user, with no approval: `{host, command}`, the command one line of up to 4096 bytes. The server's gate decides what runs. A command it refuses exits 126, and the answer then lists that server's read commands again (`read_commands`, with `read_commands_more` counting any past the first 16) so the agent can retry in the same turn.
- **`ssh_operate`** runs one command as the server's operate user once a person approves it: `{host, command, stdin, reason}`, the command up to 4096 bytes, the stdin up to 128 KiB of text, and a reason of up to 500 characters. Every call is put to a person, who reads the server, the command exactly as written, the stdin's size, SHA-256 and first 2 KiB, and the reason, and answers **Allow once** or **Deny**. Only an administrator, or a person `integrations.ssh.approvers` names by seat handle, answers, after a fresh sign-in, and Allow once is given only after reading the whole call: `GET /mcp-approvals/{id}` shows every byte of the stdin, and the answer names the digest of what it showed. On Allow once, Crewlet signs a five-minute certificate whose forced command is that command and runs it once itself, on the server as the approver was shown it: if its address, port, operate user or host keys changed while the call waited, nothing runs and the agent asks again. No certificate or key for operate ever reaches the agent's box.

Both answer `{host, exit_code, stdout, stderr, truncated, duration_ms}` (with `read_commands` and a `note` on a read the gate refused), each stream cut to `commands.max_output_bytes` and the answer to one tool answer, and marked as text a server printed so the agent reads it as data. The output goes only to the turn that ran the command: the run's call log and an operate call's recorded result keep its size and digest alone. The agent's own kept session transcript holds what it read, as it holds everything the agent worked with, readable only by administrators and the agent's lead.

In a turn somebody woke, an agent reaches its servers only for somebody of the company: a person of the company, a colleague agent, a schedule or Crewlet itself. A turn woken by somebody the company does not know, such as a guest in a shared room or an unauthenticated webhook, reaches no server, and the tool says why. That holds through colleagues: an agent asked by a colleague acting for such a person acts for them too.

Every certificate Crewlet signs is recorded before it is used, and every command once it ran, failed or was denied: the agent, the turn, the server and user, the command (sealed), the stdin's size and digest, the exit status, the output's size and digest (never its text), the approver and the certificate's serial. `GET /setup/integrations/ssh/audit` pages both.

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
| `hosts[].containers` | Lets the read user list containers and read their logs through `sudo -n`. Off by default. |
| `hosts[].read_allowlist` | Up to 100 rules replacing the default read allowlist, each an absolute path, then words of the gate's own characters. |
| `roles[].integrations.ssh.level` | `read` or `operate`, one level for every server the agent names. |
| `roles[].integrations.ssh.hosts` | The servers the agent may reach, each a host id above. Required when the agent is on. |

An agent at `operate` on a server with no operate user reaches that server at read, and the document says so as an advisory. A server an agent names cannot be removed or renamed until it is removed from that agent. A person's seat holds no SSH block.

## Turning access off

Turning `integrations.ssh` off, turning an agent off, or removing a server from an agent stops Crewlet signing for it at the next use. A certificate already issued lasts at most its few minutes of validity. Nothing is made on a server for an agent, so there is nothing to remove.

## What is not supported yet

- Receiving events from a server.
