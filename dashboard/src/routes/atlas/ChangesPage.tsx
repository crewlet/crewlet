/**
 * Company › Atlas › Changes: what the atlas recorded, company-wide, newest
 * first. The band's "Recorded this week" leads here: the same fourteen days
 * as bars, then the log's lines, each led by the entry it is about.
 *
 * It is the atlas's own log, so it holds what people, integrations and agents
 * recorded. What the engine reads from the settings and its own records at
 * every read is not recorded, and leaves no line.
 */

import { ActivityStrip, PageNote, Section, Skeleton, Text } from "@crewlethq/ui";
import { fmtDate } from "~/lib/format.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { Changes } from "./Changes.tsx";
import { ATLAS_POLL_MS } from "./filters.ts";
// THE ATLAS'S OWN SHEET, for a reader who opens this page first.
import "~/styles/atlas.css";

const DAY_MS = 86_400_000;
const NO_PARAMS = {};

export function ChangesPage() {
  const activity = useQuery("atlas_activity", NO_PARAMS, { pollMs: ATLAS_POLL_MS });
  const answer = activity.data;
  const from = answer ? Date.parse(answer.from) : 0;
  const total = answer?.days.reduce((sum, day) => sum + day, 0) ?? 0;
  return (
    <div className="atlas-screen">
      <PageNote>
        What people, integrations and agents recorded in the atlas, newest first. What the engine
        reads from the settings and its own records at every read is not recorded here.
      </PageNote>
      <Section
        spacing="tight"
        headingLevel={2}
        title={
          answer
            ? `${total.toLocaleString()} ${total === 1 ? "line" : "lines"} in the last ${answer.days.length} days`
            : "The last days"
        }
      >
        {answer ? (
          <div className="atlas-days">
            <ActivityStrip
              label={`Lines recorded in the atlas each day, the last ${answer.days.length} days`}
              buckets={answer.days.map((v, day) => ({ t: from + day * DAY_MS, v }))}
              reading={{ formatTime: (at) => fmtDate(new Date(at).toISOString()) }}
              emptyLabel="Nothing recorded in the window."
            />
          </div>
        ) : activity.loading ? (
          <Skeleton variant="text" rows={2} label="Counting the atlas log" />
        ) : (
          <Text variant="body" tone="secondary">
            {activity.detail ?? "The atlas log could not be counted."}
          </Text>
        )}
      </Section>
      <Section spacing="tight" headingLevel={2} title="Every change">
        <Changes />
      </Section>
    </div>
  );
}
