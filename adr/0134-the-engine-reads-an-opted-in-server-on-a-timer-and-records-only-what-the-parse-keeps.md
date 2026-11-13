# ADR-0134: The engine reads an opted-in server on a timer, and records only what the parse keeps

- **Status:** accepted
- **Authority:** `internal/ssh`
- **Enforced-by:** `internal/ssh.TestAServerNotOptedInIsNeverContacted`, `internal/ssh.TestAnInventoryCertificateRunsOnlyTheCommandItNames`, `internal/ssh.TestAServerSetUpWithoutTheInventoryRefusesIt`, `internal/ssh.TestARequestOutsideTheLockedShapesIsNotSigned`, `internal/ssh.TestTheParsersDropTheCommandColumn`, `internal/ssh.TestNoCredentialShapeReachesTheAtlasFromAServer`, `internal/ssh.TestTheInventoryBecomesWorkloadsAndOpenPorts`, `internal/ssh.TestAnInventoryPartThatCouldNotBeReadIsAFinding`, `internal/accessdomain.TestTheEnginesInventoryIsRecordedUnderNoSeat`
- **Cost-when-tried:** not tried. Until this record the engine ran a command on a server only inside an agent's turn or at a person's press of Test connection; nothing ran on a timer.
- **Tag-status:** unreleased

## The decision

This record amends [ADR-0107](0107-an-agent-reads-a-server-for-the-company-and-operates-it-only-as-a-person-approved.md) in two places and leaves the rest of it standing. ADR-0107 has two engine tools reach a server, both inside a turn; this adds a third reach, the engine's own, on the atlas's refresh interval. ADR-0107 keeps a command's output out of every record; this lets typed values parsed from the output of the inventory's own commands be recorded in the atlas, and still keeps the output itself out of every record.

**A server is read only where the company opts it in, and only where the server itself was told.** `integrations.ssh.hosts[].inventory` is off by default. The engine never signs an inventory certificate for a server whose document entry does not carry it, so such a server is never contacted. Switching it on reaches a server only when its setup is run on it again: the setup writes the inventory's principal and the inventory's own allowlist only for a server that opts in, so a server whose owner ran nothing refuses the certificate at sign-in.

**The commands are a closed set of two, named in code.** `/usr/bin/ss -tln` lists listening TCP sockets; without `-p` it never asks which process holds one. `/usr/bin/sudo -n /usr/bin/docker ps` lists running containers, runs only on a server whose `containers` is on, and is the exact command that setting already lets the read user run, with no option that could inspect a container. Neither changes anything on the server.

**The certificate is the engine's own and binds its one command.** Each command runs on a certificate of purpose `inventory`, valid two minutes, since it is used once to sign in. Its only principal is `crewlet.<host id>.inventory`, which no seat's certificate carries, and its force-command is the command. `Sign` refuses the inventory's principal with any command outside the closed set, and refuses that principal to an agent's purpose. The server's gate runs the command a certificate names, never the one the client sent, and holds it to the inventory's allowlist rather than the read allowlist an administrator edits. The key ID is `crewlet/_engine/inventory/<command id>`, so the server's log tells the engine's reads from every agent's.

**Every run is recorded as a seat's read is.** The certificate is recorded before it is presented and the command once it ran or failed, with its sealed text, sizes, digest, exit status and the certificate's serial. The holder is `_engine`, in the place a seat's handle takes; a handle holds no underscore, so no seat can be named it. A command whose record did not land fails the whole read: nothing it printed is read into the atlas.

**Only what the parse keeps is recorded, and the parse keeps no command line.** The parsers read the columns they keep and no other: a socket's local address, and a container's image and name. A container's command, ports, state and age, and a socket's process, are never copied out of a row, and a row that does not sit under its table's header is skipped whole rather than cut. What is kept goes through the atlas's grammars: a port as a number, an image as a registry's reference, a name as a key, each refused where it is shaped like a credential. The output is held in memory for the parse and dropped. Output past the company's limit is not read at all.

## Why the obvious alternative is wrong

**Reading what agents' `ssh_read` calls printed** would cost no new reach, and it is the wrong source twice over. That output is deliberately in no record (ADR-0107), so reading it means keeping it; and what an agent ran is whatever its task needed, not an inventory, so the atlas would know the servers agents happened to visit and not the others.

**Running the inventory on an agent's read certificate, or under the read principal,** would need no server change. It would also put commands nobody asked a seat to run in that seat's name in the server's log and in the audit, and would let a server that never agreed to a timer be read on one: every server set up for agents already accepts the read principal. A principal of its own is what makes the server's agreement a fact on the server.

**`docker ps --format` with fixed fields** would never print the command column. The gate refuses braces, which a template needs, and admitting them for one command would widen what every read may send. Parsing the table by its header's columns and never reading the command cell costs nothing on the server.

**On by default for servers with containers on** would fill the map sooner. Running commands on a production server on a timer is a change in what the engine does there, and an owner who never chose it would learn of it from the server's log.

## What this does not decide

Other commands (running services, the operating system's release), which would join the closed set with a record of their own; reading a server's containers further than `docker ps` prints; and how the atlas joins a workload to the repository its image is built from.
