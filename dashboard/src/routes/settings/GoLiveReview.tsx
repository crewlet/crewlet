/**
 * The go-live check's review: what each agent may do and where it runs, as its
 * next turn is told it, for the owner to read before the agents start. Facts
 * only; whether they match the company's policies is the owner's reading.
 */

import { Card, DescriptionList, Text, type DescriptionListEntry } from "@crewlethq/ui";
import { ShieldCheckGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { boxWords, integrationWords, toolServerWords } from "~/lib/agentAccess.ts";
import { useQuery } from "~/lib/useQuery.ts";
import type { ToldAccess } from "~/protocol/goLive.ts";

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
          {data?.agents.map((a) => (
            <section key={a.seat} className="col gap-1" aria-label={`What ${a.name} can do`}>
              <Text as="h3" variant="label">
                {a.name}
              </Text>
              <DescriptionList dense termWidth="5rem" items={entries(a.access)} />
            </section>
          ))}
        </QueryState>
      </Card.Body>
    </Card>
  );
}
