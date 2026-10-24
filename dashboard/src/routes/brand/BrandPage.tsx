/**
 * Company › Brand (`#/company/brand`): how the company looks and sounds in
 * every doc, deck and page its agents make.
 *
 * # What the page shows
 *
 * The brand an administrator ADOPTED (`brand{}`): one version of the company's
 * brand kit, snapshotted by the engine, which every artifact and agent reads.
 * Four cards (Logos, Colours, Typography, Voice) in the main column and a
 * sticky preview beside them. Every reader reads it, downloads a logo and
 * copies a colour: the brand is the company's public face, and this page is
 * its guidelines for people too.
 *
 * # Who changes it, and how
 *
 * An administrator, a person, with a recent proof. Each card edits in place
 * (Edit, then Save or Cancel, one card at a time, the preview drawing every
 * change), and Save publishes a version of the company's kit and adopts it in
 * one gesture, after a dialog that lists what changes, which contrast pairs
 * fall short and what adopting makes public. A version somebody else drafted
 * (an agent, an editor of the kit) is offered for review and adoption from the
 * notice under the header. A reader who may not adopt is handed no control at
 * all.
 *
 * # Nothing is claimed before the brand arrives
 *
 * "No brand yet" is a statement about the company, so the page draws a
 * skeleton until the read answers, and only then the cards or the empty state.
 */

import { useMemo, useState } from "react";
import {
  AsideLayout,
  Button,
  ButtonLink,
  Callout,
  Card,
  ConfirmModal,
  EmptyState,
  HeadingLevelProvider,
  Menu,
  Skeleton,
  Stack,
  useToast,
} from "@crewlethq/ui";
import {
  EllipsisGlyph,
  ExternalLinkGlyph,
  PencilGlyph,
  PlusGlyph,
  RotateCcwClockGlyph,
  SwatchBookGlyph,
  WandSparklesGlyph,
  XGlyph,
} from "@crewlethq/icons/glyphs";
import { href, useLeaveGuard, useNavigator } from "~/app/router.tsx";
import { usePageMenu } from "~/app/Shell.tsx";
import { artifactPath } from "~/app/routes.ts";
import { ObjectHeader, type Fact } from "~/app/frame/ObjectHeader.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { fmtDate } from "~/lib/format.ts";
import { useKeyedGesture } from "~/lib/keyedWrite.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { actorName } from "~/lib/artifacts/words.ts";
import { clearBrand } from "~/lib/brand/api.ts";
import { mayAdoptBrand, serializeBrandDocument, type Adoption } from "~/lib/brand/brand.ts";
import { draftSource, saveFiles, type BrandEdit } from "~/lib/brand/draft.ts";
import {
  adoptionSource,
  logoDigests,
  useBrand,
  useBrandAssets,
  type BrandAssets,
} from "~/lib/brand/useBrand.ts";
import { useBrandSave } from "~/lib/brand/useBrandSave.ts";
import {
  BRAND_CARDS,
  BrandCard,
  ColoursBody,
  LogosBody,
  TypographyBody,
  VoiceBody,
  type BrandCardEdit,
  type BrandCardKey,
} from "./BrandCards.tsx";
import {
  ColoursEditor,
  LogosEditor,
  TypographyEditor,
  VoiceEditor,
  cardProblem,
  type BrandEditorProps,
} from "./BrandEditors.tsx";
import { BrandPreview } from "./BrandPreview.tsx";
import { AdoptDialog } from "./AdoptDialog.tsx";
import { ReviewAdopt } from "./ReviewAdopt.tsx";
import { SetupDialog } from "./SetupDialog.tsx";

const DESCRIPTION = "How the company looks and sounds in every doc, deck and page its agents make.";

/** Each card's editor. */
const EDITORS: Readonly<Record<BrandCardKey, (props: BrandEditorProps) => React.ReactNode>> = {
  logos: LogosEditor,
  colours: ColoursEditor,
  type: TypographyEditor,
  voice: VoiceEditor,
};

/** The card being changed, and the edit as it stands. */
interface Editing {
  card: BrandCardKey;
  edit: BrandEdit;
}

/** Whether an edit changes anything from the adopted brand. */
function changed(editing: Editing | null, adoption: Adoption | null | undefined): boolean {
  if (!editing || !adoption) return false;
  return (
    editing.edit.files.size > 0 ||
    serializeBrandDocument(editing.edit.document) !== serializeBrandDocument(adoption.document)
  );
}

export function BrandPage() {
  const viewer = useViewer();
  const brand = useBrand();
  const adoption = brand.adoption;
  const org = useOrg();
  const people = useArtifactPeople();
  // ADOPTING IS AN ADMINISTRATOR'S, AND A PERSON'S (`artifacts.brand.adopt`):
  // an agent never holds admin, and the page offers it no control.
  const canAdopt = mayAdoptBrand(viewer);

  const [editing, setEditing] = useState<Editing | null>(null);
  const [confirming, setConfirming] = useState<"save" | "clear" | "setup" | null>(null);
  const [reviewing, setReviewing] = useState<number | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [leaving, setLeaving] = useState<{ leave: () => void } | null>(null);

  const base = useMemo(() => (adoption ? adoptionSource(adoption) : null), [adoption]);
  const shown = useMemo(() => (editing ? draftSource(base, editing.edit) : base), [editing, base]);
  const assets = useBrandAssets(shown);

  const dirty = changed(editing, adoption);
  useLeaveGuard(
    dirty
      ? (_to, leave) => {
          setLeaving({ leave });
          return true;
        }
      : null,
  );

  const refreshed = () => {
    setEditing(null);
    setConfirming(null);
    setReviewing(null);
    brand.refetch();
  };

  const facts: Fact[] = adoption
    ? [
        {
          label: "Adopted",
          value: `${fmtDate(adoption.adopted_at)} by ${actorName(adoption.adopted_by, people.seatName)}`,
        },
        {
          label: "Version",
          value: String(adoption.kit_version),
          // A LINK ONLY WHERE IT OPENS: the kit an agent drafted may be
          // shared with nobody, and a reader who cannot see it would land
          // on Not Found. An administrator opens any artifact.
          ...(canAdopt
            ? {
                path: artifactPath(adoption.kit),
                query: { version: String(adoption.kit_version) },
              }
            : {}),
        },
      ]
    : [];

  return (
    <Stack gap={5}>
      <ObjectHeader
        kind="Company"
        icon="swatch-book"
        title="Brand"
        description={DESCRIPTION}
        facts={facts}
      />
      {canAdopt && adoption ? (
        <BarActions adoption={adoption} onClear={() => setConfirming("clear")} />
      ) : null}
      {notice ? (
        <Callout
          variant="warning"
          action={
            <Button
              size="small"
              variant="ghost"
              leadingIcon={<XGlyph size="sm" />}
              onClick={() => setNotice(null)}
            >
              Dismiss
            </Button>
          }
        >
          {notice}
        </Callout>
      ) : null}
      {adoption?.newer_version ? (
        <NewerNotice
          adoption={adoption}
          by={adoption.newer_by ? actorName(adoption.newer_by, people.seatName) : ""}
          canAdopt={canAdopt}
          onReview={() => setReviewing(adoption.newer_version ?? null)}
        />
      ) : null}
      {adoption === undefined ? (
        <div aria-busy="true">
          <Card>
            <Skeleton variant="text" rows={6} label="Loading the company's brand" />
          </Card>
        </div>
      ) : adoption === null ? (
        <NoBrand canAdopt={canAdopt} onSetUp={() => setConfirming("setup")} />
      ) : (
        <Cards
          adoption={adoption}
          editing={editing}
          assets={assets}
          canAdopt={canAdopt}
          onEdit={setEditing}
          onSave={() => setConfirming("save")}
        />
      )}

      {confirming === "setup" ? (
        <SetupDialog
          company={org?.name ?? ""}
          onClose={() => setConfirming(null)}
          onAdopted={(text) => {
            setNotice(text);
            refreshed();
          }}
        />
      ) : null}
      {confirming === "save" && editing && adoption ? (
        <SaveDialog
          adoption={adoption}
          editing={editing}
          onClose={() => setConfirming(null)}
          onSaved={(text) => {
            setNotice(text);
            refreshed();
          }}
        />
      ) : null}
      {confirming === "clear" && adoption ? (
        <ClearDialog
          adoption={adoption}
          onClose={() => setConfirming(null)}
          onCleared={refreshed}
        />
      ) : null}
      {reviewing !== null && adoption ? (
        <ReviewAdopt
          kit={adoption.kit}
          version={reviewing}
          adoption={adoption}
          onClose={() => setReviewing(null)}
          onAdopted={refreshed}
        />
      ) : null}
      <ConfirmModal
        open={leaving !== null}
        onClose={() => setLeaving(null)}
        onConfirm={() => {
          const go = leaving?.leave;
          setLeaving(null);
          setEditing(null);
          go?.();
        }}
        title="Discard your changes?"
        message={
          editing
            ? `Your changes to ${BRAND_CARDS[editing.card].title} are not saved.`
            : "Your changes are not saved."
        }
        confirmLabel="Discard changes"
        cancelLabel="Keep editing"
        destructive
      />
    </Stack>
  );
}

/**
 * The administrator's controls in the page bar: the kit's history, and a way to
 * stop using the brand. ON A PHONE THE MENU FOLDS INTO THE BAR'S ONE "MORE"
 * (`usePageMenu`): beside it, its own ellipsis was a second "···" a reader
 * could not tell apart. History stays the one action in view.
 */
function BarActions({ adoption, onClear }: { adoption: Adoption; onClear: () => void }) {
  const nav = useNavigator();
  const openKit = () => nav.to(artifactPath(adoption.kit));
  usePageMenu([
    {
      key: "kit",
      label: "Open the brand kit",
      icon: <ExternalLinkGlyph size="sm" />,
      onSelect: openKit,
    },
    {
      key: "clear",
      label: "Stop using this brand",
      icon: <XGlyph size="sm" />,
      onSelect: onClear,
    },
  ]);
  return (
    <PageActions>
      <ButtonLink
        href={href(artifactPath(adoption.kit), { panel: "history" })}
        size="small"
        variant="ghost"
        leadingIcon={<RotateCcwClockGlyph size="sm" />}
      >
        History
      </ButtonLink>
      <span className="page-action-folds">
        <Menu
          label="More brand actions"
          icon={<EllipsisGlyph size="sm" />}
          triggerVariant="ghost"
          align="end"
          items={[
            {
              key: "kit",
              label: "Open the brand kit",
              icon: <ExternalLinkGlyph size="sm" />,
              onSelect: openKit,
            },
            { kind: "separator", key: "sep" },
            {
              key: "clear",
              label: "Stop using this brand",
              icon: <XGlyph size="sm" />,
              danger: true,
              onSelect: onClear,
            },
          ]}
        />
      </span>
    </PageActions>
  );
}

/** A version of the kit newer than the adopted one: who published it, and a way to review it. */
function NewerNotice({
  adoption,
  by,
  canAdopt,
  onReview,
}: {
  adoption: Adoption;
  by: string;
  canAdopt: boolean;
  onReview: () => void;
}) {
  const who = by ? ` by ${by}` : "";
  return (
    <Callout
      variant="info"
      action={
        canAdopt ? (
          <Button size="small" variant="secondary" onClick={onReview}>
            Review and adopt
          </Button>
        ) : undefined
      }
    >
      {`Version ${adoption.newer_version}${who} is newer than the adopted version ${adoption.kit_version}.`}
      {canAdopt ? "" : " An administrator can review and adopt it."}
    </Callout>
  );
}

/** No brand is adopted: the way to set one up for an administrator, and what that means for everyone else. */
function NoBrand({ canAdopt, onSetUp }: { canAdopt: boolean; onSetUp: () => void }) {
  return (
    <>
      {canAdopt ? (
        <Callout variant="info" icon={<WandSparklesGlyph size="sm" />}>
          Add your logo, colours and type, and every doc, deck and page your agents make will look
          like yours.
        </Callout>
      ) : null}
      <Card>
        <EmptyState
          icon={<SwatchBookGlyph size="lg" />}
          title="No brand yet"
          description="Artifacts use Crewlet's look until an administrator adds the company's logo, colours and type."
          action={
            canAdopt ? (
              <Button variant="primary" leadingIcon={<PlusGlyph size="sm" />} onClick={onSetUp}>
                Set up your brand
              </Button>
            ) : undefined
          }
        />
      </Card>
    </>
  );
}

/** The four cards and the preview beside them, each card read or changed in place. */
function Cards({
  adoption,
  editing,
  assets,
  canAdopt,
  onEdit,
  onSave,
}: {
  adoption: Adoption;
  editing: Editing | null;
  assets: BrandAssets;
  canAdopt: boolean;
  onEdit: (next: Editing | null) => void;
  onSave: () => void;
}) {
  const document = editing?.edit.document ?? adoption.document;
  const cardEdit = (card: BrandCardKey): BrandCardEdit | undefined => {
    if (!canAdopt) return undefined;
    const title = BRAND_CARDS[card].title;
    if (editing?.card === card) {
      const Editor = EDITORS[card];
      const hold = cardProblem(card, editing.edit);
      return {
        editor: (
          <Editor edit={editing.edit} onEdit={(edit) => onEdit({ card, edit })} assets={assets} />
        ),
        footer: (
          <>
            <Button size="small" variant="ghost" onClick={() => onEdit(null)}>
              Cancel
            </Button>
            <Button size="small" variant="primary" disabledReason={hold} onClick={onSave}>
              Save
            </Button>
          </>
        ),
      };
    }
    return {
      actions: (
        <Button
          size="small"
          variant="ghost"
          leadingIcon={<PencilGlyph size="sm" />}
          aria-label={`Edit ${title}`}
          disabledReason={
            editing
              ? `Save or cancel your changes to ${BRAND_CARDS[editing.card].title} first.`
              : undefined
          }
          onClick={() => onEdit({ card, edit: { document: adoption.document, files: new Map() } })}
        >
          Edit
        </Button>
      ),
    };
  };
  return (
    <AsideLayout
      asideLabel="Preview"
      aside={<BrandPreview document={document} assets={assets} draft={editing !== null} />}
    >
      {/* ONE LEVEL UNDER THE PAGE'S TITLE: every card's title heads a part of
          the page the title names. */}
      <HeadingLevelProvider level={3}>
        <BrandCard card="logos" edit={cardEdit("logos")}>
          <LogosBody document={document} assets={assets} />
        </BrandCard>
        <BrandCard card="colours" edit={cardEdit("colours")}>
          <ColoursBody colors={document.colors} />
        </BrandCard>
        <BrandCard card="type" edit={cardEdit("type")}>
          <TypographyBody type={document.type} assets={assets} />
        </BrandCard>
        <BrandCard card="voice" edit={cardEdit("voice")}>
          <VoiceBody voice={document.voice} />
        </BrandCard>
      </HeadingLevelProvider>
    </AsideLayout>
  );
}

/** Saving one card's change: publish a version of the kit and adopt it, after the adopt dialog. */
function SaveDialog({
  adoption,
  editing,
  onClose,
  onSaved,
}: {
  adoption: Adoption;
  editing: Editing;
  onClose: () => void;
  /** Adopted (no words), or a version saved and not adopted, in its words. */
  onSaved: (notice: string | null) => void;
}) {
  const toast = useToast();
  const save = useBrandSave();
  const [failed, setFailed] = useState<string | null>(null);
  const base = useMemo(() => adoptionSource(adoption), [adoption]);
  const draft = useMemo(() => draftSource(base, editing.edit), [base, editing.edit]);
  const doc = editing.edit.document;

  const confirm = async () => {
    setFailed(null);
    let files;
    try {
      files = await saveFiles(base, editing.edit);
    } catch (err) {
      setFailed(err instanceof Error ? err.message : String(err));
      return;
    }
    const answer = await save.run({
      kit: adoption.kit,
      // THE KIT'S HEAD: the newest version, which the engine answers an
      // administrator beside the adopted one.
      head: adoption.newer_version ?? adoption.kit_version,
      title: `${doc.name.trim()} brand`,
      document: doc,
      files,
    });
    if (answer?.kind === "adopted") {
      toast.ok(`The ${doc.name.trim()} brand is adopted`);
      onSaved(null);
    } else if (answer?.kind === "published") {
      onSaved(answer.text);
    }
  };

  const answer = save.answer;
  return (
    <AdoptDialog
      open
      onClose={onClose}
      title={`Save and adopt this as the ${doc.name.trim()} brand?`}
      before={adoption.document}
      after={doc}
      digests={{ before: logoDigests(base), after: logoDigests(draft) }}
      confirmLabel="Save and adopt"
      busy={save.busy}
      answer={
        failed ??
        (answer && (answer.kind === "refused" || answer.kind === "unknown")
          ? answer.text
          : undefined)
      }
      onConfirm={() => void confirm()}
    />
  );
}

/** Stopping the adopted brand: artifacts return to Crewlet's look. The kit stays. */
function ClearDialog({
  adoption,
  onClose,
  onCleared,
}: {
  adoption: Adoption;
  onClose: () => void;
  onCleared: () => void;
}) {
  const toast = useToast();
  const gesture = useKeyedGesture();
  const name = adoption.document.name;
  return (
    <ConfirmModal
      open
      onClose={onClose}
      onConfirm={async () => {
        const answer = await gesture.run(clearBrand());
        if (answer?.kind === "done") {
          toast.ok(`The ${name} brand is no longer used`);
          onCleared();
        }
      }}
      title={`Stop using the ${name} brand?`}
      message={
        gesture.answer && gesture.answer.kind !== "done"
          ? gesture.answer.text
          : "Docs and dashboards return to Crewlet's look now, and pages and designs when they are next published. Existing decks keep their look, and the brand kit stays in Artifacts to adopt again."
      }
      confirmLabel="Stop using it"
      cancelLabel="Keep it"
      destructive
      submitting={gesture.busy}
    />
  );
}
