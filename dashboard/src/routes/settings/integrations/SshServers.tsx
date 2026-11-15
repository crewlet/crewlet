/**
 * The SSH integration's servers: each one added, edited and removed here, its
 * server setup shown and its connection tested, and for each whether
 * Crewlet itself reads it for Atlas.
 *
 * A SERVER IS ITS HOST KEYS FIRST. Add a server reads the keys the address
 * presents (`POST .../hosts/scan`) and shows each fingerprint to be compared
 * with the server's console before it can be saved, or takes the lines pasted
 * from the server itself; nothing is learned on first use. A server is saved
 * whole (`PUT .../hosts/{id}`), as a revision of the company document built on
 * the one the list was read from (`revision_id`), so an edit keeps what it
 * does not draw (the allowlist, the Atlas switch) and a change made meanwhile
 * is refused rather than overwritten (`sshServer.ts`). A new server then shows
 * its setup, which is run on it as root (`GET .../hosts/{id}/setup`), and Test
 * connection proves it (`POST .../hosts/{id}/test`). Remove is held while an
 * agent is granted the server, as the engine refuses it.
 *
 * THE ATLAS COMMANDS ARE SHOWN BEFORE THE SWITCH TAKES EFFECT. Reading a server on a
 * timer is a change in what Crewlet does on it, so the switch only opens the
 * question: the dialog lists the exact commands, as the engine answers them
 * (`inventory_commands`), and nothing is written until its second press. Only
 * that one field is written (`PUT .../hosts/{id}/inventory`).
 *
 * Every write is drawn for every reader and held with the reason the company
 * document's writers give (`useConfigWriteAccess`): an administrator's, and
 * nobody's here where the document is managed elsewhere.
 */

import { useState } from "react";
import {
  Button,
  Callout,
  Checkbox,
  CodeBlock,
  FormField,
  Input,
  Link,
  List,
  ListItem,
  Section,
  Skeleton,
  Stack,
  Switch,
  Text,
  Textarea,
} from "@crewlethq/ui";
import { ConfirmModal, Modal } from "~/components/Modal.tsx";
import { QueryState } from "~/components/common.tsx";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { useRest } from "~/lib/useRest.ts";
import { pathSegment, rest, RestError } from "~/protocol/index.ts";
import type { SetupSSHScannedKey, SetupSSHServer, SetupToolState } from "~/contract/setup.ts";
import { integrationDocHref } from "./docs.ts";
import {
  formOf,
  keyLines,
  lastTestLine,
  removeHeld,
  serverBody,
  serverFormHeld,
  type ServerForm,
} from "./sshServer.ts";

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

/** What a refused or failed request says, in the engine's words. */
function said(err: unknown): string {
  if (err instanceof RestError) {
    return [err.sentence, err.detail].filter(Boolean).join(" ") || err.message;
  }
  return String(err);
}

export function SshServers({ managed }: { managed: string | null }) {
  const state = useRest(SSH_STATE, readServers);
  const access = useConfigWriteAccess();
  const [asking, setAsking] = useState<{ host: SetupSSHServer; on: boolean } | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<SetupSSHServer | "new" | null>(null);
  const [removing, setRemoving] = useState<SetupSSHServer | null>(null);
  const [setupOf, setSetupOf] = useState<string | null>(null);
  const [testing, setTesting] = useState<string | null>(null);
  const [testError, setTestError] = useState<{ id: string; said: string } | null>(null);

  const ssh = state.data?.ssh;
  const hosts = ssh?.hosts ?? [];
  // EVERY WRITE HERE IS A REVISION OF THE COMPANY DOCUMENT, held as one is.
  const held = access.can ? undefined : access.reason;

  async function test(host: SetupSSHServer) {
    setTesting(host.id);
    setTestError(null);
    try {
      await rest.post(`/setup/integrations/ssh/hosts/${pathSegment(host.id)}/test`, {});
    } catch (err) {
      setTestError({ id: host.id, said: said(err) });
    } finally {
      // THE ROW'S RESULT IS THE FLEET'S RECORD of the latest test, read again.
      await state.reload({ quiet: true });
      setTesting(null);
    }
  }

  async function remove() {
    if (!removing) return;
    setBusy(true);
    setError(null);
    try {
      await rest.del(`/setup/integrations/ssh/hosts/${pathSegment(removing.id)}`);
      await state.reload({ quiet: true });
      setRemoving(null);
    } catch (err) {
      setError(said(err));
    } finally {
      setBusy(false);
    }
  }
  // WHAT WOULD RUN ON THIS SERVER: the containers' command only where the
  // server lets its read user list them.
  const commandsFor = (host: SetupSSHServer) =>
    (ssh?.inventory_commands ?? []).filter((c) => !c.containers || host.containers);

  function close() {
    if (busy) return;
    setAsking(null);
    setRemoving(null);
    setError(null);
  }

  async function confirm() {
    if (!asking) return;
    setBusy(true);
    setError(null);
    try {
      await rest.put(`/setup/integrations/ssh/hosts/${pathSegment(asking.host.id)}/inventory`, {
        inventory: asking.on,
      });
      await state.reload({ quiet: true });
      setAsking(null);
    } catch (err) {
      setError(said(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Section
      spacing="tight"
      title="Servers"
      description="Each server pins its host keys, and Crewlet reads it for Atlas only once you switch it on."
      actions={
        <Button
          size="small"
          variant="secondary"
          disabledReason={ssh ? held : "Reading the servers…"}
          onClick={() => setEditing("new")}
        >
          Add a server
        </Button>
      }
    >
      <QueryState error={state.code} refusal={state.refusal} loading={state.loading && !ssh}>
        {hosts.length === 0 ? (
          <Text as="p" variant="caption">
            No server is registered yet.{" "}
            <Link external href={integrationDocHref("ssh", "add-a-server")}>
              How adding a server works
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
                {lastTestLine(host) && (
                  <Text as="div" variant="caption" tone="secondary">
                    {lastTestLine(host)}
                  </Text>
                )}
                {testError?.id === host.id && (
                  <Callout variant="danger" live="assertive">
                    {testError.said}
                  </Callout>
                )}
                <div className="row wrap gap-1" role="group" aria-label={`${host.id}'s actions`}>
                  <Button
                    size="small"
                    variant="ghost"
                    aria-label={`Edit ${host.id}`}
                    disabledReason={held}
                    onClick={() => setEditing(host)}
                  >
                    Edit
                  </Button>
                  <Button
                    size="small"
                    variant="ghost"
                    aria-label={`Server setup for ${host.id}`}
                    onClick={() => setSetupOf(host.id)}
                  >
                    Server setup
                  </Button>
                  <Button
                    size="small"
                    variant="ghost"
                    aria-label={`Test the connection to ${host.id}`}
                    loading={testing === host.id}
                    disabledReason={held}
                    onClick={() => void test(host)}
                  >
                    Test connection
                  </Button>
                  <Button
                    size="small"
                    variant="ghost"
                    aria-label={`Remove ${host.id}`}
                    disabledReason={held ?? removeHeld(host)}
                    onClick={() => setRemoving(host)}
                  >
                    Remove
                  </Button>
                </div>
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
      {editing && ssh && (
        <ServerDialog
          host={editing === "new" ? null : editing}
          revision={ssh.revision_id}
          taken={hosts.map((h) => h.id)}
          onClose={() => setEditing(null)}
          onSaved={async (id, added) => {
            setEditing(null);
            await state.reload({ quiet: true });
            // A NEW SERVER IS SET UP NEXT: nothing reaches it until its setup runs.
            if (added) setSetupOf(id);
          }}
        />
      )}
      {setupOf && <SetupDialog id={setupOf} onClose={() => setSetupOf(null)} />}
      {removing && (
        <ConfirmModal
          open
          destructive
          title={`Remove ${removing.id}?`}
          message={`Crewlet stops pinning ${removing.id}'s host keys and signs no certificate for it, and Atlas stops reading it. The users and files its setup made stay on the server until you remove them there.`}
          cancelLabel="Keep it"
          confirmLabel="Remove server"
          submitting={busy}
          onClose={close}
          onConfirm={remove}
        >
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

/**
 * Add a server, or edit one: its fields, and its host keys read from the
 * address (each fingerprint compared before it is pinned) or pasted from the
 * server itself.
 */
function ServerDialog({
  host,
  revision,
  taken,
  onClose,
  onSaved,
}: {
  host: SetupSSHServer | null;
  revision: string;
  taken: readonly string[];
  onClose: () => void;
  onSaved: (id: string, added: boolean) => Promise<void>;
}) {
  const [form, setForm] = useState<ServerForm>(() => formOf(host));
  const [scanned, setScanned] = useState<SetupSSHScannedKey[] | null>(null);
  const [compared, setCompared] = useState(false);
  const [scanning, setScanning] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const set = (patch: Partial<ServerForm>) => setForm((f) => ({ ...f, ...patch }));
  // THE LINES A SCAN FILLED IN, still as it filled them, are pinned only once
  // their fingerprints were compared; a person who pasted them read them on
  // the server itself.
  const pinnable = (scanned ?? []).filter((k) => !k.problem);
  const fromScan =
    scanned !== null && keyLines(form.keys).join("\n") === pinnable.map((k) => k.line).join("\n");
  const blocked = serverFormHeld(form, taken, host !== null, fromScan && !compared);

  async function scan() {
    setScanning(true);
    setError(null);
    try {
      const port = form.port.trim() === "" ? 0 : Number(form.port);
      const answer = (await rest.post("/setup/integrations/ssh/hosts/scan", {
        address: form.address.trim(),
        port,
      })) as { host_keys: SetupSSHScannedKey[] };
      setScanned(answer.host_keys);
      setCompared(false);
      set({
        keys: answer.host_keys
          .filter((k) => !k.problem)
          .map((k) => k.line)
          .join("\n"),
      });
    } catch (err) {
      setError(said(err));
    } finally {
      setScanning(false);
    }
  }

  async function save() {
    if (blocked || saving) return;
    const id = host?.id ?? form.id.trim();
    setSaving(true);
    setError(null);
    try {
      await rest.put(
        `/setup/integrations/ssh/hosts/${pathSegment(id)}`,
        serverBody(form, host, revision),
      );
      await onSaved(id, host === null);
    } catch (err) {
      setError(said(err));
    } finally {
      setSaving(false);
    }
  }

  return (
    <Modal
      open
      size="md"
      title={host ? `Edit ${host.id}` : "Add a server"}
      onClose={onClose}
      onSubmit={() => void save()}
      dismissable={!saving}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={saving}>
            Cancel
          </Button>
          <Button
            variant="primary"
            loading={saving}
            disabledReason={blocked}
            onClick={() => void save()}
          >
            {host ? "Save" : "Add server"}
          </Button>
        </>
      }
    >
      <Stack gap={3}>
        {!host && (
          <FormField
            label="Name"
            helper="Up to 40 lowercase letters, digits and hyphens. It is part of every certificate for this server, so it stays as it is."
          >
            {(field) => (
              <Input
                id={field.id}
                aria-describedby={field.describedBy}
                width="full"
                spellCheck={false}
                placeholder="api-1"
                value={form.id}
                onChange={(event) => set({ id: event.target.value })}
              />
            )}
          </FormField>
        )}
        <FormField
          label="Address"
          helper="Its host name or IP address, with no scheme, user, port or path."
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              spellCheck={false}
              placeholder="api-1.example.com"
              value={form.address}
              onChange={(event) => set({ address: event.target.value })}
            />
          )}
        </FormField>
        <FormField label="Port" optional helper="Empty is 22.">
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              type="number"
              inputMode="numeric"
              min={1}
              max={65535}
              step={1}
              width="xs"
              placeholder="22"
              value={form.port}
              onChange={(event) => set({ port: event.target.value })}
            />
          )}
        </FormField>
        <FormField
          label="Read user"
          helper="The login user for read access, which its setup creates. Every command it runs passes the server's read allowlist."
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              spellCheck={false}
              value={form.read}
              onChange={(event) => set({ read: event.target.value })}
            />
          )}
        </FormField>
        <FormField
          label="Operate user"
          optional
          helper="The login user for approved operate commands, such as crewlet-ops. Empty leaves the server read-only."
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              spellCheck={false}
              value={form.operate}
              onChange={(event) => set({ operate: event.target.value })}
            />
          )}
        </FormField>
        <FormField
          label="Description"
          optional
          helper="What the server is, shown to agents and approvers."
        >
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              value={form.description}
              onChange={(event) => set({ description: event.target.value })}
            />
          )}
        </FormField>
        <Checkbox
          label="Let the read user list containers"
          description="Adds docker ps and docker logs --tail, through sudo -n, to what it may run."
          checked={form.containers}
          onCheckedChange={(containers) => set({ containers })}
        />
        <Stack gap={2}>
          <span>
            <Button
              size="small"
              variant="secondary"
              loading={scanning}
              disabledReason={form.address.trim() === "" ? "Give its address first." : undefined}
              onClick={() => void scan()}
            >
              {host ? "Read its host keys again" : "Read its host keys"}
            </Button>
          </span>
          {scanned && (
            <>
              <List variant="divided" density="compact" aria-label="The keys it presented">
                {scanned.map((k) => (
                  <ListItem key={k.line}>
                    <Text as="div" mono>
                      {k.fingerprint_sha256}
                    </Text>
                    <Text as="div" variant="caption" tone="secondary">
                      {k.problem ? `${k.type}: not pinned, ${k.problem}` : k.type}
                    </Text>
                  </ListItem>
                ))}
              </List>
              <Checkbox
                label="I compared each fingerprint with the server's console"
                description="ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub on the server, and the same for each key type, prints the fingerprint it holds."
                checked={compared}
                onCheckedChange={setCompared}
              />
            </>
          )}
          <FormField
            label="Host key lines"
            helper="One a line, as /etc/ssh/ssh_host_*_key.pub holds them. Crewlet accepts no other key from this server."
          >
            {(field) => (
              <Textarea
                id={field.id}
                aria-describedby={field.describedBy}
                rows={3}
                spellCheck={false}
                value={form.keys}
                onChange={(event) => set({ keys: event.target.value })}
              />
            )}
          </FormField>
        </Stack>
        {error && (
          <Callout variant="danger" live="assertive">
            {error}
          </Callout>
        )}
      </Stack>
    </Modal>
  );
}

/** A server's setup, rendered by the engine for it, to run on it as root. */
function SetupDialog({ id, onClose }: { id: string; onClose: () => void }) {
  const setup = useRest(
    `/setup/integrations/ssh/hosts/${pathSegment(id)}/setup`,
    async (signal) =>
      (await rest.get(`/setup/integrations/ssh/hosts/${pathSegment(id)}/setup`, signal)) as {
        script: string;
      },
  );
  return (
    <Modal
      open
      size="lg"
      title={`Set up ${id}`}
      onClose={onClose}
      footer={
        <Button variant="secondary" onClick={onClose}>
          Done
        </Button>
      }
    >
      <Stack gap={3}>
        <Text as="p">
          Run this on {id} as root. It trusts Crewlet's certificate authority, creates the two users
          and installs the gate their commands pass. It is safe to run again, and is run again after
          a rotation, an allowlist change or a new operate user. Then press Test connection.
        </Text>
        <QueryState error={setup.code} refusal={setup.refusal} loading={false}>
          {setup.data ? (
            <CodeBlock selectable copyable label={`The setup of ${id}`} code={setup.data.script} />
          ) : (
            <Skeleton variant="text" rows={6} label={`Loading ${id}'s setup`} />
          )}
        </QueryState>
      </Stack>
    </Modal>
  );
}
