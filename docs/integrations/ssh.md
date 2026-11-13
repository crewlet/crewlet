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
| `operate` | Everything `read` can, plus run any command a person, or the company's delegate, approves first. Each approval is for one command, once, exactly as shown to the approver. | Run a command nobody approved, or run an approved command twice |

Granting `operate` asks for a fresh sign-in (step-up). Only administrators, the people named in `integrations.ssh.approvers`, and the people an `ssh_operate` rule of [delegated decisions](../guides/delegated-decisions.md) escalates to approve an operate command, unless that rule's delegate allows it first.

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
- writes the one principal each user accepts and, on a server that [lets the atlas read it](#let-the-atlas-read-a-server), the inventory's principal beside the read user's;
- installs the gate, `/usr/local/sbin/crewlet-gate`, the only program a Crewlet certificate runs, and the read allowlist, `/etc/ssh/crewlet/read-allowlist`;
- writes the inventory's own allowlist, `/etc/ssh/crewlet/inventory-allowlist`, on a server that lets the atlas read it, and removes it on one that does not;
- gives the operate user full sudo (edit `/etc/sudoers.d/crewlet-ops` to narrow it) and, with `containers`, gives the read user `sudo -n` for `docker ps` and `docker logs --tail` alone;
- adds `/etc/ssh/sshd_config.d/60-crewlet.conf`, which applies to the two Crewlet users only: certificates from Crewlet's authority, no `authorized_keys`, no password, no PTY, no forwarding of any kind, `LogLevel VERBOSE`, and the read user's every command forced through the gate;
- checks the result with `sshd -t` and reloads sshd. It changes nothing for any other user.

The gate refuses an empty command (an interactive shell), any quote or shell character, an option where a rule takes a word, and anything off the allowlist, each with exit status 126, and logs each command it runs or refuses with the certificate's key ID. sshd's own log names the key ID and serial of every certificate it accepts.

The gate matches a command against a rule word for word, with two places where a program's bare name stands for the absolute path the rule names: the first word, which the gate runs by that path, and the word after `sudo -n`, which sudo resolves through its `secure_path` and runs only if the sudoers line names that same path. So `sudo -n docker ps` matches `/usr/bin/sudo -n /usr/bin/docker ps`, while `sudo -n ./docker ps`, `sudo -n /tmp/docker ps` and `sudo -n docker ps -a` are refused. Every other word is exact, and a bare name never stands for a file path. A certificate that names a command of its own, which only the engine's [inventory](#let-the-atlas-read-a-server) carries, runs that command whatever the client asked for, and only where the inventory's allowlist holds it. This is gate version 3; a server set up with an earlier version runs no inventory and keeps answering its old version, so Test connection fails its gate version step until its setup runs again. An agent's reads keep working on it meanwhile.

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
- **`ssh_operate`** runs one command as the server's operate user once it is approved: `{host, command, stdin, reason}`, the command up to 4096 bytes, the stdin up to 128 KiB of text, and a reason of up to 500 characters. Every call is put to a person, who reads the server, the command exactly as written, the stdin's size, SHA-256 and first 2 KiB, and the reason, and answers **Allow once** or **Deny**. Only an administrator, or a person `integrations.ssh.approvers` names by seat handle, answers, after a fresh sign-in, and Allow once is given only after reading the whole call: `GET /mcp-approvals/{id}` shows every byte of the stdin, and the answer names the digest of what it showed. On Allow once, Crewlet signs a five-minute certificate whose forced command is that command and runs it once itself, on the server as the approver was shown it: if its address, port, operate user or host keys changed while the call waited, nothing runs and the agent asks again. No certificate or key for operate ever reaches the agent's box.

  Where the company delegates `ssh_operate` ([Delegated decisions](../guides/delegated-decisions.md)), the call goes to the rule's delegate first, which reads it whole and allows, escalates or denies it within the limits below; it reaches the people above only once escalated. A delegate's allow is the same Allow once.
- **`ask_to_operate`** asks a person to approve one exact command before the agent calls it: `{item, ask, host, command, stdin, reason}` puts a decision on the work item quoting the server, the command and the stdin's SHA-256. Choosing **Run it once** is an SSH approval, so only the people above may choose it, after a fresh sign-in. The agent's `ssh_operate` call in the turn that answer starts runs without a second prompt where it matches byte for byte, once.

### What a delegate never allows

Whatever a delegate answers, Crewlet puts a command to people when its command or stdin matches the always-escalate list, case ignored:

| Class | Matches, for example |
|---|---|
| `sshd_config` | `/etc/ssh`, `sshd_config`, `sshd` |
| `sudoers` | `sudoers`, `visudo`, `/etc/sudo` |
| `users_and_groups` | `useradd`, `usermod`, `userdel`, `passwd`, `chpasswd`, `groupadd`, `/etc/passwd`, `/etc/group` |
| `authorized_keys` | `authorized_keys`, `.ssh/` |
| `crewlet_gate` | `crewlet-gate`, `read-allowlist`, `/etc/ssh/crewlet`, `crewlet-ops` |
| `certificate_authority` | `TrustedUserCAKeys`, `user_ca`, `ssh-keygen`, `ca.pub` |
| `firewall` | `iptables`, `ip6tables`, `nft`, `ufw`, `firewall-cmd`, `/etc/nftables` |
| `secret_files` | `/etc/shadow`, private key files (`id_rsa`, `ssh_host_*_key`, `.pem`, `.key`), a private key block, an `.env` file outside `/tmp` and `/var/tmp` |
| `data_deletion` | `rm -r`, `find -delete`, `shred`, `dd of=/dev/…`, `mkfs`, `wipefs`, `DROP DATABASE`, `TRUNCATE`, `docker volume rm`, `docker system prune`, `kubectl delete` |
| `crewlet_company` | `crewlet config`, `crewlet secrets`, `crewlet iam`, `crewlet budgets` |
| `download_into_shell` | `curl … \| sh`, `wget … \| bash`, `base64 -d … \| sh`, `$(curl …)`, `source <(…)` |
| `hidden_text` | `eval`, `\x41` escapes, `$'…'`, `base64 -d`, `xxd -r`, a glob in `/etc`, `/root`, `/boot`, `/usr` or `/var/log` |
| `auditing_and_logging` | `auditctl`, `auditd`, `rsyslog`, `journald`, `journalctl --vacuum`, removing or truncating `/var/log`, `history -c`, `HISTFILE`, `setenforce` |

A shell can spell any of these another way, which is why the list also escalates the spellings that hide what runs, and the delegate is told to escalate whatever it cannot read. `integrations.ssh.always_escalate` adds the company's own patterns; nothing removes a class. Delegates also allow at most 10 commands per server in one clock hour.

Both answer `{host, exit_code, stdout, stderr, truncated, duration_ms}` (with `read_commands` and a `note` on a read the gate refused), each stream cut to `commands.max_output_bytes` and the answer to one tool answer, and marked as text a server printed so the agent reads it as data. The output goes only to the turn that ran the command: the run's call log and an operate call's recorded result keep its size and digest alone. The agent's own kept session transcript holds what it read, as it holds everything the agent worked with, readable only by administrators and the agent's lead.

In a turn somebody woke, an agent reaches its servers only for somebody of the company: a person of the company, a colleague agent, a schedule or Crewlet itself. A turn a credential acting as no seat woke, such as a pipeline's token filing a task, reaches no server either: a token reaches only the apps its permissions name, and a server is no app. A turn woken by somebody the company does not know, such as a guest in a shared room or an unauthenticated webhook, reaches no server, and the tool says why. That holds through colleagues: an agent asked by a colleague acting for such a person acts for them too.

Every certificate Crewlet signs is recorded before it is used, and every command once it ran, failed or was denied: the agent, the turn, the server and user, the command (sealed), the stdin's size and digest, the exit status, the output's size and digest (never its text), the approver and the certificate's serial. Every verdict on an operate command is recorded too, before it acts: a delegate's, with its verdict, confidence, sealed reason and findings and why Crewlet overruled it, Crewlet's own escalation at a deadline, and a person's approval through a decision. `GET /setup/integrations/ssh/audit` pages all three (`kind=certificate`, `command` or `verdict`).

## Let the atlas read a server

The [atlas](../concepts/atlas.md) can show what a server is doing: the ports it listens on and the containers it runs. For that the engine itself, never an agent, runs two read-only commands on the server each time the atlas refreshes (`atlas.refresh_interval_seconds`, an hour by default). It is **off for every server** until you switch it on for that server, and the server refuses it until its setup has been run again.

The commands are a closed set. Nothing else can be run this way:

| Command | Run on | Why it is safe to run on a timer | What the atlas keeps |
|---|---|---|---|
| `/usr/bin/ss -tln` | every server that opts in | Lists listening TCP sockets by address and port. It takes no `-p`, so it never asks which process holds a socket, and it changes nothing. | The port of each socket bound beyond the loopback address, as the server's open ports. |
| `/usr/bin/sudo -n /usr/bin/docker ps` | a server that opts in and has `containers` on | Lists running containers. It is the exact command the `containers` setting already lets the read user run, with no option, so it cannot inspect a container or read its environment. | Each container's name and image, as a workload that runs on the server. |

`docker ps` prints each container's command line, and a command line can carry a credential somebody passed as an argument. The engine reads the image and name columns of the table and no other: the command, the ports, the state and the age of a container are never copied out of the answer. A container whose name or image is shaped like a credential is refused and counted, never shown. What a command printed is held in memory for the parse and dropped; like every command's output, it reaches no record.

To switch it on for a server:

1. Open **Company › Integrations › SSH › Settings**. Under **Servers**, switch **Read for Atlas** on for the server. The dialog lists the exact commands that will run on that server before anything is saved. The same switch is `hosts[].inventory: true` in the company document, and `PUT /setup/integrations/ssh/hosts/{id}/inventory` with `{"inventory": true}`.
2. Run the server's setup again as root (`GET /setup/integrations/ssh/hosts/{id}/setup`). It adds the inventory's principal, `crewlet.<host id>.inventory`, and the inventory's allowlist. Until then the server refuses the inventory's certificate and the atlas says so on the SSH source.
3. Press Test connection (`POST /setup/integrations/ssh/hosts/{id}/test`). On a server that opts in it adds a step, `inventory_binding`: the engine signs in with an inventory certificate, asks for a different command, and passes only if the server ran the certificate's command instead.

How each run is held:

- **Its own certificate, for one command.** The engine signs a certificate for each command, valid for two minutes, whose only principal is `crewlet.<host id>.inventory` and whose force-command is that one command. The engine signs no inventory certificate for any other command, and the server's gate runs the command the certificate names, not the one a client sends. The certificate's key ID is `crewlet/_engine/inventory/<command id>`, so the server's own log tells the engine's reads from every agent's.
- **Recorded like an agent's command.** The certificate is recorded before it is used and the command once it ran or failed, in the SSH audit (`GET /setup/integrations/ssh/audit?seat=_engine`), held by `_engine` in the place an agent's handle takes: its text, sizes, exit status and the digest of its output, never the output.
- **Within the company's limits.** A command is stopped at `commands.timeout_seconds`, and output past `commands.max_output_bytes` is not read at all, since half a table would look like containers that stopped.
- **A server that cannot be read is a finding.** A server that is down, was not set up again, or whose command failed is named on the SSH source's standing with what to do, and what an earlier read recorded for it stands.

Switching it off stops the commands at the next refresh. Run the setup again to take the principal and the allowlist off the server. The ports and containers the atlas held for it are retired once two reads a day apart have not seen them.

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
    always_escalate:               # optional: patterns no delegate may allow, added to the engine's own list
      - '\bkubectl\s+scale\b'
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
        inventory: true            # optional: let the engine read its ports and containers for the atlas

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
| `always_escalate` | Up to 64 regular expressions (RE2), each up to 512 bytes, matched against an operate command and its stdin. A command any of them matches is put to people whatever a delegate answers. They add to the engine's own list and never shorten it. |
| `commands.timeout_seconds` | How long a command may run before Crewlet stops it: 10 to 900 seconds, 120 when empty. |
| `commands.max_output_bytes` | How much of each of a command's output streams reaches the agent, the rest cut: 4096 to 1048576 bytes, 65536 when empty. |
| `hosts[].id` | Up to 40 lowercase letters, digits and hyphens, unique, at most 200 servers. |
| `hosts[].address` | A host name or an IP address alone, with no scheme, user, port or path. A loopback, unspecified, link-local or multicast address, and `localhost`, are refused. |
| `hosts[].host_keys` | One to eight public key lines as `/etc/ssh/ssh_host_*_key.pub` holds them: `ssh-ed25519`, `ecdsa-sha2-nistp256`, `-nistp384` or `-nistp521`, or `ssh-rsa` of at least 3072 bits. A certificate, a line with options and a repeated key are refused. |
| `hosts[].users.read` | The login user for read access, whose every command the server forces through its allowlist. |
| `hosts[].users.operate` | The login user for approved commands. It must differ from the read user. Absent leaves the server read-only. |
| `hosts[].description` | One line of up to 200 characters, shown to agents and approvers. |
| `hosts[].containers` | Lets the read user list containers and read their logs through `sudo -n`. Off by default. |
| `hosts[].inventory` | Lets the engine itself read the server's listening ports, and its containers where `containers` is on, for the atlas. Off by default; see [Let the atlas read a server](#let-the-atlas-read-a-server). |
| `hosts[].read_allowlist` | Up to 100 rules replacing the default read allowlist, each an absolute path, then words of the gate's own characters. |
| `roles[].integrations.ssh.level` | `read` or `operate`, one level for every server the agent names. |
| `roles[].integrations.ssh.hosts` | The servers the agent may reach, each a host id above. Required when the agent is on. |

An agent at `operate` on a server with no operate user reaches that server at read, and the document says so as an advisory. A server an agent names cannot be removed or renamed until it is removed from that agent. A person's seat holds no SSH block.

## Turning access off

Turning `integrations.ssh` off, turning an agent off, or removing a server from an agent stops Crewlet signing for it at the next use. A certificate already issued lasts at most its few minutes of validity. Nothing is made on a server for an agent, so there is nothing to remove.

## What is not supported yet

- Receiving events from a server.
