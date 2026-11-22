# Apple Ads Platform API operations

Every operation of Apple Ads Platform API 1.0 as Crewlet offers it, generated from the integration's catalog: never edit this page.

| Class | Operations |
|---|---|
| [read](#read) | 63 |
| [operate](#operate) | 7 |
| [public](#public) | 2 |
| [spend](#spend) | 16 |
| [admin](#admin) | 13 |
| never | 0 |

## read

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| Acls | `getUserAcls` | `GET /v1/acls` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| AdAccounts | `adAccountsIdGet` | `GET /v1/ad-accounts/{id}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| Adgroups | `adgroupsIdGet` | `GET /v1/adgroups/{id}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| AdgroupsQuery | `adgroupsQueryPost` | `POST /v1/adgroups/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| Ads | `adsIdGet` | `GET /v1/ads/{id}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| AdsQuery | `adsQueryPost` | `POST /v1/ads/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| AdvertiserResources | `getAdvertiserResources` | `GET /v1/advertiser-resources` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| Apps | `getAppDetailsByAdamId` | `GET /v1/apps/{adamId}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| AppsLocaleDetails | `queryAppLocaleDetails` | `POST /v1/apps/{adamId}/locale-details/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| Assets | `getAsset` | `GET /v1/assets/{id}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| AssetsQuery | `queryAssets` | `POST /v1/assets/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| BusinessBrands | `getBrand` | `GET /v1/business-brands/{id}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| BusinessBrandsQuery | `queryBrands` | `POST /v1/business-brands/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| BusinessCategories | `getCategory` | `GET /v1/business-categories/{id}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| BusinessCategoriesQuery | `queryCategories` | `POST /v1/business-categories/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| Campaigns | `campaignsIdGet` | `GET /v1/campaigns/{id}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| CampaignsLegacyAppLimitedStatusReasonDetails | `campaignsIdLegacyAppLimitedStatusReasonDetailsGet` | `GET /v1/campaigns/{id}/legacy-app-limited-status-reason-details` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| CampaignsQuery | `campaignsQueryPost` | `POST /v1/campaigns/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ChangeHistory | `getChangeDetails` | `GET /v1/change-history/{detailId}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ChangeHistoryQuery | `queryAuditSummary` | `POST /v1/change-history/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| Creatives | `creativesIdGet` | `GET /v1/creatives/{id}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| CreativesQuery | `creativesQueryPost` | `POST /v1/creatives/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| EligibilitiesApps | `eligibilitiesAppsQueryPost` | `POST /v1/eligibilities/apps/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| InsightsApps | `impressionShareQuery` | `POST /v1/insights/apps/impression-share/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| InsightsApps | `searchTermPopularityQuery` | `POST /v1/insights/apps/search-term-popularity/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| Keywords | `keywordsIdGet` | `GET /v1/keywords/{id}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| KeywordsQuery | `keywordsQueryPost` | `POST /v1/keywords/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| LocationGroups | `getLocationGroup` | `GET /v1/location-groups/{id}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| LocationGroupsQuery | `queryLocationGroups` | `POST /v1/location-groups/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| Locations | `getLocation` | `GET /v1/locations/{id}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| LocationsQuery | `queryLocations` | `POST /v1/locations/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| Me | `getMe` | `GET /v1/me` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| MetadataApps | `querySupportedAppLanguages` | `POST /v1/metadata/apps/supported-languages/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| NegativeKeywords | `negativeKeywordsIdGet` | `GET /v1/negative-keywords/{id}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| NegativeKeywordsQuery | `negativeKeywordsQueryPost` | `POST /v1/negative-keywords/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| Orgs | `orgsIdGet` | `GET /v1/orgs/{id}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ProductPages | `getProductPageById` | `GET /v1/product-pages/{productPageId}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ProductPagesLocaleDetails | `queryProductPageLocaleDetails` | `POST /v1/product-pages/locale-details/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ProductPagesQuery | `queryProductPages` | `POST /v1/product-pages/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| RecommendationsDailyBudgets | `queryDailyBudgetRecommendations` | `POST /v1/recommendations/daily-budgets/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| RecommendationsTargetCpas | `queryTargetCpaRecommendations` | `POST /v1/recommendations/target-cpas/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| RejectionReasonsApps | `rejectionReasonsAppsQueryPost` | `POST /v1/rejection-reasons/apps/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| RejectionReasonsApps | `rejectionReasonsAppsRejectionReasonIdGet` | `GET /v1/rejection-reasons/apps/{rejectionReasonId}` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| RejectionReasonsBusinessBrands | `queryRejectionReasonsByBusinessBrand` | `POST /v1/rejection-reasons/business-brands/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ReportsApps | `appsAdGroupReports` | `POST /v1/reports/apps/adgroups/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ReportsApps | `appsAdReports` | `POST /v1/reports/apps/ads/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ReportsApps | `appsCampaignReports` | `POST /v1/reports/apps/campaigns/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ReportsApps | `appsKeywordReports` | `POST /v1/reports/apps/keywords/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ReportsApps | `appsSearchTermReports` | `POST /v1/reports/apps/searchterms/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ReportsBusinessBrands | `brandsAdGroupReports` | `POST /v1/reports/business-brands/adgroups/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ReportsBusinessBrands | `brandsAdReports` | `POST /v1/reports/business-brands/ads/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ReportsBusinessBrands | `brandsCampaignReports` | `POST /v1/reports/business-brands/campaigns/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ReportsBusinessBrands | `brandsKeywordReports` | `POST /v1/reports/business-brands/keywords/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| ReportsBusinessBrands | `brandsSearchTermReports` | `POST /v1/reports/business-brands/searchterms/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| SearchApps | `searchApps` | `GET /v1/search/apps` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| SearchGeo | `searchGeos` | `GET /v1/search/geo` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| SearchGeo | `getGeosByIds` | `POST /v1/search/geo` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| SharedBudgets | `sharedBudgetsIdGet` | `GET /v1/shared-budgets/{id}` |  | Admin, API Account Manager |  |
| SharedBudgetsQuery | `sharedBudgetsQueryPost` | `POST /v1/shared-budgets/query` |  | Admin, API Account Manager |  |
| SuggestionsCategories | `queryCategorySuggestions` | `POST /v1/suggestions/categories/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| SuggestionsKeywords | `queryKeywordSuggestions` | `POST /v1/suggestions/keywords/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| SuggestionsPhrases | `queryPhraseSuggestions` | `POST /v1/suggestions/phrases/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |
| SuggestionsTargetCpas | `queryTargetCpaSuggestion` | `POST /v1/suggestions/target-cpas/query` |  | Admin, API Account Manager, API Account Read Only, Limited Access: API Read & Write, Limited Access: API Read Only |  |

## operate

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| AssetsUpload | `uploadAsset` | `POST /v1/assets/upload` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| LocationGroups | `createLocationGroup` | `POST /v1/location-groups` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| NegativeKeywords | `negativeKeywordsPost` | `POST /v1/negative-keywords` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| NegativeKeywordsBulkCreate | `negativeKeywordsBulkCreatePost` | `POST /v1/negative-keywords/bulk-create` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| NegativeKeywordsBulkUpdate | `negativeKeywordsBulkUpdatePost` | `POST /v1/negative-keywords/bulk-update` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| RecommendationsDailyBudgets | `dismissDailyBudgetRecommendations` | `POST /v1/recommendations/daily-budgets/dismiss` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| RecommendationsTargetCpas | `dismissTargetCpaRecommendations` | `POST /v1/recommendations/target-cpas/dismiss` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |

## public

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| Creatives | `creativesPost` | `POST /v1/creatives` | unused: operate | Admin, API Account Manager, Limited Access: API Read & Write |  |
| Creatives | `creativesIdPut` | `PUT /v1/creatives/{id}` | unused: operate | Admin, API Account Manager, Limited Access: API Read & Write |  |

## spend

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| Adgroups | `adgroupsPost` | `POST /v1/adgroups` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| Adgroups | `adgroupsIdPut` | `PUT /v1/adgroups/{id}` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| Ads | `adsPost` | `POST /v1/ads` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| Ads | `adsIdPut` | `PUT /v1/ads/{id}` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| Campaigns | `campaignsPost` | `POST /v1/campaigns` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| Campaigns | `campaignsIdPut` | `PUT /v1/campaigns/{id}` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| Keywords | `keywordsPost` | `POST /v1/keywords` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| Keywords | `keywordsIdPut` | `PUT /v1/keywords/{id}` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| KeywordsBulkCreate | `keywordsBulkCreatePost` | `POST /v1/keywords/bulk-create` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| KeywordsBulkUpdate | `keywordsBulkUpdatePost` | `POST /v1/keywords/bulk-update` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| LocationGroups | `updateLocationGroup` | `PUT /v1/location-groups/{id}` | unused: operate | Admin, API Account Manager, Limited Access: API Read & Write |  |
| NegativeKeywords | `negativeKeywordsIdPut` | `PUT /v1/negative-keywords/{id}` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| RecommendationsDailyBudgets | `applyDailyBudgetRecommendations` | `POST /v1/recommendations/daily-budgets/apply` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| RecommendationsTargetCpas | `applyTargetCpaRecommendations` | `POST /v1/recommendations/target-cpas/apply` |  | Admin, API Account Manager, Limited Access: API Read & Write |  |
| SharedBudgets | `sharedBudgetsPost` | `POST /v1/shared-budgets` |  | Admin, API Account Manager |  |
| SharedBudgets | `sharedBudgetsIdPut` | `PUT /v1/shared-budgets/{id}` |  | Admin, API Account Manager |  |

## admin

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| AdAccounts | `adAccountsPost` | `POST /v1/ad-accounts` |  | Admin, API Account Manager |  |
| AdAccounts | `adAccountsIdPut` | `PUT /v1/ad-accounts/{id}` |  | Admin, API Account Manager |  |
| Adgroups | `adgroupsIdDelete` | `DELETE /v1/adgroups/{id}` |  | Admin, API Account Manager, Limited Access: API Read & Write | cannot be undone |
| Ads | `adsIdDelete` | `DELETE /v1/ads/{id}` |  | Admin, API Account Manager, Limited Access: API Read & Write | cannot be undone |
| Assets | `deleteAsset` | `DELETE /v1/assets/{id}` |  | Admin, API Account Manager, Limited Access: API Read & Write | cannot be undone |
| Campaigns | `campaignsIdDelete` | `DELETE /v1/campaigns/{id}` |  | Admin, API Account Manager, Limited Access: API Read & Write | cannot be undone |
| Creatives | `creativesIdDelete` | `DELETE /v1/creatives/{id}` |  | Admin, API Account Manager, Limited Access: API Read & Write | cannot be undone |
| Keywords | `keywordsIdDelete` | `DELETE /v1/keywords/{id}` |  | Admin, API Account Manager, Limited Access: API Read & Write | cannot be undone |
| KeywordsBulkDelete | `keywordsBulkDeletePost` | `POST /v1/keywords/bulk-delete` |  | Admin, API Account Manager, Limited Access: API Read & Write | cannot be undone |
| LocationGroups | `deleteLocationGroup` | `DELETE /v1/location-groups/{id}` |  | Admin, API Account Manager, Limited Access: API Read & Write | cannot be undone |
| NegativeKeywords | `negativeKeywordsIdDelete` | `DELETE /v1/negative-keywords/{id}` |  | Admin, API Account Manager, Limited Access: API Read & Write | cannot be undone |
| NegativeKeywordsBulkDelete | `negativeKeywordsBulkDeletePost` | `POST /v1/negative-keywords/bulk-delete` |  | Admin, API Account Manager, Limited Access: API Read & Write | cannot be undone |
| SharedBudgets | `sharedBudgetsIdDelete` | `DELETE /v1/shared-budgets/{id}` |  | Admin, API Account Manager | cannot be undone |
