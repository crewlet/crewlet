# ADR-0108: A read rule's program is typed by its bare name only where sudo resolves it, and the seat is told the commands as it types them

- **Status:** accepted
- **Authority:** `internal/ssh`
- **Enforced-by:** `internal/ssh.TestTheGateTakesAProgramsBareNameAfterSudo`, `internal/ssh.TestTheGateTakesAProgramsBareNameAfterSudoOnARealServer`, `internal/ssh.TestAHostsReadCommandsAreListedAsAnAgentTypesThem`, `internal/capability.TestYourAccessListsEachHostsReadCommands`, `internal/capability.TestAHostsReadCommandsAreBounded`, `internal/capability.TestAChangedAllowlistIsSaidAndIsNoChange`, `internal/agent/builtin.TestARefusedReadNamesTheHostsReadCommands`
- **Tag-status:** unreleased

## The decision

**The gate matches a rule word for word, with two places where a program's
bare name stands for the absolute path the rule names.** The first is a
rule's first word, which the gate itself runs by that path. The second is the
word after `sudo -n`, where the rule names an absolute path: there `docker`
stands for `/usr/bin/docker`, and anything holding a `/` must be that path
exactly, so `./docker`, `bin/docker` and `/tmp/docker` are refused. Every
other word is exact, and a bare name never stands for a path argument
(`tail -n 20 syslog` never matches a rule naming `/var/log/syslog`).

This is safe because nothing the agent types chooses the program. The gate
runs the rule's own absolute path for `sudo`, and sudo resolves the bare name
through its `secure_path`, which only root writes, and then runs it only if
the sudoers line the setup wrote names that same absolute path. A name that
resolves anywhere else is refused by sudo, so the gate's match and sudo's
check agree on one program. The gate carries version 2, and the rendered
setup is version 2 with it.

**A seat is told each granted host's read commands as it types them.** One
function renders a host's allowlist in that form: programs by their bare
names, and a placeholder (`<unit>`, `<n>`, `<container>`, `<value>`,
`<arg>`) for each word a rule leaves open. "Your access" lists at most 16 per
host with the rest counted; the list is part of the snapshot, so the next
turn's section says a changed allowlist, with no change line of its own and
outside the digest a pending access review is measured to, and the shorter
compaction levels drop it.
A read the gate refused (exit 126) is answered with the same list, so the
seat retries correctly in the same turn.

## Why the obvious alternative is wrong

Accepting a bare name for any word whose rule names a path would let
`tail -n 20 syslog` stand for `/var/log/syslog` and make the file read depend
on the read user's working directory. Telling the agent to type absolute
paths instead keeps the gate simple and leaves the agent guessing paths that
differ between distributions; the gate is the place that knows the path, and
sudo is the place that resolves the name.

## What this does not decide

The default allowlist itself, and the operate user's commands, which run as
approved and are not matched against any list.
