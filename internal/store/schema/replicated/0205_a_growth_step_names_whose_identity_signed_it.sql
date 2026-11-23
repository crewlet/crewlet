-- A growth step names whose identity signed it at the vendor (ADR-0179):
-- `signer` is `own` where the call went as the seat's own identity there (its
-- App Store Connect team key, its Meta system user), `company` where it went
-- as the company's, and '' where the product holds no identity of a seat's
-- own. Never the credential, which the writer refuses.
--
-- WHO HAS TO AGREE ON IT: every data node, identically, derived from the
-- CREWLET_ACCESS_LOG stream by its applier, as 0203's table is.
ALTER TABLE access_growth ADD COLUMN signer TEXT NOT NULL DEFAULT '';
