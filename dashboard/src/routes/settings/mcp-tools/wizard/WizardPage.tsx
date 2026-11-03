/**
 * The add or edit wizard as the page it stands in place of. An edit opens on
 * the server as it is saved, read whole before the wizard draws: its entry as
 * the configuration writes it (with its own revision's tag), what it is sent
 * (its answer's `values`), how the engine signs in to it (`resolved.auth`)
 * and the catalogue entry the engine recognises it as, which is what offers a
 * vendor's integration as a sign-in. Without the entry an edit of an
 * integration's server showed no method chosen.
 */

import { Callout, Skeleton } from "@crewlethq/ui";
import type { McpMemberServerStatus } from "~/contract/mcp.ts";
import { useRest } from "~/lib/useRest.ts";
import { getEntity } from "~/protocol/configWrite.ts";
import { readCatalogue } from "../api.ts";
import { useServerDetail, type McpData } from "../data.ts";
import { isStep } from "./draft.ts";
import { ServerWizard } from "./ServerWizard.tsx";

export function WizardPage({
  data,
  editing,
  step,
  onClose,
  onSaved,
}: {
  data: McpData;
  /** The server to edit; null adds one. */
  editing: McpMemberServerStatus | null;
  /** The step the address names, "" for the first. */
  step: string;
  onClose: () => void;
  onSaved: (name: string, signInNext: boolean) => void;
}) {
  const start = isStep(step) ? step : null;
  if (!editing) {
    return (
      <ServerWizard data={data} saved={null} start={start} onClose={onClose} onSaved={onSaved} />
    );
  }
  return (
    <EditWizard
      data={data}
      server={editing}
      start={start ?? "connection"}
      onClose={onClose}
      onSaved={onSaved}
    />
  );
}

function EditWizard({
  data,
  server,
  start,
  onClose,
  onSaved,
}: {
  data: McpData;
  server: McpMemberServerStatus;
  start: Parameters<typeof ServerWizard>[0]["start"];
  onClose: () => void;
  onSaved: (name: string, signInNext: boolean) => void;
}) {
  const detail = useServerDetail(server.name);
  const entity = useRest(`mcp-entity-edit:${server.name}`, (signal) =>
    getEntity("mcp-servers", server.name, signal),
  );
  const catalogue = useRest(server.catalogue ? "mcp-catalogue:" : null, (signal) =>
    readCatalogue("", signal),
  );
  const answer = detail.data;
  const read = entity.data;
  if (answer?.reading === "member") return null;
  if (read && read.kind !== "entity") {
    return (
      <Callout variant="danger" title={`${server.title} could not be opened for editing`}>
        {read.kind === "missing"
          ? "The configuration no longer has this server."
          : "The configuration could not be read. Close this and try again."}
      </Callout>
    );
  }
  if (!answer || !read || (server.catalogue && !catalogue.data && !catalogue.error)) {
    return <Skeleton variant="text" rows={8} label={`Loading ${server.title}`} />;
  }
  return (
    <ServerWizard
      data={data}
      saved={{
        server,
        entity: read.entity,
        values: answer.values,
        entry: catalogue.data?.curated.find((e) => e.id === server.catalogue) ?? null,
        signIn: answer.resolved?.auth ?? "none",
      }}
      start={start}
      onClose={onClose}
      onSaved={onSaved}
    />
  );
}
