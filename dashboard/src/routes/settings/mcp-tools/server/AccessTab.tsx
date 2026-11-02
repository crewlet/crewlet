/**
 * A server's Access: which agents get it, and why each one does.
 *
 * GRANTS GO TO TEAMS FIRST, agents as overrides, because the organization is
 * the execution graph and seats come and go with revisions: a team's grant
 * reaches whoever joins it, and an agent can be added on its own or left out
 * of its team's grant. A server bound to a vendor's per-agent credential takes
 * its agents from that integration's Agents tab, which stays the one place
 * they are decided (the engine refuses `access` on it), so this tab reads it
 * and links there rather than copying it.
 *
 * THE ENGINE SAYS WHO IT REACHES. The table is the detail's `reaches` (each
 * seat with its reason and the unit or lane it came through) beside the
 * answer's count of what each agent may call there; the picker is only the
 * form that writes the entry's `access`, and the engine's dry run judges
 * every name it holds.
 *
 * ONE WAY TO CHANGE IT: the choices edit in place, and Save and Discard
 * appear once they differ from what is saved.
 */

import { useMemo, useState } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  Card,
  EmptyValue,
  StatusDot,
  TableTwoLineCell,
  Text,
  useToast,
  type DataGridColumn,
} from "@crewlethq/ui";
import { UsersGlyph } from "@crewlethq/icons/glyphs";
import type { McpMemberServerStatus, McpReach, McpServerAnswer } from "~/contract/mcp.ts";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { href } from "~/app/router.tsx";
import { indexOrg } from "~/lib/seats.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { saveAccess } from "../api.ts";
import { useServerEntry, type McpData } from "../data.ts";
import { accessOf, GrantPicker, sameAccess, type Access } from "../GrantPicker.tsx";
import { limitWords, reasonWords, STATE, writeRefusalWords } from "../words.ts";

interface ReachRow {
  seat: string;
  name: string;
  /** Why the server reaches the agent, or null where it does not. */
  reach: McpReach | null;
}

export function AccessTab({
  server,
  detail,
  data,
  onSaved,
}: {
  server: McpMemberServerStatus;
  detail: McpServerAnswer;
  data: McpData;
  /** Read the server again, after its grant was written. */
  onSaved: () => void;
}) {
  const org = useOrg();
  const units = useMemo(() => new Map(indexOrg(org).units.map((u) => [u.id, u.name])), [org]);
  const binding = detail.resolved?.binding ?? null;
  // THE GRANT AS WRITTEN, the form's starting point: `reaches` is who it
  // reaches, which a team's grant and a list of its agents both explain.
  const entry = useServerEntry(server.name, data.admin && server.configured && !binding);
  const access: Access | null = entry.data ? accessOf(entry.data.access) : null;
  const agents = data.answer?.reading === "admin" ? data.answer.agents : null;
  // EVERY AGENT, the ones the grant does not reach said so: who is left out
  // is what a person checks a grant for.
  const reaches = new Map(detail.reaches.map((r) => [r.seat, r]));
  const rows: ReachRow[] = data.agents.map((seat) => ({
    seat: seat.handle,
    name: seat.name,
    reach: reaches.get(seat.handle) ?? null,
  }));
  const columns: DataGridColumn<ReachRow>[] = [
    {
      key: "agent",
      header: "Agent",
      floor: "12rem",
      sortValue: (r) => r.name,
      cell: (r) => (
        <Text as="span" variant="inherit" truncate>
          {r.name}
        </Text>
      ),
    },
    {
      key: "why",
      header: "Reaches it",
      sortValue: (r) => r.reach?.reason ?? "",
      // A REASON IS WORDS, not a state: in a success chip beside a "Not
      // running" word it read as a second status. What a team lead's limit
      // holds it to for the agent is said under it, as the engine resolved it.
      cell: (r) =>
        r.reach ? (
          <TableTwoLineCell
            value={reasonWords(r.reach, units)}
            detail={limitWords(r.reach.limited, units) || undefined}
          />
        ) : (
          <Text as="span" variant="caption" tone="tertiary">
            No
          </Text>
        ),
    },
    {
      key: "tools",
      header: "Tools it may call",
      align: "right",
      shrink: true,
      sortValue: (r) => toolsOf(r.seat)?.tools ?? -1,
      cell: (r) => {
        if (!r.reach) {
          return (
            <Text as="span" variant="caption" tone="tertiary">
              None
            </Text>
          );
        }
        const s = toolsOf(r.seat);
        if (!s) return <EmptyValue label="Not counted yet" />;
        if (s.state !== "running" && s.tools === 0) {
          const state = STATE[s.state];
          return <StatusDot tone={state.tone}>{state.label}</StatusDot>;
        }
        return <span className="mcp-num">{s.tools}</span>;
      },
    },
  ];
  function toolsOf(seat: string) {
    return (
      agents?.find((a) => a.seat === seat)?.servers.find((x) => x.server === server.name) ?? null
    );
  }

  return (
    <div className="mcp-stack">
      {binding ? (
        <Callout
          variant="info"
          title={`The ${server.title} integration decides which agents get it`}
          action={
            <ButtonLink
              size="small"
              variant="secondary"
              href={href(["company", "integrations", binding.vendor], { open: "agents" })}
            >
              Open its Agents tab
            </ButtonLink>
          }
        >
          Each agent turned on at the integration, with the access it has there, gets its own
          instance of this server. Change who has it on the integration's Agents tab.
        </Callout>
      ) : data.admin && server.configured ? (
        access ? (
          <GrantForm
            key={JSON.stringify(access)}
            server={server}
            saved={access}
            onSaved={() => {
              entry.reload();
              onSaved();
            }}
          />
        ) : null
      ) : null}
      <Card padding="none">
        <Card.Header icon={<UsersGlyph size="sm" />} count={detail.reaches.length || undefined}>
          <Card.Title>What each agent can call</Card.Title>
        </Card.Header>
        <DataGrid<ReachRow>
          name="reach"
          rows={rows}
          rowKey={(r) => r.seat}
          columns={columns}
          empty={{ title: "The company has no agent yet", icon: "users" }}
        />
      </Card>
    </div>
  );
}

/** The grant, edited in place, saved through the entity route. */
function GrantForm({
  server,
  saved,
  onSaved,
}: {
  server: McpMemberServerStatus;
  saved: Access;
  onSaved: () => void;
}) {
  const toast = useToast();
  const [draft, setDraft] = useState<Access>(saved);
  const [saving, setSaving] = useState(false);
  const [refused, setRefused] = useState("");
  const changed = !sameAccess(draft, saved);
  const save = async () => {
    setSaving(true);
    setRefused("");
    try {
      const outcome = await saveAccess(server.name, draft, `Changed who reaches ${server.name}`);
      const said = writeRefusalWords(outcome, server.name, "write");
      if (said) {
        setRefused(said);
        return;
      }
      toast.show({
        variant: "success",
        title: "Access saved",
        message: "Each agent sees the change on its next turn.",
      });
      onSaved();
    } finally {
      setSaving(false);
    }
  };
  return (
    <Card padding="none">
      <Card.Header icon={<UsersGlyph size="sm" />}>
        <Card.Title>Who gets it</Card.Title>
      </Card.Header>
      <Card.Body>
        <div className="mcp-stack">
          <GrantPicker
            label={`Who gets ${server.title}`}
            value={draft}
            onChange={(next) => {
              setDraft(next);
              setRefused("");
            }}
          />
          {refused ? (
            <Callout variant="danger" title="Nothing was saved">
              {refused}
            </Callout>
          ) : null}
          {changed ? (
            <div className="mcp-actions">
              <Button size="small" variant="primary" loading={saving} onClick={() => void save()}>
                Save access
              </Button>
              <Button
                size="small"
                variant="ghost"
                disabled={saving}
                onClick={() => {
                  setDraft(saved);
                  setRefused("");
                }}
              >
                Discard
              </Button>
            </div>
          ) : null}
        </div>
      </Card.Body>
    </Card>
  );
}
