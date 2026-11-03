/**
 * Every flow a server's action opens, by the address (`?flow=`): test, store
 * a value, restart, disable or enable, and remove. One place mounts them, for
 * the list and the server page alike, so an alarm's fix and a menu item open
 * the same dialog. An administrator's alone: a member is offered none. The
 * add and edit wizard is a page of its own, which `McpTools` draws in the
 * page's place.
 */

import { useState } from "react";
import { ConfirmModal, useToast } from "@crewlethq/ui";
import type { McpMemberServerStatus } from "~/contract/mcp.ts";
import { setEnabled, restartServer } from "../api.ts";
import { useServerDetail, type McpData } from "../data.ts";
import type { useFlow } from "../parts.tsx";
import { writeRefusalWords } from "../words.ts";
import { RemoveDialog } from "./RemoveDialog.tsx";
import { TestDialog } from "./TestDialog.tsx";
import { ValueDialog } from "./ValueDialog.tsx";

type FlowState = ReturnType<typeof useFlow>;

/** A write is sent without a signal: started, it runs to its answer whatever the screen does. */
const unbounded = () => new AbortController().signal;

export function Flows({
  data,
  name,
  flow,
  onRemoved,
}: {
  data: McpData;
  name: string;
  flow: FlowState;
  /** Where the page goes once the server it was on is gone. */
  onRemoved: () => void;
}) {
  const server = data.server(name);
  if (!server || !server.configured || !flow.flow) return null;
  const done = () => data.reload();
  switch (flow.flow) {
    case "test":
      return (
        <TestDialog
          server={server}
          seat={flow.seat}
          data={data}
          onClose={flow.close}
          onDone={done}
        />
      );
    case "value":
      return flow.key ? (
        <StoredValue server={server} valueKey={flow.key} onClose={flow.close} onDone={done} />
      ) : null;
    case "restart":
      return (
        <RestartDialog
          server={server}
          seat={flow.seat}
          data={data}
          onClose={flow.close}
          onDone={done}
        />
      );
    case "disable":
      return <DisableDialog server={server} onClose={flow.close} onDone={done} />;
    case "remove":
      return (
        <RemoveDialog
          server={server}
          data={data}
          onClose={flow.close}
          onDisable={() => flow.open("disable")}
          onDone={() => {
            onRemoved();
            done();
          }}
        />
      );
    default:
      return null;
  }
}

/**
 * Disable keeps every setting and secret, and stops offering the tools;
 * enabling brings everything back. One entity write, `enabled` alone moved.
 */
function DisableDialog({
  server,
  onClose,
  onDone,
}: {
  server: McpMemberServerStatus;
  onClose: () => void;
  onDone: () => void;
}) {
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  // WHAT THE DIALOG WAS OPENED TO DO, read once: the write it makes turns the
  // live state over, and a title read from that renamed the dialog as it
  // succeeded.
  const [enabling] = useState(() => !server.enabled);
  const go = async () => {
    setBusy(true);
    const outcome = await setEnabled(
      server.name,
      enabling,
      `${enabling ? "Enabled" : "Disabled"} the MCP server ${server.name}`,
      unbounded(),
    );
    if (outcome.kind === "saved") {
      toast.show({
        variant: "success",
        title: enabling ? `${server.title} enabled` : `${server.title} disabled`,
      });
      onDone();
      onClose();
      return;
    }
    toast.show({
      variant: "danger",
      title: "Nothing was changed",
      message: writeRefusalWords(outcome, server.name, "write"),
    });
    setBusy(false);
  };
  return (
    <ConfirmModal
      open
      onClose={onClose}
      onConfirm={go}
      submitting={busy}
      title={enabling ? `Enable ${server.title}?` : `Disable ${server.title}?`}
      message={
        enabling
          ? "It starts on the next apply, and the agents it reaches see its tools on their next turn."
          : `No agent is offered its ${server.tools === 1 ? "tool" : `${server.tools} tools`} from its next turn; a running turn finishes first. Its settings and secrets are kept, so enabling it brings everything back.`
      }
      confirmLabel={enabling ? "Enable" : "Disable"}
      cancelLabel="Cancel"
      destructive={!enabling}
    />
  );
}

/**
 * Restart every instance now, or one agent's (`seat`). The engine records a
 * stamp each node hears, so the answer is the request as recorded and each
 * instance comes back as its node acts on it.
 */
function RestartDialog({
  server,
  seat,
  data,
  onClose,
  onDone,
}: {
  server: McpMemberServerStatus;
  seat: string;
  data: McpData;
  onClose: () => void;
  onDone: () => void;
}) {
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const who = seat ? data.seatName(seat) : "";
  const go = async () => {
    setBusy(true);
    const answer = await restartServer(server.name, seat);
    if (answer.kind === "ok") {
      toast.show({
        variant: "success",
        title: who ? `Restarting ${server.title} for ${who}` : `Restarting ${server.title}`,
        message: "Each node restarts its instances as it hears the request.",
      });
      onDone();
      onClose();
      return;
    }
    toast.show({ variant: "danger", title: "Nothing was restarted", message: answer.sentence });
    setBusy(false);
  };
  return (
    <ConfirmModal
      open
      onClose={onClose}
      onConfirm={go}
      submitting={busy}
      title={who ? `Restart ${server.title} for ${who}?` : `Restart ${server.title}?`}
      message={`${
        who ? `${who}'s instance starts` : "Every instance starts"
      } a fresh session now, and one that is down is tried at once rather than at its next attempt. A call in flight when its session ends is refused, since it may have taken effect. Nothing in the configuration changes.`}
      confirmLabel="Restart"
      cancelLabel="Cancel"
    />
  );
}

/**
 * The value dialog over the server's own answer, which lists each key it is
 * sent and whether the store holds it: the same read the server's page makes,
 * so it is the one already in hand.
 */
function StoredValue({
  server,
  valueKey,
  onClose,
  onDone,
}: {
  server: McpMemberServerStatus;
  valueKey: string;
  onClose: () => void;
  onDone: () => void;
}) {
  const detail = useServerDetail(server.name);
  const values = detail.data?.reading === "admin" ? detail.data.values : [];
  return (
    <ValueDialog
      server={server}
      valueKey={valueKey}
      value={values.find((v) => v.key === valueKey) ?? null}
      onClose={onClose}
      onDone={() => {
        onDone();
        void detail.reload({ quiet: true });
      }}
    />
  );
}
