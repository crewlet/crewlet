/**
 * An agent's Access tab: its access to every integration the company has
 * connected, one section each, drawn and edited with the integration's own
 * Agents tab component restricted to this agent (D13). A save here is the
 * same save, the same review and the same sync as on the integration's page.
 */

import { Section } from "@crewlethq/ui";
import { VendorMark } from "@crewlethq/icons";
import { QueryState } from "~/components/common.tsx";
import { href } from "~/app/router.tsx";
import { useAccess } from "~/routes/settings/integrations/access/api.ts";
import { AgentsAccess } from "~/routes/settings/integrations/access/AgentsAccess.tsx";
import { useIntegrations, type Model, type Tool } from "~/routes/settings/integrations/model.ts";

function IntegrationAccess({ tool, model, handle }: { tool: Tool; model: Model; handle: string }) {
  const access = useAccess(tool.entry.key);
  const held = model.managed ?? (model.admin ? undefined : "Admins change an agent's access.");
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
            avatarOf={model.avatarOf}
            only={handle}
          />
        )}
      </QueryState>
    </Section>
  );
}

export function Access({ handle }: { handle: string }) {
  const model = useIntegrations();
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
    <div className="agent-access-page">
      {connected.map((tool) => (
        <IntegrationAccess key={tool.entry.key} tool={tool} model={model} handle={handle} />
      ))}
    </div>
  );
}
