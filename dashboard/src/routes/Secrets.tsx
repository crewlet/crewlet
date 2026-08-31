/**
 * The company's credentials — names and provenance, never values.
 *
 * This surface existed on the engine and had ZERO references in the previous
 * dashboard: `/secrets` was reachable, guarded, and unreachable from any
 * screen. Reads are guarded here too, deliberately — the list of what a
 * company holds a credential for is itself worth guarding.
 *
 * There is no reveal button. The one route that returns a value needs an
 * explicit flag and logs the access, and putting that behind a click in a
 * dashboard that anyone with the token can open is not a trade worth making;
 * `crewlet secrets get` is the deliberate path.
 */

import { ScreenHead } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { Badge, Empty, Panel, Skeleton, Stat, StatRow } from "~/ui/primitives.tsx";
import { DataTable } from "~/ui/DataTable.tsx";
import { Icon } from "~/ui/Icon.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtDateTime, relTime, tsKey, plural } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import type { SecretRow } from "~/protocol/index.ts";

export function Secrets() {
  const now = useNow();
  // `config_entities` is the guarded family; secrets ride the same guard. The
  // socket carries the operator token on every query frame.
  const { data, loading, error } = useQuery("config_entities", { kind: "secrets" });

  const rows = ((data as unknown as { secrets?: SecretRow[] })?.secrets ?? []) as SecretRow[];
  const fromStore = rows.filter((r) => r.source === "store").length;

  return (
    <>
      <ScreenHead
        title="Secrets"
        sub="The company's sealed credentials. Names, key ids and provenance — this screen never asks for a value."
        badges={<Badge outline>{plural(rows.length, "credential")} held</Badge>}
      />

      <div className="banner neutral">
        <Icon name="shield" size="sm" />
        <span className="col" style={{ gap: 4 }}>
          <span>
            These live in the fleet's coordination store, sealed with the Tier A keyring, and every
            node reads them. A <code className="inline">${"{VAR}"}</code> in the company config
            resolves here first and falls back to the process environment.
          </span>
          <span className="t-caption">
            Values are never sent to this page. Reading one is{" "}
            <code className="inline">crewlet secrets get</code>, which logs the access.
          </span>
        </span>
      </div>

      <Panel padding="none">
        <StatRow cols={3}>
          <Stat icon="key" label="Credentials" value={rows.length} sub="names the fleet holds" />
          <Stat
            icon="database"
            label="In the secret store"
            value={fromStore}
            sub="the rest resolve from this process's environment"
          />
          <Stat
            icon="shield"
            label="Distinct key ids"
            value={new Set(rows.map((r) => r.key_id)).size}
            sub="a rekey moves every value onto a new one"
          />
        </StatRow>
      </Panel>

      {loading && <Skeleton rows={4} />}
      <QueryState
        error={error}
        loading={loading}
        empty={
          rows.length
            ? undefined
            : {
                title: "No secrets are stored",
                hint: "Set one with crewlet secrets set, or let a provisioning command hand one straight to the engine.",
              }
        }
      >
        <Panel padding="none">
          <DataTable<SecretRow>
            rows={rows}
            rowKey={(s) => s.name}
            defaultSort={{ key: "name", dir: "asc" }}
            columns={[
              {
                key: "name",
                header: "Name",
                sortValue: (s) => s.name,
                cell: (s) => <code className="inline">{s.name}</code>,
              },
              {
                key: "source",
                header: "Source",
                shrink: true,
                sortValue: (s) => s.source,
                cell: (s) => <Badge outline>{s.source}</Badge>,
              },
              {
                key: "key",
                header: "Key id",
                shrink: true,
                sortValue: (s) => s.key_id,
                cell: (s) => <code className="inline">{s.key_id}</code>,
              },
              {
                key: "by",
                header: "Set by",
                sortValue: (s) => s.updated_by,
                cell: (s) => s.updated_by || <span className="faint">—</span>,
              },
              {
                key: "at",
                header: "Updated",
                shrink: true,
                sortValue: (s) => tsKey(s.updated_at),
                cell: (s) => (
                  <span className="t-caption" title={fmtDateTime(s.updated_at)}>
                    {relTime(s.updated_at, now)}
                  </span>
                ),
              },
            ]}
          />
        </Panel>
      </QueryState>
    </>
  );
}
