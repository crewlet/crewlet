/**
 * The SSH integration's servers, and for each one whether Crewlet itself
 * reads it for Atlas.
 *
 * THE COMMANDS ARE SHOWN BEFORE THE SWITCH TAKES EFFECT. Reading a server on a
 * timer is a change in what Crewlet does on it, so the switch only opens the
 * question: the dialog lists the exact commands, as the engine answers them
 * (`inventory_commands`), and nothing is written until its second press.
 *
 * ONLY THAT ONE FIELD IS WRITTEN (`PUT .../hosts/{id}/inventory`): this screen
 * holds none of a server's key lines, so it never sends the server whole.
 */

import { useState } from "react";
import {
  Callout,
  CodeBlock,
  ConfirmModal,
  Link,
  List,
  ListItem,
  Section,
  Switch,
  Text,
} from "@crewlethq/ui";
import { QueryState } from "~/components/common.tsx";
import { useRest } from "~/lib/useRest.ts";
import { pathSegment, rest, RestError } from "~/protocol/index.ts";
import type { SetupSSHServer, SetupToolState } from "~/contract/setup.ts";
import { integrationDocHref } from "./docs.ts";

const SSH_STATE = "/setup/integrations/ssh";

async function readServers(signal: AbortSignal): Promise<SetupToolState | null> {
  return (await rest.get(SSH_STATE, signal)) as SetupToolState | null;
}

/** What each inventory command reads, and what Crewlet keeps of it. */
function reads(containers: boolean): string {
  return containers
    ? "Lists the running containers. Crewlet keeps each container's name and image, and drops its command line as it reads the answer."
    : "Lists the TCP ports the server listens on. It names no process.";
}

export function SshServers({ managed }: { managed: string | null }) {
  const state = useRest(SSH_STATE, readServers);
  const [asking, setAsking] = useState<{ host: SetupSSHServer; on: boolean } | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const ssh = state.data?.ssh;
  const hosts = ssh?.hosts ?? [];
  // WHAT WOULD RUN ON THIS SERVER: the containers' command only where the
  // server lets its read user list them.
  const commandsFor = (host: SetupSSHServer) =>
    (ssh?.inventory_commands ?? []).filter((c) => !c.containers || host.containers);

  function close() {
    if (busy) return;
    setAsking(null);
    setError(null);
  }

  async function confirm() {
    if (!asking) return;
    setBusy(true);
    setError(null);
    try {
      await rest.put(`${SSH_STATE}/hosts/${pathSegment(asking.host.id)}/inventory`, {
        inventory: asking.on,
      });
      await state.reload({ quiet: true });
      setAsking(null);
    } catch (err) {
      setError(err instanceof RestError ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Section
      spacing="tight"
      title="Servers"
      description="Crewlet reads a server for Atlas only once you switch it on."
    >
      <QueryState error={state.code} refusal={state.refusal} loading={state.loading && !ssh}>
        {hosts.length === 0 ? (
          <Text as="p" variant="caption">
            No server is registered yet.{" "}
            <Link external href={integrationDocHref("ssh", "add-a-server")}>
              Add a server
            </Link>
          </Text>
        ) : (
          <List variant="divided" density="compact">
            {hosts.map((host) => (
              <ListItem
                key={host.id}
                trailing={
                  <Switch
                    size="sm"
                    label="Read for Atlas"
                    checked={host.inventory ?? false}
                    disabledReason={managed ?? undefined}
                    onCheckedChange={(on) => {
                      if (!managed) setAsking({ host, on });
                    }}
                  />
                }
              >
                <Text as="div" mono>
                  {host.id}
                </Text>
                <Text as="div" variant="caption" tone="secondary">
                  {host.description ? `${host.address} · ${host.description}` : host.address}
                </Text>
              </ListItem>
            ))}
          </List>
        )}
      </QueryState>
      {asking?.on && (
        <ConfirmModal
          open
          shape="framed"
          size="md"
          title={`Let Crewlet read ${asking.host.id} for Atlas?`}
          confirmLabel="Switch on"
          submitting={busy}
          onClose={close}
          onConfirm={confirm}
        >
          <Text as="p">
            Crewlet itself will run exactly these read-only commands on {asking.host.id}, as{" "}
            <span className="mono">{asking.host.users.read}</span>, each time Atlas refreshes. No
            agent holds this access, and no other command can run under it.
          </Text>
          <CodeBlock
            selectable
            wrap
            copyable={false}
            label={`The commands Crewlet runs on ${asking.host.id}`}
            code={commandsFor(asking.host)
              .map((c) => c.command)
              .join("\n")}
          />
          <List variant="bulleted" density="compact">
            {commandsFor(asking.host).map((c) => (
              <ListItem key={c.command}>{reads(c.containers)}</ListItem>
            ))}
            {!asking.host.containers && (
              <ListItem>
                Containers are not listed on this server, because its container reads are off.
              </ListItem>
            )}
            <ListItem>
              Every run is recorded in the SSH audit, as an agent's command is. Atlas keeps the open
              ports, the container names and their images, and nothing else the commands print.
            </ListItem>
          </List>
          <Callout variant="info">
            The server accepts this only after its setup is run on it again.{" "}
            <Link external href={integrationDocHref("ssh", "let-the-atlas-read-a-server")}>
              How to run it
            </Link>
          </Callout>
          {error && (
            <Callout variant="danger" live="assertive">
              {error}
            </Callout>
          )}
        </ConfirmModal>
      )}
      {asking && !asking.on && (
        <ConfirmModal
          open
          title={`Stop reading ${asking.host.id} for Atlas?`}
          message="Crewlet stops running the commands at once. What Atlas already shows of this server's ports and containers is retired once two refreshes a day apart have not seen it."
          confirmLabel="Switch off"
          submitting={busy}
          onClose={close}
          onConfirm={confirm}
        >
          {error && (
            <Callout variant="danger" live="assertive">
              {error}
            </Callout>
          )}
        </ConfirmModal>
      )}
    </Section>
  );
}
