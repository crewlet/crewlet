/**
 * A brand kit's view: one version of the kit drawn as the company's Brand page
 * draws the adopted brand, the same four cards and the same preview, read from
 * the version's own `brand.json` and files.
 *
 * WHETHER IT IS THE COMPANY'S BRAND is said over the cards: this version is
 * the adopted one, or it is not, and an administrator may review and adopt it
 * here. The kit itself is changed on Company › Brand, or by an agent or an
 * editor publishing a version; this view draws and never writes.
 */

import { useMemo, useState } from "react";
import {
  AsideLayout,
  Button,
  ButtonLink,
  Callout,
  HeadingLevelProvider,
  Skeleton,
} from "@crewlethq/ui";
import type { ArtifactDetail } from "~/protocol/index.ts";
import { href } from "~/app/router.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useRest } from "~/lib/useRest.ts";
import { versionText } from "~/lib/artifacts/api.ts";
import { BRAND_ENTRY } from "~/lib/brand/api.ts";
import { mayAdoptBrand, parseBrandDocument } from "~/lib/brand/brand.ts";
import { useBrand, useBrandAssets, versionSource } from "~/lib/brand/useBrand.ts";
import {
  BrandCard,
  ColoursBody,
  LogosBody,
  TypographyBody,
  VoiceBody,
} from "~/routes/brand/BrandCards.tsx";
import { BrandPreview } from "~/routes/brand/BrandPreview.tsx";
import { ReviewAdopt } from "~/routes/brand/ReviewAdopt.tsx";

/** Where the company's adopted brand is shown and changed. */
const BRAND_PAGE = ["company", "brand"];

export function BrandView({ detail, version }: { detail: ArtifactDetail; version: number }) {
  const viewer = useViewer();
  const brand = useBrand();
  const [reviewing, setReviewing] = useState(false);
  const read = useRest(version > 0 ? `brand-kit:${detail.id}:${version}` : null, async (signal) =>
    parseBrandDocument(await versionText(detail.id, version, BRAND_ENTRY, signal)),
  );
  const manifest = useQuery(
    "artifact_version",
    { id: detail.id, n: version },
    { enabled: version > 0 },
  );
  const doc = read.data;
  const source = useMemo(() => {
    if (!doc) return null;
    const digests = new Map(
      (manifest.data?.manifest.files ?? []).map((f) => [f.path, f.object.hash] as const),
    );
    return versionSource(detail.id, version, doc, digests);
  }, [doc, manifest.data, detail.id, version]);
  const assets = useBrandAssets(source);

  if (read.error || (read.data === null && !read.loading)) {
    return (
      <Callout variant="warning" title="This version cannot be drawn">
        Its brand file is not one this page can read. Open its files from History.
      </Callout>
    );
  }
  if (!doc) return <Skeleton variant="text" rows={6} label="Loading the brand kit" />;

  const adoption = brand.adoption;
  const adopted = adoption?.kit === detail.id && adoption.kit_version === version;
  const canAdopt = mayAdoptBrand(viewer);
  return (
    <div className="brand-view">
      {adopted ? (
        <Callout
          variant="success"
          action={
            <ButtonLink href={href(BRAND_PAGE)} size="small" variant="ghost">
              Open Company › Brand
            </ButtonLink>
          }
        >
          This version is the company's brand.
        </Callout>
      ) : canAdopt ? (
        <Callout
          variant="info"
          action={
            <Button size="small" variant="secondary" onClick={() => setReviewing(true)}>
              Review and adopt
            </Button>
          }
        >
          {`Version ${version} is not the company's brand. Adopting it makes it the look of every doc, deck and page agents make.`}
        </Callout>
      ) : (
        <Callout variant="neutral">
          An administrator adopts the company's brand on Company › Brand.
        </Callout>
      )}
      <AsideLayout
        asideLabel="Preview"
        aside={<BrandPreview document={doc} assets={assets} draft={false} />}
      >
        <HeadingLevelProvider level={3}>
          <BrandCard card="logos">
            <LogosBody document={doc} assets={assets} />
          </BrandCard>
          <BrandCard card="colours">
            <ColoursBody colors={doc.colors} />
          </BrandCard>
          <BrandCard card="type">
            <TypographyBody type={doc.type} assets={assets} />
          </BrandCard>
          <BrandCard card="voice">
            <VoiceBody voice={doc.voice} />
          </BrandCard>
        </HeadingLevelProvider>
      </AsideLayout>
      {reviewing ? (
        <ReviewAdopt
          kit={detail.id}
          version={version}
          adoption={adoption ?? null}
          onClose={() => setReviewing(false)}
          onAdopted={() => {
            setReviewing(false);
            brand.refetch();
          }}
        />
      ) : null}
    </div>
  );
}
