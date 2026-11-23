/**
 * The org builder's confirmation dialogs with long names, for the layout suite.
 * Imported by tests only.
 */

import { fireEvent, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { pick } from "~/testing.tsx";
import type { CompanyDocument } from "~/protocol/index.ts";
import { DeleteDialog } from "./DeleteDialog.tsx";
import { MoveDialog } from "./MoveDialog.tsx";
import { NodeEditor } from "./NodeEditor.tsx";
import { ReviewSaveDialog } from "./ReviewSaveDialog.tsx";
import { COMPANY_KEY } from "./model/keys.ts";
import type { ChangeSet, EntityRef } from "./model/changes.ts";
import type { BuilderState } from "./model/reducer.ts";
import { draftWarnings } from "./model/warnings.ts";
import { fixtureCompany } from "./model/testkit.ts";
import { keyedState } from "./testState.ts";

export const LONG_SEAT =
  "Senior Site Reliability Engineer, Payments Platform and Regional Failover";
export const LONG_UNIT = "Platform Reliability and Regional Failover Engineering";
export const LONG_SECRET = "DEV_GITHUB_APP_PRIVATE_KEY_FOR_THE_PAYMENTS_PLATFORM_REPOSITORIES";

/** The fixture company with a long seat, unit and secret name, and vendor identities. */
export function longCompany(): CompanyDocument {
  const doc = fixtureCompany();
  doc.integrations = {
    ...doc.integrations,
    mattermost: { enabled: true, url: "https://chat.example.com", team: "acme" },
  };
  const engineering = doc.units![0]!;
  engineering.roles![1] = {
    name: "Dev",
    handle: "dev",
    goal: "Build",
    integrations: {
      github: { app_slug: "acme-dev", private_key: `\${${LONG_SECRET}}` },
      mattermost: { bot_token: "${DEV_MM_TOKEN}", username: "dev-bot" },
    },
  };
  engineering.children![0]!.name = LONG_UNIT;
  engineering.children![0]!.roles![0]!.name = LONG_SEAT;
  return doc;
}

const ref = (key: string, kind: EntityRef["kind"], name: string): EntityRef =>
  ({ key, kind, name }) as EntityRef;

/** A save that renames the company, moves and removes seats and changes what they derive. */
export function reviewChanges(): ChangeSet {
  const sre = ref("seat:sre", "seat", LONG_SEAT);
  const dev = ref("seat:dev", "seat", "Dev");
  const vp = ref("seat:vp-engineering", "seat", "VP Engineering");
  const platform = ref("unit:platform", "unit", LONG_UNIT);
  const sales = ref("unit:sales", "unit", "Sales");
  return {
    derivedKnown: true,
    added: [],
    removed: [ref("seat:account-executive", "seat", "Account Executive")],
    renamed: [],
    moved: [{ ref: sre, from: platform, to: sales }],
    edited: [{ ref: dev, fields: ["goal"] }],
    companyRename: { before: "Acme", after: "Acme Labs" },
    onboarding: [{ cause: "company_rename", seats: [vp, dev, sre] }],
    unitRenames: [],
    reportsTo: [{ ref: sre, before: vp, after: null, orderOnly: false }],
    leads: [],
    channels: [],
    routing: [],
    credentialServers: [{ ref: sre, gained: [], lost: ["tracker"] }],
    uncarriedCredentials: [],
    clearedReferences: [],
    datadogFallback: { before: "sre", after: "dev" },
    gitlabAccessLevels: [{ handle: "sre", before: "maintainer", after: null }],
    massRemoval: null,
    acknowledgements: ["company_rename", "credential_servers"],
    summary: "Rename the company and move the SRE to Sales",
  } as ChangeSet;
}

export interface Scene {
  name: string;
  state: BuilderState;
  ui: ReactNode;
  /** What a reader does after it opens, so the consequences are on screen. */
  prepare?: () => void;
}

export function scenes(): Scene[] {
  const state = keyedState(longCompany(), {
    seats: { "units[0].roles[1]": { manager: "vp-engineering" } },
  });
  const mass = keyedState({
    name: "X",
    units: [
      {
        name: "Ops",
        id: "ops",
        roles: [
          { name: "Runner", handle: "runner" },
          { name: "Second", handle: "second" },
        ],
      },
      { name: "Other", id: "other", roles: [{ name: "Keeper", handle: "keeper" }] },
    ],
  });
  const noop = () => {};
  return [
    { name: "delete", state, ui: <DeleteDialog nodeKey="unit:engineering" onClose={noop} /> },
    { name: "delete-mass", state: mass, ui: <DeleteDialog nodeKey="unit:ops" onClose={noop} /> },
    {
      name: "move",
      state,
      ui: <MoveDialog nodeKey="seat:vp-engineering" onClose={noop} />,
      prepare: () => pick(screen.getByLabelText("Move to"), "Sales"),
    },
    {
      name: "rename",
      state,
      ui: <NodeEditor nodeKey={COMPANY_KEY} onClose={noop} />,
      prepare: () =>
        fireEvent.change(screen.getByLabelText(/^Company name/), {
          target: { value: "Acme Labs" },
        }),
    },
    {
      name: "review",
      state,
      ui: (
        <ReviewSaveDialog
          mode="edit"
          changes={reviewChanges()}
          rules={{ review: true, save: true, waiting: false, reason: null }}
          status="clean"
          warnings={draftWarnings(state)}
          problemCount={0}
          documentProblems={[]}
          phase={{ kind: "idle" }}
          onSave={noop}
          onCheckAgain={noop}
          onClose={noop}
        />
      ),
    },
  ];
}
