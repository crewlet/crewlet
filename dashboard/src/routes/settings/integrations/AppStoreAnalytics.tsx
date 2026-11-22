/**
 * App Store analytics: impressions and product page views need an ongoing
 * report request per app, which only an Admin key can create. A team that
 * keeps no Admin key turns it on here with one pasted once
 * (`POST /setup/integrations/app_store_connect/analytics`): Crewlet signs each
 * app's request with it at once and keeps no copy, and the answer tells the
 * person to revoke it. The key leaves this card's state the moment it is sent.
 */

import { useState } from "react";
import {
  Button,
  Callout,
  FormField,
  Input,
  List,
  ListItem,
  Section,
  Select,
  Stack,
  Text,
  ValueField,
} from "@crewlethq/ui";
import { ConfirmModal, Modal } from "~/components/Modal.tsx";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import { pathSegment, rest, RestError } from "~/protocol/index.ts";
import type { SetupAppStoreAnalyticsAnswer } from "~/contract/setup.ts";
import { analyticsLine, type ConnectedKey } from "./appStoreAnalytics.ts";

export function AppStoreAnalytics({ connections }: { connections: readonly string[] }) {
  const access = useConfigWriteAccess();
  const [open, setOpen] = useState(false);
  const [answer, setAnswer] = useState<SetupAppStoreAnalyticsAnswer | null>(null);
  const held = !access.can
    ? access.reason
    : connections.length === 0
      ? "Connect a team first: the key turns analytics on for a connection's apps."
      : undefined;
  return (
    <Section title="App Store analytics">
      <Stack gap={2}>
        <Text>
          Impressions and product page views need a report request for each app that only an Admin
          key can create. Paste one here to turn them on: Crewlet uses it once and keeps no copy.
        </Text>
        {answer && (
          <Callout variant={answer.apps.some((a) => a.state === "failed") ? "warning" : "success"}>
            <List>
              {answer.apps.map((a) => (
                <ListItem key={a.app}>{analyticsLine(a)}</ListItem>
              ))}
            </List>
            <Text>{answer.revoke}</Text>
          </Callout>
        )}
        <div>
          <Button
            size="small"
            variant="secondary"
            disabledReason={held}
            onClick={() => setOpen(true)}
          >
            Turn on App Store analytics
          </Button>
        </div>
      </Stack>
      {open && (
        <TurnOn
          connections={connections}
          onClose={() => setOpen(false)}
          onDone={(a) => {
            setAnswer(a);
            setOpen(false);
          }}
        />
      )}
    </Section>
  );
}

function TurnOn({
  connections,
  onClose,
  onDone,
}: {
  connections: readonly string[];
  onClose: () => void;
  onDone: (a: SetupAppStoreAnalyticsAnswer) => void;
}) {
  const [connection, setConnection] = useState(connections[0] ?? "");
  const [keyId, setKeyId] = useState("");
  const [key, setKey] = useState("");
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const blocked =
    keyId.trim() === "" || key.trim() === ""
      ? "Paste the Admin key's ID and its .p8 file."
      : undefined;
  async function send() {
    if (blocked) return;
    setSending(true);
    setError(null);
    const body = { connection, key_id: keyId.trim(), private_key: key };
    // OUT OF STATE AT ONCE: the key is held by nothing on this page once sent.
    setKey("");
    try {
      const sent = await rest.request("POST", "/setup/integrations/app_store_connect/analytics", {
        body,
      });
      onDone(sent.body as SetupAppStoreAnalyticsAnswer);
    } catch (err) {
      setError(err instanceof RestError ? err.message : "Nothing was turned on; try again.");
    } finally {
      setSending(false);
    }
  }
  return (
    <Modal
      open
      size="md"
      title="Turn on App Store analytics"
      onClose={onClose}
      onSubmit={() => void send()}
      dismissable={!sending}
      footer={
        <>
          <Button variant="secondary" onClick={onClose} disabled={sending}>
            Cancel
          </Button>
          <Button
            variant="primary"
            loading={sending}
            disabledReason={blocked}
            onClick={() => void send()}
          >
            Turn on
          </Button>
        </>
      }
    >
      <Stack gap={3}>
        {connections.length > 1 && (
          <Select
            width="full"
            ariaLabel="Connection"
            value={connection}
            onChange={(value) => setConnection(String(value))}
            options={connections.map((c) => ({ value: c, label: c }))}
          />
        )}
        <FormField label="Admin key ID" helper="Ten capital letters and digits, beside the key.">
          {(field) => (
            <Input
              id={field.id}
              aria-describedby={field.describedBy}
              width="full"
              spellCheck={false}
              value={keyId}
              onChange={(event) => setKeyId(event.target.value)}
            />
          )}
        </FormField>
        {/* MASKED, LINE BREAKS KEPT, AND A FILE PICKER: the kit's box for a key
            that spans lines, as the setup form draws one. */}
        <ValueField
          label="Admin key (.p8)"
          kind="secret-multiline"
          accept=".p8"
          rows={6}
          value={key}
          onChange={setKey}
          help="Create a team key with the Admin role, paste it, then revoke it once this is done."
        />
        {error && <Callout variant="danger">{error}</Callout>}
      </Stack>
    </Modal>
  );
}

/**
 * The connected keys, the Settings summary's API keys row, each removed in one
 * action (`DELETE /setup/integrations/app_store_connect/keys/{connection}/{slot}`):
 * Crewlet stops using it on the next call and deletes its sealed copy. A
 * connection's last key stays until another is added.
 */
export function AppStoreKeys({ keys }: { keys: readonly ConnectedKey[] }) {
  const access = useConfigWriteAccess();
  const [removing, setRemoving] = useState<ConnectedKey | null>(null);
  const [busy, setBusy] = useState(false);
  const [said, setSaid] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  if (keys.length === 0) return null;
  const last = (k: ConnectedKey) => keys.filter((o) => o.connection === k.connection).length === 1;
  async function remove() {
    if (!removing) return;
    setBusy(true);
    setError(null);
    try {
      await rest.request(
        "DELETE",
        `/setup/integrations/app_store_connect/keys/${pathSegment(removing.connection)}/${pathSegment(removing.slot)}`,
      );
      setSaid(`${removing.label} removed. Revoke it in App Store Connect too.`);
      setRemoving(null);
    } catch (err) {
      setError(err instanceof RestError ? err.message : "Nothing was removed; try again.");
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      {/* ROWS OF THEIR OWN, not a List: a list row's inset would set the names off the column. */}
      <div className="col">
        {said && <Callout variant="success">{said}</Callout>}
        {keys.map((k) => (
          <span key={`${k.connection}:${k.slot}`} className="row gap-3">
            <span>
              {k.label}
              {keys.some((o) => o.connection !== k.connection) ? ` (${k.connection})` : ""}
            </span>
            <Button
              size="small"
              variant="secondary"
              disabledReason={
                !access.can
                  ? access.reason
                  : last(k)
                    ? "A connection keeps at least one key: add another first."
                    : undefined
              }
              onClick={() => setRemoving(k)}
            >
              Remove
            </Button>
          </span>
        ))}
      </div>
      {removing && (
        <ConfirmModal
          open
          destructive
          title={`Remove the ${removing.label}?`}
          confirmLabel="Remove"
          submitting={busy}
          onClose={() => setRemoving(null)}
          onConfirm={() => void remove()}
        >
          <Text as="p">
            Crewlet stops using it on its next call and deletes its sealed copy. Revoke it in App
            Store Connect too.
          </Text>
          {error && <Callout variant="danger">{error}</Callout>}
        </ConfirmModal>
      )}
    </>
  );
}
