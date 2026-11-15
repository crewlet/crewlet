/**
 * The go-live check's review: what each agent may do and where it runs, as its
 * next turn is told it, for the owner to read before the agents start. Facts
 * only; whether they match the company's policies is the owner's reading.
 *
 * AGENTS TOLD THE SAME ARE ONE ENTRY. A company's agents mostly share a box
 * and hold nothing yet, and an entry each drew the same three lines six times
 * over ("A directory on the engine host, as the engine's user…"), so the one
 * agent told something different was the one a reader had to find. The
 * entries are grouped by the words drawn, in the answer's order, and an entry
 * is headed by every name it holds.
 */

import { Card, DescriptionList, Text, type DescriptionListEntry } from "@crewlethq/ui";
import { ShieldCheckGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { boxWords, integrationWords, toolServerWords } from "~/lib/agentAccess.ts";
import { listed } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { GoLiveReviewAnswer, ToldAccess } from "~/protocol/goLive.ts";

function lines(items: string[], none: string): string {
  return items.length > 0 ? items.join(" · ") : none;
}

/** One agent's access, as three lines. */
function entries(access: ToldAccess): DescriptionListEntry[] {
  return [
    ["Acts at", lines(access.integrations.filter((i) => i.on).map(integrationWords), "No vendor")],
    ["Tools", lines(access.mcp.map(toolServerWords), "No tool servers")],
    ["Runs in", lines(access.boxes.map(boxWords), "No sandbox")],
  ];
}

/** Agents told exactly the same three lines, in the answer's order, with those lines. */
export interface AlikeAgents {
  seats: string[];
  names: string[];
  entries: DescriptionListEntry[];
}

/**
 * The agents grouped by what they are told, each group where its first agent
 * stands. Two agents are alike when every line is the same words, so a group
 * never hides a difference a reader would see in its entries.
 */
export function alikeAgents(agents: GoLiveReviewAnswer["agents"]): AlikeAgents[] {
  const groups = new Map<string, AlikeAgents>();
  for (const agent of agents) {
    const lines = entries(agent.access);
    const said = JSON.stringify(lines);
    const group = groups.get(said);
    if (group) {
      group.seats.push(agent.seat);
      group.names.push(agent.name);
    } else {
      groups.set(said, { seats: [agent.seat], names: [agent.name], entries: lines });
    }
  }
  return [...groups.values()];
}

export function GoLiveReview() {
  const { data, error, refusal, detail, loading } = useQuery("go_live_review");
  return (
    <Card padding="none">
      <Card.Header icon={<ShieldCheckGlyph size="sm" />} count={data?.agents.length}>
        <Card.Title>Review what each agent can do</Card.Title>
      </Card.Header>
      <Card.Body padding="md" className="col gap-3">
        <Text as="p" tone="secondary">
          Each agent is told this at the start of every turn. Check it against what you want each
          one to do, for example who can merge or operate a server, and whether a sandbox has the
          browser your work needs.
        </Text>
        <QueryState error={error} refusal={refusal} detail={detail} loading={loading && !data}>
          {alikeAgents(data?.agents ?? []).map((group) => {
            const who = listed(group.names);
            return (
              <section
                key={group.seats.join(" ")}
                className="col gap-1"
                aria-label={`What ${who} can do`}
              >
                <Text as="h3" variant="label">
                  {who}
                </Text>
                <DescriptionList dense termWidth="5rem" items={group.entries} />
              </section>
            );
          })}
        </QueryState>
      </Card.Body>
    </Card>
  );
}
