# ADR-0107: An agent reads a server for the company alone, and changes one only as a person approved

- **Status:** accepted
- **Authority:** `internal/agent/builtin`
- **Enforced-by:** `internal/agent/builtin.TestATurnSomebodyOutsideTheCompanyWokeReachesNoServer`, `internal/agent/builtin.TestAnOperateCallIsHeldInEveryCase`, `internal/agent/builtin.TestAnOperateCallIsMadeOnlyAsApproved`, `internal/agent/builtin.TestASeatCannotReachAHostItWasNotGranted`, `internal/agent/builtin.TestAReadsOutputIsFencedAndFitsOneAnswer`, `internal/ssh.TestEveryCertificateAndCommandIsRecorded`, `internal/ssh.TestACommandThatDidNotRunIsRecordedByWhatStoppedIt`, `internal/api/mcpapi.TestOnlyAnAdministratorOrANamedApproverAnswersAnOperateCall`, `internal/api/mcpapi.TestApprovingAnOperateCallNeedsAStepUp`
- **Tag-status:** unreleased

## The decision

**Two engine tools reach a server, and neither puts anything in a box.**
`ssh_read` runs a command as the host's read user, whose server holds it to
the read allowlist, with no person asked. `ssh_operate` asks a person before
every call, Allow once or Deny and never Always allow, and only an
administrator or a person `integrations.ssh.approvers` names answers it, at a
fresh step-up. Once approved, the engine signs a five-minute certificate
whose force-command is exactly the approved command and runs it itself, with
exactly the approved stdin. No level or configuration lowers `ssh_operate`
from asking.

**In a turn somebody woke, the seat reaches servers for the company alone.**
Whoever the turn acts for is decided on a company-member row: a person of
the company, a colleague seat, a schedule or the engine may use the seat's
grant; somebody the company does not know, such as a guest in a shared room or
an unauthenticated webhook, reaches no server, and the tool says why in one
line. An operate call such a turn makes is refused before anybody is asked.

**What a seat reaches is the document at every call.** Turning the
integration, the seat's access or a host off refuses the next call, and an
approved call whose access was turned off while it waited runs nothing.

**Every certificate is recorded before it is used, and every command once it
ran, failed or was denied.** A read certificate is signed once a turn and
reused while it is valid; an operate certificate is signed for its held call
alone. A command's record keeps its sealed text, sizes, digests, exit status,
approver and the certificate's serial, and the closed word for what stopped
it; never its output. Output returns only into the turn that ran it, fenced as
text a server printed, at external trust.

## Why the obvious alternative is wrong

An operate certificate handed to the agent's box for its turn is the shape a
person's own SSH access takes, and it is the wrong one: nothing in the box can
stop a prompt-injected agent from running any command with it while it lasts,
and no socket keeps its key out of the box. A seat that reached servers for
whoever messaged it would hand a server's shell to anybody in a room it
listens on.

## What this does not decide

Reads from inside an agent's box, the dashboard's SSH page and the
certificate authority's rotation, which are decided where they are built.
