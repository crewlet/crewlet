# App Store Connect API operations

Every operation of App Store Connect API 4.6.1 as Crewlet offers it, generated from the integration's catalog: never edit this page.

| Class | Operations |
|---|---|
| [read](#read) | 851 |
| [operate](#operate) | 73 |
| [public](#public) | 282 |
| spend | 0 |
| [admin](#admin) | 136 |
| never | 0 |

## read

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| AccessibilityDeclarations | `accessibilityDeclarations_getInstance` | `GET /v1/accessibilityDeclarations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Actors | `actors_getCollection` | `GET /v1/actors` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Actors | `actors_getInstance` | `GET /v1/actors/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AlternativeDistributionDomains | `alternativeDistributionDomains_getCollection` | `GET /v1/alternativeDistributionDomains` |  | Account Holder, Admin, App Manager |  |
| AlternativeDistributionDomains | `alternativeDistributionDomains_getInstance` | `GET /v1/alternativeDistributionDomains/{id}` |  | Account Holder, Admin, App Manager |  |
| AlternativeDistributionKeys | `alternativeDistributionKeys_getCollection` | `GET /v1/alternativeDistributionKeys` |  | Account Holder, Admin, App Manager |  |
| AlternativeDistributionKeys | `alternativeDistributionKeys_getInstance` | `GET /v1/alternativeDistributionKeys/{id}` |  | Account Holder, Admin, App Manager |  |
| AlternativeDistributionPackageDeltas | `alternativeDistributionPackageDeltas_getInstance` | `GET /v1/alternativeDistributionPackageDeltas/{id}` |  | Account Holder, Admin, App Manager |  |
| AlternativeDistributionPackageVariants | `alternativeDistributionPackageVariants_getInstance` | `GET /v1/alternativeDistributionPackageVariants/{id}` |  | Account Holder, Admin, App Manager |  |
| AlternativeDistributionPackageVersions | `alternativeDistributionPackageVersions_getInstance` | `GET /v1/alternativeDistributionPackageVersions/{id}` |  | Account Holder, Admin, App Manager |  |
| AlternativeDistributionPackageVersions | `alternativeDistributionPackageVersions_deltas_getToManyRelated` | `GET /v1/alternativeDistributionPackageVersions/{id}/deltas` |  | Account Holder, Admin, App Manager |  |
| AlternativeDistributionPackageVersions | `alternativeDistributionPackageVersions_deltas_getToManyRelationship` | `GET /v1/alternativeDistributionPackageVersions/{id}/relationships/deltas` |  | Account Holder, Admin, App Manager |  |
| AlternativeDistributionPackageVersions | `alternativeDistributionPackageVersions_variants_getToManyRelationship` | `GET /v1/alternativeDistributionPackageVersions/{id}/relationships/variants` |  | Account Holder, Admin, App Manager |  |
| AlternativeDistributionPackageVersions | `alternativeDistributionPackageVersions_variants_getToManyRelated` | `GET /v1/alternativeDistributionPackageVersions/{id}/variants` |  | Account Holder, Admin, App Manager |  |
| AlternativeDistributionPackages | `alternativeDistributionPackages_getInstance` | `GET /v1/alternativeDistributionPackages/{id}` |  | Account Holder, Admin, App Manager |  |
| AlternativeDistributionPackages | `alternativeDistributionPackages_versions_getToManyRelationship` | `GET /v1/alternativeDistributionPackages/{id}/relationships/versions` |  | Account Holder, Admin, App Manager |  |
| AlternativeDistributionPackages | `alternativeDistributionPackages_versions_getToManyRelated` | `GET /v1/alternativeDistributionPackages/{id}/versions` |  | Account Holder, Admin, App Manager |  |
| AnalyticsReportInstances | `analyticsReportInstances_getInstance` | `GET /v1/analyticsReportInstances/{id}` |  | Account Holder, Admin, Finance, Sales, Access to Reports |  |
| AnalyticsReportInstances | `analyticsReportInstances_segments_getToManyRelationship` | `GET /v1/analyticsReportInstances/{id}/relationships/segments` |  | Account Holder, Admin, Finance, Sales, Access to Reports |  |
| AnalyticsReportInstances | `analyticsReportInstances_segments_getToManyRelated` | `GET /v1/analyticsReportInstances/{id}/segments` |  | Account Holder, Admin, Finance, Sales, Access to Reports |  |
| AnalyticsReportRequests | `analyticsReportRequests_getInstance` | `GET /v1/analyticsReportRequests/{id}` |  | Account Holder, Admin, Finance, Sales, Access to Reports |  |
| AnalyticsReportRequests | `analyticsReportRequests_reports_getToManyRelationship` | `GET /v1/analyticsReportRequests/{id}/relationships/reports` |  | Account Holder, Admin, Finance, Sales, Access to Reports |  |
| AnalyticsReportRequests | `analyticsReportRequests_reports_getToManyRelated` | `GET /v1/analyticsReportRequests/{id}/reports` |  | Account Holder, Admin, Finance, Sales, Access to Reports |  |
| AnalyticsReportSegments | `analyticsReportSegments_getInstance` | `GET /v1/analyticsReportSegments/{id}` |  | Account Holder, Admin, Finance, Sales, Access to Reports |  |
| AnalyticsReports | `analyticsReports_getInstance` | `GET /v1/analyticsReports/{id}` |  | Account Holder, Admin, Finance, Sales, Access to Reports |  |
| AnalyticsReports | `analyticsReports_instances_getToManyRelated` | `GET /v1/analyticsReports/{id}/instances` |  | Account Holder, Admin, Finance, Sales, Access to Reports |  |
| AnalyticsReports | `analyticsReports_instances_getToManyRelationship` | `GET /v1/analyticsReports/{id}/relationships/instances` |  | Account Holder, Admin, Finance, Sales, Access to Reports |  |
| AndroidToIosAppMappingDetails | `androidToIosAppMappingDetails_getInstance` | `GET /v1/androidToIosAppMappingDetails/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAssetLibraries | `appAssetLibraries_getInstance` | `GET /v1/appAssetLibraries/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAssetLibraries | `appAssetLibraries_images_getToManyRelated` | `GET /v1/appAssetLibraries/{id}/images` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAssetLibraries | `appAssetLibraries_images_getToManyRelationship` | `GET /v1/appAssetLibraries/{id}/relationships/images` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAssetLibraries | `appAssetLibraries_videos_getToManyRelationship` | `GET /v1/appAssetLibraries/{id}/relationships/videos` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAssetLibraries | `appAssetLibraries_videos_getToManyRelated` | `GET /v1/appAssetLibraries/{id}/videos` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAssetLibraryImages | `appAssetLibraryImages_getInstance` | `GET /v1/appAssetLibraryImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAssetLibraryImages | `appAssetLibraryImages_placements_getToManyRelated` | `GET /v1/appAssetLibraryImages/{id}/placements` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAssetLibraryImages | `appAssetLibraryImages_placements_getToManyRelationship` | `GET /v1/appAssetLibraryImages/{id}/relationships/placements` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAssetLibraryPlacements | `appAssetLibraryPlacements_getInstance` | `GET /v1/appAssetLibraryPlacements/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAssetLibraryRefData | `appAssetLibraryRefData_getCollection` | `GET /v1/appAssetLibraryRefData` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAssetLibraryRefData | `appAssetLibraryRefData_getInstance` | `GET /v1/appAssetLibraryRefData/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAssetLibraryVideos | `appAssetLibraryVideos_getInstance` | `GET /v1/appAssetLibraryVideos/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAssetLibraryVideos | `appAssetLibraryVideos_placements_getToManyRelated` | `GET /v1/appAssetLibraryVideos/{id}/placements` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAssetLibraryVideos | `appAssetLibraryVideos_placements_getToManyRelationship` | `GET /v1/appAssetLibraryVideos/{id}/relationships/placements` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppAvailabilities | `appAvailabilitiesV2_getInstance` | `GET /v2/appAvailabilities/{id}` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppAvailabilities | `appAvailabilitiesV2_territoryAvailabilities_getToManyRelationship` | `GET /v2/appAvailabilities/{id}/relationships/territoryAvailabilities` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppAvailabilities | `appAvailabilitiesV2_territoryAvailabilities_getToManyRelated` | `GET /v2/appAvailabilities/{id}/territoryAvailabilities` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppCategories | `appCategories_getCollection` | `GET /v1/appCategories` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support, Finance, Sales, Access to Reports |  |
| AppCategories | `appCategories_getInstance` | `GET /v1/appCategories/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support, Finance, Sales, Access to Reports |  |
| AppCategories | `appCategories_parent_getToOneRelated` | `GET /v1/appCategories/{id}/parent` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support, Finance, Sales, Access to Reports |  |
| AppCategories | `appCategories_parent_getToOneRelationship` | `GET /v1/appCategories/{id}/relationships/parent` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support, Finance, Sales, Access to Reports |  |
| AppCategories | `appCategories_subcategories_getToManyRelationship` | `GET /v1/appCategories/{id}/relationships/subcategories` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support, Finance, Sales, Access to Reports |  |
| AppCategories | `appCategories_subcategories_getToManyRelated` | `GET /v1/appCategories/{id}/subcategories` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support, Finance, Sales, Access to Reports |  |
| AppClipAdvancedExperienceImages | `appClipAdvancedExperienceImages_getInstance` | `GET /v1/appClipAdvancedExperienceImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClipAdvancedExperiences | `appClipAdvancedExperiences_getInstance` | `GET /v1/appClipAdvancedExperiences/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClipAppStoreReviewDetails | `appClipAppStoreReviewDetails_getInstance` | `GET /v1/appClipAppStoreReviewDetails/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClipDefaultExperienceLocalizations | `appClipDefaultExperienceLocalizations_getInstance` | `GET /v1/appClipDefaultExperienceLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClipDefaultExperienceLocalizations | `appClipDefaultExperienceLocalizations_appClipHeaderImage_getToOneRelated` | `GET /v1/appClipDefaultExperienceLocalizations/{id}/appClipHeaderImage` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClipDefaultExperienceLocalizations | `appClipDefaultExperienceLocalizations_appClipHeaderImage_getToOneRelationship` | `GET /v1/appClipDefaultExperienceLocalizations/{id}/relationships/appClipHeaderImage` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClipDefaultExperiences | `appClipDefaultExperiences_getInstance` | `GET /v1/appClipDefaultExperiences/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClipDefaultExperiences | `appClipDefaultExperiences_appClipAppStoreReviewDetail_getToOneRelated` | `GET /v1/appClipDefaultExperiences/{id}/appClipAppStoreReviewDetail` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClipDefaultExperiences | `appClipDefaultExperiences_appClipDefaultExperienceLocalizations_getToManyRelated` | `GET /v1/appClipDefaultExperiences/{id}/appClipDefaultExperienceLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClipDefaultExperiences | `appClipDefaultExperiences_appClipAppStoreReviewDetail_getToOneRelationship` | `GET /v1/appClipDefaultExperiences/{id}/relationships/appClipAppStoreReviewDetail` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClipDefaultExperiences | `appClipDefaultExperiences_appClipDefaultExperienceLocalizations_getToManyRelationship` | `GET /v1/appClipDefaultExperiences/{id}/relationships/appClipDefaultExperienceLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClipDefaultExperiences | `appClipDefaultExperiences_releaseWithAppStoreVersion_getToOneRelationship` | `GET /v1/appClipDefaultExperiences/{id}/relationships/releaseWithAppStoreVersion` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClipDefaultExperiences | `appClipDefaultExperiences_releaseWithAppStoreVersion_getToOneRelated` | `GET /v1/appClipDefaultExperiences/{id}/releaseWithAppStoreVersion` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClipHeaderImages | `appClipHeaderImages_getInstance` | `GET /v1/appClipHeaderImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClips | `appClips_getInstance` | `GET /v1/appClips/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClips | `appClips_appClipAdvancedExperiences_getToManyRelated` | `GET /v1/appClips/{id}/appClipAdvancedExperiences` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClips | `appClips_appClipDefaultExperiences_getToManyRelated` | `GET /v1/appClips/{id}/appClipDefaultExperiences` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClips | `appClips_appClipAdvancedExperiences_getToManyRelationship` | `GET /v1/appClips/{id}/relationships/appClipAdvancedExperiences` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppClips | `appClips_appClipDefaultExperiences_getToManyRelationship` | `GET /v1/appClips/{id}/relationships/appClipDefaultExperiences` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPageLocalizations | `appCustomProductPageLocalizations_getInstance` | `GET /v1/appCustomProductPageLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPageLocalizations | `appCustomProductPageLocalizations_appPreviewSets_getToManyRelated` | `GET /v1/appCustomProductPageLocalizations/{id}/appPreviewSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPageLocalizations | `appCustomProductPageLocalizations_appScreenshotSets_getToManyRelated` | `GET /v1/appCustomProductPageLocalizations/{id}/appScreenshotSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPageLocalizations | `appCustomProductPageLocalizations_placements_getToManyRelated` | `GET /v1/appCustomProductPageLocalizations/{id}/placements` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPageLocalizations | `appCustomProductPageLocalizations_appPreviewSets_getToManyRelationship` | `GET /v1/appCustomProductPageLocalizations/{id}/relationships/appPreviewSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPageLocalizations | `appCustomProductPageLocalizations_appScreenshotSets_getToManyRelationship` | `GET /v1/appCustomProductPageLocalizations/{id}/relationships/appScreenshotSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPageLocalizations | `appCustomProductPageLocalizations_placements_getToManyRelationship` | `GET /v1/appCustomProductPageLocalizations/{id}/relationships/placements` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPageLocalizations | `appCustomProductPageLocalizations_searchKeywords_getToManyRelationship` | `GET /v1/appCustomProductPageLocalizations/{id}/relationships/searchKeywords` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPageLocalizations | `appCustomProductPageLocalizations_searchKeywords_getToManyRelated` | `GET /v1/appCustomProductPageLocalizations/{id}/searchKeywords` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPageVersions | `appCustomProductPageVersions_getInstance` | `GET /v1/appCustomProductPageVersions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPageVersions | `appCustomProductPageVersions_appCustomProductPageLocalizations_getToManyRelated` | `GET /v1/appCustomProductPageVersions/{id}/appCustomProductPageLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPageVersions | `appCustomProductPageVersions_appCustomProductPageLocalizations_getToManyRelationship` | `GET /v1/appCustomProductPageVersions/{id}/relationships/appCustomProductPageLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPages | `appCustomProductPages_getInstance` | `GET /v1/appCustomProductPages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPages | `appCustomProductPages_appCustomProductPageVersions_getToManyRelated` | `GET /v1/appCustomProductPages/{id}/appCustomProductPageVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppCustomProductPages | `appCustomProductPages_appCustomProductPageVersions_getToManyRelationship` | `GET /v1/appCustomProductPages/{id}/relationships/appCustomProductPageVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEncryptionDeclarationDocuments | `appEncryptionDeclarationDocuments_getInstance` | `GET /v1/appEncryptionDeclarationDocuments/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEncryptionDeclarations | `appEncryptionDeclarations_getCollection` | `GET /v1/appEncryptionDeclarations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEncryptionDeclarations | `appEncryptionDeclarations_getInstance` | `GET /v1/appEncryptionDeclarations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEncryptionDeclarations | `appEncryptionDeclarations_app_getToOneRelated` | `GET /v1/appEncryptionDeclarations/{id}/app` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEncryptionDeclarations | `appEncryptionDeclarations_appEncryptionDeclarationDocument_getToOneRelated` | `GET /v1/appEncryptionDeclarations/{id}/appEncryptionDeclarationDocument` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEncryptionDeclarations | `appEncryptionDeclarations_app_getToOneRelationship` | `GET /v1/appEncryptionDeclarations/{id}/relationships/app` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEncryptionDeclarations | `appEncryptionDeclarations_appEncryptionDeclarationDocument_getToOneRelationship` | `GET /v1/appEncryptionDeclarations/{id}/relationships/appEncryptionDeclarationDocument` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEventLocalizations | `appEventLocalizations_getInstance` | `GET /v1/appEventLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEventLocalizations | `appEventLocalizations_appEventScreenshots_getToManyRelated` | `GET /v1/appEventLocalizations/{id}/appEventScreenshots` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEventLocalizations | `appEventLocalizations_appEventVideoClips_getToManyRelated` | `GET /v1/appEventLocalizations/{id}/appEventVideoClips` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEventLocalizations | `appEventLocalizations_placements_getToManyRelated` | `GET /v1/appEventLocalizations/{id}/placements` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEventLocalizations | `appEventLocalizations_appEventScreenshots_getToManyRelationship` | `GET /v1/appEventLocalizations/{id}/relationships/appEventScreenshots` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEventLocalizations | `appEventLocalizations_appEventVideoClips_getToManyRelationship` | `GET /v1/appEventLocalizations/{id}/relationships/appEventVideoClips` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEventLocalizations | `appEventLocalizations_placements_getToManyRelationship` | `GET /v1/appEventLocalizations/{id}/relationships/placements` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEventScreenshots | `appEventScreenshots_getInstance` | `GET /v1/appEventScreenshots/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEventVideoClips | `appEventVideoClips_getInstance` | `GET /v1/appEventVideoClips/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEvents | `appEvents_getInstance` | `GET /v1/appEvents/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEvents | `appEvents_localizations_getToManyRelated` | `GET /v1/appEvents/{id}/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppEvents | `appEvents_localizations_getToManyRelationship` | `GET /v1/appEvents/{id}/relationships/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppInfoLocalizations | `appInfoLocalizations_getInstance` | `GET /v1/appInfoLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_getInstance` | `GET /v1/appInfos/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_ageRatingDeclaration_getToOneRelated` | `GET /v1/appInfos/{id}/ageRatingDeclaration` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_appInfoLocalizations_getToManyRelated` | `GET /v1/appInfos/{id}/appInfoLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_primaryCategory_getToOneRelated` | `GET /v1/appInfos/{id}/primaryCategory` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_primarySubcategoryOne_getToOneRelated` | `GET /v1/appInfos/{id}/primarySubcategoryOne` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_primarySubcategoryTwo_getToOneRelated` | `GET /v1/appInfos/{id}/primarySubcategoryTwo` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_ageRatingDeclaration_getToOneRelationship` | `GET /v1/appInfos/{id}/relationships/ageRatingDeclaration` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_appInfoLocalizations_getToManyRelationship` | `GET /v1/appInfos/{id}/relationships/appInfoLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_primaryCategory_getToOneRelationship` | `GET /v1/appInfos/{id}/relationships/primaryCategory` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_primarySubcategoryOne_getToOneRelationship` | `GET /v1/appInfos/{id}/relationships/primarySubcategoryOne` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_primarySubcategoryTwo_getToOneRelationship` | `GET /v1/appInfos/{id}/relationships/primarySubcategoryTwo` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_secondaryCategory_getToOneRelationship` | `GET /v1/appInfos/{id}/relationships/secondaryCategory` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_secondarySubcategoryOne_getToOneRelationship` | `GET /v1/appInfos/{id}/relationships/secondarySubcategoryOne` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_secondarySubcategoryTwo_getToOneRelationship` | `GET /v1/appInfos/{id}/relationships/secondarySubcategoryTwo` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_territoryAgeRatings_getToManyRelationship` | `GET /v1/appInfos/{id}/relationships/territoryAgeRatings` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_secondaryCategory_getToOneRelated` | `GET /v1/appInfos/{id}/secondaryCategory` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_secondarySubcategoryOne_getToOneRelated` | `GET /v1/appInfos/{id}/secondarySubcategoryOne` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_secondarySubcategoryTwo_getToOneRelated` | `GET /v1/appInfos/{id}/secondarySubcategoryTwo` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppInfos | `appInfos_territoryAgeRatings_getToManyRelated` | `GET /v1/appInfos/{id}/territoryAgeRatings` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppPreviewSets | `appPreviewSets_getInstance` | `GET /v1/appPreviewSets/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppPreviewSets | `appPreviewSets_appPreviews_getToManyRelated` | `GET /v1/appPreviewSets/{id}/appPreviews` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppPreviewSets | `appPreviewSets_appPreviews_getToManyRelationship` | `GET /v1/appPreviewSets/{id}/relationships/appPreviews` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppPreviews | `appPreviews_getInstance` | `GET /v1/appPreviews/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppPricePoints | `appPricePointsV3_getInstance` | `GET /v3/appPricePoints/{id}` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppPricePoints | `appPricePointsV3_equalizations_getToManyRelated` | `GET /v3/appPricePoints/{id}/equalizations` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppPricePoints | `appPricePointsV3_equalizations_getToManyRelationship` | `GET /v3/appPricePoints/{id}/relationships/equalizations` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppPriceSchedules | `appPriceSchedules_getInstance` | `GET /v1/appPriceSchedules/{id}` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppPriceSchedules | `appPriceSchedules_automaticPrices_getToManyRelated` | `GET /v1/appPriceSchedules/{id}/automaticPrices` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppPriceSchedules | `appPriceSchedules_baseTerritory_getToOneRelated` | `GET /v1/appPriceSchedules/{id}/baseTerritory` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppPriceSchedules | `appPriceSchedules_manualPrices_getToManyRelated` | `GET /v1/appPriceSchedules/{id}/manualPrices` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppPriceSchedules | `appPriceSchedules_automaticPrices_getToManyRelationship` | `GET /v1/appPriceSchedules/{id}/relationships/automaticPrices` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppPriceSchedules | `appPriceSchedules_baseTerritory_getToOneRelationship` | `GET /v1/appPriceSchedules/{id}/relationships/baseTerritory` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppPriceSchedules | `appPriceSchedules_manualPrices_getToManyRelationship` | `GET /v1/appPriceSchedules/{id}/relationships/manualPrices` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppScreenshotSets | `appScreenshotSets_getInstance` | `GET /v1/appScreenshotSets/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppScreenshotSets | `appScreenshotSets_appScreenshots_getToManyRelated` | `GET /v1/appScreenshotSets/{id}/appScreenshots` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppScreenshotSets | `appScreenshotSets_appScreenshots_getToManyRelationship` | `GET /v1/appScreenshotSets/{id}/relationships/appScreenshots` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppScreenshots | `appScreenshots_getInstance` | `GET /v1/appScreenshots/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreReviewAttachments | `appStoreReviewAttachments_getInstance` | `GET /v1/appStoreReviewAttachments/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreReviewDetails | `appStoreReviewDetails_getInstance` | `GET /v1/appStoreReviewDetails/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreReviewDetails | `appStoreReviewDetails_appStoreReviewAttachments_getToManyRelated` | `GET /v1/appStoreReviewDetails/{id}/appStoreReviewAttachments` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreReviewDetails | `appStoreReviewDetails_appStoreReviewAttachments_getToManyRelationship` | `GET /v1/appStoreReviewDetails/{id}/relationships/appStoreReviewAttachments` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersionExperimentTreatmentLocalizations | `appStoreVersionExperimentTreatmentLocalizations_getInstance` | `GET /v1/appStoreVersionExperimentTreatmentLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperimentTreatmentLocalizations | `appStoreVersionExperimentTreatmentLocalizations_appPreviewSets_getToManyRelated` | `GET /v1/appStoreVersionExperimentTreatmentLocalizations/{id}/appPreviewSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperimentTreatmentLocalizations | `appStoreVersionExperimentTreatmentLocalizations_appScreenshotSets_getToManyRelated` | `GET /v1/appStoreVersionExperimentTreatmentLocalizations/{id}/appScreenshotSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperimentTreatmentLocalizations | `appStoreVersionExperimentTreatmentLocalizations_placements_getToManyRelated` | `GET /v1/appStoreVersionExperimentTreatmentLocalizations/{id}/placements` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperimentTreatmentLocalizations | `appStoreVersionExperimentTreatmentLocalizations_appPreviewSets_getToManyRelationship` | `GET /v1/appStoreVersionExperimentTreatmentLocalizations/{id}/relationships/appPreviewSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperimentTreatmentLocalizations | `appStoreVersionExperimentTreatmentLocalizations_appScreenshotSets_getToManyRelationship` | `GET /v1/appStoreVersionExperimentTreatmentLocalizations/{id}/relationships/appScreenshotSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperimentTreatmentLocalizations | `appStoreVersionExperimentTreatmentLocalizations_placements_getToManyRelationship` | `GET /v1/appStoreVersionExperimentTreatmentLocalizations/{id}/relationships/placements` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperimentTreatments | `appStoreVersionExperimentTreatments_getInstance` | `GET /v1/appStoreVersionExperimentTreatments/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperimentTreatments | `appStoreVersionExperimentTreatments_appStoreVersionExperimentTreatmentLocalizations_getToManyRelated` | `GET /v1/appStoreVersionExperimentTreatments/{id}/appStoreVersionExperimentTreatmentLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperimentTreatments | `appStoreVersionExperimentTreatments_appStoreVersionExperimentTreatmentLocalizations_getToManyRelationship` | `GET /v1/appStoreVersionExperimentTreatments/{id}/relationships/appStoreVersionExperimentTreatmentLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperiments | `appStoreVersionExperiments_getInstance` | `GET /v1/appStoreVersionExperiments/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperiments | `appStoreVersionExperiments_appStoreVersionExperimentTreatments_getToManyRelated` | `GET /v1/appStoreVersionExperiments/{id}/appStoreVersionExperimentTreatments` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperiments | `appStoreVersionExperiments_appStoreVersionExperimentTreatments_getToManyRelationship` | `GET /v1/appStoreVersionExperiments/{id}/relationships/appStoreVersionExperimentTreatments` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperiments | `appStoreVersionExperimentsV2_getInstance` | `GET /v2/appStoreVersionExperiments/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperiments | `appStoreVersionExperimentsV2_appStoreVersionExperimentTreatments_getToManyRelated` | `GET /v2/appStoreVersionExperiments/{id}/appStoreVersionExperimentTreatments` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionExperiments | `appStoreVersionExperimentsV2_appStoreVersionExperimentTreatments_getToManyRelationship` | `GET /v2/appStoreVersionExperiments/{id}/relationships/appStoreVersionExperimentTreatments` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersionLocalizations | `appStoreVersionLocalizations_getInstance` | `GET /v1/appStoreVersionLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersionLocalizations | `appStoreVersionLocalizations_appPreviewSets_getToManyRelated` | `GET /v1/appStoreVersionLocalizations/{id}/appPreviewSets` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersionLocalizations | `appStoreVersionLocalizations_appScreenshotSets_getToManyRelated` | `GET /v1/appStoreVersionLocalizations/{id}/appScreenshotSets` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersionLocalizations | `appStoreVersionLocalizations_placements_getToManyRelated` | `GET /v1/appStoreVersionLocalizations/{id}/placements` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersionLocalizations | `appStoreVersionLocalizations_appPreviewSets_getToManyRelationship` | `GET /v1/appStoreVersionLocalizations/{id}/relationships/appPreviewSets` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersionLocalizations | `appStoreVersionLocalizations_appScreenshotSets_getToManyRelationship` | `GET /v1/appStoreVersionLocalizations/{id}/relationships/appScreenshotSets` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersionLocalizations | `appStoreVersionLocalizations_placements_getToManyRelationship` | `GET /v1/appStoreVersionLocalizations/{id}/relationships/placements` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersionLocalizations | `appStoreVersionLocalizations_searchKeywords_getToManyRelationship` | `GET /v1/appStoreVersionLocalizations/{id}/relationships/searchKeywords` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersionLocalizations | `appStoreVersionLocalizations_searchKeywords_getToManyRelated` | `GET /v1/appStoreVersionLocalizations/{id}/searchKeywords` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersions | `appStoreVersions_getInstance` | `GET /v1/appStoreVersions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersions | `appStoreVersions_alternativeDistributionPackage_getToOneRelated` | `GET /v1/appStoreVersions/{id}/alternativeDistributionPackage` |  | Account Holder, Admin, App Manager |  |
| AppStoreVersions | `appStoreVersions_appClipDefaultExperience_getToOneRelated` | `GET /v1/appStoreVersions/{id}/appClipDefaultExperience` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersions | `appStoreVersions_appStoreReviewDetail_getToOneRelated` | `GET /v1/appStoreVersions/{id}/appStoreReviewDetail` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersions | `appStoreVersions_appStoreVersionExperiments_getToManyRelated` | `GET /v1/appStoreVersions/{id}/appStoreVersionExperiments` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersions | `appStoreVersions_appStoreVersionExperimentsV2_getToManyRelated` | `GET /v1/appStoreVersions/{id}/appStoreVersionExperimentsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersions | `appStoreVersions_appStoreVersionLocalizations_getToManyRelated` | `GET /v1/appStoreVersions/{id}/appStoreVersionLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersions | `appStoreVersions_appStoreVersionPhasedRelease_getToOneRelated` | `GET /v1/appStoreVersions/{id}/appStoreVersionPhasedRelease` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersions | `appStoreVersions_appStoreVersionSubmission_getToOneRelated` | `GET /v1/appStoreVersions/{id}/appStoreVersionSubmission` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersions | `appStoreVersions_build_getToOneRelated` | `GET /v1/appStoreVersions/{id}/build` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersions | `appStoreVersions_customerReviews_getToManyRelated` | `GET /v1/appStoreVersions/{id}/customerReviews` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersions | `appStoreVersions_gameCenterAppVersion_getToOneRelated` | `GET /v1/appStoreVersions/{id}/gameCenterAppVersion` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersions | `appStoreVersions_alternativeDistributionPackage_getToOneRelationship` | `GET /v1/appStoreVersions/{id}/relationships/alternativeDistributionPackage` |  | Account Holder, Admin, App Manager |  |
| AppStoreVersions | `appStoreVersions_appClipDefaultExperience_getToOneRelationship` | `GET /v1/appStoreVersions/{id}/relationships/appClipDefaultExperience` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersions | `appStoreVersions_appStoreReviewDetail_getToOneRelationship` | `GET /v1/appStoreVersions/{id}/relationships/appStoreReviewDetail` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersions | `appStoreVersions_appStoreVersionExperiments_getToManyRelationship` | `GET /v1/appStoreVersions/{id}/relationships/appStoreVersionExperiments` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersions | `appStoreVersions_appStoreVersionExperimentsV2_getToManyRelationship` | `GET /v1/appStoreVersions/{id}/relationships/appStoreVersionExperimentsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersions | `appStoreVersions_appStoreVersionLocalizations_getToManyRelationship` | `GET /v1/appStoreVersions/{id}/relationships/appStoreVersionLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersions | `appStoreVersions_appStoreVersionPhasedRelease_getToOneRelationship` | `GET /v1/appStoreVersions/{id}/relationships/appStoreVersionPhasedRelease` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersions | `appStoreVersions_appStoreVersionSubmission_getToOneRelationship` | `GET /v1/appStoreVersions/{id}/relationships/appStoreVersionSubmission` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersions | `appStoreVersions_build_getToOneRelationship` | `GET /v1/appStoreVersions/{id}/relationships/build` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersions | `appStoreVersions_customerReviews_getToManyRelationship` | `GET /v1/appStoreVersions/{id}/relationships/customerReviews` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersions | `appStoreVersions_gameCenterAppVersion_getToOneRelationship` | `GET /v1/appStoreVersions/{id}/relationships/gameCenterAppVersion` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| AppStoreVersions | `appStoreVersions_routingAppCoverage_getToOneRelationship` | `GET /v1/appStoreVersions/{id}/relationships/routingAppCoverage` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppStoreVersions | `appStoreVersions_routingAppCoverage_getToOneRelated` | `GET /v1/appStoreVersions/{id}/routingAppCoverage` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppTags | `appTags_territories_getToManyRelationship` | `GET /v1/appTags/{id}/relationships/territories` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| AppTags | `appTags_territories_getToManyRelated` | `GET /v1/appTags/{id}/territories` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_getCollection` | `GET /v1/apps` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support, Finance, Sales, Access to Reports |  |
| Apps | `apps_getInstance` | `GET /v1/apps/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support, Finance, Sales, Access to Reports |  |
| Apps | `apps_accessibilityDeclarations_getToManyRelated` | `GET /v1/apps/{id}/accessibilityDeclarations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_alternativeDistributionKey_getToOneRelated` | `GET /v1/apps/{id}/alternativeDistributionKey` |  | Account Holder, Admin, App Manager |  |
| Apps | `apps_analyticsReportRequests_getToManyRelated` | `GET /v1/apps/{id}/analyticsReportRequests` |  | Account Holder, Admin, Finance, Sales, Access to Reports |  |
| Apps | `apps_androidToIosAppMappingDetails_getToManyRelated` | `GET /v1/apps/{id}/androidToIosAppMappingDetails` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_appAvailabilityV2_getToOneRelated` | `GET /v1/apps/{id}/appAvailabilityV2` |  | Account Holder, Admin, App Manager, Marketing |  |
| Apps | `apps_appClips_getToManyRelated` | `GET /v1/apps/{id}/appClips` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_appCustomProductPages_getToManyRelated` | `GET /v1/apps/{id}/appCustomProductPages` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_appEncryptionDeclarations_getToManyRelated` | `GET /v1/apps/{id}/appEncryptionDeclarations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_appEvents_getToManyRelated` | `GET /v1/apps/{id}/appEvents` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_appInfos_getToManyRelated` | `GET /v1/apps/{id}/appInfos` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_appPricePoints_getToManyRelated` | `GET /v1/apps/{id}/appPricePoints` |  | Account Holder, Admin, App Manager, Marketing |  |
| Apps | `apps_appPriceSchedule_getToOneRelated` | `GET /v1/apps/{id}/appPriceSchedule` |  | Account Holder, Admin, App Manager, Marketing |  |
| Apps | `apps_appStoreVersionExperimentsV2_getToManyRelated` | `GET /v1/apps/{id}/appStoreVersionExperimentsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_appStoreVersions_getToManyRelated` | `GET /v1/apps/{id}/appStoreVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_appTags_getToManyRelated` | `GET /v1/apps/{id}/appTags` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_assetLibrary_getToOneRelated` | `GET /v1/apps/{id}/assetLibrary` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_backgroundAssets_getToManyRelated` | `GET /v1/apps/{id}/backgroundAssets` |  | Account Holder, Admin, App Manager, Developer |  |
| Apps | `apps_betaAppLocalizations_getToManyRelated` | `GET /v1/apps/{id}/betaAppLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_betaAppReviewDetail_getToOneRelated` | `GET /v1/apps/{id}/betaAppReviewDetail` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_betaFeedbackCrashSubmissions_getToManyRelated` | `GET /v1/apps/{id}/betaFeedbackCrashSubmissions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_betaFeedbackScreenshotSubmissions_getToManyRelated` | `GET /v1/apps/{id}/betaFeedbackScreenshotSubmissions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_betaGroups_getToManyRelated` | `GET /v1/apps/{id}/betaGroups` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_betaLicenseAgreement_getToOneRelated` | `GET /v1/apps/{id}/betaLicenseAgreement` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_buildUploads_getToManyRelated` | `GET /v1/apps/{id}/buildUploads` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_builds_getToManyRelated` | `GET /v1/apps/{id}/builds` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_ciProduct_getToOneRelated` | `GET /v1/apps/{id}/ciProduct` |  | Account Holder, Admin, App Manager, Developer |  |
| Apps | `apps_customerReviewSummarizations_getToManyRelated` | `GET /v1/apps/{id}/customerReviewSummarizations` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_customerReviews_getToManyRelated` | `GET /v1/apps/{id}/customerReviews` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_endUserLicenseAgreement_getToOneRelated` | `GET /v1/apps/{id}/endUserLicenseAgreement` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_gameCenterDetail_getToOneRelated` | `GET /v1/apps/{id}/gameCenterDetail` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_gameCenterEnabledVersions_getToManyRelated` | `GET /v1/apps/{id}/gameCenterEnabledVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_inAppPurchases_getToManyRelated` | `GET /v1/apps/{id}/inAppPurchases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_inAppPurchasesV2_getToManyRelated` | `GET /v1/apps/{id}/inAppPurchasesV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_marketplaceSearchDetail_getToOneRelated` | `GET /v1/apps/{id}/marketplaceSearchDetail` |  | Account Holder, Admin, App Manager |  |
| Apps | `apps_betaTesterUsages_getMetrics` | `GET /v1/apps/{id}/metrics/betaTesterUsages` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_perfPowerMetrics_getToManyRelated` | `GET /v1/apps/{id}/perfPowerMetrics` |  | Account Holder, Admin, App Manager, Developer, Finance, Marketing, Sales, Customer Support |  |
| Apps | `apps_performanceOverviews_getToManyRelated` | `GET /v1/apps/{id}/performanceOverviews` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_preReleaseVersions_getToManyRelated` | `GET /v1/apps/{id}/preReleaseVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_promotedPurchases_getToManyRelated` | `GET /v1/apps/{id}/promotedPurchases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_accessibilityDeclarations_getToManyRelationship` | `GET /v1/apps/{id}/relationships/accessibilityDeclarations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_alternativeDistributionKey_getToOneRelationship` | `GET /v1/apps/{id}/relationships/alternativeDistributionKey` |  | Account Holder, Admin, App Manager |  |
| Apps | `apps_analyticsReportRequests_getToManyRelationship` | `GET /v1/apps/{id}/relationships/analyticsReportRequests` |  | Account Holder, Admin, Finance, Sales, Access to Reports |  |
| Apps | `apps_androidToIosAppMappingDetails_getToManyRelationship` | `GET /v1/apps/{id}/relationships/androidToIosAppMappingDetails` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_appAvailabilityV2_getToOneRelationship` | `GET /v1/apps/{id}/relationships/appAvailabilityV2` |  | Account Holder, Admin, App Manager, Marketing |  |
| Apps | `apps_appClips_getToManyRelationship` | `GET /v1/apps/{id}/relationships/appClips` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_appCustomProductPages_getToManyRelationship` | `GET /v1/apps/{id}/relationships/appCustomProductPages` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_appEncryptionDeclarations_getToManyRelationship` | `GET /v1/apps/{id}/relationships/appEncryptionDeclarations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_appEvents_getToManyRelationship` | `GET /v1/apps/{id}/relationships/appEvents` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_appInfos_getToManyRelationship` | `GET /v1/apps/{id}/relationships/appInfos` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_appPricePoints_getToManyRelationship` | `GET /v1/apps/{id}/relationships/appPricePoints` |  | Account Holder, Admin, App Manager, Marketing |  |
| Apps | `apps_appPriceSchedule_getToOneRelationship` | `GET /v1/apps/{id}/relationships/appPriceSchedule` |  | Account Holder, Admin, App Manager, Marketing |  |
| Apps | `apps_appStoreVersionExperimentsV2_getToManyRelationship` | `GET /v1/apps/{id}/relationships/appStoreVersionExperimentsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_appStoreVersions_getToManyRelationship` | `GET /v1/apps/{id}/relationships/appStoreVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_appTags_getToManyRelationship` | `GET /v1/apps/{id}/relationships/appTags` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_assetLibrary_getToOneRelationship` | `GET /v1/apps/{id}/relationships/assetLibrary` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_backgroundAssets_getToManyRelationship` | `GET /v1/apps/{id}/relationships/backgroundAssets` |  | Account Holder, Admin, App Manager, Developer |  |
| Apps | `apps_betaAppLocalizations_getToManyRelationship` | `GET /v1/apps/{id}/relationships/betaAppLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_betaAppReviewDetail_getToOneRelationship` | `GET /v1/apps/{id}/relationships/betaAppReviewDetail` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_betaFeedbackCrashSubmissions_getToManyRelationship` | `GET /v1/apps/{id}/relationships/betaFeedbackCrashSubmissions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_betaFeedbackScreenshotSubmissions_getToManyRelationship` | `GET /v1/apps/{id}/relationships/betaFeedbackScreenshotSubmissions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_betaGroups_getToManyRelationship` | `GET /v1/apps/{id}/relationships/betaGroups` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_betaLicenseAgreement_getToOneRelationship` | `GET /v1/apps/{id}/relationships/betaLicenseAgreement` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_buildUploads_getToManyRelationship` | `GET /v1/apps/{id}/relationships/buildUploads` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_builds_getToManyRelationship` | `GET /v1/apps/{id}/relationships/builds` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_ciProduct_getToOneRelationship` | `GET /v1/apps/{id}/relationships/ciProduct` |  | Account Holder, Admin, App Manager, Developer |  |
| Apps | `apps_customerReviews_getToManyRelationship` | `GET /v1/apps/{id}/relationships/customerReviews` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_endUserLicenseAgreement_getToOneRelationship` | `GET /v1/apps/{id}/relationships/endUserLicenseAgreement` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_gameCenterDetail_getToOneRelationship` | `GET /v1/apps/{id}/relationships/gameCenterDetail` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_gameCenterEnabledVersions_getToManyRelationship` | `GET /v1/apps/{id}/relationships/gameCenterEnabledVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_inAppPurchases_getToManyRelationship` | `GET /v1/apps/{id}/relationships/inAppPurchases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_inAppPurchasesV2_getToManyRelationship` | `GET /v1/apps/{id}/relationships/inAppPurchasesV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_marketplaceSearchDetail_getToOneRelationship` | `GET /v1/apps/{id}/relationships/marketplaceSearchDetail` |  | Account Holder, Admin, App Manager |  |
| Apps | `apps_preReleaseVersions_getToManyRelationship` | `GET /v1/apps/{id}/relationships/preReleaseVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_promotedPurchases_getToManyRelationship` | `GET /v1/apps/{id}/relationships/promotedPurchases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_retentionMessages_getToManyRelationship` | `GET /v1/apps/{id}/relationships/retentionMessages` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_reviewSubmissions_getToManyRelationship` | `GET /v1/apps/{id}/relationships/reviewSubmissions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_searchKeywords_getToManyRelationship` | `GET /v1/apps/{id}/relationships/searchKeywords` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_subscriptionGracePeriod_getToOneRelationship` | `GET /v1/apps/{id}/relationships/subscriptionGracePeriod` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_subscriptionGroups_getToManyRelationship` | `GET /v1/apps/{id}/relationships/subscriptionGroups` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_webhooks_getToManyRelationship` | `GET /v1/apps/{id}/relationships/webhooks` |  | Account Holder, Admin, App Manager |  |
| Apps | `apps_retentionMessages_getToManyRelated` | `GET /v1/apps/{id}/retentionMessages` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_reviewSubmissions_getToManyRelated` | `GET /v1/apps/{id}/reviewSubmissions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_searchKeywords_getToManyRelated` | `GET /v1/apps/{id}/searchKeywords` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Apps | `apps_subscriptionGracePeriod_getToOneRelated` | `GET /v1/apps/{id}/subscriptionGracePeriod` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_subscriptionGroups_getToManyRelated` | `GET /v1/apps/{id}/subscriptionGroups` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Apps | `apps_webhooks_getToManyRelated` | `GET /v1/apps/{id}/webhooks` |  | Account Holder, Admin, App Manager |  |
| BackgroundAssetUploadFiles | `backgroundAssetUploadFiles_getInstance` | `GET /v1/backgroundAssetUploadFiles/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| BackgroundAssetVersionAppStoreReleases | `backgroundAssetVersionAppStoreReleases_getInstance` | `GET /v1/backgroundAssetVersionAppStoreReleases/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| BackgroundAssetVersionExternalBetaReleases | `backgroundAssetVersionExternalBetaReleases_getInstance` | `GET /v1/backgroundAssetVersionExternalBetaReleases/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| BackgroundAssetVersionInternalBetaReleases | `backgroundAssetVersionInternalBetaReleases_getInstance` | `GET /v1/backgroundAssetVersionInternalBetaReleases/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| BackgroundAssetVersions | `backgroundAssetVersions_getInstance` | `GET /v1/backgroundAssetVersions/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| BackgroundAssetVersions | `backgroundAssetVersions_backgroundAssetUploadFiles_getToManyRelated` | `GET /v1/backgroundAssetVersions/{id}/backgroundAssetUploadFiles` |  | Account Holder, Admin, App Manager, Developer |  |
| BackgroundAssetVersions | `backgroundAssetVersions_backgroundAssetUploadFiles_getToManyRelationship` | `GET /v1/backgroundAssetVersions/{id}/relationships/backgroundAssetUploadFiles` |  | Account Holder, Admin, App Manager, Developer |  |
| BackgroundAssets | `backgroundAssets_getInstance` | `GET /v1/backgroundAssets/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| BackgroundAssets | `backgroundAssets_versions_getToManyRelationship` | `GET /v1/backgroundAssets/{id}/relationships/versions` |  | Account Holder, Admin, App Manager, Developer |  |
| BackgroundAssets | `backgroundAssets_versions_getToManyRelated` | `GET /v1/backgroundAssets/{id}/versions` |  | Account Holder, Admin, App Manager, Developer |  |
| BetaAppClipInvocations | `betaAppClipInvocations_getInstance` | `GET /v1/betaAppClipInvocations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaAppLocalizations | `betaAppLocalizations_getCollection` | `GET /v1/betaAppLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaAppLocalizations | `betaAppLocalizations_getInstance` | `GET /v1/betaAppLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaAppLocalizations | `betaAppLocalizations_app_getToOneRelated` | `GET /v1/betaAppLocalizations/{id}/app` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaAppLocalizations | `betaAppLocalizations_app_getToOneRelationship` | `GET /v1/betaAppLocalizations/{id}/relationships/app` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaAppReviewDetails | `betaAppReviewDetails_getCollection` | `GET /v1/betaAppReviewDetails` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaAppReviewDetails | `betaAppReviewDetails_getInstance` | `GET /v1/betaAppReviewDetails/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaAppReviewDetails | `betaAppReviewDetails_app_getToOneRelated` | `GET /v1/betaAppReviewDetails/{id}/app` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaAppReviewDetails | `betaAppReviewDetails_app_getToOneRelationship` | `GET /v1/betaAppReviewDetails/{id}/relationships/app` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaAppReviewSubmissions | `betaAppReviewSubmissions_getCollection` | `GET /v1/betaAppReviewSubmissions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaAppReviewSubmissions | `betaAppReviewSubmissions_getInstance` | `GET /v1/betaAppReviewSubmissions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaAppReviewSubmissions | `betaAppReviewSubmissions_build_getToOneRelated` | `GET /v1/betaAppReviewSubmissions/{id}/build` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaAppReviewSubmissions | `betaAppReviewSubmissions_build_getToOneRelationship` | `GET /v1/betaAppReviewSubmissions/{id}/relationships/build` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaBuildLocalizations | `betaBuildLocalizations_getCollection` | `GET /v1/betaBuildLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaBuildLocalizations | `betaBuildLocalizations_getInstance` | `GET /v1/betaBuildLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaBuildLocalizations | `betaBuildLocalizations_build_getToOneRelated` | `GET /v1/betaBuildLocalizations/{id}/build` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaBuildLocalizations | `betaBuildLocalizations_build_getToOneRelationship` | `GET /v1/betaBuildLocalizations/{id}/relationships/build` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaCrashLogs | `betaCrashLogs_getInstance` | `GET /v1/betaCrashLogs/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaFeedbackCrashSubmissions | `betaFeedbackCrashSubmissions_getInstance` | `GET /v1/betaFeedbackCrashSubmissions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaFeedbackCrashSubmissions | `betaFeedbackCrashSubmissions_crashLog_getToOneRelated` | `GET /v1/betaFeedbackCrashSubmissions/{id}/crashLog` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaFeedbackCrashSubmissions | `betaFeedbackCrashSubmissions_crashLog_getToOneRelationship` | `GET /v1/betaFeedbackCrashSubmissions/{id}/relationships/crashLog` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaFeedbackScreenshotSubmissions | `betaFeedbackScreenshotSubmissions_getInstance` | `GET /v1/betaFeedbackScreenshotSubmissions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_getCollection` | `GET /v1/betaGroups` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_getInstance` | `GET /v1/betaGroups/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_app_getToOneRelated` | `GET /v1/betaGroups/{id}/app` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_betaRecruitmentCriteria_getToOneRelated` | `GET /v1/betaGroups/{id}/betaRecruitmentCriteria` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_betaRecruitmentCriterionCompatibleBuildCheck_getToOneRelated` | `GET /v1/betaGroups/{id}/betaRecruitmentCriterionCompatibleBuildCheck` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_betaTesters_getToManyRelated` | `GET /v1/betaGroups/{id}/betaTesters` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_builds_getToManyRelated` | `GET /v1/betaGroups/{id}/builds` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_betaTesterUsages_getMetrics` | `GET /v1/betaGroups/{id}/metrics/betaTesterUsages` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_publicLinkUsages_getMetrics` | `GET /v1/betaGroups/{id}/metrics/publicLinkUsages` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_app_getToOneRelationship` | `GET /v1/betaGroups/{id}/relationships/app` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_betaRecruitmentCriteria_getToOneRelationship` | `GET /v1/betaGroups/{id}/relationships/betaRecruitmentCriteria` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_betaRecruitmentCriterionCompatibleBuildCheck_getToOneRelationship` | `GET /v1/betaGroups/{id}/relationships/betaRecruitmentCriterionCompatibleBuildCheck` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_betaTesters_getToManyRelationship` | `GET /v1/betaGroups/{id}/relationships/betaTesters` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_builds_getToManyRelationship` | `GET /v1/betaGroups/{id}/relationships/builds` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaLicenseAgreements | `betaLicenseAgreements_getCollection` | `GET /v1/betaLicenseAgreements` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaLicenseAgreements | `betaLicenseAgreements_getInstance` | `GET /v1/betaLicenseAgreements/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaLicenseAgreements | `betaLicenseAgreements_app_getToOneRelated` | `GET /v1/betaLicenseAgreements/{id}/app` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaLicenseAgreements | `betaLicenseAgreements_app_getToOneRelationship` | `GET /v1/betaLicenseAgreements/{id}/relationships/app` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaRecruitmentCriterionOptions | `betaRecruitmentCriterionOptions_getCollection` | `GET /v1/betaRecruitmentCriterionOptions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_getCollection` | `GET /v1/betaTesters` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_getInstance` | `GET /v1/betaTesters/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_apps_getToManyRelated` | `GET /v1/betaTesters/{id}/apps` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_betaGroups_getToManyRelated` | `GET /v1/betaTesters/{id}/betaGroups` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_builds_getToManyRelated` | `GET /v1/betaTesters/{id}/builds` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_betaTesterUsages_getMetrics` | `GET /v1/betaTesters/{id}/metrics/betaTesterUsages` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_apps_getToManyRelationship` | `GET /v1/betaTesters/{id}/relationships/apps` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_betaGroups_getToManyRelationship` | `GET /v1/betaTesters/{id}/relationships/betaGroups` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_builds_getToManyRelationship` | `GET /v1/betaTesters/{id}/relationships/builds` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildBetaDetails | `buildBetaDetails_getCollection` | `GET /v1/buildBetaDetails` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildBetaDetails | `buildBetaDetails_getInstance` | `GET /v1/buildBetaDetails/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildBetaDetails | `buildBetaDetails_build_getToOneRelated` | `GET /v1/buildBetaDetails/{id}/build` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildBetaDetails | `buildBetaDetails_build_getToOneRelationship` | `GET /v1/buildBetaDetails/{id}/relationships/build` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildBundles | `buildBundles_appClipDomainCacheStatus_getToOneRelated` | `GET /v1/buildBundles/{id}/appClipDomainCacheStatus` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildBundles | `buildBundles_appClipDomainDebugStatus_getToOneRelated` | `GET /v1/buildBundles/{id}/appClipDomainDebugStatus` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildBundles | `buildBundles_betaAppClipInvocations_getToManyRelated` | `GET /v1/buildBundles/{id}/betaAppClipInvocations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildBundles | `buildBundles_buildBundleFileSizes_getToManyRelated` | `GET /v1/buildBundles/{id}/buildBundleFileSizes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildBundles | `buildBundles_appClipDomainCacheStatus_getToOneRelationship` | `GET /v1/buildBundles/{id}/relationships/appClipDomainCacheStatus` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildBundles | `buildBundles_appClipDomainDebugStatus_getToOneRelationship` | `GET /v1/buildBundles/{id}/relationships/appClipDomainDebugStatus` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildBundles | `buildBundles_betaAppClipInvocations_getToManyRelationship` | `GET /v1/buildBundles/{id}/relationships/betaAppClipInvocations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildBundles | `buildBundles_buildBundleFileSizes_getToManyRelationship` | `GET /v1/buildBundles/{id}/relationships/buildBundleFileSizes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildUploadFiles | `buildUploadFiles_getInstance` | `GET /v1/buildUploadFiles/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildUploads | `buildUploads_getInstance` | `GET /v1/buildUploads/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildUploads | `buildUploads_buildUploadFiles_getToManyRelated` | `GET /v1/buildUploads/{id}/buildUploadFiles` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildUploads | `buildUploads_buildUploadFiles_getToManyRelationship` | `GET /v1/buildUploads/{id}/relationships/buildUploadFiles` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_getCollection` | `GET /v1/builds` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_getInstance` | `GET /v1/builds/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_app_getToOneRelated` | `GET /v1/builds/{id}/app` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_appEncryptionDeclaration_getToOneRelated` | `GET /v1/builds/{id}/appEncryptionDeclaration` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_appStoreVersion_getToOneRelated` | `GET /v1/builds/{id}/appStoreVersion` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_betaAppReviewSubmission_getToOneRelated` | `GET /v1/builds/{id}/betaAppReviewSubmission` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_betaBuildLocalizations_getToManyRelated` | `GET /v1/builds/{id}/betaBuildLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_buildBetaDetail_getToOneRelated` | `GET /v1/builds/{id}/buildBetaDetail` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_diagnosticSignatures_getToManyRelated` | `GET /v1/builds/{id}/diagnosticSignatures` |  | Account Holder, Admin, App Manager, Developer, Finance, Marketing, Sales, Customer Support |  |
| Builds | `builds_icons_getToManyRelated` | `GET /v1/builds/{id}/icons` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_individualTesters_getToManyRelated` | `GET /v1/builds/{id}/individualTesters` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_betaBuildUsages_getMetrics` | `GET /v1/builds/{id}/metrics/betaBuildUsages` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_perfPowerMetrics_getToManyRelated` | `GET /v1/builds/{id}/perfPowerMetrics` |  | Account Holder, Admin, App Manager, Developer, Finance, Marketing, Sales, Customer Support |  |
| Builds | `builds_preReleaseVersion_getToOneRelated` | `GET /v1/builds/{id}/preReleaseVersion` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_app_getToOneRelationship` | `GET /v1/builds/{id}/relationships/app` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_appEncryptionDeclaration_getToOneRelationship` | `GET /v1/builds/{id}/relationships/appEncryptionDeclaration` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_appStoreVersion_getToOneRelationship` | `GET /v1/builds/{id}/relationships/appStoreVersion` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_betaAppReviewSubmission_getToOneRelationship` | `GET /v1/builds/{id}/relationships/betaAppReviewSubmission` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_betaBuildLocalizations_getToManyRelationship` | `GET /v1/builds/{id}/relationships/betaBuildLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_buildBetaDetail_getToOneRelationship` | `GET /v1/builds/{id}/relationships/buildBetaDetail` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_diagnosticSignatures_getToManyRelationship` | `GET /v1/builds/{id}/relationships/diagnosticSignatures` |  | Account Holder, Admin, App Manager, Developer, Finance, Marketing, Sales, Customer Support |  |
| Builds | `builds_icons_getToManyRelationship` | `GET /v1/builds/{id}/relationships/icons` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_individualTesters_getToManyRelationship` | `GET /v1/builds/{id}/relationships/individualTesters` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_preReleaseVersion_getToOneRelationship` | `GET /v1/builds/{id}/relationships/preReleaseVersion` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BundleIds | `bundleIds_getCollection` | `GET /v1/bundleIds` |  | Account Holder, Admin, App Manager, Developer |  |
| BundleIds | `bundleIds_getInstance` | `GET /v1/bundleIds/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| BundleIds | `bundleIds_app_getToOneRelated` | `GET /v1/bundleIds/{id}/app` |  | Account Holder, Admin, App Manager, Developer |  |
| BundleIds | `bundleIds_bundleIdCapabilities_getToManyRelated` | `GET /v1/bundleIds/{id}/bundleIdCapabilities` |  | Account Holder, Admin, App Manager, Developer |  |
| BundleIds | `bundleIds_profiles_getToManyRelated` | `GET /v1/bundleIds/{id}/profiles` |  | Account Holder, Admin, App Manager, Developer |  |
| BundleIds | `bundleIds_app_getToOneRelationship` | `GET /v1/bundleIds/{id}/relationships/app` |  | Account Holder, Admin, App Manager, Developer |  |
| BundleIds | `bundleIds_bundleIdCapabilities_getToManyRelationship` | `GET /v1/bundleIds/{id}/relationships/bundleIdCapabilities` |  | Account Holder, Admin, App Manager, Developer |  |
| BundleIds | `bundleIds_profiles_getToManyRelationship` | `GET /v1/bundleIds/{id}/relationships/profiles` |  | Account Holder, Admin, App Manager, Developer |  |
| Certificates | `certificates_getCollection` | `GET /v1/certificates` |  | Account Holder, Admin, App Manager, Developer |  |
| Certificates | `certificates_getInstance` | `GET /v1/certificates/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| Certificates | `certificates_passTypeId_getToOneRelated` | `GET /v1/certificates/{id}/passTypeId` |  | Account Holder, Admin, App Manager, Developer |  |
| Certificates | `certificates_passTypeId_getToOneRelationship` | `GET /v1/certificates/{id}/relationships/passTypeId` |  | Account Holder, Admin, App Manager, Developer |  |
| CiArtifacts | `ciArtifacts_getInstance` | `GET /v1/ciArtifacts/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| CiBuildActions | `ciBuildActions_getInstance` | `GET /v1/ciBuildActions/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| CiBuildActions | `ciBuildActions_artifacts_getToManyRelated` | `GET /v1/ciBuildActions/{id}/artifacts` |  | Account Holder, Admin, App Manager, Developer |  |
| CiBuildActions | `ciBuildActions_buildRun_getToOneRelated` | `GET /v1/ciBuildActions/{id}/buildRun` |  | Account Holder, Admin, App Manager, Developer |  |
| CiBuildActions | `ciBuildActions_issues_getToManyRelated` | `GET /v1/ciBuildActions/{id}/issues` |  | Account Holder, Admin, App Manager, Developer |  |
| CiBuildActions | `ciBuildActions_artifacts_getToManyRelationship` | `GET /v1/ciBuildActions/{id}/relationships/artifacts` |  | Account Holder, Admin, App Manager, Developer |  |
| CiBuildActions | `ciBuildActions_buildRun_getToOneRelationship` | `GET /v1/ciBuildActions/{id}/relationships/buildRun` |  | Account Holder, Admin, App Manager, Developer |  |
| CiBuildActions | `ciBuildActions_issues_getToManyRelationship` | `GET /v1/ciBuildActions/{id}/relationships/issues` |  | Account Holder, Admin, App Manager, Developer |  |
| CiBuildActions | `ciBuildActions_testResults_getToManyRelationship` | `GET /v1/ciBuildActions/{id}/relationships/testResults` |  | Account Holder, Admin, App Manager, Developer |  |
| CiBuildActions | `ciBuildActions_testResults_getToManyRelated` | `GET /v1/ciBuildActions/{id}/testResults` |  | Account Holder, Admin, App Manager, Developer |  |
| CiBuildRuns | `ciBuildRuns_getInstance` | `GET /v1/ciBuildRuns/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| CiBuildRuns | `ciBuildRuns_actions_getToManyRelated` | `GET /v1/ciBuildRuns/{id}/actions` |  | Account Holder, Admin, App Manager, Developer |  |
| CiBuildRuns | `ciBuildRuns_builds_getToManyRelated` | `GET /v1/ciBuildRuns/{id}/builds` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| CiBuildRuns | `ciBuildRuns_actions_getToManyRelationship` | `GET /v1/ciBuildRuns/{id}/relationships/actions` |  | Account Holder, Admin, App Manager, Developer |  |
| CiBuildRuns | `ciBuildRuns_builds_getToManyRelationship` | `GET /v1/ciBuildRuns/{id}/relationships/builds` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| CiIssues | `ciIssues_getInstance` | `GET /v1/ciIssues/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| CiMacOsVersions | `ciMacOsVersions_getCollection` | `GET /v1/ciMacOsVersions` |  | Account Holder, Admin, App Manager, Developer |  |
| CiMacOsVersions | `ciMacOsVersions_getInstance` | `GET /v1/ciMacOsVersions/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| CiMacOsVersions | `ciMacOsVersions_xcodeVersions_getToManyRelationship` | `GET /v1/ciMacOsVersions/{id}/relationships/xcodeVersions` |  | Account Holder, Admin, App Manager, Developer |  |
| CiMacOsVersions | `ciMacOsVersions_xcodeVersions_getToManyRelated` | `GET /v1/ciMacOsVersions/{id}/xcodeVersions` |  | Account Holder, Admin, App Manager, Developer |  |
| CiProducts | `ciProducts_getCollection` | `GET /v1/ciProducts` |  | Account Holder, Admin, App Manager, Developer |  |
| CiProducts | `ciProducts_getInstance` | `GET /v1/ciProducts/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| CiProducts | `ciProducts_additionalRepositories_getToManyRelated` | `GET /v1/ciProducts/{id}/additionalRepositories` |  | Account Holder, Admin, App Manager, Developer |  |
| CiProducts | `ciProducts_app_getToOneRelated` | `GET /v1/ciProducts/{id}/app` |  | Account Holder, Admin, App Manager, Developer |  |
| CiProducts | `ciProducts_buildRuns_getToManyRelated` | `GET /v1/ciProducts/{id}/buildRuns` |  | Account Holder, Admin, App Manager, Developer |  |
| CiProducts | `ciProducts_primaryRepositories_getToManyRelated` | `GET /v1/ciProducts/{id}/primaryRepositories` |  | Account Holder, Admin, App Manager, Developer |  |
| CiProducts | `ciProducts_additionalRepositories_getToManyRelationship` | `GET /v1/ciProducts/{id}/relationships/additionalRepositories` |  | Account Holder, Admin, App Manager, Developer |  |
| CiProducts | `ciProducts_app_getToOneRelationship` | `GET /v1/ciProducts/{id}/relationships/app` |  | Account Holder, Admin, App Manager, Developer |  |
| CiProducts | `ciProducts_buildRuns_getToManyRelationship` | `GET /v1/ciProducts/{id}/relationships/buildRuns` |  | Account Holder, Admin, App Manager, Developer |  |
| CiProducts | `ciProducts_primaryRepositories_getToManyRelationship` | `GET /v1/ciProducts/{id}/relationships/primaryRepositories` |  | Account Holder, Admin, App Manager, Developer |  |
| CiProducts | `ciProducts_workflows_getToManyRelationship` | `GET /v1/ciProducts/{id}/relationships/workflows` |  | Account Holder, Admin, App Manager, Developer |  |
| CiProducts | `ciProducts_workflows_getToManyRelated` | `GET /v1/ciProducts/{id}/workflows` |  | Account Holder, Admin, App Manager, Developer |  |
| CiTestResults | `ciTestResults_getInstance` | `GET /v1/ciTestResults/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| CiWorkflows | `ciWorkflows_getInstance` | `GET /v1/ciWorkflows/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| CiWorkflows | `ciWorkflows_buildRuns_getToManyRelated` | `GET /v1/ciWorkflows/{id}/buildRuns` |  | Account Holder, Admin, App Manager, Developer |  |
| CiWorkflows | `ciWorkflows_buildRuns_getToManyRelationship` | `GET /v1/ciWorkflows/{id}/relationships/buildRuns` |  | Account Holder, Admin, App Manager, Developer |  |
| CiWorkflows | `ciWorkflows_repository_getToOneRelationship` | `GET /v1/ciWorkflows/{id}/relationships/repository` |  | Account Holder, Admin, App Manager, Developer |  |
| CiWorkflows | `ciWorkflows_repository_getToOneRelated` | `GET /v1/ciWorkflows/{id}/repository` |  | Account Holder, Admin, App Manager, Developer |  |
| CiXcodeVersions | `ciXcodeVersions_getCollection` | `GET /v1/ciXcodeVersions` |  | Account Holder, Admin, App Manager, Developer |  |
| CiXcodeVersions | `ciXcodeVersions_getInstance` | `GET /v1/ciXcodeVersions/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| CiXcodeVersions | `ciXcodeVersions_macOsVersions_getToManyRelated` | `GET /v1/ciXcodeVersions/{id}/macOsVersions` |  | Account Holder, Admin, App Manager, Developer |  |
| CiXcodeVersions | `ciXcodeVersions_macOsVersions_getToManyRelationship` | `GET /v1/ciXcodeVersions/{id}/relationships/macOsVersions` |  | Account Holder, Admin, App Manager, Developer |  |
| CustomerReviewResponses | `customerReviewResponses_getInstance` | `GET /v1/customerReviewResponses/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| CustomerReviews | `customerReviews_getInstance` | `GET /v1/customerReviews/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| CustomerReviews | `customerReviews_response_getToOneRelationship` | `GET /v1/customerReviews/{id}/relationships/response` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| CustomerReviews | `customerReviews_response_getToOneRelated` | `GET /v1/customerReviews/{id}/response` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| Devices | `devices_getCollection` | `GET /v1/devices` |  | Account Holder, Admin, App Manager, Developer |  |
| Devices | `devices_getInstance` | `GET /v1/devices/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| DiagnosticSignatures | `diagnosticSignatures_logs_getToManyRelated` | `GET /v1/diagnosticSignatures/{id}/logs` |  | Account Holder, Admin, App Manager, Developer, Finance, Marketing, Sales, Customer Support |  |
| EndUserLicenseAgreements | `endUserLicenseAgreements_getInstance` | `GET /v1/endUserLicenseAgreements/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| EndUserLicenseAgreements | `endUserLicenseAgreements_territories_getToManyRelationship` | `GET /v1/endUserLicenseAgreements/{id}/relationships/territories` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| EndUserLicenseAgreements | `endUserLicenseAgreements_territories_getToManyRelated` | `GET /v1/endUserLicenseAgreements/{id}/territories` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| FinanceReports | `financeReports_getCollection` | `GET /v1/financeReports` |  | Account Holder, Admin, Finance |  |
| GameCenterAchievementImages | `gameCenterAchievementImages_getInstance` | `GET /v1/gameCenterAchievementImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementImages | `gameCenterAchievementImagesV2_getInstance` | `GET /v2/gameCenterAchievementImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementLocalizations | `gameCenterAchievementLocalizations_getInstance` | `GET /v1/gameCenterAchievementLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementLocalizations | `gameCenterAchievementLocalizations_gameCenterAchievement_getToOneRelated` | `GET /v1/gameCenterAchievementLocalizations/{id}/gameCenterAchievement` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementLocalizations | `gameCenterAchievementLocalizations_gameCenterAchievementImage_getToOneRelated` | `GET /v1/gameCenterAchievementLocalizations/{id}/gameCenterAchievementImage` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementLocalizations | `gameCenterAchievementLocalizations_gameCenterAchievement_getToOneRelationship` | `GET /v1/gameCenterAchievementLocalizations/{id}/relationships/gameCenterAchievement` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementLocalizations | `gameCenterAchievementLocalizations_gameCenterAchievementImage_getToOneRelationship` | `GET /v1/gameCenterAchievementLocalizations/{id}/relationships/gameCenterAchievementImage` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementLocalizations | `gameCenterAchievementLocalizationsV2_getInstance` | `GET /v2/gameCenterAchievementLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementLocalizations | `gameCenterAchievementLocalizationsV2_image_getToOneRelated` | `GET /v2/gameCenterAchievementLocalizations/{id}/image` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementLocalizations | `gameCenterAchievementLocalizationsV2_image_getToOneRelationship` | `GET /v2/gameCenterAchievementLocalizations/{id}/relationships/image` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementReleases | `gameCenterAchievementReleases_getInstance` | `GET /v1/gameCenterAchievementReleases/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementVersions | `gameCenterAchievementVersionsV2_getInstance` | `GET /v2/gameCenterAchievementVersions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementVersions | `gameCenterAchievementVersionsV2_localizations_getToManyRelated` | `GET /v2/gameCenterAchievementVersions/{id}/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementVersions | `gameCenterAchievementVersionsV2_localizations_getToManyRelationship` | `GET /v2/gameCenterAchievementVersions/{id}/relationships/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievements_getInstance` | `GET /v1/gameCenterAchievements/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievements_groupAchievement_getToOneRelated` | `GET /v1/gameCenterAchievements/{id}/groupAchievement` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievements_localizations_getToManyRelated` | `GET /v1/gameCenterAchievements/{id}/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievements_groupAchievement_getToOneRelationship` | `GET /v1/gameCenterAchievements/{id}/relationships/groupAchievement` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievements_localizations_getToManyRelationship` | `GET /v1/gameCenterAchievements/{id}/relationships/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievements_releases_getToManyRelationship` | `GET /v1/gameCenterAchievements/{id}/relationships/releases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievements_releases_getToManyRelated` | `GET /v1/gameCenterAchievements/{id}/releases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievementsV2_getInstance` | `GET /v2/gameCenterAchievements/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievementsV2_versions_getToManyRelationship` | `GET /v2/gameCenterAchievements/{id}/relationships/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievementsV2_versions_getToManyRelated` | `GET /v2/gameCenterAchievements/{id}/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivities | `gameCenterActivities_getInstance` | `GET /v1/gameCenterActivities/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivities | `gameCenterActivities_versions_getToManyRelationship` | `GET /v1/gameCenterActivities/{id}/relationships/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivities | `gameCenterActivities_versions_getToManyRelated` | `GET /v1/gameCenterActivities/{id}/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityImages | `gameCenterActivityImages_getInstance` | `GET /v1/gameCenterActivityImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityLocalizations | `gameCenterActivityLocalizations_getInstance` | `GET /v1/gameCenterActivityLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityLocalizations | `gameCenterActivityLocalizations_image_getToOneRelated` | `GET /v1/gameCenterActivityLocalizations/{id}/image` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityLocalizations | `gameCenterActivityLocalizations_image_getToOneRelationship` | `GET /v1/gameCenterActivityLocalizations/{id}/relationships/image` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityVersionReleases | `gameCenterActivityVersionReleases_getInstance` | `GET /v1/gameCenterActivityVersionReleases/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityVersions | `gameCenterActivityVersions_getInstance` | `GET /v1/gameCenterActivityVersions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityVersions | `gameCenterActivityVersions_defaultImage_getToOneRelated` | `GET /v1/gameCenterActivityVersions/{id}/defaultImage` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityVersions | `gameCenterActivityVersions_localizations_getToManyRelated` | `GET /v1/gameCenterActivityVersions/{id}/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityVersions | `gameCenterActivityVersions_defaultImage_getToOneRelationship` | `GET /v1/gameCenterActivityVersions/{id}/relationships/defaultImage` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityVersions | `gameCenterActivityVersions_localizations_getToManyRelationship` | `GET /v1/gameCenterActivityVersions/{id}/relationships/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAppVersions | `gameCenterAppVersions_getInstance` | `GET /v1/gameCenterAppVersions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAppVersions | `gameCenterAppVersions_appStoreVersion_getToOneRelated` | `GET /v1/gameCenterAppVersions/{id}/appStoreVersion` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAppVersions | `gameCenterAppVersions_compatibilityVersions_getToManyRelated` | `GET /v1/gameCenterAppVersions/{id}/compatibilityVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAppVersions | `gameCenterAppVersions_appStoreVersion_getToOneRelationship` | `GET /v1/gameCenterAppVersions/{id}/relationships/appStoreVersion` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAppVersions | `gameCenterAppVersions_compatibilityVersions_getToManyRelationship` | `GET /v1/gameCenterAppVersions/{id}/relationships/compatibilityVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeImages | `gameCenterChallengeImages_getInstance` | `GET /v1/gameCenterChallengeImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeLocalizations | `gameCenterChallengeLocalizations_getInstance` | `GET /v1/gameCenterChallengeLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeLocalizations | `gameCenterChallengeLocalizations_image_getToOneRelated` | `GET /v1/gameCenterChallengeLocalizations/{id}/image` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeLocalizations | `gameCenterChallengeLocalizations_image_getToOneRelationship` | `GET /v1/gameCenterChallengeLocalizations/{id}/relationships/image` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeVersionReleases | `gameCenterChallengeVersionReleases_getInstance` | `GET /v1/gameCenterChallengeVersionReleases/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeVersions | `gameCenterChallengeVersions_getInstance` | `GET /v1/gameCenterChallengeVersions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeVersions | `gameCenterChallengeVersions_defaultImage_getToOneRelated` | `GET /v1/gameCenterChallengeVersions/{id}/defaultImage` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeVersions | `gameCenterChallengeVersions_localizations_getToManyRelated` | `GET /v1/gameCenterChallengeVersions/{id}/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeVersions | `gameCenterChallengeVersions_defaultImage_getToOneRelationship` | `GET /v1/gameCenterChallengeVersions/{id}/relationships/defaultImage` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeVersions | `gameCenterChallengeVersions_localizations_getToManyRelationship` | `GET /v1/gameCenterChallengeVersions/{id}/relationships/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallenges | `gameCenterChallenges_getInstance` | `GET /v1/gameCenterChallenges/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallenges | `gameCenterChallenges_versions_getToManyRelationship` | `GET /v1/gameCenterChallenges/{id}/relationships/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallenges | `gameCenterChallenges_versions_getToManyRelated` | `GET /v1/gameCenterChallenges/{id}/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_getInstance` | `GET /v1/gameCenterDetails/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_achievementReleases_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/achievementReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_activityReleases_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/activityReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_blockedPlayers_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/blockedPlayers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_challengeReleases_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/challengeReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterAchievements_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/gameCenterAchievements` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterAchievementsV2_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/gameCenterAchievementsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterActivities_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/gameCenterActivities` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterAppVersions_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/gameCenterAppVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterChallenges_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/gameCenterChallenges` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterGroup_getToOneRelated` | `GET /v1/gameCenterDetails/{id}/gameCenterGroup` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterLeaderboardSets_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/gameCenterLeaderboardSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterLeaderboardSetsV2_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/gameCenterLeaderboardSetsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterLeaderboards_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/gameCenterLeaderboards` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterLeaderboardsV2_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/gameCenterLeaderboardsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_leaderboardReleases_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/leaderboardReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_leaderboardSetReleases_getToManyRelated` | `GET /v1/gameCenterDetails/{id}/leaderboardSetReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_classicMatchmakingRequests_getMetrics` | `GET /v1/gameCenterDetails/{id}/metrics/classicMatchmakingRequests` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_ruleBasedMatchmakingRequests_getMetrics` | `GET /v1/gameCenterDetails/{id}/metrics/ruleBasedMatchmakingRequests` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_achievementReleases_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/achievementReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_activityReleases_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/activityReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_blockedPlayers_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/blockedPlayers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_challengeReleases_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/challengeReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterAchievements_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/gameCenterAchievements` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterAchievementsV2_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/gameCenterAchievementsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterActivities_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/gameCenterActivities` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterAppVersions_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/gameCenterAppVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterChallenges_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/gameCenterChallenges` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterGroup_getToOneRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/gameCenterGroup` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterLeaderboardSets_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/gameCenterLeaderboardSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterLeaderboardSetsV2_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/gameCenterLeaderboardSetsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterLeaderboards_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/gameCenterLeaderboards` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterLeaderboardsV2_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/gameCenterLeaderboardsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_leaderboardReleases_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/leaderboardReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_leaderboardSetReleases_getToManyRelationship` | `GET /v1/gameCenterDetails/{id}/relationships/leaderboardSetReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterEnabledVersions | `gameCenterEnabledVersions_compatibleVersions_getToManyRelated` | `GET /v1/gameCenterEnabledVersions/{id}/compatibleVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterEnabledVersions | `gameCenterEnabledVersions_compatibleVersions_getToManyRelationship` | `GET /v1/gameCenterEnabledVersions/{id}/relationships/compatibleVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_getCollection` | `GET /v1/gameCenterGroups` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_getInstance` | `GET /v1/gameCenterGroups/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterAchievements_getToManyRelated` | `GET /v1/gameCenterGroups/{id}/gameCenterAchievements` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterAchievementsV2_getToManyRelated` | `GET /v1/gameCenterGroups/{id}/gameCenterAchievementsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterActivities_getToManyRelated` | `GET /v1/gameCenterGroups/{id}/gameCenterActivities` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterChallenges_getToManyRelated` | `GET /v1/gameCenterGroups/{id}/gameCenterChallenges` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterDetails_getToManyRelated` | `GET /v1/gameCenterGroups/{id}/gameCenterDetails` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterLeaderboardSets_getToManyRelated` | `GET /v1/gameCenterGroups/{id}/gameCenterLeaderboardSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterLeaderboardSetsV2_getToManyRelated` | `GET /v1/gameCenterGroups/{id}/gameCenterLeaderboardSetsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterLeaderboards_getToManyRelated` | `GET /v1/gameCenterGroups/{id}/gameCenterLeaderboards` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterLeaderboardsV2_getToManyRelated` | `GET /v1/gameCenterGroups/{id}/gameCenterLeaderboardsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterAchievements_getToManyRelationship` | `GET /v1/gameCenterGroups/{id}/relationships/gameCenterAchievements` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterAchievementsV2_getToManyRelationship` | `GET /v1/gameCenterGroups/{id}/relationships/gameCenterAchievementsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterActivities_getToManyRelationship` | `GET /v1/gameCenterGroups/{id}/relationships/gameCenterActivities` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterChallenges_getToManyRelationship` | `GET /v1/gameCenterGroups/{id}/relationships/gameCenterChallenges` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterDetails_getToManyRelationship` | `GET /v1/gameCenterGroups/{id}/relationships/gameCenterDetails` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterLeaderboardSets_getToManyRelationship` | `GET /v1/gameCenterGroups/{id}/relationships/gameCenterLeaderboardSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterLeaderboardSetsV2_getToManyRelationship` | `GET /v1/gameCenterGroups/{id}/relationships/gameCenterLeaderboardSetsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterLeaderboards_getToManyRelationship` | `GET /v1/gameCenterGroups/{id}/relationships/gameCenterLeaderboards` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterLeaderboardsV2_getToManyRelationship` | `GET /v1/gameCenterGroups/{id}/relationships/gameCenterLeaderboardsV2` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardImages | `gameCenterLeaderboardImages_getInstance` | `GET /v1/gameCenterLeaderboardImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardImages | `gameCenterLeaderboardImagesV2_getInstance` | `GET /v2/gameCenterLeaderboardImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardLocalizations | `gameCenterLeaderboardLocalizations_getInstance` | `GET /v1/gameCenterLeaderboardLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardLocalizations | `gameCenterLeaderboardLocalizations_gameCenterLeaderboardImage_getToOneRelated` | `GET /v1/gameCenterLeaderboardLocalizations/{id}/gameCenterLeaderboardImage` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardLocalizations | `gameCenterLeaderboardLocalizations_gameCenterLeaderboardImage_getToOneRelationship` | `GET /v1/gameCenterLeaderboardLocalizations/{id}/relationships/gameCenterLeaderboardImage` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardLocalizations | `gameCenterLeaderboardLocalizationsV2_getInstance` | `GET /v2/gameCenterLeaderboardLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardLocalizations | `gameCenterLeaderboardLocalizationsV2_image_getToOneRelated` | `GET /v2/gameCenterLeaderboardLocalizations/{id}/image` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardLocalizations | `gameCenterLeaderboardLocalizationsV2_image_getToOneRelationship` | `GET /v2/gameCenterLeaderboardLocalizations/{id}/relationships/image` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardReleases | `gameCenterLeaderboardReleases_getInstance` | `GET /v1/gameCenterLeaderboardReleases/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetImages | `gameCenterLeaderboardSetImages_getInstance` | `GET /v1/gameCenterLeaderboardSetImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetImages | `gameCenterLeaderboardSetImagesV2_getInstance` | `GET /v2/gameCenterLeaderboardSetImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetLocalizations | `gameCenterLeaderboardSetLocalizations_getInstance` | `GET /v1/gameCenterLeaderboardSetLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetLocalizations | `gameCenterLeaderboardSetLocalizations_gameCenterLeaderboardSetImage_getToOneRelated` | `GET /v1/gameCenterLeaderboardSetLocalizations/{id}/gameCenterLeaderboardSetImage` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetLocalizations | `gameCenterLeaderboardSetLocalizations_gameCenterLeaderboardSetImage_getToOneRelationship` | `GET /v1/gameCenterLeaderboardSetLocalizations/{id}/relationships/gameCenterLeaderboardSetImage` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetLocalizations | `gameCenterLeaderboardSetLocalizationsV2_getInstance` | `GET /v2/gameCenterLeaderboardSetLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetLocalizations | `gameCenterLeaderboardSetLocalizationsV2_image_getToOneRelated` | `GET /v2/gameCenterLeaderboardSetLocalizations/{id}/image` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetLocalizations | `gameCenterLeaderboardSetLocalizationsV2_image_getToOneRelationship` | `GET /v2/gameCenterLeaderboardSetLocalizations/{id}/relationships/image` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetMemberLocalizations | `gameCenterLeaderboardSetMemberLocalizations_getCollection` | `GET /v1/gameCenterLeaderboardSetMemberLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetMemberLocalizations | `gameCenterLeaderboardSetMemberLocalizations_gameCenterLeaderboard_getToOneRelated` | `GET /v1/gameCenterLeaderboardSetMemberLocalizations/{id}/gameCenterLeaderboard` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetMemberLocalizations | `gameCenterLeaderboardSetMemberLocalizations_gameCenterLeaderboardSet_getToOneRelated` | `GET /v1/gameCenterLeaderboardSetMemberLocalizations/{id}/gameCenterLeaderboardSet` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetMemberLocalizations | `gameCenterLeaderboardSetMemberLocalizations_gameCenterLeaderboard_getToOneRelationship` | `GET /v1/gameCenterLeaderboardSetMemberLocalizations/{id}/relationships/gameCenterLeaderboard` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetMemberLocalizations | `gameCenterLeaderboardSetMemberLocalizations_gameCenterLeaderboardSet_getToOneRelationship` | `GET /v1/gameCenterLeaderboardSetMemberLocalizations/{id}/relationships/gameCenterLeaderboardSet` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetReleases | `gameCenterLeaderboardSetReleases_getInstance` | `GET /v1/gameCenterLeaderboardSetReleases/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetVersions | `gameCenterLeaderboardSetVersionsV2_getInstance` | `GET /v2/gameCenterLeaderboardSetVersions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetVersions | `gameCenterLeaderboardSetVersionsV2_localizations_getToManyRelated` | `GET /v2/gameCenterLeaderboardSetVersions/{id}/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetVersions | `gameCenterLeaderboardSetVersionsV2_localizations_getToManyRelationship` | `GET /v2/gameCenterLeaderboardSetVersions/{id}/relationships/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_getInstance` | `GET /v1/gameCenterLeaderboardSets/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_gameCenterLeaderboards_getToManyRelated` | `GET /v1/gameCenterLeaderboardSets/{id}/gameCenterLeaderboards` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_groupLeaderboardSet_getToOneRelated` | `GET /v1/gameCenterLeaderboardSets/{id}/groupLeaderboardSet` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_localizations_getToManyRelated` | `GET /v1/gameCenterLeaderboardSets/{id}/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_gameCenterLeaderboards_getToManyRelationship` | `GET /v1/gameCenterLeaderboardSets/{id}/relationships/gameCenterLeaderboards` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_groupLeaderboardSet_getToOneRelationship` | `GET /v1/gameCenterLeaderboardSets/{id}/relationships/groupLeaderboardSet` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_localizations_getToManyRelationship` | `GET /v1/gameCenterLeaderboardSets/{id}/relationships/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_releases_getToManyRelationship` | `GET /v1/gameCenterLeaderboardSets/{id}/relationships/releases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_releases_getToManyRelated` | `GET /v1/gameCenterLeaderboardSets/{id}/releases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSetsV2_getInstance` | `GET /v2/gameCenterLeaderboardSets/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSetsV2_gameCenterLeaderboards_getToManyRelated` | `GET /v2/gameCenterLeaderboardSets/{id}/gameCenterLeaderboards` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSetsV2_gameCenterLeaderboards_getToManyRelationship` | `GET /v2/gameCenterLeaderboardSets/{id}/relationships/gameCenterLeaderboards` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSetsV2_versions_getToManyRelationship` | `GET /v2/gameCenterLeaderboardSets/{id}/relationships/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSetsV2_versions_getToManyRelated` | `GET /v2/gameCenterLeaderboardSets/{id}/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardVersions | `gameCenterLeaderboardVersionsV2_getInstance` | `GET /v2/gameCenterLeaderboardVersions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardVersions | `gameCenterLeaderboardVersionsV2_localizations_getToManyRelated` | `GET /v2/gameCenterLeaderboardVersions/{id}/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardVersions | `gameCenterLeaderboardVersionsV2_localizations_getToManyRelationship` | `GET /v2/gameCenterLeaderboardVersions/{id}/relationships/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboards_getInstance` | `GET /v1/gameCenterLeaderboards/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboards_groupLeaderboard_getToOneRelated` | `GET /v1/gameCenterLeaderboards/{id}/groupLeaderboard` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboards_localizations_getToManyRelated` | `GET /v1/gameCenterLeaderboards/{id}/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboards_groupLeaderboard_getToOneRelationship` | `GET /v1/gameCenterLeaderboards/{id}/relationships/groupLeaderboard` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboards_localizations_getToManyRelationship` | `GET /v1/gameCenterLeaderboards/{id}/relationships/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboards_releases_getToManyRelationship` | `GET /v1/gameCenterLeaderboards/{id}/relationships/releases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboards_releases_getToManyRelated` | `GET /v1/gameCenterLeaderboards/{id}/releases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboardsV2_getInstance` | `GET /v2/gameCenterLeaderboards/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboardsV2_gameCenterScoreModerations_getToManyRelated` | `GET /v2/gameCenterLeaderboards/{id}/gameCenterScoreModerations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboardsV2_gameCenterScoreModerations_getToManyRelationship` | `GET /v2/gameCenterLeaderboards/{id}/relationships/gameCenterScoreModerations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboardsV2_versions_getToManyRelationship` | `GET /v2/gameCenterLeaderboards/{id}/relationships/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboardsV2_versions_getToManyRelated` | `GET /v2/gameCenterLeaderboards/{id}/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingQueues | `gameCenterMatchmakingQueues_getCollection` | `GET /v1/gameCenterMatchmakingQueues` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingQueues | `gameCenterMatchmakingQueues_getInstance` | `GET /v1/gameCenterMatchmakingQueues/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingQueues | `gameCenterMatchmakingQueues_experimentMatchmakingQueueSizes_getMetrics` | `GET /v1/gameCenterMatchmakingQueues/{id}/metrics/experimentMatchmakingQueueSizes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingQueues | `gameCenterMatchmakingQueues_experimentMatchmakingRequests_getMetrics` | `GET /v1/gameCenterMatchmakingQueues/{id}/metrics/experimentMatchmakingRequests` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingQueues | `gameCenterMatchmakingQueues_matchmakingQueueSizes_getMetrics` | `GET /v1/gameCenterMatchmakingQueues/{id}/metrics/matchmakingQueueSizes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingQueues | `gameCenterMatchmakingQueues_matchmakingRequests_getMetrics` | `GET /v1/gameCenterMatchmakingQueues/{id}/metrics/matchmakingRequests` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingQueues | `gameCenterMatchmakingQueues_matchmakingSessions_getMetrics` | `GET /v1/gameCenterMatchmakingQueues/{id}/metrics/matchmakingSessions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRuleSets | `gameCenterMatchmakingRuleSets_getCollection` | `GET /v1/gameCenterMatchmakingRuleSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRuleSets | `gameCenterMatchmakingRuleSets_getInstance` | `GET /v1/gameCenterMatchmakingRuleSets/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRuleSets | `gameCenterMatchmakingRuleSets_matchmakingQueues_getToManyRelated` | `GET /v1/gameCenterMatchmakingRuleSets/{id}/matchmakingQueues` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRuleSets | `gameCenterMatchmakingRuleSets_matchmakingQueues_getToManyRelationship` | `GET /v1/gameCenterMatchmakingRuleSets/{id}/relationships/matchmakingQueues` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRuleSets | `gameCenterMatchmakingRuleSets_rules_getToManyRelationship` | `GET /v1/gameCenterMatchmakingRuleSets/{id}/relationships/rules` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRuleSets | `gameCenterMatchmakingRuleSets_teams_getToManyRelationship` | `GET /v1/gameCenterMatchmakingRuleSets/{id}/relationships/teams` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRuleSets | `gameCenterMatchmakingRuleSets_rules_getToManyRelated` | `GET /v1/gameCenterMatchmakingRuleSets/{id}/rules` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRuleSets | `gameCenterMatchmakingRuleSets_teams_getToManyRelated` | `GET /v1/gameCenterMatchmakingRuleSets/{id}/teams` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRules | `gameCenterMatchmakingRules_matchmakingBooleanRuleResults_getMetrics` | `GET /v1/gameCenterMatchmakingRules/{id}/metrics/matchmakingBooleanRuleResults` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRules | `gameCenterMatchmakingRules_matchmakingNumberRuleResults_getMetrics` | `GET /v1/gameCenterMatchmakingRules/{id}/metrics/matchmakingNumberRuleResults` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRules | `gameCenterMatchmakingRules_matchmakingRuleErrors_getMetrics` | `GET /v1/gameCenterMatchmakingRules/{id}/metrics/matchmakingRuleErrors` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseAppStoreReviewScreenshots | `inAppPurchaseAppStoreReviewScreenshots_getInstance` | `GET /v1/inAppPurchaseAppStoreReviewScreenshots/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseAvailabilities | `inAppPurchaseAvailabilities_getInstance` | `GET /v1/inAppPurchaseAvailabilities/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseAvailabilities | `inAppPurchaseAvailabilities_availableTerritories_getToManyRelated` | `GET /v1/inAppPurchaseAvailabilities/{id}/availableTerritories` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseAvailabilities | `inAppPurchaseAvailabilities_availableTerritories_getToManyRelationship` | `GET /v1/inAppPurchaseAvailabilities/{id}/relationships/availableTerritories` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseContents | `inAppPurchaseContents_getInstance` | `GET /v1/inAppPurchaseContents/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseImages | `inAppPurchaseImages_getInstance` | `GET /v1/inAppPurchaseImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseImages | `inAppPurchaseImagesV2_getInstance` | `GET /v2/inAppPurchaseImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseLocalizations | `inAppPurchaseLocalizations_getInstance` | `GET /v1/inAppPurchaseLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseLocalizations | `inAppPurchaseLocalizationsV2_getInstance` | `GET /v2/inAppPurchaseLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseOfferCodeCustomCodes | `inAppPurchaseOfferCodeCustomCodes_getInstance` | `GET /v1/inAppPurchaseOfferCodeCustomCodes/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseOfferCodeOneTimeUseCodes | `inAppPurchaseOfferCodeOneTimeUseCodes_getInstance` | `GET /v1/inAppPurchaseOfferCodeOneTimeUseCodes/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseOfferCodeOneTimeUseCodes | `inAppPurchaseOfferCodeOneTimeUseCodes_values_getToOneRelated` | `GET /v1/inAppPurchaseOfferCodeOneTimeUseCodes/{id}/values` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseOfferCodes | `inAppPurchaseOfferCodes_getInstance` | `GET /v1/inAppPurchaseOfferCodes/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseOfferCodes | `inAppPurchaseOfferCodes_customCodes_getToManyRelated` | `GET /v1/inAppPurchaseOfferCodes/{id}/customCodes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseOfferCodes | `inAppPurchaseOfferCodes_oneTimeUseCodes_getToManyRelated` | `GET /v1/inAppPurchaseOfferCodes/{id}/oneTimeUseCodes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseOfferCodes | `inAppPurchaseOfferCodes_prices_getToManyRelated` | `GET /v1/inAppPurchaseOfferCodes/{id}/prices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseOfferCodes | `inAppPurchaseOfferCodes_customCodes_getToManyRelationship` | `GET /v1/inAppPurchaseOfferCodes/{id}/relationships/customCodes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseOfferCodes | `inAppPurchaseOfferCodes_oneTimeUseCodes_getToManyRelationship` | `GET /v1/inAppPurchaseOfferCodes/{id}/relationships/oneTimeUseCodes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseOfferCodes | `inAppPurchaseOfferCodes_prices_getToManyRelationship` | `GET /v1/inAppPurchaseOfferCodes/{id}/relationships/prices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchasePricePoints | `inAppPurchasePricePoints_equalizations_getToManyRelated` | `GET /v1/inAppPurchasePricePoints/{id}/equalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchasePricePoints | `inAppPurchasePricePoints_equalizations_getToManyRelationship` | `GET /v1/inAppPurchasePricePoints/{id}/relationships/equalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchasePriceSchedules | `inAppPurchasePriceSchedules_getInstance` | `GET /v1/inAppPurchasePriceSchedules/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchasePriceSchedules | `inAppPurchasePriceSchedules_automaticPrices_getToManyRelated` | `GET /v1/inAppPurchasePriceSchedules/{id}/automaticPrices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchasePriceSchedules | `inAppPurchasePriceSchedules_baseTerritory_getToOneRelated` | `GET /v1/inAppPurchasePriceSchedules/{id}/baseTerritory` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchasePriceSchedules | `inAppPurchasePriceSchedules_manualPrices_getToManyRelated` | `GET /v1/inAppPurchasePriceSchedules/{id}/manualPrices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchasePriceSchedules | `inAppPurchasePriceSchedules_automaticPrices_getToManyRelationship` | `GET /v1/inAppPurchasePriceSchedules/{id}/relationships/automaticPrices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchasePriceSchedules | `inAppPurchasePriceSchedules_baseTerritory_getToOneRelationship` | `GET /v1/inAppPurchasePriceSchedules/{id}/relationships/baseTerritory` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchasePriceSchedules | `inAppPurchasePriceSchedules_manualPrices_getToManyRelationship` | `GET /v1/inAppPurchasePriceSchedules/{id}/relationships/manualPrices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseVersions | `inAppPurchaseVersions_getInstance` | `GET /v1/inAppPurchaseVersions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseVersions | `inAppPurchaseVersions_image_getToOneRelated` | `GET /v1/inAppPurchaseVersions/{id}/image` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseVersions | `inAppPurchaseVersions_images_getToManyRelated` | `GET /v1/inAppPurchaseVersions/{id}/images` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseVersions | `inAppPurchaseVersions_localizations_getToManyRelated` | `GET /v1/inAppPurchaseVersions/{id}/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseVersions | `inAppPurchaseVersions_image_getToOneRelationship` | `GET /v1/inAppPurchaseVersions/{id}/relationships/image` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseVersions | `inAppPurchaseVersions_images_getToManyRelationship` | `GET /v1/inAppPurchaseVersions/{id}/relationships/images` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseVersions | `inAppPurchaseVersions_localizations_getToManyRelationship` | `GET /v1/inAppPurchaseVersions/{id}/relationships/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchases_getInstance` | `GET /v1/inAppPurchases/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_getInstance` | `GET /v2/inAppPurchases/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_appStoreReviewScreenshot_getToOneRelated` | `GET /v2/inAppPurchases/{id}/appStoreReviewScreenshot` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_content_getToOneRelated` | `GET /v2/inAppPurchases/{id}/content` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_iapPriceSchedule_getToOneRelated` | `GET /v2/inAppPurchases/{id}/iapPriceSchedule` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_images_getToManyRelated` | `GET /v2/inAppPurchases/{id}/images` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_inAppPurchaseAvailability_getToOneRelated` | `GET /v2/inAppPurchases/{id}/inAppPurchaseAvailability` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_inAppPurchaseLocalizations_getToManyRelated` | `GET /v2/inAppPurchases/{id}/inAppPurchaseLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_offerCodes_getToManyRelated` | `GET /v2/inAppPurchases/{id}/offerCodes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_pricePoints_getToManyRelated` | `GET /v2/inAppPurchases/{id}/pricePoints` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_promotedPurchase_getToOneRelated` | `GET /v2/inAppPurchases/{id}/promotedPurchase` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_appStoreReviewScreenshot_getToOneRelationship` | `GET /v2/inAppPurchases/{id}/relationships/appStoreReviewScreenshot` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_content_getToOneRelationship` | `GET /v2/inAppPurchases/{id}/relationships/content` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_iapPriceSchedule_getToOneRelationship` | `GET /v2/inAppPurchases/{id}/relationships/iapPriceSchedule` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_images_getToManyRelationship` | `GET /v2/inAppPurchases/{id}/relationships/images` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_inAppPurchaseAvailability_getToOneRelationship` | `GET /v2/inAppPurchases/{id}/relationships/inAppPurchaseAvailability` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_inAppPurchaseLocalizations_getToManyRelationship` | `GET /v2/inAppPurchases/{id}/relationships/inAppPurchaseLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_offerCodes_getToManyRelationship` | `GET /v2/inAppPurchases/{id}/relationships/offerCodes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_pricePoints_getToManyRelationship` | `GET /v2/inAppPurchases/{id}/relationships/pricePoints` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_promotedPurchase_getToOneRelationship` | `GET /v2/inAppPurchases/{id}/relationships/promotedPurchase` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_versions_getToManyRelationship` | `GET /v2/inAppPurchases/{id}/relationships/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchases | `inAppPurchasesV2_versions_getToManyRelated` | `GET /v2/inAppPurchases/{id}/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| MarketplaceWebhooks | `marketplaceWebhooks_getCollection` | `GET /v1/marketplaceWebhooks` |  | Account Holder, Admin, App Manager |  |
| MerchantIds | `merchantIds_getCollection` | `GET /v1/merchantIds` |  | Account Holder, Admin, App Manager, Developer |  |
| MerchantIds | `merchantIds_getInstance` | `GET /v1/merchantIds/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| MerchantIds | `merchantIds_certificates_getToManyRelated` | `GET /v1/merchantIds/{id}/certificates` |  | Account Holder, Admin, App Manager, Developer |  |
| MerchantIds | `merchantIds_certificates_getToManyRelationship` | `GET /v1/merchantIds/{id}/relationships/certificates` |  | Account Holder, Admin, App Manager, Developer |  |
| Nominations | `nominations_getCollection` | `GET /v1/nominations` |  | Account Holder, Admin, App Manager, Marketing |  |
| Nominations | `nominations_getInstance` | `GET /v1/nominations/{id}` |  | Account Holder, Admin, App Manager, Marketing |  |
| PassTypeIds | `passTypeIds_getCollection` | `GET /v1/passTypeIds` |  | Account Holder, Admin, App Manager, Developer |  |
| PassTypeIds | `passTypeIds_getInstance` | `GET /v1/passTypeIds/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| PassTypeIds | `passTypeIds_certificates_getToManyRelated` | `GET /v1/passTypeIds/{id}/certificates` |  | Account Holder, Admin, App Manager, Developer |  |
| PassTypeIds | `passTypeIds_certificates_getToManyRelationship` | `GET /v1/passTypeIds/{id}/relationships/certificates` |  | Account Holder, Admin, App Manager, Developer |  |
| PreReleaseVersions | `preReleaseVersions_getCollection` | `GET /v1/preReleaseVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| PreReleaseVersions | `preReleaseVersions_getInstance` | `GET /v1/preReleaseVersions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| PreReleaseVersions | `preReleaseVersions_app_getToOneRelated` | `GET /v1/preReleaseVersions/{id}/app` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| PreReleaseVersions | `preReleaseVersions_builds_getToManyRelated` | `GET /v1/preReleaseVersions/{id}/builds` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| PreReleaseVersions | `preReleaseVersions_app_getToOneRelationship` | `GET /v1/preReleaseVersions/{id}/relationships/app` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| PreReleaseVersions | `preReleaseVersions_builds_getToManyRelationship` | `GET /v1/preReleaseVersions/{id}/relationships/builds` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Profiles | `profiles_getCollection` | `GET /v1/profiles` |  | Account Holder, Admin, App Manager, Developer |  |
| Profiles | `profiles_getInstance` | `GET /v1/profiles/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| Profiles | `profiles_bundleId_getToOneRelated` | `GET /v1/profiles/{id}/bundleId` |  | Account Holder, Admin, App Manager, Developer |  |
| Profiles | `profiles_certificates_getToManyRelated` | `GET /v1/profiles/{id}/certificates` |  | Account Holder, Admin, App Manager, Developer |  |
| Profiles | `profiles_devices_getToManyRelated` | `GET /v1/profiles/{id}/devices` |  | Account Holder, Admin, App Manager, Developer |  |
| Profiles | `profiles_bundleId_getToOneRelationship` | `GET /v1/profiles/{id}/relationships/bundleId` |  | Account Holder, Admin, App Manager, Developer |  |
| Profiles | `profiles_certificates_getToManyRelationship` | `GET /v1/profiles/{id}/relationships/certificates` |  | Account Holder, Admin, App Manager, Developer |  |
| Profiles | `profiles_devices_getToManyRelationship` | `GET /v1/profiles/{id}/relationships/devices` |  | Account Holder, Admin, App Manager, Developer |  |
| PromotedPurchases | `promotedPurchases_getInstance` | `GET /v1/promotedPurchases/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionMessageLocalizations | `retentionMessageLocalizations_getInstance` | `GET /v1/retentionMessageLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionMessageVersions | `retentionMessageVersions_getInstance` | `GET /v1/retentionMessageVersions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionMessageVersions | `retentionMessageVersions_localizations_getToManyRelated` | `GET /v1/retentionMessageVersions/{id}/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionMessageVersions | `retentionMessageVersions_placement_getToOneRelated` | `GET /v1/retentionMessageVersions/{id}/placement` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionMessageVersions | `retentionMessageVersions_localizations_getToManyRelationship` | `GET /v1/retentionMessageVersions/{id}/relationships/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionMessageVersions | `retentionMessageVersions_placement_getToOneRelationship` | `GET /v1/retentionMessageVersions/{id}/relationships/placement` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionMessages | `retentionMessages_getInstance` | `GET /v1/retentionMessages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionMessages | `retentionMessages_subscriptions_getToManyRelationship` | `GET /v1/retentionMessages/{id}/relationships/subscriptions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionMessages | `retentionMessages_versions_getToManyRelationship` | `GET /v1/retentionMessages/{id}/relationships/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionMessages | `retentionMessages_subscriptions_getToManyRelated` | `GET /v1/retentionMessages/{id}/subscriptions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionMessages | `retentionMessages_versions_getToManyRelated` | `GET /v1/retentionMessages/{id}/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionOffers | `retentionOffers_getInstance` | `GET /v1/retentionOffers/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionOffers | `retentionOffers_prices_getToManyRelated` | `GET /v1/retentionOffers/{id}/prices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionOffers | `retentionOffers_prices_getToManyRelationship` | `GET /v1/retentionOffers/{id}/relationships/prices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| ReviewSubmissions | `reviewSubmissions_getCollection` | `GET /v1/reviewSubmissions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| ReviewSubmissions | `reviewSubmissions_getInstance` | `GET /v1/reviewSubmissions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| ReviewSubmissions | `reviewSubmissions_items_getToManyRelated` | `GET /v1/reviewSubmissions/{id}/items` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| ReviewSubmissions | `reviewSubmissions_items_getToManyRelationship` | `GET /v1/reviewSubmissions/{id}/relationships/items` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RoutingAppCoverages | `routingAppCoverages_getInstance` | `GET /v1/routingAppCoverages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support |  |
| SalesReports | `salesReports_getCollection` | `GET /v1/salesReports` |  | Account Holder, Admin, Finance, Sales, Access to Reports |  |
| SandboxTesters | `sandboxTestersV2_getCollection` | `GET /v2/sandboxTesters` |  | Account Holder, Admin, App Manager, Developer |  |
| ScmGitReferences | `scmGitReferences_getInstance` | `GET /v1/scmGitReferences/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| ScmProviders | `scmProviders_getCollection` | `GET /v1/scmProviders` |  | Account Holder, Admin, App Manager, Developer |  |
| ScmProviders | `scmProviders_getInstance` | `GET /v1/scmProviders/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| ScmProviders | `scmProviders_repositories_getToManyRelationship` | `GET /v1/scmProviders/{id}/relationships/repositories` |  | Account Holder, Admin, App Manager, Developer |  |
| ScmProviders | `scmProviders_repositories_getToManyRelated` | `GET /v1/scmProviders/{id}/repositories` |  | Account Holder, Admin, App Manager, Developer |  |
| ScmPullRequests | `scmPullRequests_getInstance` | `GET /v1/scmPullRequests/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| ScmRepositories | `scmRepositories_getCollection` | `GET /v1/scmRepositories` |  | Account Holder, Admin, App Manager, Developer |  |
| ScmRepositories | `scmRepositories_getInstance` | `GET /v1/scmRepositories/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| ScmRepositories | `scmRepositories_gitReferences_getToManyRelated` | `GET /v1/scmRepositories/{id}/gitReferences` |  | Account Holder, Admin, App Manager, Developer |  |
| ScmRepositories | `scmRepositories_pullRequests_getToManyRelated` | `GET /v1/scmRepositories/{id}/pullRequests` |  | Account Holder, Admin, App Manager, Developer |  |
| ScmRepositories | `scmRepositories_gitReferences_getToManyRelationship` | `GET /v1/scmRepositories/{id}/relationships/gitReferences` |  | Account Holder, Admin, App Manager, Developer |  |
| ScmRepositories | `scmRepositories_pullRequests_getToManyRelationship` | `GET /v1/scmRepositories/{id}/relationships/pullRequests` |  | Account Holder, Admin, App Manager, Developer |  |
| SubscriptionAppStoreReviewScreenshots | `subscriptionAppStoreReviewScreenshots_getInstance` | `GET /v1/subscriptionAppStoreReviewScreenshots/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionAvailabilities | `subscriptionAvailabilities_getInstance` | `GET /v1/subscriptionAvailabilities/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionAvailabilities | `subscriptionAvailabilities_availableTerritories_getToManyRelated` | `GET /v1/subscriptionAvailabilities/{id}/availableTerritories` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionAvailabilities | `subscriptionAvailabilities_availableTerritories_getToManyRelationship` | `GET /v1/subscriptionAvailabilities/{id}/relationships/availableTerritories` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGracePeriods | `subscriptionGracePeriods_getInstance` | `GET /v1/subscriptionGracePeriods/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGroupLocalizations | `subscriptionGroupLocalizations_getInstance` | `GET /v1/subscriptionGroupLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGroupLocalizations | `subscriptionGroupLocalizationsV2_getInstance` | `GET /v2/subscriptionGroupLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGroupVersions | `subscriptionGroupVersions_getInstance` | `GET /v1/subscriptionGroupVersions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGroupVersions | `subscriptionGroupVersions_localizations_getToManyRelated` | `GET /v1/subscriptionGroupVersions/{id}/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGroupVersions | `subscriptionGroupVersions_localizations_getToManyRelationship` | `GET /v1/subscriptionGroupVersions/{id}/relationships/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGroups | `subscriptionGroups_getInstance` | `GET /v1/subscriptionGroups/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGroups | `subscriptionGroups_subscriptionGroupLocalizations_getToManyRelationship` | `GET /v1/subscriptionGroups/{id}/relationships/subscriptionGroupLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGroups | `subscriptionGroups_subscriptions_getToManyRelationship` | `GET /v1/subscriptionGroups/{id}/relationships/subscriptions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGroups | `subscriptionGroups_versions_getToManyRelationship` | `GET /v1/subscriptionGroups/{id}/relationships/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGroups | `subscriptionGroups_subscriptionGroupLocalizations_getToManyRelated` | `GET /v1/subscriptionGroups/{id}/subscriptionGroupLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGroups | `subscriptionGroups_subscriptions_getToManyRelated` | `GET /v1/subscriptionGroups/{id}/subscriptions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGroups | `subscriptionGroups_versions_getToManyRelated` | `GET /v1/subscriptionGroups/{id}/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionImages | `subscriptionImages_getInstance` | `GET /v1/subscriptionImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionImages | `subscriptionImagesV2_getInstance` | `GET /v2/subscriptionImages/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionLocalizations | `subscriptionLocalizations_getInstance` | `GET /v1/subscriptionLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionLocalizations | `subscriptionLocalizationsV2_getInstance` | `GET /v2/subscriptionLocalizations/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionOfferCodeCustomCodes | `subscriptionOfferCodeCustomCodes_getInstance` | `GET /v1/subscriptionOfferCodeCustomCodes/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionOfferCodeOneTimeUseCodes | `subscriptionOfferCodeOneTimeUseCodes_getInstance` | `GET /v1/subscriptionOfferCodeOneTimeUseCodes/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionOfferCodeOneTimeUseCodes | `subscriptionOfferCodeOneTimeUseCodes_values_getToOneRelated` | `GET /v1/subscriptionOfferCodeOneTimeUseCodes/{id}/values` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionOfferCodes | `subscriptionOfferCodes_getInstance` | `GET /v1/subscriptionOfferCodes/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionOfferCodes | `subscriptionOfferCodes_customCodes_getToManyRelated` | `GET /v1/subscriptionOfferCodes/{id}/customCodes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionOfferCodes | `subscriptionOfferCodes_oneTimeUseCodes_getToManyRelated` | `GET /v1/subscriptionOfferCodes/{id}/oneTimeUseCodes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionOfferCodes | `subscriptionOfferCodes_prices_getToManyRelated` | `GET /v1/subscriptionOfferCodes/{id}/prices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionOfferCodes | `subscriptionOfferCodes_customCodes_getToManyRelationship` | `GET /v1/subscriptionOfferCodes/{id}/relationships/customCodes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionOfferCodes | `subscriptionOfferCodes_oneTimeUseCodes_getToManyRelationship` | `GET /v1/subscriptionOfferCodes/{id}/relationships/oneTimeUseCodes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionOfferCodes | `subscriptionOfferCodes_prices_getToManyRelationship` | `GET /v1/subscriptionOfferCodes/{id}/relationships/prices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionPlanAvailabilities | `subscriptionPlanAvailabilities_getInstance` | `GET /v1/subscriptionPlanAvailabilities/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionPlanAvailabilities | `subscriptionPlanAvailabilities_availableTerritories_getToManyRelated` | `GET /v1/subscriptionPlanAvailabilities/{id}/availableTerritories` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionPlanAvailabilities | `subscriptionPlanAvailabilities_availableTerritories_getToManyRelationship` | `GET /v1/subscriptionPlanAvailabilities/{id}/relationships/availableTerritories` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionPricePoints | `subscriptionPricePoints_getInstance` | `GET /v1/subscriptionPricePoints/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionPricePoints | `subscriptionPricePoints_adjustedEqualizations_getToManyRelated` | `GET /v1/subscriptionPricePoints/{id}/adjustedEqualizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionPricePoints | `subscriptionPricePoints_equalizations_getToManyRelated` | `GET /v1/subscriptionPricePoints/{id}/equalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionPricePoints | `subscriptionPricePoints_equalizations_getToManyRelationship` | `GET /v1/subscriptionPricePoints/{id}/relationships/equalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionPriceTiers | `subscriptionPriceTiers_getCollection` | `GET /v1/subscriptionPriceTiers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionPrices | `subscriptionPrices_additionalPriceTiers_getToManyRelated` | `GET /v1/subscriptionPrices/{id}/additionalPriceTiers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionPrices | `subscriptionPrices_additionalPriceTiers_getToManyRelationship` | `GET /v1/subscriptionPrices/{id}/relationships/additionalPriceTiers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionPromotionalOffers | `subscriptionPromotionalOffers_getInstance` | `GET /v1/subscriptionPromotionalOffers/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionPromotionalOffers | `subscriptionPromotionalOffers_prices_getToManyRelated` | `GET /v1/subscriptionPromotionalOffers/{id}/prices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionPromotionalOffers | `subscriptionPromotionalOffers_prices_getToManyRelationship` | `GET /v1/subscriptionPromotionalOffers/{id}/relationships/prices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionVersions | `subscriptionVersions_getInstance` | `GET /v1/subscriptionVersions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionVersions | `subscriptionVersions_image_getToOneRelated` | `GET /v1/subscriptionVersions/{id}/image` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionVersions | `subscriptionVersions_images_getToManyRelated` | `GET /v1/subscriptionVersions/{id}/images` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionVersions | `subscriptionVersions_localizations_getToManyRelated` | `GET /v1/subscriptionVersions/{id}/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionVersions | `subscriptionVersions_image_getToOneRelationship` | `GET /v1/subscriptionVersions/{id}/relationships/image` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionVersions | `subscriptionVersions_images_getToManyRelationship` | `GET /v1/subscriptionVersions/{id}/relationships/images` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionVersions | `subscriptionVersions_localizations_getToManyRelationship` | `GET /v1/subscriptionVersions/{id}/relationships/localizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_getInstance` | `GET /v1/subscriptions/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_appStoreReviewScreenshot_getToOneRelated` | `GET /v1/subscriptions/{id}/appStoreReviewScreenshot` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_images_getToManyRelated` | `GET /v1/subscriptions/{id}/images` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_introductoryOffers_getToManyRelated` | `GET /v1/subscriptions/{id}/introductoryOffers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_offerCodes_getToManyRelated` | `GET /v1/subscriptions/{id}/offerCodes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_planAvailabilities_getToManyRelated` | `GET /v1/subscriptions/{id}/planAvailabilities` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_pricePoints_getToManyRelated` | `GET /v1/subscriptions/{id}/pricePoints` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_prices_getToManyRelated` | `GET /v1/subscriptions/{id}/prices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_promotedPurchase_getToOneRelated` | `GET /v1/subscriptions/{id}/promotedPurchase` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_promotionalOffers_getToManyRelated` | `GET /v1/subscriptions/{id}/promotionalOffers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_appStoreReviewScreenshot_getToOneRelationship` | `GET /v1/subscriptions/{id}/relationships/appStoreReviewScreenshot` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_images_getToManyRelationship` | `GET /v1/subscriptions/{id}/relationships/images` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_introductoryOffers_getToManyRelationship` | `GET /v1/subscriptions/{id}/relationships/introductoryOffers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_offerCodes_getToManyRelationship` | `GET /v1/subscriptions/{id}/relationships/offerCodes` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_planAvailabilities_getToManyRelationship` | `GET /v1/subscriptions/{id}/relationships/planAvailabilities` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_pricePoints_getToManyRelationship` | `GET /v1/subscriptions/{id}/relationships/pricePoints` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_prices_getToManyRelationship` | `GET /v1/subscriptions/{id}/relationships/prices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_promotedPurchase_getToOneRelationship` | `GET /v1/subscriptions/{id}/relationships/promotedPurchase` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_promotionalOffers_getToManyRelationship` | `GET /v1/subscriptions/{id}/relationships/promotionalOffers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_retentionMessage_getToOneRelationship` | `GET /v1/subscriptions/{id}/relationships/retentionMessage` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_retentionMessageOffers_getToManyRelationship` | `GET /v1/subscriptions/{id}/relationships/retentionMessageOffers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_retentionOffers_getToManyRelationship` | `GET /v1/subscriptions/{id}/relationships/retentionOffers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_subscriptionAvailability_getToOneRelationship` | `GET /v1/subscriptions/{id}/relationships/subscriptionAvailability` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_subscriptionLocalizations_getToManyRelationship` | `GET /v1/subscriptions/{id}/relationships/subscriptionLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_versions_getToManyRelationship` | `GET /v1/subscriptions/{id}/relationships/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_winBackOffers_getToManyRelationship` | `GET /v1/subscriptions/{id}/relationships/winBackOffers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_retentionMessage_getToOneRelated` | `GET /v1/subscriptions/{id}/retentionMessage` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_retentionMessageOffers_getToManyRelated` | `GET /v1/subscriptions/{id}/retentionMessageOffers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_retentionOffers_getToManyRelated` | `GET /v1/subscriptions/{id}/retentionOffers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_subscriptionAvailability_getToOneRelated` | `GET /v1/subscriptions/{id}/subscriptionAvailability` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_subscriptionLocalizations_getToManyRelated` | `GET /v1/subscriptions/{id}/subscriptionLocalizations` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_versions_getToManyRelated` | `GET /v1/subscriptions/{id}/versions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Subscriptions | `subscriptions_winBackOffers_getToManyRelated` | `GET /v1/subscriptions/{id}/winBackOffers` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Territories | `territories_getCollection` | `GET /v1/territories` |  | Account Holder, Admin, App Manager, Developer, Marketing, Customer Support, Finance, Sales, Access to Reports |  |
| UserInvitations | `userInvitations_getCollection` | `GET /v1/userInvitations` |  | Account Holder, Admin, App Manager, Developer, Finance, Marketing, Sales, Customer Support |  |
| UserInvitations | `userInvitations_getInstance` | `GET /v1/userInvitations/{id}` |  | Account Holder, Admin, App Manager, Developer, Finance, Marketing, Sales, Customer Support |  |
| UserInvitations | `userInvitations_visibleApps_getToManyRelationship` | `GET /v1/userInvitations/{id}/relationships/visibleApps` |  | Account Holder, Admin, App Manager, Developer, Finance, Marketing, Sales, Customer Support |  |
| UserInvitations | `userInvitations_visibleApps_getToManyRelated` | `GET /v1/userInvitations/{id}/visibleApps` |  | Account Holder, Admin, App Manager, Developer, Finance, Marketing, Sales, Customer Support |  |
| Users | `users_getCollection` | `GET /v1/users` |  | Account Holder, Admin, App Manager, Developer, Finance, Marketing, Sales, Customer Support |  |
| Users | `users_getInstance` | `GET /v1/users/{id}` |  | Account Holder, Admin, App Manager, Developer, Finance, Marketing, Sales, Customer Support |  |
| Users | `users_visibleApps_getToManyRelationship` | `GET /v1/users/{id}/relationships/visibleApps` |  | Account Holder, Admin, App Manager, Developer, Finance, Marketing, Sales, Customer Support |  |
| Users | `users_visibleApps_getToManyRelated` | `GET /v1/users/{id}/visibleApps` |  | Account Holder, Admin, App Manager, Developer, Finance, Marketing, Sales, Customer Support |  |
| Webhooks | `webhooks_getInstance` | `GET /v1/webhooks/{id}` |  | Account Holder, Admin, App Manager |  |
| Webhooks | `webhooks_deliveries_getToManyRelated` | `GET /v1/webhooks/{id}/deliveries` |  | Account Holder, Admin, App Manager |  |
| Webhooks | `webhooks_deliveries_getToManyRelationship` | `GET /v1/webhooks/{id}/relationships/deliveries` |  | Account Holder, Admin, App Manager |  |
| WinBackOffers | `winBackOffers_getInstance` | `GET /v1/winBackOffers/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| WinBackOffers | `winBackOffers_prices_getToManyRelated` | `GET /v1/winBackOffers/{id}/prices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| WinBackOffers | `winBackOffers_prices_getToManyRelationship` | `GET /v1/winBackOffers/{id}/relationships/prices` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |

## operate

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| AccessibilityDeclarations | `accessibilityDeclarations_createInstance` | `POST /v1/accessibilityDeclarations` |  | Account Holder, Admin, App Manager, Marketing |  |
| AlternativeDistributionPackages | `alternativeDistributionPackages_createInstance` | `POST /v1/alternativeDistributionPackages` |  | Account Holder, Admin, App Manager |  |
| AppAssetLibraryImages | `appAssetLibraryImages_createInstance` | `POST /v1/appAssetLibraryImages` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppAssetLibraryImages | `appAssetLibraryImages_updateInstance` | `PATCH /v1/appAssetLibraryImages/{id}` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppAssetLibraryVideos | `appAssetLibraryVideos_createInstance` | `POST /v1/appAssetLibraryVideos` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppAssetLibraryVideos | `appAssetLibraryVideos_updateInstance` | `PATCH /v1/appAssetLibraryVideos/{id}` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppClipAppStoreReviewDetails | `appClipAppStoreReviewDetails_createInstance` | `POST /v1/appClipAppStoreReviewDetails` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppClipAppStoreReviewDetails | `appClipAppStoreReviewDetails_updateInstance` | `PATCH /v1/appClipAppStoreReviewDetails/{id}` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppCustomProductPageVersions | `appCustomProductPageVersions_createInstance` | `POST /v1/appCustomProductPageVersions` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppCustomProductPages | `appCustomProductPages_createInstance` | `POST /v1/appCustomProductPages` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppEncryptionDeclarationDocuments | `appEncryptionDeclarationDocuments_createInstance` | `POST /v1/appEncryptionDeclarationDocuments` |  | Account Holder, Admin, App Manager |  |
| AppEncryptionDeclarationDocuments | `appEncryptionDeclarationDocuments_updateInstance` | `PATCH /v1/appEncryptionDeclarationDocuments/{id}` |  | Account Holder, Admin, App Manager |  |
| AppEncryptionDeclarations | `appEncryptionDeclarations_builds_createToManyRelationship` | `POST /v1/appEncryptionDeclarations/{id}/relationships/builds` |  | Account Holder, Admin, App Manager |  |
| AppEvents | `appEvents_createInstance` | `POST /v1/appEvents` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppStoreReviewAttachments | `appStoreReviewAttachments_createInstance` | `POST /v1/appStoreReviewAttachments` |  | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppStoreReviewAttachments | `appStoreReviewAttachments_updateInstance` | `PATCH /v1/appStoreReviewAttachments/{id}` |  | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppStoreReviewDetails | `appStoreReviewDetails_createInstance` | `POST /v1/appStoreReviewDetails` |  | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppStoreReviewDetails | `appStoreReviewDetails_updateInstance` | `PATCH /v1/appStoreReviewDetails/{id}` |  | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppStoreVersionExperiments | `appStoreVersionExperiments_createInstance` | `POST /v1/appStoreVersionExperiments` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppStoreVersionExperiments | `appStoreVersionExperimentsV2_createInstance` | `POST /v2/appStoreVersionExperiments` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppStoreVersions | `appStoreVersions_createInstance` | `POST /v1/appStoreVersions` |  | Account Holder, Admin, App Manager, Marketing |  |
| BackgroundAssetUploadFiles | `backgroundAssetUploadFiles_createInstance` | `POST /v1/backgroundAssetUploadFiles` |  | Account Holder, Admin, App Manager, Developer |  |
| BackgroundAssetUploadFiles | `backgroundAssetUploadFiles_updateInstance` | `PATCH /v1/backgroundAssetUploadFiles/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| BackgroundAssetVersions | `backgroundAssetVersions_createInstance` | `POST /v1/backgroundAssetVersions` |  | Account Holder, Admin, App Manager, Developer |  |
| BackgroundAssets | `backgroundAssets_createInstance` | `POST /v1/backgroundAssets` |  | Account Holder, Admin, App Manager, Developer |  |
| BetaAppReviewDetails | `betaAppReviewDetails_updateInstance` | `PATCH /v1/betaAppReviewDetails/{id}` |  | Account Holder, Admin, App Manager |  |
| BuildBetaDetails | `buildBetaDetails_updateInstance` | `PATCH /v1/buildBetaDetails/{id}` |  | Account Holder, Admin, App Manager |  |
| Builds | `builds_appEncryptionDeclaration_updateToOneRelationship` | `PATCH /v1/builds/{id}/relationships/appEncryptionDeclaration` |  | Account Holder, Admin, App Manager |  |
| GameCenterAchievementVersions | `gameCenterAchievementVersionsV2_createInstance` | `POST /v2/gameCenterAchievementVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievements_createInstance` | `POST /v1/gameCenterAchievements` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievementsV2_createInstance` | `POST /v2/gameCenterAchievements` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivities | `gameCenterActivities_createInstance` | `POST /v1/gameCenterActivities` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityVersions | `gameCenterActivityVersions_createInstance` | `POST /v1/gameCenterActivityVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAppVersions | `gameCenterAppVersions_createInstance` | `POST /v1/gameCenterAppVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeVersions | `gameCenterChallengeVersions_createInstance` | `POST /v1/gameCenterChallengeVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallenges | `gameCenterChallenges_createInstance` | `POST /v1/gameCenterChallenges` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_createInstance` | `POST /v1/gameCenterDetails` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_createInstance` | `POST /v1/gameCenterGroups` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_updateInstance` | `PATCH /v1/gameCenterGroups/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetVersions | `gameCenterLeaderboardSetVersionsV2_createInstance` | `POST /v2/gameCenterLeaderboardSetVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_createInstance` | `POST /v1/gameCenterLeaderboardSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_updateInstance` | `PATCH /v1/gameCenterLeaderboardSets/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSetsV2_createInstance` | `POST /v2/gameCenterLeaderboardSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSetsV2_updateInstance` | `PATCH /v2/gameCenterLeaderboardSets/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardVersions | `gameCenterLeaderboardVersionsV2_createInstance` | `POST /v2/gameCenterLeaderboardVersions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboards_createInstance` | `POST /v1/gameCenterLeaderboards` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboardsV2_createInstance` | `POST /v2/gameCenterLeaderboards` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingQueues | `gameCenterMatchmakingQueues_createInstance` | `POST /v1/gameCenterMatchmakingQueues` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRuleSetTests | `gameCenterMatchmakingRuleSetTests_createInstance` | `POST /v1/gameCenterMatchmakingRuleSetTests` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRuleSets | `gameCenterMatchmakingRuleSets_createInstance` | `POST /v1/gameCenterMatchmakingRuleSets` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRules | `gameCenterMatchmakingRules_createInstance` | `POST /v1/gameCenterMatchmakingRules` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingTeams | `gameCenterMatchmakingTeams_createInstance` | `POST /v1/gameCenterMatchmakingTeams` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseAppStoreReviewScreenshots | `inAppPurchaseAppStoreReviewScreenshots_createInstance` | `POST /v1/inAppPurchaseAppStoreReviewScreenshots` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseAppStoreReviewScreenshots | `inAppPurchaseAppStoreReviewScreenshots_updateInstance` | `PATCH /v1/inAppPurchaseAppStoreReviewScreenshots/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseVersions | `inAppPurchaseVersions_createInstance` | `POST /v1/inAppPurchaseVersions` |  | Account Holder, Admin, App Manager |  |
| InAppPurchases | `inAppPurchasesV2_createInstance` | `POST /v2/inAppPurchases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| RetentionMessageVersions | `retentionMessageVersions_createInstance` | `POST /v1/retentionMessageVersions` |  | Account Holder, Admin, App Manager |  |
| RetentionMessages | `retentionMessages_createInstance` | `POST /v1/retentionMessages` |  | Account Holder, Admin, App Manager |  |
| ReviewSubmissionItems | `reviewSubmissionItems_createInstance` | `POST /v1/reviewSubmissionItems` |  | Account Holder, Admin, App Manager, Marketing |  |
| ReviewSubmissions | `reviewSubmissions_createInstance` | `POST /v1/reviewSubmissions` |  | Account Holder, Admin, App Manager, Marketing |  |
| RoutingAppCoverages | `routingAppCoverages_createInstance` | `POST /v1/routingAppCoverages` |  | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| RoutingAppCoverages | `routingAppCoverages_updateInstance` | `PATCH /v1/routingAppCoverages/{id}` |  | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| SubscriptionAppStoreReviewScreenshots | `subscriptionAppStoreReviewScreenshots_createInstance` | `POST /v1/subscriptionAppStoreReviewScreenshots` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionAppStoreReviewScreenshots | `subscriptionAppStoreReviewScreenshots_updateInstance` | `PATCH /v1/subscriptionAppStoreReviewScreenshots/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGroupVersions | `subscriptionGroupVersions_createInstance` | `POST /v1/subscriptionGroupVersions` |  | Account Holder, Admin, App Manager |  |
| SubscriptionGroups | `subscriptionGroups_createInstance` | `POST /v1/subscriptionGroups` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionGroups | `subscriptionGroups_updateInstance` | `PATCH /v1/subscriptionGroups/{id}` |  | Account Holder, Admin, App Manager |  |
| SubscriptionPriceChangePreviews | `subscriptionPriceChangePreviews_createInstance` | `POST /v1/subscriptionPriceChangePreviews` |  | Account Holder, Admin, App Manager |  |
| SubscriptionPriceTierEqualizations | `subscriptionPriceTierEqualizations_createInstance` | `POST /v1/subscriptionPriceTierEqualizations` |  | Account Holder, Admin, App Manager |  |
| SubscriptionVersions | `subscriptionVersions_createInstance` | `POST /v1/subscriptionVersions` |  | Account Holder, Admin, App Manager |  |
| Subscriptions | `subscriptions_createInstance` | `POST /v1/subscriptions` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| WebhookDeliveries | `webhookDeliveries_createInstance` | `POST /v1/webhookDeliveries` |  | Account Holder, Admin, App Manager |  |
| WebhookPings | `webhookPings_createInstance` | `POST /v1/webhookPings` |  | Account Holder, Admin, App Manager |  |

## public

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| AccessibilityDeclarations | `accessibilityDeclarations_updateInstance` | `PATCH /v1/accessibilityDeclarations/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AgeRatingDeclarations | `ageRatingDeclarations_updateInstance` | `PATCH /v1/ageRatingDeclarations/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppAssetLibraryPlacementOrderingRequests | `appAssetLibraryPlacementOrderingRequests_createInstance` | `POST /v1/appAssetLibraryPlacementOrderingRequests` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppAssetLibraryPlacements | `appAssetLibraryPlacements_createInstance` | `POST /v1/appAssetLibraryPlacements` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppAssetLibraryPlacements | `appAssetLibraryPlacements_deleteInstance` | `DELETE /v1/appAssetLibraryPlacements/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppAvailabilities | `appAvailabilitiesV2_createInstance` | `POST /v2/appAvailabilities` |  | Account Holder, Admin, App Manager |  |
| AppClipAdvancedExperienceImages | `appClipAdvancedExperienceImages_createInstance` | `POST /v1/appClipAdvancedExperienceImages` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppClipAdvancedExperienceImages | `appClipAdvancedExperienceImages_updateInstance` | `PATCH /v1/appClipAdvancedExperienceImages/{id}` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppClipAdvancedExperiences | `appClipAdvancedExperiences_createInstance` | `POST /v1/appClipAdvancedExperiences` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppClipAdvancedExperiences | `appClipAdvancedExperiences_updateInstance` | `PATCH /v1/appClipAdvancedExperiences/{id}` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppClipDefaultExperienceLocalizations | `appClipDefaultExperienceLocalizations_createInstance` | `POST /v1/appClipDefaultExperienceLocalizations` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppClipDefaultExperienceLocalizations | `appClipDefaultExperienceLocalizations_updateInstance` | `PATCH /v1/appClipDefaultExperienceLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppClipDefaultExperiences | `appClipDefaultExperiences_createInstance` | `POST /v1/appClipDefaultExperiences` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppClipDefaultExperiences | `appClipDefaultExperiences_updateInstance` | `PATCH /v1/appClipDefaultExperiences/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppClipDefaultExperiences | `appClipDefaultExperiences_releaseWithAppStoreVersion_updateToOneRelationship` | `PATCH /v1/appClipDefaultExperiences/{id}/relationships/releaseWithAppStoreVersion` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppClipHeaderImages | `appClipHeaderImages_createInstance` | `POST /v1/appClipHeaderImages` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppClipHeaderImages | `appClipHeaderImages_updateInstance` | `PATCH /v1/appClipHeaderImages/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppCustomProductPageLocalizations | `appCustomProductPageLocalizations_createInstance` | `POST /v1/appCustomProductPageLocalizations` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppCustomProductPageLocalizations | `appCustomProductPageLocalizations_updateInstance` | `PATCH /v1/appCustomProductPageLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppCustomProductPageLocalizations | `appCustomProductPageLocalizations_searchKeywords_deleteToManyRelationship` | `DELETE /v1/appCustomProductPageLocalizations/{id}/relationships/searchKeywords` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppCustomProductPageLocalizations | `appCustomProductPageLocalizations_searchKeywords_createToManyRelationship` | `POST /v1/appCustomProductPageLocalizations/{id}/relationships/searchKeywords` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppCustomProductPageVersions | `appCustomProductPageVersions_updateInstance` | `PATCH /v1/appCustomProductPageVersions/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppCustomProductPages | `appCustomProductPages_updateInstance` | `PATCH /v1/appCustomProductPages/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppEventLocalizations | `appEventLocalizations_createInstance` | `POST /v1/appEventLocalizations` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppEventLocalizations | `appEventLocalizations_updateInstance` | `PATCH /v1/appEventLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppEventScreenshots | `appEventScreenshots_createInstance` | `POST /v1/appEventScreenshots` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppEventScreenshots | `appEventScreenshots_updateInstance` | `PATCH /v1/appEventScreenshots/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppEventVideoClips | `appEventVideoClips_createInstance` | `POST /v1/appEventVideoClips` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppEventVideoClips | `appEventVideoClips_updateInstance` | `PATCH /v1/appEventVideoClips/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppEvents | `appEvents_updateInstance` | `PATCH /v1/appEvents/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppInfoLocalizations | `appInfoLocalizations_createInstance` | `POST /v1/appInfoLocalizations` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppInfoLocalizations | `appInfoLocalizations_updateInstance` | `PATCH /v1/appInfoLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppInfos | `appInfos_updateInstance` | `PATCH /v1/appInfos/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppPreviewSets | `appPreviewSets_createInstance` | `POST /v1/appPreviewSets` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppPreviewSets | `appPreviewSets_appPreviews_replaceToManyRelationship` | `PATCH /v1/appPreviewSets/{id}/relationships/appPreviews` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppPreviews | `appPreviews_createInstance` | `POST /v1/appPreviews` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppPreviews | `appPreviews_updateInstance` | `PATCH /v1/appPreviews/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppPriceSchedules | `appPriceSchedules_createInstance` | `POST /v1/appPriceSchedules` |  | Account Holder, Admin, App Manager |  |
| AppScreenshotSets | `appScreenshotSets_createInstance` | `POST /v1/appScreenshotSets` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppScreenshotSets | `appScreenshotSets_appScreenshots_replaceToManyRelationship` | `PATCH /v1/appScreenshotSets/{id}/relationships/appScreenshots` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppScreenshots | `appScreenshots_createInstance` | `POST /v1/appScreenshots` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppScreenshots | `appScreenshots_updateInstance` | `PATCH /v1/appScreenshots/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppStoreVersionExperimentTreatmentLocalizations | `appStoreVersionExperimentTreatmentLocalizations_createInstance` | `POST /v1/appStoreVersionExperimentTreatmentLocalizations` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppStoreVersionExperimentTreatments | `appStoreVersionExperimentTreatments_createInstance` | `POST /v1/appStoreVersionExperimentTreatments` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppStoreVersionExperimentTreatments | `appStoreVersionExperimentTreatments_updateInstance` | `PATCH /v1/appStoreVersionExperimentTreatments/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppStoreVersionExperiments | `appStoreVersionExperiments_updateInstance` | `PATCH /v1/appStoreVersionExperiments/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppStoreVersionExperiments | `appStoreVersionExperimentsV2_updateInstance` | `PATCH /v2/appStoreVersionExperiments/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppStoreVersionLocalizations | `appStoreVersionLocalizations_createInstance` | `POST /v1/appStoreVersionLocalizations` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppStoreVersionLocalizations | `appStoreVersionLocalizations_updateInstance` | `PATCH /v1/appStoreVersionLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppStoreVersionLocalizations | `appStoreVersionLocalizations_searchKeywords_deleteToManyRelationship` | `DELETE /v1/appStoreVersionLocalizations/{id}/relationships/searchKeywords` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppStoreVersionLocalizations | `appStoreVersionLocalizations_searchKeywords_createToManyRelationship` | `POST /v1/appStoreVersionLocalizations/{id}/relationships/searchKeywords` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppStoreVersionPhasedReleases | `appStoreVersionPhasedReleases_createInstance` | `POST /v1/appStoreVersionPhasedReleases` | draft: operate | Account Holder, Admin, App Manager |  |
| AppStoreVersionPhasedReleases | `appStoreVersionPhasedReleases_deleteInstance` | `DELETE /v1/appStoreVersionPhasedReleases/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| AppStoreVersionPhasedReleases | `appStoreVersionPhasedReleases_updateInstance` | `PATCH /v1/appStoreVersionPhasedReleases/{id}` | draft: operate | Account Holder, Admin, App Manager |  |
| AppStoreVersionPromotions | `appStoreVersionPromotions_createInstance` | `POST /v1/appStoreVersionPromotions` |  | Account Holder, Admin, App Manager, Marketing |  |
| AppStoreVersionReleaseRequests | `appStoreVersionReleaseRequests_createInstance` | `POST /v1/appStoreVersionReleaseRequests` |  | Account Holder, Admin, App Manager |  |
| AppStoreVersionSubmissions | `appStoreVersionSubmissions_deleteInstance` | `DELETE /v1/appStoreVersionSubmissions/{id}` |  | Account Holder, Admin, App Manager |  |
| AppStoreVersions | `appStoreVersions_updateInstance` | `PATCH /v1/appStoreVersions/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| AppStoreVersions | `appStoreVersions_appClipDefaultExperience_updateToOneRelationship` | `PATCH /v1/appStoreVersions/{id}/relationships/appClipDefaultExperience` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| AppStoreVersions | `appStoreVersions_build_updateToOneRelationship` | `PATCH /v1/appStoreVersions/{id}/relationships/build` | draft: operate | Account Holder, Admin, App Manager |  |
| AppTags | `appTags_updateInstance` | `PATCH /v1/appTags/{id}` |  | Account Holder, Admin, App Manager, Marketing, Customer Support |  |
| Apps | `apps_betaTesters_deleteToManyRelationship` | `DELETE /v1/apps/{id}/relationships/betaTesters` | internal: operate | Account Holder, Admin, App Manager |  |
| Apps | `apps_promotedPurchases_replaceToManyRelationship` | `PATCH /v1/apps/{id}/relationships/promotedPurchases` |  | Account Holder, Admin, App Manager |  |
| BackgroundAssets | `backgroundAssets_updateInstance` | `PATCH /v1/backgroundAssets/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer |  |
| BetaAppClipInvocationLocalizations | `betaAppClipInvocationLocalizations_createInstance` | `POST /v1/betaAppClipInvocationLocalizations` | internal: operate | Account Holder, Admin, App Manager |  |
| BetaAppClipInvocationLocalizations | `betaAppClipInvocationLocalizations_updateInstance` | `PATCH /v1/betaAppClipInvocationLocalizations/{id}` | internal: operate | Account Holder, Admin, App Manager |  |
| BetaAppClipInvocations | `betaAppClipInvocations_createInstance` | `POST /v1/betaAppClipInvocations` | internal: operate | Account Holder, Admin, App Manager |  |
| BetaAppClipInvocations | `betaAppClipInvocations_updateInstance` | `PATCH /v1/betaAppClipInvocations/{id}` | internal: operate | Account Holder, Admin, App Manager |  |
| BetaAppLocalizations | `betaAppLocalizations_createInstance` | `POST /v1/betaAppLocalizations` | internal: operate | Account Holder, Admin, App Manager |  |
| BetaAppLocalizations | `betaAppLocalizations_updateInstance` | `PATCH /v1/betaAppLocalizations/{id}` | internal: operate | Account Holder, Admin, App Manager |  |
| BetaAppReviewSubmissions | `betaAppReviewSubmissions_createInstance` | `POST /v1/betaAppReviewSubmissions` |  | Account Holder, Admin, App Manager |  |
| BetaBuildLocalizations | `betaBuildLocalizations_createInstance` | `POST /v1/betaBuildLocalizations` | internal: operate | Account Holder, Admin, App Manager |  |
| BetaBuildLocalizations | `betaBuildLocalizations_updateInstance` | `PATCH /v1/betaBuildLocalizations/{id}` | internal: operate | Account Holder, Admin, App Manager |  |
| BetaGroups | `betaGroups_createInstance` | `POST /v1/betaGroups` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_updateInstance` | `PATCH /v1/betaGroups/{id}` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_betaTesters_deleteToManyRelationship` | `DELETE /v1/betaGroups/{id}/relationships/betaTesters` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_betaTesters_createToManyRelationship` | `POST /v1/betaGroups/{id}/relationships/betaTesters` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_builds_deleteToManyRelationship` | `DELETE /v1/betaGroups/{id}/relationships/builds` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaGroups | `betaGroups_builds_createToManyRelationship` | `POST /v1/betaGroups/{id}/relationships/builds` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaLicenseAgreements | `betaLicenseAgreements_updateInstance` | `PATCH /v1/betaLicenseAgreements/{id}` | internal: operate | Account Holder, Admin, App Manager |  |
| BetaRecruitmentCriteria | `betaRecruitmentCriteria_createInstance` | `POST /v1/betaRecruitmentCriteria` |  | Account Holder, Admin, App Manager |  |
| BetaRecruitmentCriteria | `betaRecruitmentCriteria_deleteInstance` | `DELETE /v1/betaRecruitmentCriteria/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| BetaRecruitmentCriteria | `betaRecruitmentCriteria_updateInstance` | `PATCH /v1/betaRecruitmentCriteria/{id}` |  | Account Holder, Admin, App Manager |  |
| BetaTesterInvitations | `betaTesterInvitations_createInstance` | `POST /v1/betaTesterInvitations` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_createInstance` | `POST /v1/betaTesters` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_apps_deleteToManyRelationship` | `DELETE /v1/betaTesters/{id}/relationships/apps` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_betaGroups_deleteToManyRelationship` | `DELETE /v1/betaTesters/{id}/relationships/betaGroups` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_betaGroups_createToManyRelationship` | `POST /v1/betaTesters/{id}/relationships/betaGroups` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_builds_deleteToManyRelationship` | `DELETE /v1/betaTesters/{id}/relationships/builds` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BetaTesters | `betaTesters_builds_createToManyRelationship` | `POST /v1/betaTesters/{id}/relationships/builds` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| BuildBetaNotifications | `buildBetaNotifications_createInstance` | `POST /v1/buildBetaNotifications` | internal: operate | Account Holder, Admin, App Manager |  |
| Builds | `builds_updateInstance` | `PATCH /v1/builds/{id}` |  | Account Holder, Admin, App Manager |  |
| Builds | `builds_betaGroups_deleteToManyRelationship` | `DELETE /v1/builds/{id}/relationships/betaGroups` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_betaGroups_createToManyRelationship` | `POST /v1/builds/{id}/relationships/betaGroups` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_individualTesters_deleteToManyRelationship` | `DELETE /v1/builds/{id}/relationships/individualTesters` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| Builds | `builds_individualTesters_createToManyRelationship` | `POST /v1/builds/{id}/relationships/individualTesters` | internal: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| CiBuildRuns | `ciBuildRuns_createInstance` | `POST /v1/ciBuildRuns` | internal: operate | Account Holder, Admin, App Manager |  |
| CiWorkflows | `ciWorkflows_createInstance` | `POST /v1/ciWorkflows` | internal: operate | Account Holder, Admin, App Manager |  |
| CiWorkflows | `ciWorkflows_updateInstance` | `PATCH /v1/ciWorkflows/{id}` | internal: operate | Account Holder, Admin, App Manager |  |
| CustomerReviewResponses | `customerReviewResponses_createInstance` | `POST /v1/customerReviewResponses` |  | Account Holder, Admin, Customer Support |  |
| CustomerReviewResponses | `customerReviewResponses_deleteInstance` | `DELETE /v1/customerReviewResponses/{id}` |  | Account Holder, Admin, Customer Support | cannot be undone |
| EndAppAvailabilityPreOrders | `endAppAvailabilityPreOrders_createInstance` | `POST /v1/endAppAvailabilityPreOrders` |  | Account Holder, Admin, App Manager |  |
| EndUserLicenseAgreements | `endUserLicenseAgreements_createInstance` | `POST /v1/endUserLicenseAgreements` |  | Account Holder, Admin, App Manager |  |
| EndUserLicenseAgreements | `endUserLicenseAgreements_deleteInstance` | `DELETE /v1/endUserLicenseAgreements/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| EndUserLicenseAgreements | `endUserLicenseAgreements_updateInstance` | `PATCH /v1/endUserLicenseAgreements/{id}` |  | Account Holder, Admin, App Manager |  |
| GameCenterAchievementImages | `gameCenterAchievementImages_createInstance` | `POST /v1/gameCenterAchievementImages` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementImages | `gameCenterAchievementImages_updateInstance` | `PATCH /v1/gameCenterAchievementImages/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementImages | `gameCenterAchievementImagesV2_createInstance` | `POST /v2/gameCenterAchievementImages` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementImages | `gameCenterAchievementImagesV2_updateInstance` | `PATCH /v2/gameCenterAchievementImages/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementLocalizations | `gameCenterAchievementLocalizations_createInstance` | `POST /v1/gameCenterAchievementLocalizations` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementLocalizations | `gameCenterAchievementLocalizations_updateInstance` | `PATCH /v1/gameCenterAchievementLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementLocalizations | `gameCenterAchievementLocalizationsV2_createInstance` | `POST /v2/gameCenterAchievementLocalizations` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementLocalizations | `gameCenterAchievementLocalizationsV2_updateInstance` | `PATCH /v2/gameCenterAchievementLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementReleases | `gameCenterAchievementReleases_createInstance` | `POST /v1/gameCenterAchievementReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievementReleases | `gameCenterAchievementReleases_deleteInstance` | `DELETE /v1/gameCenterAchievementReleases/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterAchievements | `gameCenterAchievements_updateInstance` | `PATCH /v1/gameCenterAchievements/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievements_activity_updateToOneRelationship` | `PATCH /v1/gameCenterAchievements/{id}/relationships/activity` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievements_groupAchievement_updateToOneRelationship` | `PATCH /v1/gameCenterAchievements/{id}/relationships/groupAchievement` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievementsV2_updateInstance` | `PATCH /v2/gameCenterAchievements/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAchievements | `gameCenterAchievementsV2_activity_updateToOneRelationship` | `PATCH /v2/gameCenterAchievements/{id}/relationships/activity` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivities | `gameCenterActivities_updateInstance` | `PATCH /v1/gameCenterActivities/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivities | `gameCenterActivities_achievements_deleteToManyRelationship` | `DELETE /v1/gameCenterActivities/{id}/relationships/achievements` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivities | `gameCenterActivities_achievements_createToManyRelationship` | `POST /v1/gameCenterActivities/{id}/relationships/achievements` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivities | `gameCenterActivities_achievementsV2_deleteToManyRelationship` | `DELETE /v1/gameCenterActivities/{id}/relationships/achievementsV2` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivities | `gameCenterActivities_achievementsV2_createToManyRelationship` | `POST /v1/gameCenterActivities/{id}/relationships/achievementsV2` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivities | `gameCenterActivities_leaderboards_deleteToManyRelationship` | `DELETE /v1/gameCenterActivities/{id}/relationships/leaderboards` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivities | `gameCenterActivities_leaderboards_createToManyRelationship` | `POST /v1/gameCenterActivities/{id}/relationships/leaderboards` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivities | `gameCenterActivities_leaderboardsV2_deleteToManyRelationship` | `DELETE /v1/gameCenterActivities/{id}/relationships/leaderboardsV2` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivities | `gameCenterActivities_leaderboardsV2_createToManyRelationship` | `POST /v1/gameCenterActivities/{id}/relationships/leaderboardsV2` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityImages | `gameCenterActivityImages_createInstance` | `POST /v1/gameCenterActivityImages` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityImages | `gameCenterActivityImages_updateInstance` | `PATCH /v1/gameCenterActivityImages/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityLocalizations | `gameCenterActivityLocalizations_createInstance` | `POST /v1/gameCenterActivityLocalizations` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityLocalizations | `gameCenterActivityLocalizations_updateInstance` | `PATCH /v1/gameCenterActivityLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityVersionReleases | `gameCenterActivityVersionReleases_createInstance` | `POST /v1/gameCenterActivityVersionReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterActivityVersionReleases | `gameCenterActivityVersionReleases_deleteInstance` | `DELETE /v1/gameCenterActivityVersionReleases/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterActivityVersions | `gameCenterActivityVersions_updateInstance` | `PATCH /v1/gameCenterActivityVersions/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAppVersions | `gameCenterAppVersions_updateInstance` | `PATCH /v1/gameCenterAppVersions/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAppVersions | `gameCenterAppVersions_compatibilityVersions_deleteToManyRelationship` | `DELETE /v1/gameCenterAppVersions/{id}/relationships/compatibilityVersions` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterAppVersions | `gameCenterAppVersions_compatibilityVersions_createToManyRelationship` | `POST /v1/gameCenterAppVersions/{id}/relationships/compatibilityVersions` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeImages | `gameCenterChallengeImages_createInstance` | `POST /v1/gameCenterChallengeImages` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeImages | `gameCenterChallengeImages_updateInstance` | `PATCH /v1/gameCenterChallengeImages/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeLocalizations | `gameCenterChallengeLocalizations_createInstance` | `POST /v1/gameCenterChallengeLocalizations` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeLocalizations | `gameCenterChallengeLocalizations_updateInstance` | `PATCH /v1/gameCenterChallengeLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeVersionReleases | `gameCenterChallengeVersionReleases_createInstance` | `POST /v1/gameCenterChallengeVersionReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallengeVersionReleases | `gameCenterChallengeVersionReleases_deleteInstance` | `DELETE /v1/gameCenterChallengeVersionReleases/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterChallenges | `gameCenterChallenges_updateInstance` | `PATCH /v1/gameCenterChallenges/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallenges | `gameCenterChallenges_leaderboard_updateToOneRelationship` | `PATCH /v1/gameCenterChallenges/{id}/relationships/leaderboard` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterChallenges | `gameCenterChallenges_leaderboardV2_updateToOneRelationship` | `PATCH /v1/gameCenterChallenges/{id}/relationships/leaderboardV2` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetailPlayers | `gameCenterDetailPlayers_updateInstance` | `PATCH /v1/gameCenterDetailPlayers/{id}` |  | Account Holder, Admin, App Manager |  |
| GameCenterDetails | `gameCenterDetails_updateInstance` | `PATCH /v1/gameCenterDetails/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_challengesMinimumPlatformVersions_replaceToManyRelationship` | `PATCH /v1/gameCenterDetails/{id}/relationships/challengesMinimumPlatformVersions` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterAchievements_replaceToManyRelationship` | `PATCH /v1/gameCenterDetails/{id}/relationships/gameCenterAchievements` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterAchievementsV2_replaceToManyRelationship` | `PATCH /v1/gameCenterDetails/{id}/relationships/gameCenterAchievementsV2` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterLeaderboardSets_replaceToManyRelationship` | `PATCH /v1/gameCenterDetails/{id}/relationships/gameCenterLeaderboardSets` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterLeaderboardSetsV2_replaceToManyRelationship` | `PATCH /v1/gameCenterDetails/{id}/relationships/gameCenterLeaderboardSetsV2` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterLeaderboards_replaceToManyRelationship` | `PATCH /v1/gameCenterDetails/{id}/relationships/gameCenterLeaderboards` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterDetails | `gameCenterDetails_gameCenterLeaderboardsV2_replaceToManyRelationship` | `PATCH /v1/gameCenterDetails/{id}/relationships/gameCenterLeaderboardsV2` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterEnabledVersions | `gameCenterEnabledVersions_compatibleVersions_deleteToManyRelationship` | `DELETE /v1/gameCenterEnabledVersions/{id}/relationships/compatibleVersions` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterEnabledVersions | `gameCenterEnabledVersions_compatibleVersions_replaceToManyRelationship` | `PATCH /v1/gameCenterEnabledVersions/{id}/relationships/compatibleVersions` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterEnabledVersions | `gameCenterEnabledVersions_compatibleVersions_createToManyRelationship` | `POST /v1/gameCenterEnabledVersions/{id}/relationships/compatibleVersions` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterAchievements_replaceToManyRelationship` | `PATCH /v1/gameCenterGroups/{id}/relationships/gameCenterAchievements` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterAchievementsV2_replaceToManyRelationship` | `PATCH /v1/gameCenterGroups/{id}/relationships/gameCenterAchievementsV2` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterLeaderboardSets_replaceToManyRelationship` | `PATCH /v1/gameCenterGroups/{id}/relationships/gameCenterLeaderboardSets` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterLeaderboardSetsV2_replaceToManyRelationship` | `PATCH /v1/gameCenterGroups/{id}/relationships/gameCenterLeaderboardSetsV2` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterLeaderboards_replaceToManyRelationship` | `PATCH /v1/gameCenterGroups/{id}/relationships/gameCenterLeaderboards` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterGroups | `gameCenterGroups_gameCenterLeaderboardsV2_replaceToManyRelationship` | `PATCH /v1/gameCenterGroups/{id}/relationships/gameCenterLeaderboardsV2` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardEntrySubmissions | `gameCenterLeaderboardEntrySubmissions_createInstance` | `POST /v1/gameCenterLeaderboardEntrySubmissions` |  | Account Holder, Admin, App Manager |  |
| GameCenterLeaderboardImages | `gameCenterLeaderboardImages_createInstance` | `POST /v1/gameCenterLeaderboardImages` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardImages | `gameCenterLeaderboardImages_updateInstance` | `PATCH /v1/gameCenterLeaderboardImages/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardImages | `gameCenterLeaderboardImagesV2_createInstance` | `POST /v2/gameCenterLeaderboardImages` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardImages | `gameCenterLeaderboardImagesV2_updateInstance` | `PATCH /v2/gameCenterLeaderboardImages/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardLocalizations | `gameCenterLeaderboardLocalizations_createInstance` | `POST /v1/gameCenterLeaderboardLocalizations` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardLocalizations | `gameCenterLeaderboardLocalizations_updateInstance` | `PATCH /v1/gameCenterLeaderboardLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardLocalizations | `gameCenterLeaderboardLocalizationsV2_createInstance` | `POST /v2/gameCenterLeaderboardLocalizations` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardLocalizations | `gameCenterLeaderboardLocalizationsV2_updateInstance` | `PATCH /v2/gameCenterLeaderboardLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardReleases | `gameCenterLeaderboardReleases_createInstance` | `POST /v1/gameCenterLeaderboardReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardReleases | `gameCenterLeaderboardReleases_deleteInstance` | `DELETE /v1/gameCenterLeaderboardReleases/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboardSetImages | `gameCenterLeaderboardSetImages_createInstance` | `POST /v1/gameCenterLeaderboardSetImages` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetImages | `gameCenterLeaderboardSetImages_updateInstance` | `PATCH /v1/gameCenterLeaderboardSetImages/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetImages | `gameCenterLeaderboardSetImagesV2_createInstance` | `POST /v2/gameCenterLeaderboardSetImages` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetImages | `gameCenterLeaderboardSetImagesV2_updateInstance` | `PATCH /v2/gameCenterLeaderboardSetImages/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetLocalizations | `gameCenterLeaderboardSetLocalizations_createInstance` | `POST /v1/gameCenterLeaderboardSetLocalizations` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetLocalizations | `gameCenterLeaderboardSetLocalizations_updateInstance` | `PATCH /v1/gameCenterLeaderboardSetLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetLocalizations | `gameCenterLeaderboardSetLocalizationsV2_createInstance` | `POST /v2/gameCenterLeaderboardSetLocalizations` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetLocalizations | `gameCenterLeaderboardSetLocalizationsV2_updateInstance` | `PATCH /v2/gameCenterLeaderboardSetLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetMemberLocalizations | `gameCenterLeaderboardSetMemberLocalizations_createInstance` | `POST /v1/gameCenterLeaderboardSetMemberLocalizations` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetMemberLocalizations | `gameCenterLeaderboardSetMemberLocalizations_updateInstance` | `PATCH /v1/gameCenterLeaderboardSetMemberLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetReleases | `gameCenterLeaderboardSetReleases_createInstance` | `POST /v1/gameCenterLeaderboardSetReleases` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSetReleases | `gameCenterLeaderboardSetReleases_deleteInstance` | `DELETE /v1/gameCenterLeaderboardSetReleases/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_gameCenterLeaderboards_deleteToManyRelationship` | `DELETE /v1/gameCenterLeaderboardSets/{id}/relationships/gameCenterLeaderboards` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_gameCenterLeaderboards_replaceToManyRelationship` | `PATCH /v1/gameCenterLeaderboardSets/{id}/relationships/gameCenterLeaderboards` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_gameCenterLeaderboards_createToManyRelationship` | `POST /v1/gameCenterLeaderboardSets/{id}/relationships/gameCenterLeaderboards` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_groupLeaderboardSet_updateToOneRelationship` | `PATCH /v1/gameCenterLeaderboardSets/{id}/relationships/groupLeaderboardSet` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSetsV2_gameCenterLeaderboards_deleteToManyRelationship` | `DELETE /v2/gameCenterLeaderboardSets/{id}/relationships/gameCenterLeaderboards` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSetsV2_gameCenterLeaderboards_replaceToManyRelationship` | `PATCH /v2/gameCenterLeaderboardSets/{id}/relationships/gameCenterLeaderboards` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSetsV2_gameCenterLeaderboards_createToManyRelationship` | `POST /v2/gameCenterLeaderboardSets/{id}/relationships/gameCenterLeaderboards` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboards_updateInstance` | `PATCH /v1/gameCenterLeaderboards/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboards_activity_updateToOneRelationship` | `PATCH /v1/gameCenterLeaderboards/{id}/relationships/activity` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboards_challenge_updateToOneRelationship` | `PATCH /v1/gameCenterLeaderboards/{id}/relationships/challenge` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboards_groupLeaderboard_updateToOneRelationship` | `PATCH /v1/gameCenterLeaderboards/{id}/relationships/groupLeaderboard` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboardsV2_updateInstance` | `PATCH /v2/gameCenterLeaderboards/{id}` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboardsV2_activity_updateToOneRelationship` | `PATCH /v2/gameCenterLeaderboards/{id}/relationships/activity` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterLeaderboards | `gameCenterLeaderboardsV2_challenge_updateToOneRelationship` | `PATCH /v2/gameCenterLeaderboards/{id}/relationships/challenge` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingQueues | `gameCenterMatchmakingQueues_updateInstance` | `PATCH /v1/gameCenterMatchmakingQueues/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRuleSets | `gameCenterMatchmakingRuleSets_updateInstance` | `PATCH /v1/gameCenterMatchmakingRuleSets/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingRules | `gameCenterMatchmakingRules_updateInstance` | `PATCH /v1/gameCenterMatchmakingRules/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterMatchmakingTeams | `gameCenterMatchmakingTeams_updateInstance` | `PATCH /v1/gameCenterMatchmakingTeams/{id}` |  | Account Holder, Admin, App Manager, Developer, Marketing |  |
| GameCenterPlayerAchievementSubmissions | `gameCenterPlayerAchievementSubmissions_createInstance` | `POST /v1/gameCenterPlayerAchievementSubmissions` |  | Account Holder, Admin, App Manager |  |
| GameCenterScoreModerations | `gameCenterScoreModerations_updateInstance` | `PATCH /v1/gameCenterScoreModerations/{id}` |  | Account Holder, Admin, App Manager |  |
| InAppPurchaseAvailabilities | `inAppPurchaseAvailabilities_createInstance` | `POST /v1/inAppPurchaseAvailabilities` |  | Account Holder, Admin, App Manager |  |
| InAppPurchaseImages | `inAppPurchaseImages_createInstance` | `POST /v1/inAppPurchaseImages` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| InAppPurchaseImages | `inAppPurchaseImages_updateInstance` | `PATCH /v1/inAppPurchaseImages/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| InAppPurchaseImages | `inAppPurchaseImagesV2_createInstance` | `POST /v2/inAppPurchaseImages` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| InAppPurchaseImages | `inAppPurchaseImagesV2_updateInstance` | `PATCH /v2/inAppPurchaseImages/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| InAppPurchaseLocalizations | `inAppPurchaseLocalizations_createInstance` | `POST /v1/inAppPurchaseLocalizations` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseLocalizations | `inAppPurchaseLocalizations_updateInstance` | `PATCH /v1/inAppPurchaseLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager |  |
| InAppPurchaseLocalizations | `inAppPurchaseLocalizationsV2_createInstance` | `POST /v2/inAppPurchaseLocalizations` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| InAppPurchaseLocalizations | `inAppPurchaseLocalizationsV2_updateInstance` | `PATCH /v2/inAppPurchaseLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager |  |
| InAppPurchaseOfferCodeCustomCodes | `inAppPurchaseOfferCodeCustomCodes_createInstance` | `POST /v1/inAppPurchaseOfferCodeCustomCodes` |  | Account Holder, Admin, App Manager |  |
| InAppPurchaseOfferCodeCustomCodes | `inAppPurchaseOfferCodeCustomCodes_updateInstance` | `PATCH /v1/inAppPurchaseOfferCodeCustomCodes/{id}` |  | Account Holder, Admin, App Manager |  |
| InAppPurchaseOfferCodeOneTimeUseCodes | `inAppPurchaseOfferCodeOneTimeUseCodes_createInstance` | `POST /v1/inAppPurchaseOfferCodeOneTimeUseCodes` |  | Account Holder, Admin, App Manager |  |
| InAppPurchaseOfferCodeOneTimeUseCodes | `inAppPurchaseOfferCodeOneTimeUseCodes_updateInstance` | `PATCH /v1/inAppPurchaseOfferCodeOneTimeUseCodes/{id}` |  | Account Holder, Admin, App Manager |  |
| InAppPurchaseOfferCodes | `inAppPurchaseOfferCodes_createInstance` | `POST /v1/inAppPurchaseOfferCodes` |  | Account Holder, Admin, App Manager |  |
| InAppPurchaseOfferCodes | `inAppPurchaseOfferCodes_updateInstance` | `PATCH /v1/inAppPurchaseOfferCodes/{id}` |  | Account Holder, Admin, App Manager |  |
| InAppPurchasePriceSchedules | `inAppPurchasePriceSchedules_createInstance` | `POST /v1/inAppPurchasePriceSchedules` |  | Account Holder, Admin, App Manager |  |
| InAppPurchaseSubmissions | `inAppPurchaseSubmissions_createInstance` | `POST /v1/inAppPurchaseSubmissions` |  | Account Holder, Admin, App Manager |  |
| InAppPurchases | `inAppPurchasesV2_updateInstance` | `PATCH /v2/inAppPurchases/{id}` | draft: operate | Account Holder, Admin, App Manager |  |
| Nominations | `nominations_createInstance` | `POST /v1/nominations` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| Nominations | `nominations_updateInstance` | `PATCH /v1/nominations/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| PromotedPurchases | `promotedPurchases_createInstance` | `POST /v1/promotedPurchases` | draft: operate | Account Holder, Admin, App Manager |  |
| PromotedPurchases | `promotedPurchases_updateInstance` | `PATCH /v1/promotedPurchases/{id}` | draft: operate | Account Holder, Admin, App Manager |  |
| RetentionMessageLocalizations | `retentionMessageLocalizations_createInstance` | `POST /v1/retentionMessageLocalizations` | draft: operate | Account Holder, Admin, App Manager |  |
| RetentionMessageLocalizations | `retentionMessageLocalizations_updateInstance` | `PATCH /v1/retentionMessageLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager |  |
| RetentionOffers | `retentionOffers_createInstance` | `POST /v1/retentionOffers` |  | Account Holder, Admin, App Manager |  |
| RetentionOffers | `retentionOffers_deleteInstance` | `DELETE /v1/retentionOffers/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| RetentionOffers | `retentionOffers_updateInstance` | `PATCH /v1/retentionOffers/{id}` |  | Account Holder, Admin, App Manager |  |
| ReviewSubmissionItems | `reviewSubmissionItems_deleteInstance` | `DELETE /v1/reviewSubmissionItems/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| ReviewSubmissionItems | `reviewSubmissionItems_updateInstance` | `PATCH /v1/reviewSubmissionItems/{id}` |  | Account Holder, Admin, App Manager, Marketing |  |
| ReviewSubmissions | `reviewSubmissions_updateInstance` | `PATCH /v1/reviewSubmissions/{id}` |  | Account Holder, Admin, App Manager, Marketing |  |
| SubscriptionAvailabilities | `subscriptionAvailabilities_createInstance` | `POST /v1/subscriptionAvailabilities` |  | Account Holder, Admin, App Manager |  |
| SubscriptionGracePeriods | `subscriptionGracePeriods_updateInstance` | `PATCH /v1/subscriptionGracePeriods/{id}` |  | Account Holder, Admin, App Manager |  |
| SubscriptionGroupLocalizations | `subscriptionGroupLocalizations_createInstance` | `POST /v1/subscriptionGroupLocalizations` | draft: operate | Account Holder, Admin, App Manager |  |
| SubscriptionGroupLocalizations | `subscriptionGroupLocalizations_updateInstance` | `PATCH /v1/subscriptionGroupLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager |  |
| SubscriptionGroupLocalizations | `subscriptionGroupLocalizationsV2_createInstance` | `POST /v2/subscriptionGroupLocalizations` | draft: operate | Account Holder, Admin, App Manager |  |
| SubscriptionGroupLocalizations | `subscriptionGroupLocalizationsV2_updateInstance` | `PATCH /v2/subscriptionGroupLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager |  |
| SubscriptionGroupSubmissions | `subscriptionGroupSubmissions_createInstance` | `POST /v1/subscriptionGroupSubmissions` |  | Account Holder, Admin, App Manager |  |
| SubscriptionImages | `subscriptionImages_createInstance` | `POST /v1/subscriptionImages` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| SubscriptionImages | `subscriptionImages_updateInstance` | `PATCH /v1/subscriptionImages/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| SubscriptionImages | `subscriptionImagesV2_createInstance` | `POST /v2/subscriptionImages` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| SubscriptionImages | `subscriptionImagesV2_updateInstance` | `PATCH /v2/subscriptionImages/{id}` | draft: operate | Account Holder, Admin, App Manager, Marketing |  |
| SubscriptionIntroductoryOffers | `subscriptionIntroductoryOffers_createInstance` | `POST /v1/subscriptionIntroductoryOffers` |  | Account Holder, Admin, App Manager |  |
| SubscriptionIntroductoryOffers | `subscriptionIntroductoryOffers_deleteInstance` | `DELETE /v1/subscriptionIntroductoryOffers/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| SubscriptionIntroductoryOffers | `subscriptionIntroductoryOffers_updateInstance` | `PATCH /v1/subscriptionIntroductoryOffers/{id}` |  | Account Holder, Admin, App Manager |  |
| SubscriptionLocalizations | `subscriptionLocalizations_createInstance` | `POST /v1/subscriptionLocalizations` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionLocalizations | `subscriptionLocalizations_updateInstance` | `PATCH /v1/subscriptionLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager |  |
| SubscriptionLocalizations | `subscriptionLocalizationsV2_createInstance` | `POST /v2/subscriptionLocalizations` | draft: operate | Account Holder, Admin, App Manager, Developer, Marketing |  |
| SubscriptionLocalizations | `subscriptionLocalizationsV2_updateInstance` | `PATCH /v2/subscriptionLocalizations/{id}` | draft: operate | Account Holder, Admin, App Manager |  |
| SubscriptionOfferCodeCustomCodes | `subscriptionOfferCodeCustomCodes_createInstance` | `POST /v1/subscriptionOfferCodeCustomCodes` |  | Account Holder, Admin, App Manager |  |
| SubscriptionOfferCodeCustomCodes | `subscriptionOfferCodeCustomCodes_updateInstance` | `PATCH /v1/subscriptionOfferCodeCustomCodes/{id}` |  | Account Holder, Admin, App Manager |  |
| SubscriptionOfferCodeOneTimeUseCodes | `subscriptionOfferCodeOneTimeUseCodes_createInstance` | `POST /v1/subscriptionOfferCodeOneTimeUseCodes` |  | Account Holder, Admin, App Manager |  |
| SubscriptionOfferCodeOneTimeUseCodes | `subscriptionOfferCodeOneTimeUseCodes_updateInstance` | `PATCH /v1/subscriptionOfferCodeOneTimeUseCodes/{id}` |  | Account Holder, Admin, App Manager |  |
| SubscriptionOfferCodes | `subscriptionOfferCodes_createInstance` | `POST /v1/subscriptionOfferCodes` |  | Account Holder, Admin, App Manager |  |
| SubscriptionOfferCodes | `subscriptionOfferCodes_updateInstance` | `PATCH /v1/subscriptionOfferCodes/{id}` |  | Account Holder, Admin, App Manager |  |
| SubscriptionPlanAvailabilities | `subscriptionPlanAvailabilities_createInstance` | `POST /v1/subscriptionPlanAvailabilities` |  | Account Holder, Admin, App Manager |  |
| SubscriptionPlanAvailabilities | `subscriptionPlanAvailabilities_updateInstance` | `PATCH /v1/subscriptionPlanAvailabilities/{id}` |  | Account Holder, Admin, App Manager |  |
| SubscriptionPlanAvailabilities | `subscriptionPlanAvailabilities_availableTerritories_replaceToManyRelationship` | `PATCH /v1/subscriptionPlanAvailabilities/{id}/relationships/availableTerritories` |  | Account Holder, Admin, App Manager |  |
| SubscriptionPrices | `subscriptionPrices_createInstance` | `POST /v1/subscriptionPrices` |  | Account Holder, Admin, App Manager |  |
| SubscriptionPrices | `subscriptionPrices_deleteInstance` | `DELETE /v1/subscriptionPrices/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| SubscriptionPromotionalOffers | `subscriptionPromotionalOffers_createInstance` | `POST /v1/subscriptionPromotionalOffers` |  | Account Holder, Admin, App Manager |  |
| SubscriptionPromotionalOffers | `subscriptionPromotionalOffers_deleteInstance` | `DELETE /v1/subscriptionPromotionalOffers/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| SubscriptionPromotionalOffers | `subscriptionPromotionalOffers_updateInstance` | `PATCH /v1/subscriptionPromotionalOffers/{id}` |  | Account Holder, Admin, App Manager |  |
| SubscriptionSubmissions | `subscriptionSubmissions_createInstance` | `POST /v1/subscriptionSubmissions` |  | Account Holder, Admin, App Manager |  |
| Subscriptions | `subscriptions_updateInstance` | `PATCH /v1/subscriptions/{id}` | draft: operate | Account Holder, Admin, App Manager |  |
| Subscriptions | `subscriptions_introductoryOffers_deleteToManyRelationship` | `DELETE /v1/subscriptions/{id}/relationships/introductoryOffers` |  | Account Holder, Admin, App Manager | cannot be undone |
| Subscriptions | `subscriptions_prices_deleteToManyRelationship` | `DELETE /v1/subscriptions/{id}/relationships/prices` |  | Account Holder, Admin, App Manager | cannot be undone |
| Subscriptions | `subscriptions_retentionMessage_updateToOneRelationship` | `PATCH /v1/subscriptions/{id}/relationships/retentionMessage` |  | Account Holder, Admin, App Manager |  |
| Subscriptions | `subscriptions_retentionMessageOffers_deleteToManyRelationship` | `DELETE /v1/subscriptions/{id}/relationships/retentionMessageOffers` |  | Account Holder, Admin, App Manager |  |
| Subscriptions | `subscriptions_retentionMessageOffers_createToManyRelationship` | `POST /v1/subscriptions/{id}/relationships/retentionMessageOffers` |  | Account Holder, Admin, App Manager |  |
| TerritoryAvailabilities | `territoryAvailabilities_updateInstance` | `PATCH /v1/territoryAvailabilities/{id}` |  | Account Holder, Admin, App Manager |  |
| WinBackOffers | `winBackOffers_createInstance` | `POST /v1/winBackOffers` |  | Account Holder, Admin, App Manager |  |
| WinBackOffers | `winBackOffers_deleteInstance` | `DELETE /v1/winBackOffers/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| WinBackOffers | `winBackOffers_updateInstance` | `PATCH /v1/winBackOffers/{id}` |  | Account Holder, Admin, App Manager |  |

## admin

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| AccessibilityDeclarations | `accessibilityDeclarations_deleteInstance` | `DELETE /v1/accessibilityDeclarations/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AlternativeDistributionDomains | `alternativeDistributionDomains_createInstance` | `POST /v1/alternativeDistributionDomains` |  | Account Holder, Admin |  |
| AlternativeDistributionDomains | `alternativeDistributionDomains_deleteInstance` | `DELETE /v1/alternativeDistributionDomains/{id}` |  | Account Holder, Admin | cannot be undone |
| AlternativeDistributionKeys | `alternativeDistributionKeys_createInstance` | `POST /v1/alternativeDistributionKeys` |  | Account Holder, Admin |  |
| AlternativeDistributionKeys | `alternativeDistributionKeys_deleteInstance` | `DELETE /v1/alternativeDistributionKeys/{id}` |  | Account Holder, Admin | cannot be undone |
| AnalyticsReportRequests | `analyticsReportRequests_createInstance` | `POST /v1/analyticsReportRequests` |  | Account Holder, Admin |  |
| AnalyticsReportRequests | `analyticsReportRequests_deleteInstance` | `DELETE /v1/analyticsReportRequests/{id}` |  | Account Holder, Admin | cannot be undone |
| AndroidToIosAppMappingDetails | `androidToIosAppMappingDetails_createInstance` | `POST /v1/androidToIosAppMappingDetails` |  | Account Holder, Admin, App Manager |  |
| AndroidToIosAppMappingDetails | `androidToIosAppMappingDetails_deleteInstance` | `DELETE /v1/androidToIosAppMappingDetails/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| AndroidToIosAppMappingDetails | `androidToIosAppMappingDetails_updateInstance` | `PATCH /v1/androidToIosAppMappingDetails/{id}` |  | Account Holder, Admin, App Manager |  |
| AppAssetLibraryImages | `appAssetLibraryImages_deleteInstance` | `DELETE /v1/appAssetLibraryImages/{id}` |  | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppAssetLibraryVideos | `appAssetLibraryVideos_deleteInstance` | `DELETE /v1/appAssetLibraryVideos/{id}` |  | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppClipDefaultExperienceLocalizations | `appClipDefaultExperienceLocalizations_deleteInstance` | `DELETE /v1/appClipDefaultExperienceLocalizations/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppClipDefaultExperiences | `appClipDefaultExperiences_deleteInstance` | `DELETE /v1/appClipDefaultExperiences/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppClipHeaderImages | `appClipHeaderImages_deleteInstance` | `DELETE /v1/appClipHeaderImages/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppCustomProductPageLocalizations | `appCustomProductPageLocalizations_deleteInstance` | `DELETE /v1/appCustomProductPageLocalizations/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppCustomProductPages | `appCustomProductPages_deleteInstance` | `DELETE /v1/appCustomProductPages/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppEncryptionDeclarations | `appEncryptionDeclarations_createInstance` | `POST /v1/appEncryptionDeclarations` |  | Account Holder, Admin, App Manager |  |
| AppEventLocalizations | `appEventLocalizations_deleteInstance` | `DELETE /v1/appEventLocalizations/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppEventScreenshots | `appEventScreenshots_deleteInstance` | `DELETE /v1/appEventScreenshots/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppEventVideoClips | `appEventVideoClips_deleteInstance` | `DELETE /v1/appEventVideoClips/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppEvents | `appEvents_deleteInstance` | `DELETE /v1/appEvents/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppInfoLocalizations | `appInfoLocalizations_deleteInstance` | `DELETE /v1/appInfoLocalizations/{id}` | live: public | Account Holder, Admin, App Manager, Marketing, Customer Support | cannot be undone |
| AppPreviewSets | `appPreviewSets_deleteInstance` | `DELETE /v1/appPreviewSets/{id}` | live: public | Account Holder, Admin, App Manager, Marketing, Customer Support | cannot be undone |
| AppPreviews | `appPreviews_deleteInstance` | `DELETE /v1/appPreviews/{id}` | live: public | Account Holder, Admin, App Manager, Marketing, Customer Support | cannot be undone |
| AppScreenshotSets | `appScreenshotSets_deleteInstance` | `DELETE /v1/appScreenshotSets/{id}` | live: public | Account Holder, Admin, App Manager, Marketing, Customer Support | cannot be undone |
| AppScreenshots | `appScreenshots_deleteInstance` | `DELETE /v1/appScreenshots/{id}` | live: public | Account Holder, Admin, App Manager, Marketing, Customer Support | cannot be undone |
| AppStoreReviewAttachments | `appStoreReviewAttachments_deleteInstance` | `DELETE /v1/appStoreReviewAttachments/{id}` |  | Account Holder, Admin, App Manager, Marketing, Customer Support | cannot be undone |
| AppStoreVersionExperimentTreatmentLocalizations | `appStoreVersionExperimentTreatmentLocalizations_deleteInstance` | `DELETE /v1/appStoreVersionExperimentTreatmentLocalizations/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppStoreVersionExperimentTreatments | `appStoreVersionExperimentTreatments_deleteInstance` | `DELETE /v1/appStoreVersionExperimentTreatments/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppStoreVersionExperiments | `appStoreVersionExperiments_deleteInstance` | `DELETE /v1/appStoreVersionExperiments/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppStoreVersionExperiments | `appStoreVersionExperimentsV2_deleteInstance` | `DELETE /v2/appStoreVersionExperiments/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| AppStoreVersionLocalizations | `appStoreVersionLocalizations_deleteInstance` | `DELETE /v1/appStoreVersionLocalizations/{id}` |  | Account Holder, Admin, App Manager, Marketing, Customer Support | cannot be undone |
| AppStoreVersions | `appStoreVersions_deleteInstance` | `DELETE /v1/appStoreVersions/{id}` |  | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| Apps | `apps_updateInstance` | `PATCH /v1/apps/{id}` |  | Account Holder, Admin, App Manager |  |
| BetaAppClipInvocationLocalizations | `betaAppClipInvocationLocalizations_deleteInstance` | `DELETE /v1/betaAppClipInvocationLocalizations/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| BetaAppClipInvocations | `betaAppClipInvocations_deleteInstance` | `DELETE /v1/betaAppClipInvocations/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| BetaAppLocalizations | `betaAppLocalizations_deleteInstance` | `DELETE /v1/betaAppLocalizations/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| BetaBuildLocalizations | `betaBuildLocalizations_deleteInstance` | `DELETE /v1/betaBuildLocalizations/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| BetaFeedbackCrashSubmissions | `betaFeedbackCrashSubmissions_deleteInstance` | `DELETE /v1/betaFeedbackCrashSubmissions/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| BetaFeedbackScreenshotSubmissions | `betaFeedbackScreenshotSubmissions_deleteInstance` | `DELETE /v1/betaFeedbackScreenshotSubmissions/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| BetaGroups | `betaGroups_deleteInstance` | `DELETE /v1/betaGroups/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| BetaTesters | `betaTesters_deleteInstance` | `DELETE /v1/betaTesters/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| BuildUploadFiles | `buildUploadFiles_createInstance` | `POST /v1/buildUploadFiles` |  | Account Holder, Admin |  |
| BuildUploadFiles | `buildUploadFiles_updateInstance` | `PATCH /v1/buildUploadFiles/{id}` |  | Account Holder, Admin |  |
| BuildUploads | `buildUploads_createInstance` | `POST /v1/buildUploads` |  | Account Holder, Admin |  |
| BuildUploads | `buildUploads_deleteInstance` | `DELETE /v1/buildUploads/{id}` |  | Account Holder, Admin | cannot be undone |
| BundleIdCapabilities | `bundleIdCapabilities_createInstance` | `POST /v1/bundleIdCapabilities` |  | Account Holder, Admin, App Manager, Developer |  |
| BundleIdCapabilities | `bundleIdCapabilities_deleteInstance` | `DELETE /v1/bundleIdCapabilities/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| BundleIdCapabilities | `bundleIdCapabilities_updateInstance` | `PATCH /v1/bundleIdCapabilities/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| BundleIds | `bundleIds_createInstance` | `POST /v1/bundleIds` |  | Account Holder, Admin, App Manager, Developer |  |
| BundleIds | `bundleIds_deleteInstance` | `DELETE /v1/bundleIds/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| BundleIds | `bundleIds_updateInstance` | `PATCH /v1/bundleIds/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| Certificates | `certificates_createInstance` | `POST /v1/certificates` |  | Account Holder, Admin, App Manager, Developer |  |
| Certificates | `certificates_deleteInstance` | `DELETE /v1/certificates/{id}` |  | Account Holder, Admin, App Manager, Developer | cannot be undone |
| Certificates | `certificates_updateInstance` | `PATCH /v1/certificates/{id}` |  | Account Holder, Admin, App Manager |  |
| CiProducts | `ciProducts_deleteInstance` | `DELETE /v1/ciProducts/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| CiWorkflows | `ciWorkflows_deleteInstance` | `DELETE /v1/ciWorkflows/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| Devices | `devices_createInstance` | `POST /v1/devices` |  | Account Holder, Admin, App Manager, Developer |  |
| Devices | `devices_updateInstance` | `PATCH /v1/devices/{id}` |  | Account Holder, Admin, App Manager |  |
| GameCenterAchievementImages | `gameCenterAchievementImages_deleteInstance` | `DELETE /v1/gameCenterAchievementImages/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterAchievementImages | `gameCenterAchievementImagesV2_deleteInstance` | `DELETE /v2/gameCenterAchievementImages/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterAchievementLocalizations | `gameCenterAchievementLocalizations_deleteInstance` | `DELETE /v1/gameCenterAchievementLocalizations/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterAchievementLocalizations | `gameCenterAchievementLocalizationsV2_deleteInstance` | `DELETE /v2/gameCenterAchievementLocalizations/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterAchievements | `gameCenterAchievements_deleteInstance` | `DELETE /v1/gameCenterAchievements/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterAchievements | `gameCenterAchievementsV2_deleteInstance` | `DELETE /v2/gameCenterAchievements/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterActivities | `gameCenterActivities_deleteInstance` | `DELETE /v1/gameCenterActivities/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterActivityImages | `gameCenterActivityImages_deleteInstance` | `DELETE /v1/gameCenterActivityImages/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterActivityLocalizations | `gameCenterActivityLocalizations_deleteInstance` | `DELETE /v1/gameCenterActivityLocalizations/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterChallengeImages | `gameCenterChallengeImages_deleteInstance` | `DELETE /v1/gameCenterChallengeImages/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterChallengeLocalizations | `gameCenterChallengeLocalizations_deleteInstance` | `DELETE /v1/gameCenterChallengeLocalizations/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterChallenges | `gameCenterChallenges_deleteInstance` | `DELETE /v1/gameCenterChallenges/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterGroups | `gameCenterGroups_deleteInstance` | `DELETE /v1/gameCenterGroups/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboardImages | `gameCenterLeaderboardImages_deleteInstance` | `DELETE /v1/gameCenterLeaderboardImages/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboardImages | `gameCenterLeaderboardImagesV2_deleteInstance` | `DELETE /v2/gameCenterLeaderboardImages/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboardLocalizations | `gameCenterLeaderboardLocalizations_deleteInstance` | `DELETE /v1/gameCenterLeaderboardLocalizations/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboardLocalizations | `gameCenterLeaderboardLocalizationsV2_deleteInstance` | `DELETE /v2/gameCenterLeaderboardLocalizations/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboardSetImages | `gameCenterLeaderboardSetImages_deleteInstance` | `DELETE /v1/gameCenterLeaderboardSetImages/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboardSetImages | `gameCenterLeaderboardSetImagesV2_deleteInstance` | `DELETE /v2/gameCenterLeaderboardSetImages/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboardSetLocalizations | `gameCenterLeaderboardSetLocalizations_deleteInstance` | `DELETE /v1/gameCenterLeaderboardSetLocalizations/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboardSetLocalizations | `gameCenterLeaderboardSetLocalizationsV2_deleteInstance` | `DELETE /v2/gameCenterLeaderboardSetLocalizations/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboardSetMemberLocalizations | `gameCenterLeaderboardSetMemberLocalizations_deleteInstance` | `DELETE /v1/gameCenterLeaderboardSetMemberLocalizations/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSets_deleteInstance` | `DELETE /v1/gameCenterLeaderboardSets/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboardSets | `gameCenterLeaderboardSetsV2_deleteInstance` | `DELETE /v2/gameCenterLeaderboardSets/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboards | `gameCenterLeaderboards_deleteInstance` | `DELETE /v1/gameCenterLeaderboards/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterLeaderboards | `gameCenterLeaderboardsV2_deleteInstance` | `DELETE /v2/gameCenterLeaderboards/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterMatchmakingQueues | `gameCenterMatchmakingQueues_deleteInstance` | `DELETE /v1/gameCenterMatchmakingQueues/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterMatchmakingRuleSets | `gameCenterMatchmakingRuleSets_deleteInstance` | `DELETE /v1/gameCenterMatchmakingRuleSets/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterMatchmakingRules | `gameCenterMatchmakingRules_deleteInstance` | `DELETE /v1/gameCenterMatchmakingRules/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| GameCenterMatchmakingTeams | `gameCenterMatchmakingTeams_deleteInstance` | `DELETE /v1/gameCenterMatchmakingTeams/{id}` | live: public | Account Holder, Admin, App Manager, Developer, Marketing | cannot be undone |
| InAppPurchaseAppStoreReviewScreenshots | `inAppPurchaseAppStoreReviewScreenshots_deleteInstance` | `DELETE /v1/inAppPurchaseAppStoreReviewScreenshots/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| InAppPurchaseImages | `inAppPurchaseImages_deleteInstance` | `DELETE /v1/inAppPurchaseImages/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| InAppPurchaseImages | `inAppPurchaseImagesV2_deleteInstance` | `DELETE /v2/inAppPurchaseImages/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| InAppPurchaseLocalizations | `inAppPurchaseLocalizations_deleteInstance` | `DELETE /v1/inAppPurchaseLocalizations/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| InAppPurchaseLocalizations | `inAppPurchaseLocalizationsV2_deleteInstance` | `DELETE /v2/inAppPurchaseLocalizations/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| InAppPurchases | `inAppPurchasesV2_deleteInstance` | `DELETE /v2/inAppPurchases/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| MarketplaceSearchDetails | `marketplaceSearchDetails_createInstance` | `POST /v1/marketplaceSearchDetails` |  | Account Holder, Admin |  |
| MarketplaceSearchDetails | `marketplaceSearchDetails_deleteInstance` | `DELETE /v1/marketplaceSearchDetails/{id}` |  | Account Holder, Admin | cannot be undone |
| MarketplaceSearchDetails | `marketplaceSearchDetails_updateInstance` | `PATCH /v1/marketplaceSearchDetails/{id}` |  | Account Holder, Admin |  |
| MarketplaceWebhooks | `marketplaceWebhooks_createInstance` | `POST /v1/marketplaceWebhooks` |  | Account Holder, Admin |  |
| MarketplaceWebhooks | `marketplaceWebhooks_deleteInstance` | `DELETE /v1/marketplaceWebhooks/{id}` |  | Account Holder, Admin | cannot be undone |
| MarketplaceWebhooks | `marketplaceWebhooks_updateInstance` | `PATCH /v1/marketplaceWebhooks/{id}` |  | Account Holder, Admin |  |
| MerchantIds | `merchantIds_createInstance` | `POST /v1/merchantIds` |  | Account Holder, Admin, App Manager |  |
| MerchantIds | `merchantIds_deleteInstance` | `DELETE /v1/merchantIds/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| MerchantIds | `merchantIds_updateInstance` | `PATCH /v1/merchantIds/{id}` |  | Account Holder, Admin, App Manager |  |
| Nominations | `nominations_deleteInstance` | `DELETE /v1/nominations/{id}` |  | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| PassTypeIds | `passTypeIds_createInstance` | `POST /v1/passTypeIds` |  | Account Holder, Admin, App Manager |  |
| PassTypeIds | `passTypeIds_deleteInstance` | `DELETE /v1/passTypeIds/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| PassTypeIds | `passTypeIds_updateInstance` | `PATCH /v1/passTypeIds/{id}` |  | Account Holder, Admin, App Manager |  |
| Profiles | `profiles_createInstance` | `POST /v1/profiles` |  | Account Holder, Admin, App Manager, Developer |  |
| Profiles | `profiles_deleteInstance` | `DELETE /v1/profiles/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| PromotedPurchases | `promotedPurchases_deleteInstance` | `DELETE /v1/promotedPurchases/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| RetentionMessageLocalizations | `retentionMessageLocalizations_deleteInstance` | `DELETE /v1/retentionMessageLocalizations/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| RetentionMessages | `retentionMessages_deleteInstance` | `DELETE /v1/retentionMessages/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| RoutingAppCoverages | `routingAppCoverages_deleteInstance` | `DELETE /v1/routingAppCoverages/{id}` |  | Account Holder, Admin, App Manager, Marketing, Customer Support | cannot be undone |
| SandboxTesters | `sandboxTestersV2_updateInstance` | `PATCH /v2/sandboxTesters/{id}` |  | Account Holder, Admin, App Manager, Developer |  |
| SandboxTestersClearPurchaseHistoryRequest | `sandboxTestersClearPurchaseHistoryRequestV2_createInstance` | `POST /v2/sandboxTestersClearPurchaseHistoryRequest` |  | Account Holder, Admin, App Manager, Developer | cannot be undone |
| SubscriptionAppStoreReviewScreenshots | `subscriptionAppStoreReviewScreenshots_deleteInstance` | `DELETE /v1/subscriptionAppStoreReviewScreenshots/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| SubscriptionGroupLocalizations | `subscriptionGroupLocalizations_deleteInstance` | `DELETE /v1/subscriptionGroupLocalizations/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| SubscriptionGroupLocalizations | `subscriptionGroupLocalizationsV2_deleteInstance` | `DELETE /v2/subscriptionGroupLocalizations/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| SubscriptionGroups | `subscriptionGroups_deleteInstance` | `DELETE /v1/subscriptionGroups/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| SubscriptionImages | `subscriptionImages_deleteInstance` | `DELETE /v1/subscriptionImages/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| SubscriptionImages | `subscriptionImagesV2_deleteInstance` | `DELETE /v2/subscriptionImages/{id}` | live: public | Account Holder, Admin, App Manager, Marketing | cannot be undone |
| SubscriptionLocalizations | `subscriptionLocalizations_deleteInstance` | `DELETE /v1/subscriptionLocalizations/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| SubscriptionLocalizations | `subscriptionLocalizationsV2_deleteInstance` | `DELETE /v2/subscriptionLocalizations/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| Subscriptions | `subscriptions_deleteInstance` | `DELETE /v1/subscriptions/{id}` | live: public | Account Holder, Admin, App Manager | cannot be undone |
| UserInvitations | `userInvitations_createInstance` | `POST /v1/userInvitations` |  | Account Holder, Admin, App Manager |  |
| UserInvitations | `userInvitations_deleteInstance` | `DELETE /v1/userInvitations/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| Users | `users_deleteInstance` | `DELETE /v1/users/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| Users | `users_updateInstance` | `PATCH /v1/users/{id}` |  | Account Holder, Admin, App Manager |  |
| Users | `users_visibleApps_deleteToManyRelationship` | `DELETE /v1/users/{id}/relationships/visibleApps` |  | Account Holder, Admin, App Manager |  |
| Users | `users_visibleApps_replaceToManyRelationship` | `PATCH /v1/users/{id}/relationships/visibleApps` |  | Account Holder, Admin, App Manager |  |
| Users | `users_visibleApps_createToManyRelationship` | `POST /v1/users/{id}/relationships/visibleApps` |  | Account Holder, Admin, App Manager |  |
| Webhooks | `webhooks_createInstance` | `POST /v1/webhooks` |  | Account Holder, Admin, App Manager |  |
| Webhooks | `webhooks_deleteInstance` | `DELETE /v1/webhooks/{id}` |  | Account Holder, Admin, App Manager | cannot be undone |
| Webhooks | `webhooks_updateInstance` | `PATCH /v1/webhooks/{id}` |  | Account Holder, Admin, App Manager |  |
