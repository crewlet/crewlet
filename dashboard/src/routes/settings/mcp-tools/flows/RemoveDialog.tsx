/**
 * Remove a server: what loses it, what still names it, and the stored values
 * that may go with it, in one write.
 *
 * THE ENGINE LISTS EVERY REFERENCE. A dry run of the removal answers what
 * names the server from elsewhere (an agent's or a team's `mcp_env` and
 * `mcp_limits`, a box's `sandbox.mcp.servers`) and the stored values nothing
 * else reads once it is
 * gone; the removal clears those fields in the same revision, or is refused
 * naming them. A value may be deleted with it only where nothing else reads
 * it, which the engine decides again as it deletes. Disabling keeps
 * everything and is offered first.
 *
 * THE REMOVAL STATES THE REVISION ITS LIST WAS READ FROM (`If-Match`, the
 * entry's own tag), so a colleague's change in between is a conflict to read
 * again rather than a removal of something the dialog never showed.
 */

import { useEffect, useRef, useState } from "react";
import {
  Button,
  Callout,
  Checkbox,
  FormField,
  Modal,
  Spinner,
  Text,
  useToast,
} from "@crewlethq/ui";
import type { McpDependants, McpMemberServerStatus, McpSecretDeletion } from "~/contract/mcp.ts";
import { listed, plural } from "~/lib/format.ts";
import { useLifetimeSignal } from "~/lib/useLifetimeSignal.ts";
import { getEntity } from "~/protocol/configWrite.ts";
import { removeServer } from "../api.ts";
import type { McpData } from "../data.ts";
import { writeRefusalWords } from "../words.ts";

type Load =
  | { kind: "loading" }
  | { kind: "failed"; message: string }
  | { kind: "ready"; etag: string; dependants: McpDependants };

/** How many fields name the server from elsewhere, which the removal clears or is refused over. */
function namedElsewhere(d: McpDependants): number {
  return d.mcp_env.length + d.sandbox.length + d.mcp_limits.length;
}

/** What the toast says became of the values the removal was asked to delete. */
function secretsWords(secrets: readonly McpSecretDeletion[]): string | undefined {
  const deleted = secrets.filter((s) => s.outcome === "deleted" || s.outcome === "absent");
  const kept = secrets.filter((s) => s.outcome === "kept");
  const failed = secrets.filter((s) => s.outcome === "failed");
  const lines = [
    ...(deleted.length ? [`Deleted ${listed(deleted.map((s) => s.name))}.`] : []),
    ...(kept.length
      ? [
          `Kept ${listed(kept.map((s) => s.name))}: a change made meanwhile reads ${kept.length === 1 ? "it" : "them"}.`,
        ]
      : []),
    ...(failed.length
      ? [
          `${listed(failed.map((s) => s.name))} may not have been deleted: delete ${failed.length === 1 ? "it" : "them"} from Secrets.`,
        ]
      : []),
  ];
  return lines.length ? lines.join(" ") : undefined;
}

export function RemoveDialog({
  server,
  data,
  onClose,
  onDone,
  onDisable,
}: {
  server: McpMemberServerStatus;
  data: McpData;
  onClose: () => void;
  onDone: () => void;
  onDisable: () => void;
}) {
  const toast = useToast();
  const lifetime = useLifetimeSignal();
  const [load, setLoad] = useState<Load>({ kind: "loading" });
  const [clear, setClear] = useState(false);
  const [secrets, setSecrets] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    // THE DRY RUN SAYS WHAT THE WRITE WOULD MEET, before anybody presses Remove.
    const controller = new AbortController();
    void (async () => {
      const read = await getEntity("mcp-servers", server.name, controller.signal);
      if (read.kind !== "entity") {
        setLoad({ kind: "failed", message: writeRefusalWords(read, server.name, "check") });
        return;
      }
      const check = await removeServer(
        server.name,
        { clear: true, deleteSecrets: [] },
        { etag: read.etag, summary: "", dryRun: true },
        controller.signal,
      );
      if (!check.check) {
        setLoad({
          kind: "failed",
          message: writeRefusalWords(check.outcome, server.name, "check"),
        });
        return;
      }
      const dependants = check.check.dependants;
      setClear(namedElsewhere(dependants) === 0);
      setSecrets(dependants.secrets);
      setLoad({ kind: "ready", etag: read.etag, dependants });
    })().catch(() => undefined);
    return () => controller.abort();
  }, [server.name]);

  const remove = async () => {
    if (load.kind !== "ready") return;
    setBusy(true);
    const alive = lifetime();
    const answer = await removeServer(
      server.name,
      { clear, deleteSecrets: secrets },
      { etag: load.etag, summary: `Removed the MCP server ${server.name}`, dryRun: false },
    );
    // THE ANSWER IS SAID WHEREVER THE READER IS, but only a dialog still
    // open moves the page on.
    if (alive.aborted) return;
    if (answer.written) {
      toast.show({
        variant: "success",
        title: `${server.title} removed`,
        message: secretsWords(answer.written.secrets),
      });
      onDone();
      return;
    }
    toast.show({
      variant: "danger",
      title: "Nothing was removed",
      message: writeRefusalWords(answer.outcome, server.name, "write"),
    });
    setBusy(false);
  };

  const losing = (data.reaches(server.name) ?? []).map((s) => s.name);
  const deps = load.kind === "ready" ? load.dependants : null;
  const elsewhere = deps ? namedElsewhere(deps) : 0;
  // FOCUS STARTS ON CANCEL, as in every prompt of the family: Remove is
  // disabled until the dry run answers and is destructive after, and the
  // first control in the footer is Disable instead, a different action.
  const cancel = useRef<HTMLButtonElement>(null);
  return (
    <Modal
      open
      onClose={onClose}
      title={`Remove ${server.title}?`}
      size="md"
      initialFocus={() => cancel.current}
      footerStart={
        server.enabled ? (
          <Button variant="ghost" onClick={onDisable}>
            Disable instead
          </Button>
        ) : undefined
      }
      footer={
        <>
          <Button ref={cancel} variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="danger"
            loading={busy}
            disabled={load.kind !== "ready" || (elsewhere > 0 && !clear)}
            onClick={() => void remove()}
          >
            Remove server
          </Button>
        </>
      }
    >
      {load.kind === "loading" ? (
        <div className="mcp-testing">
          <Spinner size="sm" aria-label="Checking" />
          <Text as="span" tone="secondary">
            Finding everything that names {server.name}.
          </Text>
        </div>
      ) : load.kind === "failed" ? (
        <Callout variant="danger" title="The removal could not be checked">
          {load.message}
        </Callout>
      ) : (
        <div className="mcp-stack">
          <Text as="p">
            {losing.length === 0
              ? "No agent reaches it, so no agent loses a tool."
              : server.tools > 0
                ? `${listed(losing)} ${losing.length === 1 ? "loses" : "lose"} its ${plural(server.tools, "tool")} on ${losing.length === 1 ? "its" : "their"} next turn. A running turn finishes first.`
                : `${listed(losing)} ${losing.length === 1 ? "is" : "are"} no longer granted it from ${losing.length === 1 ? "its" : "their"} next turn.`}{" "}
            Its history goes with it
            {deps?.oauth ? ", and its sign-in is revoked at the vendor" : ""}. Disabling keeps
            everything instead.
          </Text>
          {deps && elsewhere > 0 ? (
            <Callout variant="warning" title={`Still named in ${plural(elsewhere, "place")}`}>
              <div className="mcp-field-group">
                <ul className="mcp-list">
                  {deps.mcp_env.map((d) => (
                    <li key={`env-${d.kind}-${d.key}`}>
                      {d.kind === "seat" ? "Agent" : "Team"} {d.name}: its own{" "}
                      <span className="mono">mcp_env.{server.name}</span> values
                    </li>
                  ))}
                  {deps.sandbox.map((d) => (
                    <li key={`box-${d.seat}`}>
                      {d.name}'s coding box: <span className="mono">sandbox.mcp.servers</span>
                    </li>
                  ))}
                  {deps.mcp_limits.map((d) => (
                    <li key={`limits-${d.kind}-${d.key}`}>
                      {d.kind === "seat" ? "Agent" : "Team"} {d.name}: its{" "}
                      <span className="mono">mcp_limits.{server.name}</span> limits
                    </li>
                  ))}
                </ul>
                <Checkbox
                  label="Clear these in the same change"
                  checked={clear}
                  onCheckedChange={setClear}
                />
              </div>
            </Callout>
          ) : null}
          {deps && deps.secrets.length > 0 ? (
            // A GROUP OF CHOICES UNDER THE FORM'S OWN LABEL: a 14px legend
            // was a third heading register in one dialog.
            <FormField as="fieldset" label="Secrets only this server reads">
              <div className="mcp-field-group">
                {deps.secrets.map((v) => (
                  <Checkbox
                    key={v}
                    label={<span className="mono">{v}</span>}
                    description="Deleted from the secret store with the server"
                    checked={secrets.includes(v)}
                    onCheckedChange={(on) =>
                      setSecrets(on ? [...secrets, v] : secrets.filter((x) => x !== v))
                    }
                  />
                ))}
              </div>
            </FormField>
          ) : null}
        </div>
      )}
    </Modal>
  );
}
