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

import { Fragment, useEffect, useState, type ReactNode } from "react";
import {
  Callout,
  ResourceName,
  Spinner,
  Text,
  useToast,
  type ConsequenceItem,
} from "@crewlethq/ui";
import { ConfirmModal } from "~/components/Modal.tsx";
import type { McpDependants, McpMemberServerStatus } from "~/contract/mcp.ts";
import { listed, plural } from "~/lib/format.ts";
import { removedSecretsWords, someFailed } from "~/lib/removedSecrets.ts";
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

/** Joins sentence parts as `listed` joins words. */
function joined(parts: readonly ReactNode[]): ReactNode {
  return parts.map((part, i) => (
    <Fragment key={i}>
      {i === 0 ? "" : i === parts.length - 1 ? (parts.length > 2 ? ", and " : " and ") : ", "}
      {part}
    </Fragment>
  ));
}

/** Where the server is named from elsewhere: whose field, and which. */
function settingsWords(d: McpDependants, server: string): ReactNode {
  const place = (names: string[], field: string, what: string) =>
    names.length === 1 ? (
      <>
        {names[0]}’s <ResourceName>{field}</ResourceName> {what}
      </>
    ) : (
      <>
        the <ResourceName>{field}</ResourceName> {what} of {listed(names)}
      </>
    );
  const parts: ReactNode[] = [];
  if (d.mcp_env.length > 0)
    parts.push(
      place(
        d.mcp_env.map((x) => x.name),
        `mcp_env.${server}`,
        "values",
      ),
    );
  if (d.mcp_limits.length > 0)
    parts.push(
      place(
        d.mcp_limits.map((x) => x.name),
        `mcp_limits.${server}`,
        "limits",
      ),
    );
  if (d.sandbox.length > 0)
    parts.push(
      place(
        d.sandbox.map((x) => x.name),
        "sandbox.mcp.servers",
        "entry",
      ),
    );
  return <>{joined(parts)}.</>;
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
        variant: someFailed(answer.written.secrets) ? "warning" : "success",
        title: `${server.title} removed`,
        message: removedSecretsWords(answer.written.secrets),
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
  const consequences: ConsequenceItem[] = deps
    ? [
        ...(elsewhere > 0
          ? [
              {
                kind: "choice" as const,
                key: "clear",
                label: `Clear its settings in ${plural(elsewhere, "place")}`,
                description: settingsWords(deps, server.name),
                required: true,
                checked: clear,
                onCheckedChange: setClear,
              },
            ]
          : []),
        ...deps.secrets.map((v) => ({
          kind: "choice" as const,
          key: `secret:${v}`,
          label: "Delete its secret",
          name: v,
          description: "Only this server reads it.",
          checked: secrets.includes(v),
          onCheckedChange: (on: boolean) =>
            setSecrets(on ? [...secrets, v] : secrets.filter((x) => x !== v)),
        })),
        ...(server.enabled
          ? [
              {
                kind: "fact" as const,
                key: "disable",
                tone: "info" as const,
                text: "Disabling it instead keeps everything.",
              },
            ]
          : []),
      ]
    : [];
  // FOCUS STARTS ON CANCEL: the body holds no control while the dry run runs,
  // and the footer's start is Disable instead, a different action.
  return (
    <ConfirmModal
      open
      destructive
      shape="framed"
      size="md"
      onClose={onClose}
      onConfirm={remove}
      title={`Remove ${server.title}?`}
      message={
        deps ? (
          <>
            {losing.length === 0 ? (
              "No agent reaches it, so no agent loses a tool."
            ) : server.tools > 0 ? (
              <>
                {listed(losing)} {losing.length === 1 ? "loses" : "lose"} its{" "}
                <strong>{plural(server.tools, "tool")}</strong> on{" "}
                {losing.length === 1 ? "its" : "their"} next turn. A running turn finishes first.
              </>
            ) : (
              `${listed(losing)} ${losing.length === 1 ? "is" : "are"} no longer granted it from ${losing.length === 1 ? "its" : "their"} next turn.`
            )}{" "}
            Its history goes with it
            {deps.oauth ? ", and its sign-in is revoked at the vendor" : ""}.
          </>
        ) : undefined
      }
      consequencesHeading="When you remove it"
      consequences={consequences}
      alternative={server.enabled ? { label: "Disable instead", onSelect: onDisable } : undefined}
      confirmLabel="Remove server"
      confirmDisabled={load.kind !== "ready"}
      confirmDisabledReason={
        load.kind === "loading"
          ? "Waiting for the check of what names it."
          : load.kind === "failed"
            ? "The removal could not be checked."
            : server.enabled
              ? "Clear its settings first, or disable it instead."
              : "Clear its settings first."
      }
      submitting={busy}
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
      ) : null}
    </ConfirmModal>
  );
}
