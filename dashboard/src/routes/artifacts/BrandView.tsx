/**
 * A brand kit's view: one version of the kit drawn as the company's Brand page
 * draws the adopted brand, the same four cards and the same preview, read from
 * the version's own `brand.json` and files.
 *
 * WHETHER IT IS THE COMPANY'S BRAND is said over the cards: this version is
 * the published one (adopted, in the engine's word), or it is not, and an
 * administrator may review and publish it here. The kit itself is changed on
 * Company › Brand, or by an agent or an editor publishing a version; this
 * view draws and never writes. A draft that lacks a primary logo or colour is
 * drawn as far as it goes (the preview falls back to Crewlet's defaults),
 * says what it lacks, and is never offered for publishing.
 */

import { useState } from "react";
import { Button, ButtonLink, Callout, Skeleton } from "@crewlethq/ui";
import type { ArtifactDetail } from "~/protocol/index.ts";
import { href } from "~/app/router.tsx";
import { useViewer } from "~/lib/viewer.ts";
import { mayAdoptBrand } from "~/lib/brand/brand.ts";
import { useBrand, useBrandAssets } from "~/lib/brand/useBrand.ts";
import { useBrandVersion } from "~/lib/brand/useBrandVersion.ts";
import { draftReadiness } from "~/lib/brand/validate.ts";
import { BrandBoard } from "~/routes/brand/BrandBoard.tsx";
import { ReviewAdopt } from "~/routes/brand/ReviewAdopt.tsx";
import { BRAND_PAGE } from "~/components/brand.tsx";

export function BrandView({ detail, version }: { detail: ArtifactDetail; version: number }) {
  const viewer = useViewer();
  const brand = useBrand();
  const [reviewing, setReviewing] = useState(false);
  const read = useBrandVersion(detail.id, version);
  const doc = read.document;
  const assets = useBrandAssets(read.source);

  if (read.error || (doc === null && !read.loading)) {
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
  const readiness = draftReadiness(doc);
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
      ) : readiness ? (
        <Callout variant="warning">{readiness}</Callout>
      ) : canAdopt ? (
        <Callout
          variant="info"
          action={
            <Button size="small" variant="secondary" onClick={() => setReviewing(true)}>
              Publish this version
            </Button>
          }
        >
          {`Version ${version} is not the company's brand. Publishing it makes it the look of every doc, deck and page agents make.`}
        </Callout>
      ) : (
        <Callout variant="neutral">
          An administrator publishes the company's brand on Company › Brand.
        </Callout>
      )}
      <BrandBoard document={doc} assets={assets} drawn="brand" />
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
