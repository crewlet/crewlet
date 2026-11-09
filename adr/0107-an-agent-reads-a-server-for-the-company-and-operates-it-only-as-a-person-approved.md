# ADR-0107: An agent reads a server for the company alone, and changes one only as a person approved

- **Status:** accepted
- **Authority:** `internal/agent/builtin`
- **Enforced-by:** `internal/agent/builtin.TestATurnSomebodyOutsideTheCompanyWokeReachesNoServer`, `internal/agent/builtin.TestAnOperateCallIsHeldInEveryCase`, `internal/agent/builtin.TestAnOperateCallIsMadeOnlyAsApproved`, `internal/agent/builtin.TestASeatCannotReachAHostItWasNotGranted`, `internal/agent/builtin.TestAReadsOutputIsFencedAndFitsOneAnswer`, `internal/ssh.TestEveryCertificateAndCommandIsRecorded`, `internal/ssh.TestACommandThatDidNotRunIsRecordedByWhatStoppedIt`, `internal/api/mcpapi.TestOnlyAnAdministratorOrANamedApproverAnswersAnOperateCall`, `internal/api/mcpapi.TestApprovingAnOperateCallNeedsAStepUp`, `internal/api/mcpapi.TestAnOperateCallsApproverReadsAllOfIt`, `internal/agent/builtin.TestAReadSeatHasNoOperateTool`, `internal/agent/builtin.TestWhatAServerPrintedReachesTheTurnAndNoRecord`, `internal/agent/builtin.TestAnAskFromAStrangersTurnCarriesTheStranger`, `internal/agent/runner.TestAnApprovedCallsUnkeptOutputReachesTheRunAlone`, `internal/engine.TestTurningSSHOffStopsIssuanceAtOnce`, `internal/ssh.TestAHeldOperateCallApprovedOnceRunsOnce`, `internal/ssh.TestNoOperateMaterialReachesABox`
- **Tag-status:** unreleased

## The decision

**Two engine tools reach a server, and neither puts anything in a box.**
`ssh_read` runs a command as the host's read user, whose server holds it to
the read allowlist, with no person asked. `ssh_operate` asks a person before
every call, Allow once or Deny and never Always allow, and only an
administrator or a person `integrations.ssh.approvers` names answers it, at a
fresh step-up, after reading the whole call: the question shows the start of
the stdin, `GET /mcp-approvals/{id}` shows every byte of it to whoever may
answer, and Allow once names the digest of what that read showed. Once
approved, the engine signs a five-minute certificate whose force-command is
exactly the approved command and runs it itself, with exactly the approved
stdin, on the server the person was shown: the held call is bound to a digest
of the host's address, port, operate user and pinned keys, and a call whose
host changed while it waited runs nothing. No level or configuration lowers `ssh_operate`
from asking.

**In a turn somebody woke, the seat reaches servers for the company alone.**
Whoever the turn acts for is decided on a company-member row: a person of
the company, a colleague seat, a schedule or the engine may use the seat's
grant; somebody the company does not know, such as a guest in a shared room or
an unauthenticated webhook, reaches no server, and the tool says why in one
line. An operate call such a turn makes is refused before anybody is asked.
The stranger follows a colleague's ask: an ask made from such a turn wakes the
colleague for them too, so a guest cannot reach a server by asking an agent to
ask the one that holds it.

**What a seat reaches is the document at every call.** Turning the
integration, the seat's access or a host off refuses the next call, and an
approved call whose access was turned off while it waited runs nothing.

**Every certificate is recorded before it is used, and every command once it
ran, failed or was denied.** A read certificate is signed once a turn and
reused while it is valid; an operate certificate is signed for its held call
alone. A command's record keeps its sealed text, sizes, digests, exit status,
approver and the certificate's serial, and the closed word for what stopped
it; never its output. Output returns only into the turn that ran it, fenced as
text a server printed, at external trust: the run's call log and the held
call's recorded result keep its size and digest alone. The seat's own kept
session transcript holds what the agent read, since it is the agent's working
memory; it is readable only by administrators and the seat's lead, and that is
accepted.

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
