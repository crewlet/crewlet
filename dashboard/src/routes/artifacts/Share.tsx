/**
 * Who can open an artifact, and at what role: the design system's
 * `ShareDialog`, over the engine's effective access (`artifact_access`) and
 * its share routes.
 *
 * CHANGES ARE CONFIRMED, NOT OPTIMISTIC. A role change, a removal or the
 * company row shows it is saving until the engine answers, and a refusal is
 * said on the row it belongs to, in the engine's words. A share never grants
 * above the sharer's own role, so the dialog offers only the roles the reader
 * holds or below; owner moves only by transfer, in Settings.
 *
 * A READER WHO CANNOT SHARE STILL SEES WHO CAN OPEN IT, to the extent the
 * engine shows them: the owner and editors see the audience, a viewer sees the
 * owner and how many shares there are.
 */

import { useMemo, useState } from "react";
import {
  PrincipalPicker,
  ShareDialog,
  type CompanyAccess,
  type PrincipalOption,
  type ShareAudienceRow,
} from "@crewlethq/ui";
import {
  newActOpID,
  type ArtifactAudience,
  type ArtifactDetail,
  type ShareableRole,
} from "~/protocol/index.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { keyedWrite } from "~/lib/keyedWrite.ts";
import { shareArtifact, unshareArtifact } from "~/lib/artifacts/api.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import {
  audiencePrincipal,
  principalAudience,
  useArtifactPeople,
  type ArtifactPeople,
} from "~/lib/artifacts/people.ts";
import { GRANTABLE, ROLE_WORDS, audienceKey, roleIncludes } from "~/lib/artifacts/words.ts";
import { plural } from "~/lib/format.ts";
import { artifactAddress } from "./parts.tsx";

/** How many matches the picker lists: a screenful, never the company. */
const PICK_LIMIT = 12;

/** The org's seats and units matching `query`, as the picker groups them. */
export function principalOptions(
  people: ArtifactPeople,
  query: string,
  taken: ReadonlySet<string>,
): PrincipalOption[] {
  const needle = query.trim().toLowerCase();
  if (!needle) return [];
  const out: PrincipalOption[] = [];
  for (const seat of people.index.seats) {
    if (!seat.name.toLowerCase().includes(needle) && !seat.handle.toLowerCase().includes(needle))
      continue;
    const principal = people.seatPrincipal(seat.handle);
    const key = `seat:${seat.handle}`;
    out.push(
      taken.has(key)
        ? { ...principal, disabled: true, disabledReason: "Already has access" }
        : principal,
    );
  }
  for (const unit of people.index.units) {
    if (!unit.name.toLowerCase().includes(needle) && !unit.id.toLowerCase().includes(needle))
      continue;
    const principal = people.unitPrincipal(unit.id);
    const key = `unit:${unit.id}`;
    out.push(
      taken.has(key)
        ? { ...principal, disabled: true, disabledReason: "Already has access" }
        : principal,
    );
  }
  return out.slice(0, PICK_LIMIT);
}

/**
 * The share dialog, opened by the artifact bar's Share (or its key) and
 * mounted only while open, so the access it reads is read when it is asked
 * for and fresh each time.
 */
export function ArtifactShare({
  detail,
  open,
  onClose,
  onChanged,
}: {
  detail: ArtifactDetail;
  open: boolean;
  onClose: () => void;
  onChanged: () => void;
}) {
  return open ? <ShareSurface detail={detail} onClose={onClose} onChanged={onChanged} /> : null;
}

function ShareSurface({
  detail,
  onClose,
  onChanged,
}: {
  detail: ArtifactDetail;
  onClose: () => void;
  onChanged: () => void;
}) {
  const people = useArtifactPeople();
  const access = useQuery("artifact_access", { id: detail.id });
  const manage = useArtifactAccess({ role: "owner", held: detail.role });
  const [rows, setRows] = useState<Record<string, { pending?: boolean; refusal?: string }>>({});
  const [company, setCompany] = useState<{ pending?: boolean; refusal?: string }>({});
  const [query, setQuery] = useState("");

  const answer = access.data;
  const shares = useMemo(() => answer?.shares ?? [], [answer]);
  const taken = useMemo(() => new Set(shares.map((s) => audienceKey(s.audience))), [shares]);
  const companyShare = shares.find((s) => s.audience.kind === "company");
  const companyAccess: CompanyAccess =
    companyShare && companyShare.role !== "owner" ? (companyShare.role as ShareableRole) : "none";

  // A SHARE NEVER GRANTS ABOVE THE SHARER'S OWN ROLE (the domain refuses it),
  // so the dialog offers what the reader holds or below.
  const grantable = GRANTABLE.filter((r) => roleIncludes(detail.role, r));

  const send = async (
    key: string,
    req: ReturnType<typeof shareArtifact>,
    onRow: (s: { pending?: boolean; refusal?: string }) => void,
  ) => {
    onRow({ pending: true });
    const result = await keyedWrite({ ...req, key: newActOpID() });
    if (result.kind === "done") {
      onRow({});
      access.refetch();
      onChanged();
    } else {
      onRow({ refusal: result.text });
    }
    return key;
  };
  const setRow = (id: string) => (state: { pending?: boolean; refusal?: string }) =>
    setRows((was) => ({ ...was, [id]: state }));

  const audience: ShareAudienceRow[] = useMemo(() => {
    if (!answer) return [];
    const list: ShareAudienceRow[] = [
      {
        id: "owner",
        principal: people.seatPrincipal(answer.owner),
        role: "owner",
        editable: false,
      },
    ];
    for (const lead of answer.leads ?? []) {
      list.push({
        id: `lead:${lead}`,
        principal: people.seatPrincipal(lead),
        role: "owner",
        via: "Leads the agent that owns it",
        editable: false,
      });
    }
    for (const share of shares) {
      if (share.audience.kind === "company") continue;
      const id = audienceKey(share.audience);
      const state = rows[id] ?? {};
      list.push({
        id,
        principal: audiencePrincipal(share.audience, people),
        role: share.role,
        ...(state.pending ? { pending: true } : {}),
        ...(state.refusal ? { refusal: state.refusal } : {}),
      });
    }
    return list;
  }, [answer, shares, rows, people]);

  const audienceOf = (row: ShareAudienceRow): ArtifactAudience =>
    row.principal.kind === "team"
      ? { kind: "unit", id: row.principal.id }
      : { kind: "seat", id: row.principal.id };

  const readOnly = manage.can
    ? undefined
    : {
        reason:
          answer && !answer.shares && answer.share_count > 0
            ? `${manage.reason} It is shared ${plural(answer.share_count, "time")}.`
            : manage.reason,
      };

  return (
    <ShareDialog
      open
      onClose={onClose}
      title={detail.title}
      audience={audience}
      grantableRoles={grantable}
      companyName={people.company}
      companyAccess={companyAccess}
      {...(company.pending ? { companyPending: true } : {})}
      {...(company.refusal ? { companyRefusal: company.refusal } : {})}
      link={artifactAddress(detail.id)}
      {...(readOnly ? { readOnly } : {})}
      note={detail.elevated ? "You can manage this because you are an administrator." : undefined}
      labels={{ roles: ROLE_WORDS }}
      picker={
        manage.can
          ? ({ role }) => (
              <PrincipalPicker
                label="Add people, agents or teams"
                placeholder="Add people, agents or teams"
                query={query}
                onQueryChange={setQuery}
                options={principalOptions(people, query, taken)}
                onPick={(principal) => {
                  const who = principalAudience(principal);
                  void send(
                    audienceKey(who),
                    shareArtifact(detail.id, who, role),
                    setRow(audienceKey(who)),
                  );
                }}
              />
            )
          : undefined
      }
      onRoleChange={(row, role) => {
        const who = audienceOf(row);
        void send(row.id, shareArtifact(detail.id, who, role), setRow(row.id));
      }}
      onRemove={(row) => {
        const who = audienceOf(row);
        void send(row.id, unshareArtifact(detail.id, who), setRow(row.id));
      }}
      onCompanyAccessChange={(next) => {
        const req =
          next === "none"
            ? unshareArtifact(detail.id, { kind: "company" })
            : shareArtifact(detail.id, { kind: "company" }, next);
        void send("company", req, setCompany);
      }}
    />
  );
}
