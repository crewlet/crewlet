/**
 * An agent's Access tab: its access to every integration the company has
 * connected, one section each, drawn and edited with the integration's own
 * Agents tab component restricted to this agent (D13). A save here is the
 * same save, the same review and the same sync as on the integration's page.
 *
 * ONE DRAFT FOR ASK CREWLET, over every section: the page context carries one
 * draft, so each section puts its own into the page's (`AccessDraftParts`)
 * rather than declaring it, and the page declares them together. A change
 * Ask Crewlet proposes lands in each section it touches, saved by that
 * section's own Review.
 */

import { useState } from "react";
import { Section } from "@crewlethq/ui";
import { VendorMark } from "@crewlethq/icons";
import { QueryState } from "~/components/common.tsx";
import { href } from "~/app/router.tsx";
import { useAccess, useOperatorManaged } from "~/routes/settings/integrations/access/api.ts";
import { AgentsAccess } from "~/routes/settings/integrations/access/AgentsAccess.tsx";
import {
  AccessDraftParts,
  AccessDraftPartsContext,
  useGatheredAccessDraft,
} from "~/routes/settings/integrations/access/assistantDraft.ts";
import { accessHold } from "~/routes/settings/integrations/access/logic.ts";
import { seatStepFor } from "~/routes/settings/integrations/catalogue.tsx";
import { useIntegrations, type Model, type Tool } from "~/routes/settings/integrations/model.ts";

function IntegrationAccess({ tool, model, handle }: { tool: Tool; model: Model; handle: string }) {
  const access = useAccess(tool.entry.key);
  const readOnly = useOperatorManaged(access.data);
  const held = accessHold(model.managed, model.admin, readOnly);
  return (
    <Section
      title={
        <span className="agent-access-heading">
          <VendorMark vendor={tool.entry.vendor} size={20} />
          <a className="prose-link" href={href(["company", "integrations", tool.entry.key])}>
            {tool.entry.name}
          </a>
        </span>
      }
    >
      <QueryState
        error={access.code}
        refusal={access.refusal}
        loading={access.loading && !access.data}
      >
        {access.data && (
          <AgentsAccess
            kind={tool.entry.key}
            name={tool.entry.name}
            data={access.data}
            reload={() => void access.reload({ quiet: true })}
            held={held}
            readOnly={readOnly}
            onAnswer={access.adopt}
            stepFor={seatStepFor(tool, model)}
            avatarOf={model.avatarOf}
            only={handle}
          />
        )}
      </QueryState>
    </Section>
  );
}

/** The page's one draft over every section open to change on it. */
function GatheredDraft({ parts, agent }: { parts: AccessDraftParts; agent: string }) {
  useGatheredAccessDraft(parts, agent);
  return null;
}

export function Access({ handle, name }: { handle: string; name: string }) {
  const model = useIntegrations();
  const [parts] = useState(() => new AccessDraftParts());
  const connected = model.tools.filter((t) => t.configured);
  if (connected.length === 0) {
    return (
      <p className="t-caption">
        No integration is connected yet. Connect one under Company › Integrations, then choose this
        agent&apos;s access there or here.
      </p>
    );
  }
  return (
    <AccessDraftPartsContext.Provider value={parts}>
      <GatheredDraft parts={parts} agent={name} />
      <div className="agent-access-page">
        {connected.map((tool) => (
          <IntegrationAccess key={tool.entry.key} tool={tool} model={model} handle={handle} />
        ))}
      </div>
    </AccessDraftPartsContext.Provider>
  );
}
