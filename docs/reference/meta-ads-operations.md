# Meta Marketing API operations

Every operation of Meta Marketing API v27.0 as Crewlet offers it, generated from the integration's catalog: never edit this page.

| Class | Operations |
|---|---|
| [read](#read) | 653 |
| [operate](#operate) | 50 |
| [public](#public) | 23 |
| [spend](#spend) | 27 |
| [admin](#admin) | 190 |
| [never](#never) | 42 |

## read

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| (root) | `root.ad_campaign_placement.get` | `GET /ad_campaign_placement` |  | Ad account: View performance |  |
| (root) | `root.integrity_appeals_ads_eligibility.get` | `GET /integrity/appeals/ads/eligibility` |  | Ad account: View performance |  |
| (root) | `root.integrity_appeals_ads_status.get` | `GET /integrity/appeals/ads/status` |  | Ad account: View performance |  |
| ALMAdAccountInfo | `alm_ad_account_info.get` | `GET /{alm_ad_account_info_id}` |  | Business: Employee |  |
| ALMEndAdvertiserInfo | `alm_end_advertiser_info.get` | `GET /{alm_end_advertiser_info_id}` |  | Business: Employee |  |
| ALMEvent | `alm_event.get` | `GET /{alm_event_id}` |  | Business: Employee |  |
| Ad | `ad.get` | `GET /{ad_id}` |  | Ad account: View performance |  |
| Ad | `ad.adcreatives.list` | `GET /{ad_id}/adcreatives` |  | Ad account: View performance |  |
| Ad | `ad.adrules_governed.list` | `GET /{ad_id}/adrules_governed` |  | Ad account: View performance |  |
| Ad | `ad.copies.list` | `GET /{ad_id}/copies` |  | Ad account: View performance |  |
| Ad | `ad.insights.list` | `GET /{ad_id}/insights` |  | Ad account: View performance |  |
| Ad | `ad.insights.create` | `POST /{ad_id}/insights` |  | Ad account: View performance |  |
| Ad | `ad.leads.list` | `GET /{ad_id}/leads` |  | Page: Ads, Page: Leads access |  |
| Ad | `ad.previews.list` | `GET /{ad_id}/previews` |  | Ad account: View performance, Page: Ads |  |
| Ad | `ad.targetingsentencelines.list` | `GET /{ad_id}/targetingsentencelines` |  | Ad account: View performance |  |
| AdAccount | `ad_account.get` | `GET /act_{ad_account_id}` |  | Ad account: View performance |  |
| AdAccount | `ad_account.account_controls.list` | `GET /act_{ad_account_id}/account_controls` |  | Ad account: View performance |  |
| AdAccount | `ad_account.activities.list` | `GET /act_{ad_account_id}/activities` |  | Ad account: View performance |  |
| AdAccount | `ad_account.ad_place_page_sets.list` | `GET /act_{ad_account_id}/ad_place_page_sets` |  | Ad account: View performance |  |
| AdAccount | `ad_account.ad_saved_keywords.list` | `GET /act_{ad_account_id}/ad_saved_keywords` |  | Ad account: View performance |  |
| AdAccount | `ad_account.ad_studies.list` | `GET /act_{ad_account_id}/ad_studies` |  | Ad account: View performance |  |
| AdAccount | `ad_account.adcloudplayables.list` | `GET /act_{ad_account_id}/adcloudplayables` |  | Ad account: View performance |  |
| AdAccount | `ad_account.adcreatives.list` | `GET /act_{ad_account_id}/adcreatives` |  | Ad account: View performance, Page: Ads |  |
| AdAccount | `ad_account.adcreativesbylabels.list` | `GET /act_{ad_account_id}/adcreativesbylabels` |  | Ad account: View performance |  |
| AdAccount | `ad_account.adimages.list` | `GET /act_{ad_account_id}/adimages` |  | Ad account: View performance |  |
| AdAccount | `ad_account.adlabels.list` | `GET /act_{ad_account_id}/adlabels` |  | Ad account: View performance |  |
| AdAccount | `ad_account.adplayables.list` | `GET /act_{ad_account_id}/adplayables` |  | Ad account: View performance |  |
| AdAccount | `ad_account.adrules_history.list` | `GET /act_{ad_account_id}/adrules_history` |  | Ad account: View performance |  |
| AdAccount | `ad_account.adrules_library.list` | `GET /act_{ad_account_id}/adrules_library` |  | Ad account: View performance |  |
| AdAccount | `ad_account.ads.list` | `GET /act_{ad_account_id}/ads` |  | Ad account: View performance, Page: Ads |  |
| AdAccount | `ad_account.ads_reporting_mmm_reports.list` | `GET /act_{ad_account_id}/ads_reporting_mmm_reports` |  | Ad account: View performance |  |
| AdAccount | `ad_account.ads_reporting_mmm_schedulers.list` | `GET /act_{ad_account_id}/ads_reporting_mmm_schedulers` |  | Ad account: View performance |  |
| AdAccount | `ad_account.ads_reporting_report_specs.list` | `GET /act_{ad_account_id}/ads_reporting_report_specs` |  | Ad account: View performance |  |
| AdAccount | `ad_account.ads_volume.list` | `GET /act_{ad_account_id}/ads_volume` |  | Ad account: View performance |  |
| AdAccount | `ad_account.adsbylabels.list` | `GET /act_{ad_account_id}/adsbylabels` |  | Ad account: View performance |  |
| AdAccount | `ad_account.adsets.list` | `GET /act_{ad_account_id}/adsets` |  | Ad account: View performance |  |
| AdAccount | `ad_account.adsetsbylabels.list` | `GET /act_{ad_account_id}/adsetsbylabels` |  | Ad account: View performance |  |
| AdAccount | `ad_account.adspixels.list` | `GET /act_{ad_account_id}/adspixels` |  | Ad account: View performance |  |
| AdAccount | `ad_account.advertisable_applications.list` | `GET /act_{ad_account_id}/advertisable_applications` |  | Ad account: View performance |  |
| AdAccount | `ad_account.advideos.list` | `GET /act_{ad_account_id}/advideos` |  | Ad account: View performance |  |
| AdAccount | `ad_account.affectedadsets.list` | `GET /act_{ad_account_id}/affectedadsets` |  | Ad account: View performance |  |
| AdAccount | `ad_account.applications.list` | `GET /act_{ad_account_id}/applications` |  | Ad account: View performance |  |
| AdAccount | `ad_account.async_requests.list` | `GET /act_{ad_account_id}/async_requests` |  | Ad account: View performance |  |
| AdAccount | `ad_account.asyncadcreatives.list` | `GET /act_{ad_account_id}/asyncadcreatives` |  | Ad account: View performance, Page: Ads |  |
| AdAccount | `ad_account.asyncadrequestsets.list` | `GET /act_{ad_account_id}/asyncadrequestsets` |  | Ad account: View performance, Page: Ads |  |
| AdAccount | `ad_account.audience_funnel.list` | `GET /act_{ad_account_id}/audience_funnel` |  | Ad account: View performance |  |
| AdAccount | `ad_account.broadtargetingcategories.list` | `GET /act_{ad_account_id}/broadtargetingcategories` |  | Ad account: View performance |  |
| AdAccount | `ad_account.businessprojects.list` | `GET /act_{ad_account_id}/businessprojects` |  | Ad account: View performance |  |
| AdAccount | `ad_account.campaigns.list` | `GET /act_{ad_account_id}/campaigns` |  | Ad account: View performance |  |
| AdAccount | `ad_account.campaignsbylabels.list` | `GET /act_{ad_account_id}/campaignsbylabels` |  | Ad account: View performance |  |
| AdAccount | `ad_account.connected_instagram_accounts.list` | `GET /act_{ad_account_id}/connected_instagram_accounts` |  | Ad account: View performance |  |
| AdAccount | `ad_account.connected_instagram_accounts_with_iabp.list` | `GET /act_{ad_account_id}/connected_instagram_accounts_with_iabp` |  | Ad account: View performance |  |
| AdAccount | `ad_account.conversion_goals.list` | `GET /act_{ad_account_id}/conversion_goals` |  | Ad account: View performance |  |
| AdAccount | `ad_account.customaudiences.list` | `GET /act_{ad_account_id}/customaudiences` |  | Ad account: View performance |  |
| AdAccount | `ad_account.customaudiencestos.list` | `GET /act_{ad_account_id}/customaudiencestos` |  | Ad account: View performance |  |
| AdAccount | `ad_account.customconversions.list` | `GET /act_{ad_account_id}/customconversions` |  | Ad account: View performance |  |
| AdAccount | `ad_account.delivery_estimate.list` | `GET /act_{ad_account_id}/delivery_estimate` |  | Ad account: View performance |  |
| AdAccount | `ad_account.deprecatedtargetingadsets.list` | `GET /act_{ad_account_id}/deprecatedtargetingadsets` |  | Ad account: View performance |  |
| AdAccount | `ad_account.dsa_recommendations.list` | `GET /act_{ad_account_id}/dsa_recommendations` |  | Ad account: View performance |  |
| AdAccount | `ad_account.generatepreviews.list` | `GET /act_{ad_account_id}/generatepreviews` |  | Ad account: View performance, Page: Ads |  |
| AdAccount | `ad_account.impacting_ad_studies.list` | `GET /act_{ad_account_id}/impacting_ad_studies` |  | Ad account: View performance |  |
| AdAccount | `ad_account.insights.list` | `GET /act_{ad_account_id}/insights` |  | Ad account: View performance |  |
| AdAccount | `ad_account.insights.create` | `POST /act_{ad_account_id}/insights` |  | Ad account: View performance |  |
| AdAccount | `ad_account.insights_feature_settings_list_features.list` | `GET /act_{ad_account_id}/insights/feature-settings/list-features` |  | Ad account: View performance |  |
| AdAccount | `ad_account.instagram_accounts.list` | `GET /act_{ad_account_id}/instagram_accounts` |  | Ad account: View performance |  |
| AdAccount | `ad_account.ios_fourteen_campaign_limits.list` | `GET /act_{ad_account_id}/ios_fourteen_campaign_limits` |  | Ad account: View performance |  |
| AdAccount | `ad_account.light_adsets.list` | `GET /act_{ad_account_id}/light_adsets` |  | Ad account: View performance |  |
| AdAccount | `ad_account.light_campaigns.list` | `GET /act_{ad_account_id}/light_campaigns` |  | Ad account: View performance |  |
| AdAccount | `ad_account.lightads.list` | `GET /act_{ad_account_id}/lightads` |  | Ad account: View performance |  |
| AdAccount | `ad_account.matched_search_applications.list` | `GET /act_{ad_account_id}/matched_search_applications` |  | Ad account: View performance |  |
| AdAccount | `ad_account.max_bid.list` | `GET /act_{ad_account_id}/max_bid` |  | Ad account: View performance |  |
| AdAccount | `ad_account.mcmeconversions.list` | `GET /act_{ad_account_id}/mcmeconversions` |  | Ad account: View performance |  |
| AdAccount | `ad_account.message_delivery_estimate.list` | `GET /act_{ad_account_id}/message_delivery_estimate` |  | Ad account: View performance |  |
| AdAccount | `ad_account.minimum_budgets.list` | `GET /act_{ad_account_id}/minimum_budgets` |  | Ad account: View performance |  |
| AdAccount | `ad_account.onbehalf_requests.list` | `GET /act_{ad_account_id}/onbehalf_requests` |  | Ad account: View performance |  |
| AdAccount | `ad_account.promote_pages.list` | `GET /act_{ad_account_id}/promote_pages` |  | Ad account: View performance, Page: Ads |  |
| AdAccount | `ad_account.publisher_block_lists.list` | `GET /act_{ad_account_id}/publisher_block_lists` |  | Ad account: View performance |  |
| AdAccount | `ad_account.reachestimate.list` | `GET /act_{ad_account_id}/reachestimate` |  | Ad account: View performance |  |
| AdAccount | `ad_account.reachfrequencypredictions.list` | `GET /act_{ad_account_id}/reachfrequencypredictions` |  | Ad account: View performance |  |
| AdAccount | `ad_account.recommendations.list` | `GET /act_{ad_account_id}/recommendations` |  | Ad account: View performance |  |
| AdAccount | `ad_account.saved_audiences.list` | `GET /act_{ad_account_id}/saved_audiences` |  | Ad account: View performance |  |
| AdAccount | `ad_account.targetingbrowse.list` | `GET /act_{ad_account_id}/targetingbrowse` |  | Ad account: View performance |  |
| AdAccount | `ad_account.targetingsearch.list` | `GET /act_{ad_account_id}/targetingsearch` |  | Ad account: View performance |  |
| AdAccount | `ad_account.targetingsentencelines.list` | `GET /act_{ad_account_id}/targetingsentencelines` |  | Ad account: View performance |  |
| AdAccount | `ad_account.targetingsuggestions.list` | `GET /act_{ad_account_id}/targetingsuggestions` |  | Ad account: View performance |  |
| AdAccount | `ad_account.targetingvalidation.list` | `GET /act_{ad_account_id}/targetingvalidation` |  | Ad account: View performance |  |
| AdAccount | `ad_account.tracking.list` | `GET /act_{ad_account_id}/tracking` |  | Ad account: View performance |  |
| AdAccount | `ad_account.value_rule_set.list` | `GET /act_{ad_account_id}/value_rule_set` |  | Ad account: View performance |  |
| AdAccount | `ad_account.value_rule_set_translation.create` | `POST /act_{ad_account_id}/value_rule_set_translation` |  | Ad account: View performance |  |
| AdAccount | `ad_account.video_ads.list` | `GET /act_{ad_account_id}/video_ads` |  | Ad account: View performance |  |
| AdAccountAgencyFeeConfig | `ad_account_agency_fee_config.get` | `GET /{ad_account_agency_fee_config_id}` |  | Ad account: View performance |  |
| AdAccountUserSettings | `ad_account_user_settings.get` | `GET /{ad_account_user_settings_id}` |  | Ad account: View performance |  |
| AdAsyncRequest | `ad_async_request.get` | `GET /{ad_async_request_id}` |  | Ad account: View performance |  |
| AdAsyncRequestSet | `ad_async_request_set.get` | `GET /{ad_async_request_set_id}` |  | Ad account: View performance |  |
| AdAsyncRequestSet | `ad_async_request_set.requests.list` | `GET /{ad_async_request_set_id}/requests` |  | Ad account: View performance |  |
| AdColumnSizes | `ad_column_sizes.get` | `GET /{ad_column_sizes_id}` |  | Ad account: View performance |  |
| AdCreationPackageConfig | `ad_creation_package_config.get` | `GET /{ad_creation_package_config_id}` |  | Ad account: View performance |  |
| AdCreative | `ad_creative.get` | `GET /{ad_creative_id}` |  | Ad account: View performance |  |
| AdCreative | `ad_creative.creative_insights.list` | `GET /{ad_creative_id}/creative_insights` |  | Ad account: View performance |  |
| AdCreative | `ad_creative.previews.list` | `GET /{ad_creative_id}/previews` |  | Ad account: View performance, Page: Ads |  |
| AdCustomDerivedMetrics | `ad_custom_derived_metrics.get` | `GET /{ad_custom_derived_metrics_id}` |  | Ad account: View performance |  |
| AdDraft | `ad_draft.get` | `GET /{ad_draft_id}` |  | Ad account: View performance |  |
| AdExportPreset | `ad_export_preset.get` | `GET /{ad_export_preset_id}` |  | Ad account: View performance |  |
| AdImage | `ad_image.get` | `GET /{ad_image_id}` |  | Ad account: View performance |  |
| AdLabel | `ad_label.get` | `GET /{ad_label_id}` |  | Ad account: View performance |  |
| AdLabel | `ad_label.adcreatives.list` | `GET /{ad_label_id}/adcreatives` |  | Ad account: View performance |  |
| AdLabel | `ad_label.ads.list` | `GET /{ad_label_id}/ads` |  | Ad account: View performance |  |
| AdLabel | `ad_label.adsets.list` | `GET /{ad_label_id}/adsets` |  | Ad account: View performance |  |
| AdLabel | `ad_label.campaigns.list` | `GET /{ad_label_id}/campaigns` |  | Ad account: View performance |  |
| AdLightAdgroup | `ad_light_adgroup.get` | `GET /{ad_light_adgroup_id}` |  | Ad account: View performance |  |
| AdLightCampaign | `ad_light_campaign.get` | `GET /{ad_light_campaign_id}` |  | Ad account: View performance |  |
| AdLightCampaignGroup | `ad_light_campaign_group.get` | `GET /{ad_light_campaign_group_id}` |  | Ad account: View performance |  |
| AdPlacePageSet | `ad_place_page_set.get` | `GET /{ad_place_page_set_id}` |  | Ad account: View performance |  |
| AdPlacement | `ad_placement.get` | `GET /{ad_placement_id}` |  | Business: Admin, Business: Employee |  |
| AdProposal | `ad_proposal.get` | `GET /{ad_proposal_id}` |  | Ad account: View performance |  |
| AdReportRun | `ad_report_run.get` | `GET /{ad_report_run_id}` |  | Ad account: View performance |  |
| AdReportRun | `ad_report_run.insights.list` | `GET /{ad_report_run_id}/insights` |  | Ad account: View performance |  |
| AdRule | `ad_rule.get` | `GET /{ad_rule_id}` |  | Ad account: View performance |  |
| AdRule | `ad_rule.history.list` | `GET /{ad_rule_id}/history` |  | Ad account: View performance |  |
| AdRule | `ad_rule.preview.create` | `POST /{ad_rule_id}/preview` |  | Ad account: View performance |  |
| AdRuleEvaluationSpec | `ad_rule_evaluation_spec.get` | `GET /{ad_rule_evaluation_spec_id}` |  | Ad account: View performance |  |
| AdRuleExecutionSpec | `ad_rule_execution_spec.get` | `GET /{ad_rule_execution_spec_id}` |  | Ad account: View performance |  |
| AdSavedKeywords | `ad_saved_keywords.get` | `GET /{ad_saved_keywords_id}` |  | Ad account: View performance |  |
| AdSavedLocation | `ad_saved_location.get` | `GET /{ad_saved_location_id}` |  | Ad account: View performance |  |
| AdSavedReport | `ad_saved_report.get` | `GET /{ad_saved_report_id}` |  | Ad account: View performance |  |
| AdSet | `ad_set.get` | `GET /{ad_set_id}` |  | Ad account: View performance |  |
| AdSet | `ad_set.activities.list` | `GET /{ad_set_id}/activities` |  | Ad account: View performance |  |
| AdSet | `ad_set.ad_studies.list` | `GET /{ad_set_id}/ad_studies` |  | Ad account: View performance |  |
| AdSet | `ad_set.adcreatives.list` | `GET /{ad_set_id}/adcreatives` |  | Ad account: View performance |  |
| AdSet | `ad_set.adrules_governed.list` | `GET /{ad_set_id}/adrules_governed` |  | Ad account: View performance |  |
| AdSet | `ad_set.ads.list` | `GET /{ad_set_id}/ads` |  | Ad account: View performance |  |
| AdSet | `ad_set.asyncadrequests.list` | `GET /{ad_set_id}/asyncadrequests` |  | Ad account: View performance |  |
| AdSet | `ad_set.budget_schedules.list` | `GET /{ad_set_id}/budget_schedules` |  | Ad account: View performance |  |
| AdSet | `ad_set.copies.list` | `GET /{ad_set_id}/copies` |  | Ad account: View performance |  |
| AdSet | `ad_set.delivery_estimate.list` | `GET /{ad_set_id}/delivery_estimate` |  | Ad account: View performance |  |
| AdSet | `ad_set.insights.list` | `GET /{ad_set_id}/insights` |  | Ad account: View performance |  |
| AdSet | `ad_set.insights.create` | `POST /{ad_set_id}/insights` |  | Ad account: View performance |  |
| AdSet | `ad_set.message_delivery_estimate.list` | `GET /{ad_set_id}/message_delivery_estimate` |  | Ad account: View performance |  |
| AdSet | `ad_set.targetingsentencelines.list` | `GET /{ad_set_id}/targetingsentencelines` |  | Ad account: View performance |  |
| AdStudy | `ad_study.get` | `GET /{ad_study_id}` |  | Ad account: View performance |  |
| AdStudy | `ad_study.cells.list` | `GET /{ad_study_id}/cells` |  | Ad account: View performance |  |
| AdStudy | `ad_study.instances.list` | `GET /{ad_study_id}/instances` |  | Ad account: View performance |  |
| AdStudy | `ad_study.objectives.list` | `GET /{ad_study_id}/objectives` |  | Ad account: View performance |  |
| AdStudyCell | `ad_study_cell.get` | `GET /{ad_study_cell_id}` |  | Ad account: View performance |  |
| AdStudyCell | `ad_study_cell.adaccounts.list` | `GET /{ad_study_cell_id}/adaccounts` |  | Ad account: View performance |  |
| AdStudyCell | `ad_study_cell.adsets.list` | `GET /{ad_study_cell_id}/adsets` |  | Ad account: View performance |  |
| AdStudyCell | `ad_study_cell.campaigns.list` | `GET /{ad_study_cell_id}/campaigns` |  | Ad account: View performance |  |
| AdStudyObjective | `ad_study_objective.get` | `GET /{ad_study_objective_id}` |  | Ad account: View performance |  |
| AdStudyObjective | `ad_study_objective.adspixels.list` | `GET /{ad_study_objective_id}/adspixels` |  | Ad account: View performance |  |
| AdStudyObjective | `ad_study_objective.applications.list` | `GET /{ad_study_objective_id}/applications` |  | Ad account: View performance |  |
| AdStudyObjective | `ad_study_objective.brand_requests.list` | `GET /{ad_study_objective_id}/brand_requests` |  | Ad account: View performance |  |
| AdStudyObjective | `ad_study_objective.customconversions.list` | `GET /{ad_study_objective_id}/customconversions` |  | Ad account: View performance |  |
| AdStudyObjective | `ad_study_objective.offline_conversion_data_sets.list` | `GET /{ad_study_objective_id}/offline_conversion_data_sets` |  | Ad account: View performance |  |
| AdStudyObjective | `ad_study_objective.partner_private_studies.list` | `GET /{ad_study_objective_id}/partner_private_studies` |  | Ad account: View performance |  |
| AdStudyObjective | `ad_study_objective.partnerstudies.list` | `GET /{ad_study_objective_id}/partnerstudies` |  | Ad account: View performance |  |
| AdTopline | `ad_topline.get` | `GET /{ad_topline_id}` |  | Ad account: View performance |  |
| AdToplineDetail | `ad_topline_detail.get` | `GET /{ad_topline_detail_id}` |  | Ad account: View performance |  |
| AdVideo | `ad_video.get` | `GET /{ad_video_id}` |  | Ad account: View performance |  |
| AdVideo | `ad_video.boost_ads_list.list` | `GET /{ad_video_id}/boost_ads_list` |  | Ad account: View performance |  |
| AdVideo | `ad_video.captions.list` | `GET /{ad_video_id}/captions` |  | Ad account: View performance |  |
| AdVideo | `ad_video.collaborators.list` | `GET /{ad_video_id}/collaborators` |  | Ad account: View performance |  |
| AdVideo | `ad_video.comments.list` | `GET /{ad_video_id}/comments` |  | Ad account: View performance |  |
| AdVideo | `ad_video.crosspost_shared_pages.list` | `GET /{ad_video_id}/crosspost_shared_pages` |  | Ad account: View performance |  |
| AdVideo | `ad_video.likes.list` | `GET /{ad_video_id}/likes` |  | Ad account: View performance |  |
| AdVideo | `ad_video.poll_settings.list` | `GET /{ad_video_id}/poll_settings` |  | Ad account: View performance |  |
| AdVideo | `ad_video.polls.list` | `GET /{ad_video_id}/polls` |  | Ad account: View performance |  |
| AdVideo | `ad_video.sponsor_tags.list` | `GET /{ad_video_id}/sponsor_tags` |  | Ad account: View performance |  |
| AdVideo | `ad_video.tags.list` | `GET /{ad_video_id}/tags` |  | Ad account: View performance |  |
| AdVideo | `ad_video.thumbnails.list` | `GET /{ad_video_id}/thumbnails` |  | Ad account: View performance |  |
| AdVideo | `ad_video.video_insights.list` | `GET /{ad_video_id}/video_insights` |  | Ad account: View performance |  |
| AdgroupFacebookFeedback | `adgroup_facebook_feedback.comments.list` | `GET /{adgroup_facebook_feedback_id}/comments` |  | Ad account: View performance |  |
| AdsConversionGoal | `ads_conversion_goal.get` | `GET /{ads_conversion_goal_id}` |  | Ad account: View performance |  |
| AdsConversionGoal | `ads_conversion_goal.conversion_events.list` | `GET /{ads_conversion_goal_id}/conversion_events` |  | Ad account: View performance |  |
| AdsCreationSavedState | `ads_creation_saved_state.get` | `GET /{ads_creation_saved_state_id}` |  | Ad account: View performance |  |
| AdsMcmeConversion | `ads_mcme_conversion.get` | `GET /{ads_mcme_conversion_id}` |  | Ad account: View performance |  |
| AdsNamingTemplate | `ads_naming_template.get` | `GET /{ads_naming_template_id}` |  | Ad account: View performance |  |
| AdsPivotRules | `ads_pivot_rules.get` | `GET /{ads_pivot_rules_id}` |  | Ad account: View performance |  |
| AdsPixel | `ads_pixel.get` | `GET /{ads_pixel_id}` |  | Dataset: Analyze |  |
| AdsPixel | `ads_pixel.activities.head` | `HEAD /{ads_pixel_id}/activities` |  | Dataset: Analyze |  |
| AdsPixel | `ads_pixel.adaccounts.list` | `GET /{ads_pixel_id}/adaccounts` |  | Dataset: Analyze |  |
| AdsPixel | `ads_pixel.agencies.list` | `GET /{ads_pixel_id}/agencies` |  | Dataset: Analyze |  |
| AdsPixel | `ads_pixel.assigned_users.list` | `GET /{ads_pixel_id}/assigned_users` |  | Dataset: Analyze |  |
| AdsPixel | `ads_pixel.da_checks.list` | `GET /{ads_pixel_id}/da_checks` |  | Dataset: Analyze |  |
| AdsPixel | `ads_pixel.offline_event_uploads.list` | `GET /{ads_pixel_id}/offline_event_uploads` |  | Dataset: Analyze |  |
| AdsPixel | `ads_pixel.openbridge_configurations.list` | `GET /{ads_pixel_id}/openbridge_configurations` |  | Dataset: Analyze |  |
| AdsPixel | `ads_pixel.shared_accounts.list` | `GET /{ads_pixel_id}/shared_accounts` |  | Dataset: Analyze |  |
| AdsPixel | `ads_pixel.shared_agencies.list` | `GET /{ads_pixel_id}/shared_agencies` |  | Dataset: Analyze |  |
| AdsPixel | `ads_pixel.stats.list` | `GET /{ads_pixel_id}/stats` |  | Dataset: Analyze |  |
| AdsPixelCapabilityOverride | `ads_pixel_capability_override.get` | `GET /{ads_pixel_capability_override_id}` |  | Dataset: Analyze |  |
| AdsQuickViews | `ads_quick_views.get` | `GET /{ads_quick_views_id}` |  | Ad account: View performance |  |
| AdsReportBuilderExportCore | `ads_report_builder_export_core.get` | `GET /{ads_report_builder_export_core_id}` |  | Ad account: View performance |  |
| AdsReportBuilderMMMReport | `ads_report_builder_mmm_report.get` | `GET /{ads_report_builder_mmm_report_id}` |  | Ad account: View performance |  |
| AdsReportBuilderMMMReportScheduler | `ads_report_builder_mmm_report_scheduler.get` | `GET /{ads_report_builder_mmm_report_scheduler_id}` |  | Ad account: View performance |  |
| AdsReportBuilderSavedReport | `ads_report_builder_saved_report.get` | `GET /{ads_report_builder_saved_report_id}` |  | Ad account: View performance |  |
| AdsStartYourDayWidget | `ads_start_your_day_widget.get` | `GET /{ads_start_your_day_widget_id}` |  | Ad account: View performance |  |
| AdsUserSettings | `ads_user_settings.get` | `GET /{ads_user_settings_id}` |  | Ad account: View performance |  |
| AdsValueAdjustmentRuleCollection | `ads_value_adjustment_rule_collection.get` | `GET /{ads_value_adjustment_rule_collection_id}` |  | Ad account: View performance |  |
| AdsValueAdjustmentRuleCollection | `ads_value_adjustment_rule_collection.rules.list` | `GET /{ads_value_adjustment_rule_collection_id}/rules` |  | Ad account: View performance |  |
| AdvAInstance | `adv_a_instance.get` | `GET /{adv_a_instance_id}` |  | Ad account: View performance |  |
| AppEventConfig | `app_event_config.get` | `GET /{app_event_config_id}` |  | App: Admin, App: Developer, App: Advertiser |  |
| Application | `application.get` | `GET /{application_id}` |  | App: Admin, App: Developer, App: Advertiser |  |
| Application | `application.aem_attribution.list` | `GET /{application_id}/aem_attribution` |  | App: Admin, App: Developer, App: Advertiser |  |
| Application | `application.aem_conversion_configs.list` | `GET /{application_id}/aem_conversion_configs` |  | App: Admin, App: Developer, App: Advertiser |  |
| Application | `application.aem_conversion_filter.list` | `GET /{application_id}/aem_conversion_filter` |  | App: Admin, App: Developer, App: Advertiser |  |
| Application | `application.agencies.list` | `GET /{application_id}/agencies` |  | App: Admin, App: Developer, App: Advertiser |  |
| Application | `application.app_capi_settings.list` | `GET /{application_id}/app_capi_settings` |  | App: Admin, App: Developer, App: Advertiser |  |
| Application | `application.app_event_types.list` | `GET /{application_id}/app_event_types` |  | App: Admin, App: Developer, App: Advertiser |  |
| Application | `application.authorized_adaccounts.list` | `GET /{application_id}/authorized_adaccounts` |  | App: Admin, App: Developer, App: Advertiser |  |
| Application | `application.connected_client_businesses.list` | `GET /{application_id}/connected_client_businesses` |  | App: Admin, App: Developer, App: Advertiser |  |
| Application | `application.da_checks.list` | `GET /{application_id}/da_checks` |  | App: Admin, App: Developer, App: Advertiser |  |
| Application | `application.linked_dataset.list` | `GET /{application_id}/linked_dataset` |  | App: Admin, App: Developer, App: Advertiser |  |
| ArAdsDataContainer | `ar_ads_data_container.get` | `GET /{ar_ads_data_container_id}` |  | Page: Insights |  |
| AutomotiveModel | `automotive_model.get` | `GET /{automotive_model_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| AutomotiveModel | `automotive_model.channels_to_integrity_status.list` | `GET /{automotive_model_id}/channels_to_integrity_status` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| AutomotiveModel | `automotive_model.override_details.list` | `GET /{automotive_model_id}/override_details` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| AutomotiveModel | `automotive_model.videos_metadata.list` | `GET /{automotive_model_id}/videos_metadata` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| BCPCampaign | `bcp_campaign.get` | `GET /{bcp_campaign_id}` |  | Ad account: View performance |  |
| BidSchedule | `bid_schedule.get` | `GET /{bid_schedule_id}` |  | Ad account: View performance |  |
| BrandRequest | `brand_request.get` | `GET /{brand_request_id}` |  | Ad account: View performance |  |
| BrandSafetyDownloadable | `brand_safety_downloadable.get` | `GET /{brand_safety_downloadable_id}` |  | Ad account: View performance |  |
| Business | `business.get` | `GET /{business_id}` |  | Business: Employee |  |
| Business | `business.ad_account_infos.list` | `GET /{business_id}/ad_account_infos` |  | Business: Employee |  |
| Business | `business.ad_custom_derived_metrics.list` | `GET /{business_id}/ad_custom_derived_metrics` |  | Business: Employee |  |
| Business | `business.ad_studies.list` | `GET /{business_id}/ad_studies` |  | Business: Employee |  |
| Business | `business.adnetworkanalytics.list` | `GET /{business_id}/adnetworkanalytics` |  | Business: Admin, Business: Employee |  |
| Business | `business.adnetworkanalytics.create` | `POST /{business_id}/adnetworkanalytics` |  | Business: Admin, Business: Employee |  |
| Business | `business.adnetworkanalytics_results.list` | `GET /{business_id}/adnetworkanalytics_results` |  | Business: Admin, Business: Employee |  |
| Business | `business.ads_dataset.list` | `GET /{business_id}/ads_dataset` |  | Business: Employee |  |
| Business | `business.ads_reporting_mmm_reports.list` | `GET /{business_id}/ads_reporting_mmm_reports` |  | Business: Employee |  |
| Business | `business.ads_reporting_mmm_schedulers.list` | `GET /{business_id}/ads_reporting_mmm_schedulers` |  | Business: Employee |  |
| Business | `business.adspixels.list` | `GET /{business_id}/adspixels` |  | Business: Employee |  |
| Business | `business.agencies.list` | `GET /{business_id}/agencies` |  | Business: Employee |  |
| Business | `business.an_placements.list` | `GET /{business_id}/an_placements` |  | Business: Admin, Business: Employee |  |
| Business | `business.business_asset_groups.list` | `GET /{business_id}/business_asset_groups` |  | Business: Employee |  |
| Business | `business.business_invoices.list` | `GET /{business_id}/business_invoices` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| Business | `business.business_users.list` | `GET /{business_id}/business_users` |  | Business: Employee |  |
| Business | `business.businessprojects.list` | `GET /{business_id}/businessprojects` |  | Business: Employee |  |
| Business | `business.catalog_advertiser_insights.list` | `GET /{business_id}/catalog_advertiser_insights` |  | Business: Employee |  |
| Business | `business.client_ad_accounts.list` | `GET /{business_id}/client_ad_accounts` |  | Business: Employee |  |
| Business | `business.client_apps.list` | `GET /{business_id}/client_apps` |  | Business: Employee |  |
| Business | `business.client_instagram_assets.list` | `GET /{business_id}/client_instagram_assets` |  | Business: Employee |  |
| Business | `business.client_offsite_signal_container_business_objects.list` | `GET /{business_id}/client_offsite_signal_container_business_objects` |  | Business: Employee |  |
| Business | `business.client_pages.list` | `GET /{business_id}/client_pages` |  | Business: Employee |  |
| Business | `business.client_pixels.list` | `GET /{business_id}/client_pixels` |  | Business: Employee |  |
| Business | `business.client_product_catalogs.list` | `GET /{business_id}/client_product_catalogs` |  | Business: Employee |  |
| Business | `business.client_whatsapp_business_accounts.list` | `GET /{business_id}/client_whatsapp_business_accounts` |  | Business: Employee |  |
| Business | `business.clients.list` | `GET /{business_id}/clients` |  | Business: Employee |  |
| Business | `business.collaborative_ads_collaboration_requests.list` | `GET /{business_id}/collaborative_ads_collaboration_requests` |  | Business: Employee |  |
| Business | `business.collaborative_ads_suggested_partners.list` | `GET /{business_id}/collaborative_ads_suggested_partners` |  | Business: Employee |  |
| Business | `business.commerce_merchant_settings.list` | `GET /{business_id}/commerce_merchant_settings` |  | Business: Employee |  |
| Business | `business.cpas_business_setup_config.list` | `GET /{business_id}/cpas_business_setup_config` |  | Business: Employee |  |
| Business | `business.cpas_merchant_config.list` | `GET /{business_id}/cpas_merchant_config` |  | Business: Employee |  |
| Business | `business.creditcards.list` | `GET /{business_id}/creditcards` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| Business | `business.event_source_groups.list` | `GET /{business_id}/event_source_groups` |  | Business: Employee |  |
| Business | `business.extendedcreditapplications.list` | `GET /{business_id}/extendedcreditapplications` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| Business | `business.extendedcredits.list` | `GET /{business_id}/extendedcredits` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| Business | `business.initiated_audience_sharing_requests.list` | `GET /{business_id}/initiated_audience_sharing_requests` |  | Business: Employee |  |
| Business | `business.instagram_accounts.list` | `GET /{business_id}/instagram_accounts` |  | Business: Employee |  |
| Business | `business.instagram_business_accounts.list` | `GET /{business_id}/instagram_business_accounts` |  | Business: Employee |  |
| Business | `business.managed_partner_ads_funding_source_details.list` | `GET /{business_id}/managed_partner_ads_funding_source_details` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| Business | `business.openbridge_configurations.list` | `GET /{business_id}/openbridge_configurations` |  | Business: Employee |  |
| Business | `business.owned_ad_accounts.list` | `GET /{business_id}/owned_ad_accounts` |  | Business: Employee |  |
| Business | `business.owned_apps.list` | `GET /{business_id}/owned_apps` |  | Business: Employee |  |
| Business | `business.owned_businesses.list` | `GET /{business_id}/owned_businesses` |  | Business: Employee |  |
| Business | `business.owned_instagram_accounts.list` | `GET /{business_id}/owned_instagram_accounts` |  | Business: Employee |  |
| Business | `business.owned_instagram_assets.list` | `GET /{business_id}/owned_instagram_assets` |  | Business: Employee |  |
| Business | `business.owned_offsite_signal_container_business_objects.list` | `GET /{business_id}/owned_offsite_signal_container_business_objects` |  | Business: Employee |  |
| Business | `business.owned_pages.list` | `GET /{business_id}/owned_pages` |  | Business: Employee |  |
| Business | `business.owned_pixels.list` | `GET /{business_id}/owned_pixels` |  | Business: Employee |  |
| Business | `business.owned_product_catalogs.list` | `GET /{business_id}/owned_product_catalogs` |  | Business: Employee |  |
| Business | `business.owned_whatsapp_business_accounts.list` | `GET /{business_id}/owned_whatsapp_business_accounts` |  | Business: Employee |  |
| Business | `business.partner_account_linking.list` | `GET /{business_id}/partner_account_linking` |  | Business: Employee |  |
| Business | `business.pending_client_ad_accounts.list` | `GET /{business_id}/pending_client_ad_accounts` |  | Business: Employee |  |
| Business | `business.pending_client_apps.list` | `GET /{business_id}/pending_client_apps` |  | Business: Employee |  |
| Business | `business.pending_client_pages.list` | `GET /{business_id}/pending_client_pages` |  | Business: Employee |  |
| Business | `business.pending_owned_ad_accounts.list` | `GET /{business_id}/pending_owned_ad_accounts` |  | Business: Employee |  |
| Business | `business.pending_owned_pages.list` | `GET /{business_id}/pending_owned_pages` |  | Business: Employee |  |
| Business | `business.pending_shared_offsite_signal_container_business_objects.list` | `GET /{business_id}/pending_shared_offsite_signal_container_business_objects` |  | Business: Employee |  |
| Business | `business.pending_users.list` | `GET /{business_id}/pending_users` |  | Business: Employee |  |
| Business | `business.picture.list` | `GET /{business_id}/picture` |  | Business: Employee |  |
| Business | `business.preverified_numbers.list` | `GET /{business_id}/preverified_numbers` |  | Business: Employee |  |
| Business | `business.received_audience_sharing_requests.list` | `GET /{business_id}/received_audience_sharing_requests` |  | Business: Employee |  |
| Business | `business.recommendations.list` | `GET /{business_id}/recommendations` |  | Business: Employee |  |
| Business | `business.reseller_guidances.list` | `GET /{business_id}/reseller_guidances` |  | Business: Employee |  |
| Business | `business.self_certified_whatsapp_business_submissions.list` | `GET /{business_id}/self_certified_whatsapp_business_submissions` |  | Business: Employee |  |
| Business | `business.system_users.list` | `GET /{business_id}/system_users` |  | Business: Employee |  |
| Business | `business.third_party_measurement_report_dataset.list` | `GET /{business_id}/third_party_measurement_report_dataset` |  | Business: Employee |  |
| BusinessAgreement | `business_agreement.get` | `GET /{business_agreement_id}` |  | Business: Employee |  |
| BusinessAssetGroup | `business_asset_group.get` | `GET /{business_asset_group_id}` |  | Business: Employee |  |
| BusinessAssetGroup | `business_asset_group.assigned_users.list` | `GET /{business_asset_group_id}/assigned_users` |  | Business: Employee |  |
| BusinessAssetGroup | `business_asset_group.contained_adaccounts.list` | `GET /{business_asset_group_id}/contained_adaccounts` |  | Business: Employee |  |
| BusinessAssetGroup | `business_asset_group.contained_applications.list` | `GET /{business_asset_group_id}/contained_applications` |  | Business: Employee |  |
| BusinessAssetGroup | `business_asset_group.contained_custom_conversions.list` | `GET /{business_asset_group_id}/contained_custom_conversions` |  | Business: Employee |  |
| BusinessAssetGroup | `business_asset_group.contained_instagram_accounts.list` | `GET /{business_asset_group_id}/contained_instagram_accounts` |  | Business: Employee |  |
| BusinessAssetGroup | `business_asset_group.contained_pages.list` | `GET /{business_asset_group_id}/contained_pages` |  | Business: Employee |  |
| BusinessAssetGroup | `business_asset_group.contained_pixels.list` | `GET /{business_asset_group_id}/contained_pixels` |  | Business: Employee |  |
| BusinessAssetGroup | `business_asset_group.contained_product_catalogs.list` | `GET /{business_asset_group_id}/contained_product_catalogs` |  | Business: Employee |  |
| BusinessAssetSharingAgreement | `business_asset_sharing_agreement.get` | `GET /{business_asset_sharing_agreement_id}` |  | Business: Employee |  |
| BusinessCreativeFolder | `business_creative_folder.get` | `GET /{business_creative_folder_id}` |  | Business: Employee |  |
| BusinessFranchiseConfig | `business_franchise_config.get` | `GET /{business_franchise_config_id}` |  | Business: Employee |  |
| BusinessImage | `business_image.get` | `GET /{business_image_id}` |  | Business: Employee |  |
| BusinessObjectTransferOwnershipAgreement | `business_object_transfer_ownership_agreement.get` | `GET /{business_object_transfer_ownership_agreement_id}` |  | Business: Employee |  |
| BusinessOwnedObjectOnBehalfOfRequest | `business_owned_object_on_behalf_of_request.get` | `GET /{business_owned_object_on_behalf_of_request_id}` |  | Business: Employee |  |
| BusinessProject | `business_project.get` | `GET /{business_project_id}` |  | Business: Employee |  |
| BusinessRequest | `business_request.get` | `GET /{business_request_id}` |  | Business: Employee |  |
| BusinessRoleRequest | `business_role_request.get` | `GET /{business_role_request_id}` |  | Business: Employee |  |
| BusinessTag | `business_tag.get` | `GET /{business_tag_id}` |  | Business: Employee |  |
| BusinessUser | `business_user.get` | `GET /{business_user_id}` |  | Business: Employee |  |
| BusinessUser | `business_user.assigned_ad_accounts.list` | `GET /{business_user_id}/assigned_ad_accounts` |  | Business: Employee |  |
| BusinessUser | `business_user.assigned_business_asset_groups.list` | `GET /{business_user_id}/assigned_business_asset_groups` |  | Business: Employee |  |
| BusinessUser | `business_user.assigned_pages.list` | `GET /{business_user_id}/assigned_pages` |  | Business: Employee |  |
| BusinessUser | `business_user.assigned_product_catalogs.list` | `GET /{business_user_id}/assigned_product_catalogs` |  | Business: Employee |  |
| BusinessUser | `business_user.assigned_whatsapp_business_accounts.list` | `GET /{business_user_id}/assigned_whatsapp_business_accounts` |  | Business: Employee |  |
| BusinessVideo | `business_video.get` | `GET /{business_video_id}` |  | Business: Employee |  |
| CPASAdCreationTemplate | `cpas_ad_creation_template.get` | `GET /{cpas_ad_creation_template_id}` |  | Business: Employee |  |
| CPASAdvertiserPartnershipRecommendation | `cpas_advertiser_partnership_recommendation.get` | `GET /{cpas_advertiser_partnership_recommendation_id}` |  | Business: Employee |  |
| CPASBusinessSetupConfig | `cpas_business_setup_config.get` | `GET /{cpas_business_setup_config_id}` |  | Business: Employee |  |
| CPASBusinessSetupConfig | `cpas_business_setup_config.ad_accounts.list` | `GET /{cpas_business_setup_config_id}/ad_accounts` |  | Business: Employee |  |
| CPASCollaborationRequest | `cpas_collaboration_request.get` | `GET /{cpas_collaboration_request_id}` |  | Business: Employee |  |
| CPASLsbImageBank | `cpas_lsb_image_bank.get` | `GET /{cpas_lsb_image_bank_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| CPASLsbImageBank | `cpas_lsb_image_bank.backup_images.list` | `GET /{cpas_lsb_image_bank_id}/backup_images` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| CPASMerchantConfig | `cpas_merchant_config.get` | `GET /{cpas_merchant_config_id}` |  | Business: Employee |  |
| CPASParentCatalogSettings | `cpas_parent_catalog_settings.get` | `GET /{cpas_parent_catalog_settings_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| CTXPartnerAppWelcomeMessageFlow | `ctx_partner_app_welcome_message_flow.get` | `GET /{ctx_partner_app_welcome_message_flow_id}` |  | Page: Insights |  |
| CalibratorExistingRule | `calibrator_existing_rule.get` | `GET /{calibrator_existing_rule_id}` |  | Ad account: View performance |  |
| CallAdsPhoneData | `call_ads_phone_data.get` | `GET /{call_ads_phone_data_id}` |  | Ad account: View performance |  |
| Campaign | `campaign.get` | `GET /{campaign_id}` |  | Ad account: View performance |  |
| Campaign | `campaign.ad_studies.list` | `GET /{campaign_id}/ad_studies` |  | Ad account: View performance |  |
| Campaign | `campaign.adrules_governed.list` | `GET /{campaign_id}/adrules_governed` |  | Ad account: View performance |  |
| Campaign | `campaign.ads.list` | `GET /{campaign_id}/ads` |  | Ad account: View performance |  |
| Campaign | `campaign.adsets.list` | `GET /{campaign_id}/adsets` |  | Ad account: View performance |  |
| Campaign | `campaign.budget_schedules.list` | `GET /{campaign_id}/budget_schedules` |  | Ad account: View performance |  |
| Campaign | `campaign.copies.list` | `GET /{campaign_id}/copies` |  | Ad account: View performance |  |
| Campaign | `campaign.insights.list` | `GET /{campaign_id}/insights` |  | Ad account: View performance |  |
| Campaign | `campaign.insights.create` | `POST /{campaign_id}/insights` |  | Ad account: View performance |  |
| Canvas | `canvas.get` | `GET /{canvas_id}` |  | Page: Ads |  |
| Canvas | `canvas.preview.list` | `GET /{canvas_id}/preview` |  | Page: Ads |  |
| Canvas | `canvas.previews.list` | `GET /{canvas_id}/previews` |  | Page: Ads |  |
| CanvasDynamicSetting | `canvas_dynamic_setting.get` | `GET /{canvas_dynamic_setting_id}` |  | Page: Insights |  |
| CanvasTemplate | `canvas_template.get` | `GET /{canvas_template_id}` |  | Page: Insights |  |
| CatalogContentVersionConfig | `catalog_content_version_config.get` | `GET /{catalog_content_version_config_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| CatalogItemOverride | `catalog_item_override.get` | `GET /{catalog_item_override_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| CatalogSmartPixelSettings | `catalog_smart_pixel_settings.get` | `GET /{catalog_smart_pixel_settings_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| CatalogWebsiteOnboardingSettings | `catalog_website_onboarding_settings.get` | `GET /{catalog_website_onboarding_settings_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| CatalogWebsiteSettings | `catalog_website_settings.get` | `GET /{catalog_website_settings_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ChinaBusinessOnboardingVettingRequest | `china_business_onboarding_vetting_request.get` | `GET /{china_business_onboarding_vetting_request_id}` |  | Business: Employee |  |
| CloudGame | `cloud_game.get` | `GET /{cloud_game_id}` |  | Ad account: View performance |  |
| CollaborativeAdsShareSettings | `collaborative_ads_share_settings.get` | `GET /{collaborative_ads_share_settings_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Comment | `comment.get` | `GET /{comment_id}` |  | Page: Community activity, Page: Insights |  |
| Comment | `comment.comments.list` | `GET /{comment_id}/comments` |  | Page: Community activity, Page: Insights |  |
| Comment | `comment.likes.list` | `GET /{comment_id}/likes` |  | Page: Community activity, Page: Insights |  |
| Comment | `comment.reactions.list` | `GET /{comment_id}/reactions` |  | Page: Community activity, Page: Insights |  |
| ContentBlockList | `content_block_list.get` | `GET /{content_block_list_id}` |  | Ad account: View performance |  |
| ContentBlockList | `content_block_list.applied_ad_accounts.list` | `GET /{content_block_list_id}/applied_ad_accounts` |  | Ad account: View performance |  |
| ContentBlockList | `content_block_list.facebook_content.list` | `GET /{content_block_list_id}/facebook_content` |  | Ad account: View performance |  |
| ContentBlockList | `content_block_list.instagram_content.list` | `GET /{content_block_list_id}/instagram_content` |  | Ad account: View performance |  |
| CreatorAssetCreative | `creator_asset_creative.get` | `GET /{creator_asset_creative_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| CreditCard | `credit_card.get` | `GET /{credit_card_id}` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| CustomAudience | `custom_audience.get` | `GET /{custom_audience_id}` |  | Ad account: View performance |  |
| CustomAudience | `custom_audience.adaccounts.list` | `GET /{custom_audience_id}/adaccounts` |  | Ad account: View performance |  |
| CustomAudience | `custom_audience.ads.list` | `GET /{custom_audience_id}/ads` |  | Ad account: View performance |  |
| CustomAudience | `custom_audience.health.list` | `GET /{custom_audience_id}/health` |  | Ad account: View performance |  |
| CustomAudience | `custom_audience.salts.list` | `GET /{custom_audience_id}/salts` |  | Ad account: View performance |  |
| CustomAudience | `custom_audience.sessions.list` | `GET /{custom_audience_id}/sessions` |  | Ad account: View performance |  |
| CustomAudience | `custom_audience.shared_account_info.list` | `GET /{custom_audience_id}/shared_account_info` |  | Ad account: View performance |  |
| CustomConversion | `custom_conversion.get` | `GET /{custom_conversion_id}` |  | Ad account: View performance |  |
| CustomConversion | `custom_conversion.stats.list` | `GET /{custom_conversion_id}/stats` |  | Ad account: View performance |  |
| Destination | `destination.get` | `GET /{destination_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Destination | `destination.channels_to_integrity_status.list` | `GET /{destination_id}/channels_to_integrity_status` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Destination | `destination.override_details.list` | `GET /{destination_id}/override_details` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Destination | `destination.videos_metadata.list` | `GET /{destination_id}/videos_metadata` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| DynamicARMetadata | `dynamic_ar_metadata.get` | `GET /{dynamic_ar_metadata_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| DynamicItemDisplayBundle | `dynamic_item_display_bundle.get` | `GET /{dynamic_item_display_bundle_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| DynamicItemDisplayBundleFolder | `dynamic_item_display_bundle_folder.get` | `GET /{dynamic_item_display_bundle_folder_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| DynamicPriceConfigByDate | `dynamic_price_config_by_date.get` | `GET /{dynamic_price_config_by_date_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| DynamicVideoMetadata | `dynamic_video_metadata.get` | `GET /{dynamic_video_metadata_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| EventSourceGroup | `event_source_group.get` | `GET /{event_source_group_id}` |  | Dataset: Analyze |  |
| EventSourceGroup | `event_source_group.shared_accounts.list` | `GET /{event_source_group_id}/shared_accounts` |  | Dataset: Analyze |  |
| ExtendedCredit | `extended_credit.get` | `GET /{extended_credit_id}` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| ExtendedCredit | `extended_credit.extended_credit_invoice_groups.list` | `GET /{extended_credit_id}/extended_credit_invoice_groups` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| ExtendedCredit | `extended_credit.owning_credit_allocation_configs.list` | `GET /{extended_credit_id}/owning_credit_allocation_configs` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| ExtendedCreditAllocationConfig | `extended_credit_allocation_config.get` | `GET /{extended_credit_allocation_config_id}` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| ExtendedCreditApplication | `extended_credit_application.get` | `GET /{extended_credit_application_id}` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| ExtendedCreditInvoiceGroup | `extended_credit_invoice_group.get` | `GET /{extended_credit_invoice_group_id}` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| ExtendedCreditInvoiceGroup | `extended_credit_invoice_group.ad_accounts.list` | `GET /{extended_credit_invoice_group_id}/ad_accounts` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| Flight | `flight.get` | `GET /{flight_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Flight | `flight.channels_to_integrity_status.list` | `GET /{flight_id}/channels_to_integrity_status` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Flight | `flight.override_details.list` | `GET /{flight_id}/override_details` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Flight | `flight.videos_metadata.list` | `GET /{flight_id}/videos_metadata` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| FranchiseProgram | `franchise_program.get` | `GET /{franchise_program_id}` |  | Business: Employee |  |
| FranchiseProgramMember | `franchise_program_member.get` | `GET /{franchise_program_member_id}` |  | Business: Employee |  |
| HighDemandPeriod | `high_demand_period.get` | `GET /{high_demand_period_id}` |  | Ad account: View performance |  |
| HomeListing | `home_listing.get` | `GET /{home_listing_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| HomeListing | `home_listing.channels_to_integrity_status.list` | `GET /{home_listing_id}/channels_to_integrity_status` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| HomeListing | `home_listing.override_details.list` | `GET /{home_listing_id}/override_details` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| HomeListing | `home_listing.videos_metadata.list` | `GET /{home_listing_id}/videos_metadata` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Hotel | `hotel.get` | `GET /{hotel_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Hotel | `hotel.channels_to_integrity_status.list` | `GET /{hotel_id}/channels_to_integrity_status` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Hotel | `hotel.hotel_rooms.list` | `GET /{hotel_id}/hotel_rooms` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Hotel | `hotel.override_details.list` | `GET /{hotel_id}/override_details` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Hotel | `hotel.videos_metadata.list` | `GET /{hotel_id}/videos_metadata` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| HotelRoom | `hotel_room.get` | `GET /{hotel_room_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| HotelRoom | `hotel_room.pricing_variables.list` | `GET /{hotel_room_id}/pricing_variables` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| IGBCAdsPermission | `igbc_ads_permission.get` | `GET /{igbc_ads_permission_id}` |  | Instagram account: Ads |  |
| IGBoostMediaAd | `ig_boost_media_ad.get` | `GET /{ig_boost_media_ad_id}` |  | Instagram account: Ads, Instagram account: Insights |  |
| IGComment | `ig_comment.get` | `GET /{ig_comment_id}` |  | Instagram account: Insights, Instagram account: Community activity |  |
| IGComment | `ig_comment.replies.list` | `GET /{ig_comment_id}/replies` |  | Instagram account: Insights, Instagram account: Community activity |  |
| IGMedia | `ig_media.get` | `GET /{ig_media_id}` |  | Instagram account: Ads, Instagram account: Insights |  |
| IGMedia | `ig_media.boost_ads_list.list` | `GET /{ig_media_id}/boost_ads_list` |  | Instagram account: Ads, Instagram account: Insights |  |
| IGMedia | `ig_media.branded_content_partner_promote.list` | `GET /{ig_media_id}/branded_content_partner_promote` |  | Instagram account: Ads |  |
| IGMedia | `ig_media.children.list` | `GET /{ig_media_id}/children` |  | Instagram account: Ads, Instagram account: Insights |  |
| IGMedia | `ig_media.comments.list` | `GET /{ig_media_id}/comments` |  | Instagram account: Insights, Instagram account: Community activity |  |
| IGMediaBoostEligibilityInfo | `ig_media_boost_eligibility_info.get` | `GET /{ig_media_boost_eligibility_info_id}` |  | Instagram account: Ads, Instagram account: Insights |  |
| IGUser | `ig_user.get` | `GET /{ig_user_id}` |  | Instagram account: Ads, Instagram account: Insights |  |
| IGUser | `ig_user.agencies.list` | `GET /{ig_user_id}/agencies` |  | Instagram account: Ads, Instagram account: Insights |  |
| IGUser | `ig_user.authorized_adaccounts.list` | `GET /{ig_user_id}/authorized_adaccounts` |  | Instagram account: Ads |  |
| IGUser | `ig_user.available_catalogs.list` | `GET /{ig_user_id}/available_catalogs` |  | Instagram account: Ads, Instagram account: Insights |  |
| IGUser | `ig_user.branded_content_ad_permissions.list` | `GET /{ig_user_id}/branded_content_ad_permissions` |  | Instagram account: Ads |  |
| IGUser | `ig_user.branded_content_advertisable_medias.list` | `GET /{ig_user_id}/branded_content_advertisable_medias` |  | Instagram account: Ads |  |
| IGUser | `ig_user.branded_content_tag_approval.list` | `GET /{ig_user_id}/branded_content_tag_approval` |  | Instagram account: Ads |  |
| IGUser | `ig_user.connected_threads_user.list` | `GET /{ig_user_id}/connected_threads_user` |  | Instagram account: Ads, Instagram account: Insights |  |
| IGUser | `ig_user.creator_marketplace_creators.list` | `GET /{ig_user_id}/creator_marketplace_creators` |  | Instagram account: Ads, Instagram account: Insights |  |
| IGUser | `ig_user.dataset.list` | `GET /{ig_user_id}/dataset` |  | Instagram account: Ads, Instagram account: Insights |  |
| IGUser | `ig_user.instagram_backed_threads_user.list` | `GET /{ig_user_id}/instagram_backed_threads_user` |  | Instagram account: Ads, Instagram account: Insights |  |
| IGUser | `ig_user.media.list` | `GET /{ig_user_id}/media` |  | Instagram account: Ads, Instagram account: Insights |  |
| IGUser | `ig_user.welcome_message_flows.list` | `GET /{ig_user_id}/welcome_message_flows` |  | Instagram account: Ads, Instagram account: Insights |  |
| InstagramBusinessAsset | `instagram_business_asset.get` | `GET /{instagram_business_asset_id}` |  | Business: Employee |  |
| InstagramBusinessAsset | `instagram_business_asset.agencies.list` | `GET /{instagram_business_asset_id}/agencies` |  | Business: Employee |  |
| InstagramBusinessAsset | `instagram_business_asset.assigned_users.list` | `GET /{instagram_business_asset_id}/assigned_users` |  | Business: Employee |  |
| InstagramUser | `instagram_user.get` | `GET /{instagram_user_id}` |  | Instagram account: Ads, Instagram account: Insights |  |
| InstagramUser | `instagram_user.upcoming_events.list` | `GET /{instagram_user_id}/upcoming_events` |  | Instagram account: Ads, Instagram account: Insights |  |
| Lead | `lead.get` | `GET /{lead_id}` |  | Page: Ads, Page: Leads access |  |
| LeadGenDataDraft | `lead_gen_data_draft.get` | `GET /{lead_gen_data_draft_id}` |  | Page: Ads |  |
| LeadGenDirectCRMIntegrationConfig | `lead_gen_direct_crm_integration_config.get` | `GET /{lead_gen_direct_crm_integration_config_id}` |  | Page: Ads |  |
| LeadgenForm | `leadgen_form.get` | `GET /{leadgen_form_id}` |  | Page: Ads |  |
| LeadgenForm | `leadgen_form.leads.list` | `GET /{leadgen_form_id}/leads` |  | Page: Ads, Page: Leads access |  |
| LeadgenForm | `leadgen_form.test_leads.list` | `GET /{leadgen_form_id}/test_leads` |  | Page: Ads, Page: Leads access |  |
| LiveVideoAdCampaignConfig | `live_video_ad_campaign_config.get` | `GET /{live_video_ad_campaign_config_id}` |  | Ad account: View performance |  |
| LocalServiceBusiness | `local_service_business.get` | `GET /{local_service_business_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| LocalServiceBusiness | `local_service_business.channels_to_integrity_status.list` | `GET /{local_service_business_id}/channels_to_integrity_status` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| LocalServiceBusiness | `local_service_business.override_details.list` | `GET /{local_service_business_id}/override_details` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| MeasurementReport | `measurement_report.get` | `GET /{measurement_report_id}` |  | Ad account: View performance |  |
| MessengerAdsPartialAutomatedStepList | `messenger_ads_partial_automated_step_list.get` | `GET /{messenger_ads_partial_automated_step_list_id}` |  | Page: Ads |  |
| MessengerAdsPartialAutomatedStepList | `messenger_ads_partial_automated_step_list.steps.list` | `GET /{messenger_ads_partial_automated_step_list_id}/steps` |  | Page: Ads |  |
| OfflineConversionDataSet | `offline_conversion_data_set.get` | `GET /{offline_conversion_data_set_id}` |  | Dataset: Analyze |  |
| OfflineConversionDataSet | `offline_conversion_data_set.adaccounts.list` | `GET /{offline_conversion_data_set_id}/adaccounts` |  | Dataset: Analyze |  |
| OfflineConversionDataSet | `offline_conversion_data_set.agencies.list` | `GET /{offline_conversion_data_set_id}/agencies` |  | Dataset: Analyze |  |
| OfflineConversionDataSet | `offline_conversion_data_set.audiences.list` | `GET /{offline_conversion_data_set_id}/audiences` |  | Dataset: Analyze |  |
| OfflineConversionDataSet | `offline_conversion_data_set.customconversions.list` | `GET /{offline_conversion_data_set_id}/customconversions` |  | Dataset: Analyze |  |
| OfflineConversionDataSet | `offline_conversion_data_set.server_events_permitted_business.list` | `GET /{offline_conversion_data_set_id}/server_events_permitted_business` |  | Dataset: Analyze |  |
| OfflineConversionDataSet | `offline_conversion_data_set.shared_accounts.list` | `GET /{offline_conversion_data_set_id}/shared_accounts` |  | Dataset: Analyze |  |
| OfflineConversionDataSet | `offline_conversion_data_set.shared_agencies.list` | `GET /{offline_conversion_data_set_id}/shared_agencies` |  | Dataset: Analyze |  |
| OfflineConversionDataSet | `offline_conversion_data_set.stats.list` | `GET /{offline_conversion_data_set_id}/stats` |  | Dataset: Analyze |  |
| OfflineConversionDataSet | `offline_conversion_data_set.uploads.list` | `GET /{offline_conversion_data_set_id}/uploads` |  | Dataset: Analyze |  |
| OfflineConversionDataSetUpload | `offline_conversion_data_set_upload.get` | `GET /{offline_conversion_data_set_upload_id}` |  | Dataset: Analyze |  |
| OfflineConversionDataSetUpload | `offline_conversion_data_set_upload.progress.list` | `GET /{offline_conversion_data_set_upload_id}/progress` |  | Dataset: Analyze |  |
| OfflineConversionDataSetUpload | `offline_conversion_data_set_upload.pull_sessions.list` | `GET /{offline_conversion_data_set_upload_id}/pull_sessions` |  | Dataset: Analyze |  |
| OfflineProductItem | `offline_product_item.get` | `GET /{offline_product_item_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| OfflineProductItem | `offline_product_item.channels_to_integrity_status.list` | `GET /{offline_product_item_id}/channels_to_integrity_status` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| OfflineProductItem | `offline_product_item.override_details.list` | `GET /{offline_product_item_id}/override_details` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| OfflineTermsOfService | `offline_terms_of_service.get` | `GET /{offline_terms_of_service_id}` |  | Ad account: View performance |  |
| OffsitePixel | `offsite_pixel.get` | `GET /{offsite_pixel_id}` |  | Dataset: Analyze |  |
| OffsiteSignalContainerBusinessObject | `offsite_signal_container_business_object.get` | `GET /{offsite_signal_container_business_object_id}` |  | Dataset: Analyze |  |
| OffsiteSignalContainerBusinessObject | `offsite_signal_container_business_object.linked_application.list` | `GET /{offsite_signal_container_business_object_id}/linked_application` |  | Dataset: Analyze |  |
| OffsiteSignalContainerBusinessObject | `offsite_signal_container_business_object.linked_page.list` | `GET /{offsite_signal_container_business_object_id}/linked_page` |  | Dataset: Analyze |  |
| OmegaCustomerTrx | `omega_customer_trx.get` | `GET /{omega_customer_trx_id}` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| OmegaCustomerTrx | `omega_customer_trx.campaigns.list` | `GET /{omega_customer_trx_id}/campaigns` |  | Business: Finance analyst, Business: Finance editor, Business: Admin |  |
| OpenBridgeConfiguration | `open_bridge_configuration.get` | `GET /{open_bridge_configuration_id}` |  | Dataset: Analyze |  |
| OwnedDomain | `owned_domain.get` | `GET /{owned_domain_id}` |  | Business: Employee |  |
| Page | `page.get` | `GET /{page_id}` |  | Page: Insights |  |
| Page | `page.ads_eligibility.list` | `GET /{page_id}/ads_eligibility` |  | Page: Ads |  |
| Page | `page.ads_posts.list` | `GET /{page_id}/ads_posts` |  | Page: Ads |  |
| Page | `page.agencies.list` | `GET /{page_id}/agencies` |  | Page: Insights |  |
| Page | `page.ar_experience.list` | `GET /{page_id}/ar_experience` |  | Page: Insights |  |
| Page | `page.assigned_users.list` | `GET /{page_id}/assigned_users` |  | Page: Insights |  |
| Page | `page.businessprojects.list` | `GET /{page_id}/businessprojects` |  | Page: Insights |  |
| Page | `page.canvas_elements.list` | `GET /{page_id}/canvas_elements` |  | Page: Ads |  |
| Page | `page.canvases.list` | `GET /{page_id}/canvases` |  | Page: Ads |  |
| Page | `page.ctx_optimization_eligibility.list` | `GET /{page_id}/ctx_optimization_eligibility` |  | Page: Ads |  |
| Page | `page.dataset.list` | `GET /{page_id}/dataset` |  | Page: Insights |  |
| Page | `page.feed.list` | `GET /{page_id}/feed` |  | Page: Insights, Page: Content |  |
| Page | `page.instagram_accounts.list` | `GET /{page_id}/instagram_accounts` |  | Page: Insights |  |
| Page | `page.leadgen_forms.list` | `GET /{page_id}/leadgen_forms` |  | Page: Ads |  |
| Page | `page.locations.list` | `GET /{page_id}/locations` |  | Page: Insights |  |
| Page | `page.messenger_lead_forms.list` | `GET /{page_id}/messenger_lead_forms` |  | Page: Ads |  |
| Page | `page.page_backed_instagram_accounts.list` | `GET /{page_id}/page_backed_instagram_accounts` |  | Page: Ads |  |
| Page | `page.posts.list` | `GET /{page_id}/posts` |  | Page: Insights |  |
| Page | `page.product_catalogs.list` | `GET /{page_id}/product_catalogs` |  | Page: Insights |  |
| Page | `page.published_posts.list` | `GET /{page_id}/published_posts` |  | Page: Insights |  |
| Page | `page.roles.list` | `GET /{page_id}/roles` |  | Page: Insights |  |
| Page | `page.rtb_dynamic_posts.list` | `GET /{page_id}/rtb_dynamic_posts` |  | Page: Ads |  |
| Page | `page.scheduled_posts.list` | `GET /{page_id}/scheduled_posts` |  | Page: Insights |  |
| Page | `page.store_locations.list` | `GET /{page_id}/store_locations` |  | Page: Insights |  |
| Page | `page.subscribed_apps.list` | `GET /{page_id}/subscribed_apps` |  | Page: Community activity |  |
| Page | `page.video_reels.list` | `GET /{page_id}/video_reels` |  | Page: Insights |  |
| Page | `page.videos.list` | `GET /{page_id}/videos` |  | Page: Insights |  |
| Page | `page.welcome_message_flows.list` | `GET /{page_id}/welcome_message_flows` |  | Page: Ads |  |
| PageLeadsAccessConfig | `page_leads_access_config.get` | `GET /{page_leads_access_config_id}` |  | Page: Ads |  |
| PagePost | `page_post.get` | `GET /{page_post_id}` |  | Page: Insights, Page: Content |  |
| PagePost | `page_post.attachments.list` | `GET /{page_post_id}/attachments` |  | Page: Insights, Page: Content |  |
| PagePost | `page_post.comments.list` | `GET /{page_post_id}/comments` |  | Page: Community activity, Page: Insights |  |
| PagePost | `page_post.dynamic_posts.list` | `GET /{page_post_id}/dynamic_posts` |  | Page: Insights, Page: Content |  |
| PagePost | `page_post.reactions.list` | `GET /{page_post_id}/reactions` |  | Page: Insights, Page: Content |  |
| PagePost | `page_post.sponsor_tags.list` | `GET /{page_post_id}/sponsor_tags` |  | Page: Insights, Page: Content |  |
| PartnerAccountLinking | `partner_account_linking.get` | `GET /{partner_account_linking_id}` |  | Dataset: Analyze |  |
| PartnerIntegrationLinked | `partner_integration_linked.get` | `GET /{partner_integration_linked_id}` |  | Dataset: Analyze |  |
| PartnerStudy | `partner_study.get` | `GET /{partner_study_id}` |  | Ad account: View performance |  |
| PlayableContent | `playable_content.get` | `GET /{playable_content_id}` |  | Ad account: View performance |  |
| PrivateLiftStudyInstance | `private_lift_study_instance.get` | `GET /{private_lift_study_instance_id}` |  | Ad account: View performance |  |
| ProductCatalog | `product_catalog.get` | `GET /{product_catalog_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.agencies.list` | `GET /{product_catalog_id}/agencies` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.assigned_users.list` | `GET /{product_catalog_id}/assigned_users` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.automotive_models.list` | `GET /{product_catalog_id}/automotive_models` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.categories.list` | `GET /{product_catalog_id}/categories` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.check_batch_request_status.list` | `GET /{product_catalog_id}/check_batch_request_status` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.check_marketplace_partner_deals_status.list` | `GET /{product_catalog_id}/check_marketplace_partner_deals_status` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.check_marketplace_partner_sellers_status.list` | `GET /{product_catalog_id}/check_marketplace_partner_sellers_status` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.collaborative_ads_lsb_image_bank.list` | `GET /{product_catalog_id}/collaborative_ads_lsb_image_bank` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.collaborative_ads_share_settings.list` | `GET /{product_catalog_id}/collaborative_ads_share_settings` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.creator_asset_creatives.list` | `GET /{product_catalog_id}/creator_asset_creatives` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.data_source_priorities.list` | `GET /{product_catalog_id}/data_source_priorities` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.data_sources.list` | `GET /{product_catalog_id}/data_sources` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.destinations.list` | `GET /{product_catalog_id}/destinations` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.diagnostics.list` | `GET /{product_catalog_id}/diagnostics` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.event_stats.list` | `GET /{product_catalog_id}/event_stats` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.external_event_sources.list` | `GET /{product_catalog_id}/external_event_sources` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.flights.list` | `GET /{product_catalog_id}/flights` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.home_listings.list` | `GET /{product_catalog_id}/home_listings` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.hotel_rooms_batch.list` | `GET /{product_catalog_id}/hotel_rooms_batch` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.hotels.list` | `GET /{product_catalog_id}/hotels` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.pricing_variables_batch.list` | `GET /{product_catalog_id}/pricing_variables_batch` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.product_feeds.list` | `GET /{product_catalog_id}/product_feeds` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.product_groups.list` | `GET /{product_catalog_id}/product_groups` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.product_sets.list` | `GET /{product_catalog_id}/product_sets` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.product_sets_batch.list` | `GET /{product_catalog_id}/product_sets_batch` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.products.list` | `GET /{product_catalog_id}/products` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.vehicle_offers.list` | `GET /{product_catalog_id}/vehicle_offers` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.vehicles.list` | `GET /{product_catalog_id}/vehicles` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.version_configs.list` | `GET /{product_catalog_id}/version_configs` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalogAutomotiveItem | `product_catalog_automotive_item.get` | `GET /{product_catalog_automotive_item_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalogHotelsItem | `product_catalog_hotels_item.get` | `GET /{product_catalog_hotels_item_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductCatalogLocalizationSettings | `product_catalog_localization_settings.get` | `GET /{product_catalog_localization_settings_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductDeliveryPreference | `product_delivery_preference.get` | `GET /{product_delivery_preference_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.get` | `GET /{product_feed_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.activities.list` | `GET /{product_feed_id}/activities` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.apps_and_software.list` | `GET /{product_feed_id}/apps_and_software` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.articles_and_publications.list` | `GET /{product_feed_id}/articles_and_publications` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.automotive_models.list` | `GET /{product_feed_id}/automotive_models` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.destinations.list` | `GET /{product_feed_id}/destinations` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.flights.list` | `GET /{product_feed_id}/flights` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.home_listings.list` | `GET /{product_feed_id}/home_listings` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.hotels.list` | `GET /{product_feed_id}/hotels` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.media_titles.list` | `GET /{product_feed_id}/media_titles` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.products.list` | `GET /{product_feed_id}/products` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.professional_services.list` | `GET /{product_feed_id}/professional_services` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.rule_preview_samples.list` | `GET /{product_feed_id}/rule_preview_samples` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.rules.list` | `GET /{product_feed_id}/rules` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.upload_schedules.list` | `GET /{product_feed_id}/upload_schedules` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.uploads.list` | `GET /{product_feed_id}/uploads` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.vehicle_offers.list` | `GET /{product_feed_id}/vehicle_offers` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeed | `product_feed.vehicles.list` | `GET /{product_feed_id}/vehicles` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeedRule | `product_feed_rule.get` | `GET /{product_feed_rule_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeedSchedule | `product_feed_schedule.get` | `GET /{product_feed_schedule_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeedUpload | `product_feed_upload.get` | `GET /{product_feed_upload_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeedUpload | `product_feed_upload.error_report.create` | `POST /{product_feed_upload_id}/error_report` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeedUpload | `product_feed_upload.errors.list` | `GET /{product_feed_upload_id}/errors` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeedUploadError | `product_feed_upload_error.get` | `GET /{product_feed_upload_error_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeedUploadError | `product_feed_upload_error.samples.list` | `GET /{product_feed_upload_error_id}/samples` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeedUploadError | `product_feed_upload_error.suggested_rules.list` | `GET /{product_feed_upload_error_id}/suggested_rules` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductFeedUploadErrorSample | `product_feed_upload_error_sample.get` | `GET /{product_feed_upload_error_sample_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductGroup | `product_group.get` | `GET /{product_group_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductGroup | `product_group.products.list` | `GET /{product_group_id}/products` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductImage | `product_image.get` | `GET /{product_image_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductItem | `product_item.get` | `GET /{product_item_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductItem | `product_item.channels_to_integrity_status.list` | `GET /{product_item_id}/channels_to_integrity_status` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductItem | `product_item.override_details.list` | `GET /{product_item_id}/override_details` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductItem | `product_item.product_sets.list` | `GET /{product_item_id}/product_sets` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductItem | `product_item.videos_metadata.list` | `GET /{product_item_id}/videos_metadata` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductItemLocalInfo | `product_item_local_info.get` | `GET /{product_item_local_info_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductItemOffer | `product_item_offer.get` | `GET /{product_item_offer_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.get` | `GET /{product_set_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.activities.list` | `GET /{product_set_id}/activities` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.apps_and_software.list` | `GET /{product_set_id}/apps_and_software` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.articles_and_publications.list` | `GET /{product_set_id}/articles_and_publications` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.automotive_models.list` | `GET /{product_set_id}/automotive_models` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.da_checks.list` | `GET /{product_set_id}/da_checks` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.destinations.list` | `GET /{product_set_id}/destinations` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.flights.list` | `GET /{product_set_id}/flights` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.home_listings.list` | `GET /{product_set_id}/home_listings` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.hotels.list` | `GET /{product_set_id}/hotels` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.media_titles.list` | `GET /{product_set_id}/media_titles` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.products.list` | `GET /{product_set_id}/products` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.professional_services.list` | `GET /{product_set_id}/professional_services` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.vehicle_offers.list` | `GET /{product_set_id}/vehicle_offers` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| ProductSet | `product_set.vehicles.list` | `GET /{product_set_id}/vehicles` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| PublisherBlockList | `publisher_block_list.get` | `GET /{publisher_block_list_id}` |  | Ad account: View performance |  |
| PublisherBlockList | `publisher_block_list.paged_web_publishers.list` | `GET /{publisher_block_list_id}/paged_web_publishers` |  | Ad account: View performance |  |
| RTBDynamicPost | `rtb_dynamic_post.get` | `GET /{rtb_dynamic_post_id}` |  | Page: Insights |  |
| RTBDynamicPost | `rtb_dynamic_post.comments.list` | `GET /{rtb_dynamic_post_id}/comments` |  | Page: Community activity, Page: Insights |  |
| RTBDynamicPost | `rtb_dynamic_post.likes.list` | `GET /{rtb_dynamic_post_id}/likes` |  | Page: Community activity, Page: Insights |  |
| ReachFrequencyPrediction | `reach_frequency_prediction.get` | `GET /{reach_frequency_prediction_id}` |  | Ad account: View performance |  |
| ResearchPollStudy | `research_poll_study.get` | `GET /{research_poll_study_id}` |  | Ad account: View performance |  |
| SavedAudience | `saved_audience.get` | `GET /{saved_audience_id}` |  | Ad account: View performance |  |
| SignalsIWLExtractor | `signals_iwl_extractor.get` | `GET /{signals_iwl_extractor_id}` |  | Dataset: Analyze |  |
| SlicedEventSourceGroup | `sliced_event_source_group.get` | `GET /{sliced_event_source_group_id}` |  | Dataset: Analyze |  |
| StoreCatalogSettings | `store_catalog_settings.get` | `GET /{store_catalog_settings_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| StoreLocation | `store_location.get` | `GET /{store_location_id}` |  | Page: Insights |  |
| SystemUser | `system_user.get` | `GET /{system_user_id}` |  | Business: Employee |  |
| SystemUser | `system_user.assigned_ad_accounts.list` | `GET /{system_user_id}/assigned_ad_accounts` |  | Business: Employee |  |
| SystemUser | `system_user.assigned_business_asset_groups.list` | `GET /{system_user_id}/assigned_business_asset_groups` |  | Business: Employee |  |
| SystemUser | `system_user.assigned_instagram_accounts.list` | `GET /{system_user_id}/assigned_instagram_accounts` |  | Business: Employee |  |
| SystemUser | `system_user.assigned_pages.list` | `GET /{system_user_id}/assigned_pages` |  | Business: Employee |  |
| SystemUser | `system_user.assigned_product_catalogs.list` | `GET /{system_user_id}/assigned_product_catalogs` |  | Business: Employee |  |
| SystemUser | `system_user.assigned_whatsapp_business_accounts.list` | `GET /{system_user_id}/assigned_whatsapp_business_accounts` |  | Business: Employee |  |
| ThirdPartyMeasurementReportDataset | `third_party_measurement_report_dataset.get` | `GET /{third_party_measurement_report_dataset_id}` |  | Dataset: Analyze |  |
| ThirdPartyPartnerLiftRequest | `third_party_partner_lift_request.get` | `GET /{third_party_partner_lift_request_id}` |  | Ad account: View performance |  |
| ThirdPartyPartnerPanelRequest | `third_party_partner_panel_request.get` | `GET /{third_party_partner_panel_request_id}` |  | Ad account: View performance |  |
| ThirdPartyPartnerPanelScheduled | `third_party_partner_panel_scheduled.get` | `GET /{third_party_partner_panel_scheduled_id}` |  | Ad account: View performance |  |
| ThirdPartyPartnerViewabilityRequest | `third_party_partner_viewability_request.get` | `GET /{third_party_partner_viewability_request_id}` |  | Ad account: View performance |  |
| TransactableItem | `transactable_item.get` | `GET /{transactable_item_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| TransactableItem | `transactable_item.channels_to_integrity_status.list` | `GET /{transactable_item_id}/channels_to_integrity_status` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| TransactableItem | `transactable_item.override_details.list` | `GET /{transactable_item_id}/override_details` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| User | `user.get` | `GET /{user_id}` |  |  |  |
| User | `user.accounts.list` | `GET /{user_id}/accounts` |  |  |  |
| User | `user.ad_studies.list` | `GET /{user_id}/ad_studies` |  | Ad account: View performance |  |
| User | `user.adaccounts.list` | `GET /{user_id}/adaccounts` |  |  |  |
| User | `user.assigned_ad_accounts.list` | `GET /{user_id}/assigned_ad_accounts` |  |  |  |
| User | `user.assigned_applications.list` | `GET /{user_id}/assigned_applications` |  |  |  |
| User | `user.assigned_business_asset_groups.list` | `GET /{user_id}/assigned_business_asset_groups` |  |  |  |
| User | `user.assigned_instagram_accounts.list` | `GET /{user_id}/assigned_instagram_accounts` |  |  |  |
| User | `user.assigned_pages.list` | `GET /{user_id}/assigned_pages` |  |  |  |
| User | `user.assigned_product_catalogs.list` | `GET /{user_id}/assigned_product_catalogs` |  |  |  |
| User | `user.assigned_whatsapp_business_accounts.list` | `GET /{user_id}/assigned_whatsapp_business_accounts` |  |  |  |
| User | `user.business_users.list` | `GET /{user_id}/business_users` |  |  |  |
| User | `user.businesses.list` | `GET /{user_id}/businesses` |  |  |  |
| User | `user.permissions.list` | `GET /{user_id}/permissions` |  |  |  |
| User | `user.personal_ad_accounts.list` | `GET /{user_id}/personal_ad_accounts` |  |  |  |
| User | `user.product_catalogs.list` | `GET /{user_id}/product_catalogs` |  |  |  |
| UserExportForCAM | `user_export_for_cam.branded_content_media.list` | `GET /{user_export_for_cam_id}/branded_content_media` |  | Instagram account: Ads |  |
| UserExportForCAM | `user_export_for_cam.insights.list` | `GET /{user_export_for_cam_id}/insights` |  | Instagram account: Ads, Instagram account: Insights |  |
| UserExportForCAM | `user_export_for_cam.past_partnership_ads_media.list` | `GET /{user_export_for_cam_id}/past_partnership_ads_media` |  | Instagram account: Ads |  |
| UserExportForCAM | `user_export_for_cam.recent_media.list` | `GET /{user_export_for_cam_id}/recent_media` |  | Instagram account: Ads, Instagram account: Insights |  |
| Vehicle | `vehicle.get` | `GET /{vehicle_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Vehicle | `vehicle.channels_to_integrity_status.list` | `GET /{vehicle_id}/channels_to_integrity_status` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Vehicle | `vehicle.override_details.list` | `GET /{vehicle_id}/override_details` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| Vehicle | `vehicle.videos_metadata.list` | `GET /{vehicle_id}/videos_metadata` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| VehicleOffer | `vehicle_offer.get` | `GET /{vehicle_offer_id}` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| VehicleOffer | `vehicle_offer.channels_to_integrity_status.list` | `GET /{vehicle_offer_id}/channels_to_integrity_status` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| VehicleOffer | `vehicle_offer.override_details.list` | `GET /{vehicle_offer_id}/override_details` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| VehicleOffer | `vehicle_offer.videos_metadata.list` | `GET /{vehicle_offer_id}/videos_metadata` |  | Catalog: Advertise, Catalog: Manage catalog |  |
| WebsiteCreativeAssetSource | `website_creative_asset_source.get` | `GET /{website_creative_asset_source_id}` |  | Ad account: View performance |  |
| WebsiteCreativeAssetSuggestions | `website_creative_asset_suggestions.get` | `GET /{website_creative_asset_suggestions_id}` |  | Ad account: View performance |  |
| WebsiteCreativeInfo | `website_creative_info.get` | `GET /{website_creative_info_id}` |  | Ad account: View performance |  |

## operate

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| Ad | `ad.adlabels.update` | `POST /{ad_id}/adlabels` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.ad_place_page_sets.create` | `POST /act_{ad_account_id}/ad_place_page_sets` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.ad_place_page_sets_async.create` | `POST /act_{ad_account_id}/ad_place_page_sets_async` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.adcreatives.create` | `POST /act_{ad_account_id}/adcreatives` |  | Ad account: Manage campaigns, Page: Ads |  |
| AdAccount | `ad_account.adimages.create` | `POST /act_{ad_account_id}/adimages` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.adlabels.create` | `POST /act_{ad_account_id}/adlabels` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.adplayables.create` | `POST /act_{ad_account_id}/adplayables` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.adspixels.create` | `POST /act_{ad_account_id}/adspixels` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.advideos.create` | `POST /act_{ad_account_id}/advideos` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.asyncadcreatives.create` | `POST /act_{ad_account_id}/asyncadcreatives` |  | Ad account: Manage campaigns, Page: Ads |  |
| AdAccount | `ad_account.block_list_drafts.create` | `POST /act_{ad_account_id}/block_list_drafts` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.customaudiences.create` | `POST /act_{ad_account_id}/customaudiences` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.customconversions.create` | `POST /act_{ad_account_id}/customconversions` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.product_audiences.create` | `POST /act_{ad_account_id}/product_audiences` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.publisher_block_lists.create` | `POST /act_{ad_account_id}/publisher_block_lists` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.tracking.update` | `POST /act_{ad_account_id}/tracking` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.video_ads.create` | `POST /act_{ad_account_id}/video_ads` |  | Ad account: Manage campaigns |  |
| AdAsyncRequestSet | `ad_async_request_set.update` | `POST /{ad_async_request_set_id}` |  | Ad account: Manage campaigns |  |
| AdCreative | `ad_creative.adlabels.update` | `POST /{ad_creative_id}/adlabels` |  | Ad account: Manage campaigns |  |
| AdLabel | `ad_label.update` | `POST /{ad_label_id}` |  | Ad account: Manage campaigns |  |
| AdSet | `ad_set.adlabels.delete` | `DELETE /{ad_set_id}/adlabels` |  | Ad account: Manage campaigns |  |
| AdSet | `ad_set.adlabels.update` | `POST /{ad_set_id}/adlabels` |  | Ad account: Manage campaigns |  |
| AdStudy | `ad_study.update` | `POST /{ad_study_id}` |  | Ad account: Manage campaigns |  |
| AdStudy | `ad_study.checkpoint.update` | `POST /{ad_study_id}/checkpoint` |  | Ad account: Manage campaigns |  |
| AdStudy | `ad_study.instances.create` | `POST /{ad_study_id}/instances` |  | Ad account: Manage campaigns |  |
| AdStudyCell | `ad_study_cell.update` | `POST /{ad_study_cell_id}` |  | Ad account: Manage campaigns |  |
| AdStudyObjective | `ad_study_objective.update` | `POST /{ad_study_objective_id}` |  | Ad account: Manage campaigns |  |
| Application | `application.aem_conversions.update` | `POST /{application_id}/aem_conversions` |  | App: Admin, App: Advertiser |  |
| Application | `application.aem_skan_readiness.update` | `POST /{application_id}/aem_skan_readiness` |  | App: Admin, App: Advertiser |  |
| Business | `business.ad_studies.create` | `POST /{business_id}/ad_studies` |  | Business: Admin, Business: Employee |  |
| Business | `business.ads_dataset.create` | `POST /{business_id}/ads_dataset` |  | Business: Employee |  |
| Business | `business.adspixels.create` | `POST /{business_id}/adspixels` |  | Business: Employee |  |
| Business | `business.block_list_drafts.create` | `POST /{business_id}/block_list_drafts` |  | Business: Employee |  |
| Business | `business.creative_folders.create` | `POST /{business_id}/creative_folders` |  | Business: Employee |  |
| Business | `business.customconversions.create` | `POST /{business_id}/customconversions` |  | Business: Employee |  |
| Business | `business.event_source_groups.create` | `POST /{business_id}/event_source_groups` |  | Business: Employee |  |
| Business | `business.images.create` | `POST /{business_id}/images` |  | Business: Employee |  |
| Business | `business.videos.create` | `POST /{business_id}/videos` |  | Business: Employee |  |
| Campaign | `campaign.adlabels.update` | `POST /{campaign_id}/adlabels` |  | Ad account: Manage campaigns |  |
| CustomAudience | `custom_audience.salts.update` | `POST /{custom_audience_id}/salts` |  | Ad account: Manage campaigns |  |
| CustomConversion | `custom_conversion.update` | `POST /{custom_conversion_id}` |  | Ad account: Manage campaigns |  |
| IGComment | `ig_comment.update` | `POST /{ig_comment_id}` |  | Instagram account: Community activity |  |
| IGUser | `ig_user.dataset.create` | `POST /{ig_user_id}/dataset` |  | Instagram account: Ads |  |
| LeadgenForm | `leadgen_form.test_leads.create` | `POST /{leadgen_form_id}/test_leads` |  | Page: Ads, Page: Leads access |  |
| Page | `page.canvas_elements.create` | `POST /{page_id}/canvas_elements` |  | Page: Ads |  |
| Page | `page.dataset.create` | `POST /{page_id}/dataset` |  | Page: Ads |  |
| PrivateLiftStudyInstance | `private_lift_study_instance.update` | `POST /{private_lift_study_instance_id}` |  | Ad account: Manage campaigns |  |
| ProductCatalog | `product_catalog.product_sets.create` | `POST /{product_catalog_id}/product_sets` |  | Catalog: Advertise |  |
| PublisherBlockList | `publisher_block_list.append_publisher_urls.update` | `POST /{publisher_block_list_id}/append_publisher_urls` |  | Ad account: Manage campaigns |  |
| User | `user.ad_studies.create` | `POST /{user_id}/ad_studies` |  | Ad account: Manage campaigns |  |

## public

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| AdVideo | `ad_video.update` | `POST /{ad_video_id}` |  | Ad account: Manage campaigns |  |
| AdVideo | `ad_video.captions.update` | `POST /{ad_video_id}/captions` |  | Ad account: Manage campaigns |  |
| AdVideo | `ad_video.comments.create` | `POST /{ad_video_id}/comments` |  | Ad account: Manage campaigns |  |
| AdVideo | `ad_video.likes.update` | `POST /{ad_video_id}/likes` |  | Ad account: Manage campaigns |  |
| AdVideo | `ad_video.polls.create` | `POST /{ad_video_id}/polls` |  | Ad account: Manage campaigns |  |
| AdVideo | `ad_video.thumbnails.update` | `POST /{ad_video_id}/thumbnails` |  | Ad account: Manage campaigns |  |
| Canvas | `canvas.update` | `POST /{canvas_id}` | draft: operate | Page: Ads |  |
| Comment | `comment.update` | `POST /{comment_id}` |  | Page: Community activity |  |
| Comment | `comment.comments.create` | `POST /{comment_id}/comments` |  | Page: Community activity |  |
| Comment | `comment.likes.update` | `POST /{comment_id}/likes` |  | Page: Community activity |  |
| IGComment | `ig_comment.replies.create` | `POST /{ig_comment_id}/replies` |  | Instagram account: Community activity |  |
| IGMedia | `ig_media.update` | `POST /{ig_media_id}` |  | Instagram account: Ads |  |
| IGMedia | `ig_media.comments.create` | `POST /{ig_media_id}/comments` |  | Instagram account: Community activity |  |
| IGUser | `ig_user.welcome_message_flows.create` | `POST /{ig_user_id}/welcome_message_flows` | new: operate | Instagram account: Ads |  |
| LeadgenForm | `leadgen_form.update` | `POST /{leadgen_form_id}` |  | Page: Ads |  |
| Page | `page.canvases.create` | `POST /{page_id}/canvases` | draft: operate | Page: Ads |  |
| Page | `page.feed.create` | `POST /{page_id}/feed` | unpublished: operate | Page: Content |  |
| Page | `page.leadgen_forms.create` | `POST /{page_id}/leadgen_forms` |  | Page: Ads |  |
| Page | `page.messenger_lead_forms.create` | `POST /{page_id}/messenger_lead_forms` |  | Page: Ads |  |
| Page | `page.welcome_message_flows.create` | `POST /{page_id}/welcome_message_flows` | new: operate | Page: Ads |  |
| PagePost | `page_post.update` | `POST /{page_post_id}` |  | Page: Content |  |
| PagePost | `page_post.comments.create` | `POST /{page_post_id}/comments` |  | Page: Community activity |  |
| ProductSet | `product_set.update` | `POST /{product_set_id}` |  | Catalog: Advertise |  |

## spend

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| (root) | `root.integrity_appeals_ads.create` | `POST /integrity/appeals/ads` |  | Ad account: Manage campaigns |  |
| Ad | `ad.update` | `POST /{ad_id}` |  | Ad account: Manage campaigns |  |
| Ad | `ad.copies.update` | `POST /{ad_id}/copies` | paused: operate | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.account_controls.create` | `POST /act_{ad_account_id}/account_controls` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.adrules_library.create` | `POST /act_{ad_account_id}/adrules_library` | pauses_only: operate | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.ads.create` | `POST /act_{ad_account_id}/ads` | paused: operate | Ad account: Manage campaigns, Page: Ads |  |
| AdAccount | `ad_account.adsets.create` | `POST /act_{ad_account_id}/adsets` | paused: operate | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.async_batch_requests.create` | `POST /act_{ad_account_id}/async_batch_requests` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.asyncadrequestsets.create` | `POST /act_{ad_account_id}/asyncadrequestsets` | paused: operate | Ad account: Manage campaigns, Page: Ads |  |
| AdAccount | `ad_account.brand_safety_content_filter_levels.update` | `POST /act_{ad_account_id}/brand_safety_content_filter_levels` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.campaigns.create` | `POST /act_{ad_account_id}/campaigns` | paused: operate | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.recommendations.create` | `POST /act_{ad_account_id}/recommendations` |  | Ad account: Manage campaigns |  |
| AdAccount | `ad_account.value_rule_set.create` | `POST /act_{ad_account_id}/value_rule_set` |  | Ad account: Manage campaigns |  |
| AdRule | `ad_rule.update` | `POST /{ad_rule_id}` |  | Ad account: Manage campaigns |  |
| AdRule | `ad_rule.execute.update` | `POST /{ad_rule_id}/execute` | pauses_only: operate | Ad account: Manage campaigns |  |
| AdSet | `ad_set.update` | `POST /{ad_set_id}` |  | Ad account: Manage campaigns |  |
| AdSet | `ad_set.budget_schedules.create` | `POST /{ad_set_id}/budget_schedules` |  | Ad account: Manage campaigns |  |
| AdSet | `ad_set.copies.update` | `POST /{ad_set_id}/copies` | paused: operate | Ad account: Manage campaigns |  |
| AdsValueAdjustmentRuleCollection | `ads_value_adjustment_rule_collection.update` | `POST /{ads_value_adjustment_rule_collection_id}` |  | Ad account: Manage campaigns |  |
| Campaign | `campaign.update` | `POST /{campaign_id}` |  | Ad account: Manage campaigns |  |
| Campaign | `campaign.budget_schedules.create` | `POST /{campaign_id}/budget_schedules` |  | Ad account: Manage campaigns |  |
| Campaign | `campaign.copies.update` | `POST /{campaign_id}/copies` | paused: operate | Ad account: Manage campaigns |  |
| CustomAudience | `custom_audience.update` | `POST /{custom_audience_id}` | unused: operate | Ad account: Manage campaigns |  |
| CustomAudience | `custom_audience.users.update` | `POST /{custom_audience_id}/users` | unused: operate | Ad account: Manage campaigns |  |
| CustomAudience | `custom_audience.usersreplace.update` | `POST /{custom_audience_id}/usersreplace` | unused: operate | Ad account: Manage campaigns |  |
| HighDemandPeriod | `high_demand_period.update` | `POST /{high_demand_period_id}` |  | Ad account: Manage campaigns |  |
| PublisherBlockList | `publisher_block_list.update` | `POST /{publisher_block_list_id}` |  | Ad account: Manage campaigns |  |

## admin

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| Ad | `ad.delete` | `DELETE /{ad_id}` |  | Ad account: Manage campaigns | cannot be undone |
| AdAccount | `ad_account.update` | `POST /act_{ad_account_id}` |  | Ad account: Manage ad account |  |
| AdAccount | `ad_account.adimages.delete` | `DELETE /act_{ad_account_id}/adimages` |  | Ad account: Manage campaigns | cannot be undone |
| AdAccount | `ad_account.advideos.delete` | `DELETE /act_{ad_account_id}/advideos` |  | Ad account: Manage campaigns | cannot be undone |
| AdAccount | `ad_account.agencies.delete` | `DELETE /act_{ad_account_id}/agencies` |  | Ad account: Manage ad account, Business: Admin |  |
| AdAccount | `ad_account.agencies.update` | `POST /act_{ad_account_id}/agencies` |  | Ad account: Manage ad account, Business: Admin |  |
| AdAccount | `ad_account.assigned_users.delete` | `DELETE /act_{ad_account_id}/assigned_users` |  | Ad account: Manage ad account, Business: Admin |  |
| AdAccount | `ad_account.assigned_users.update` | `POST /act_{ad_account_id}/assigned_users` |  | Ad account: Manage ad account, Business: Admin |  |
| AdAccount | `ad_account.campaigns.delete` | `DELETE /act_{ad_account_id}/campaigns` |  | Ad account: Manage campaigns | cannot be undone |
| AdAccount | `ad_account.customaudiencestos.update` | `POST /act_{ad_account_id}/customaudiencestos` |  | Ad account: Manage ad account |  |
| AdAccount | `ad_account.subscribed_apps.delete` | `DELETE /act_{ad_account_id}/subscribed_apps` |  | Ad account: Manage ad account |  |
| AdAccount | `ad_account.subscribed_apps.create` | `POST /act_{ad_account_id}/subscribed_apps` |  | Ad account: Manage ad account |  |
| AdAccount | `ad_account.usersofanyaudience.delete` | `DELETE /act_{ad_account_id}/usersofanyaudience` |  | Ad account: Manage campaigns | cannot be undone |
| AdAsyncRequest | `ad_async_request.delete` | `DELETE /{ad_async_request_id}` |  | Ad account: Manage campaigns | cannot be undone |
| AdAsyncRequestSet | `ad_async_request_set.delete` | `DELETE /{ad_async_request_set_id}` |  | Ad account: Manage campaigns | cannot be undone |
| AdCreative | `ad_creative.delete` | `DELETE /{ad_creative_id}` |  | Ad account: Manage campaigns | cannot be undone |
| AdCreative | `ad_creative.update` | `POST /{ad_creative_id}` |  | Ad account: Manage campaigns |  |
| AdLabel | `ad_label.delete` | `DELETE /{ad_label_id}` |  | Ad account: Manage campaigns | cannot be undone |
| AdReportRun | `ad_report_run.delete` | `DELETE /{ad_report_run_id}` |  | Ad account: Manage campaigns |  |
| AdRule | `ad_rule.delete` | `DELETE /{ad_rule_id}` |  | Ad account: Manage campaigns | cannot be undone |
| AdSet | `ad_set.delete` | `DELETE /{ad_set_id}` |  | Ad account: Manage campaigns | cannot be undone |
| AdStudy | `ad_study.delete` | `DELETE /{ad_study_id}` |  | Ad account: Manage campaigns | cannot be undone |
| AdVideo | `ad_video.delete` | `DELETE /{ad_video_id}` |  | Ad account: Manage campaigns | cannot be undone |
| AdVideo | `ad_video.collaborators.update` | `POST /{ad_video_id}/collaborators` |  | Ad account: Manage campaigns |  |
| AdsPixel | `ads_pixel.update` | `POST /{ads_pixel_id}` |  | Dataset: Manage |  |
| AdsPixel | `ads_pixel.agencies.delete` | `DELETE /{ads_pixel_id}/agencies` |  | Business: Admin |  |
| AdsPixel | `ads_pixel.agencies.update` | `POST /{ads_pixel_id}/agencies` |  | Business: Admin |  |
| AdsPixel | `ads_pixel.ahp_configs.create` | `POST /{ads_pixel_id}/ahp_configs` |  | Dataset: Manage |  |
| AdsPixel | `ads_pixel.assigned_users.update` | `POST /{ads_pixel_id}/assigned_users` |  | Business: Admin |  |
| AdsPixel | `ads_pixel.events.create` | `POST /{ads_pixel_id}/events` | test_events: operate | Dataset: Upload | cannot be undone |
| AdsPixel | `ads_pixel.shared_accounts.delete` | `DELETE /{ads_pixel_id}/shared_accounts` |  | Business: Admin |  |
| AdsPixel | `ads_pixel.shared_accounts.update` | `POST /{ads_pixel_id}/shared_accounts` |  | Business: Admin |  |
| AdsValueAdjustmentRuleCollection | `ads_value_adjustment_rule_collection.deleteruleset.update` | `POST /{ads_value_adjustment_rule_collection_id}/deleteruleset` |  | Ad account: Manage campaigns | cannot be undone |
| Application | `application.activities.create` | `POST /{application_id}/activities` |  |  | cannot be undone |
| ArticlesAndPublicationsItem | `articles_and_publications_item.delete` | `DELETE /{articles_and_publications_item_id}` |  | Catalog: Manage catalog | cannot be undone |
| Business | `business.update` | `POST /{business_id}` |  | Business: Admin |  |
| Business | `business.access_token.create` | `POST /{business_id}/access_token` |  | Business: Admin |  |
| Business | `business.ad_accounts.delete` | `DELETE /{business_id}/ad_accounts` |  | Business: Admin |  |
| Business | `business.ad_review_requests.create` | `POST /{business_id}/ad_review_requests` |  | Business: Admin |  |
| Business | `business.adaccount.create` | `POST /{business_id}/adaccount` |  | Business: Admin |  |
| Business | `business.add_phone_numbers.update` | `POST /{business_id}/add_phone_numbers` |  | Business: Admin |  |
| Business | `business.adnetwork_applications.create` | `POST /{business_id}/adnetwork_applications` |  | Business: Admin |  |
| Business | `business.agencies.delete` | `DELETE /{business_id}/agencies` |  | Business: Admin |  |
| Business | `business.bm_review_requests.create` | `POST /{business_id}/bm_review_requests` |  | Business: Admin |  |
| Business | `business.business_users.create` | `POST /{business_id}/business_users` |  | Business: Admin |  |
| Business | `business.claim_custom_conversions.create` | `POST /{business_id}/claim_custom_conversions` |  | Business: Admin |  |
| Business | `business.client_apps.update` | `POST /{business_id}/client_apps` |  | Business: Admin |  |
| Business | `business.client_pages.update` | `POST /{business_id}/client_pages` |  | Business: Admin |  |
| Business | `business.clients.delete` | `DELETE /{business_id}/clients` |  | Business: Admin |  |
| Business | `business.cpas_business_setup_config.create` | `POST /{business_id}/cpas_business_setup_config` |  | Business: Admin |  |
| Business | `business.instagram_accounts.delete` | `DELETE /{business_id}/instagram_accounts` |  | Business: Admin |  |
| Business | `business.managed_businesses.delete` | `DELETE /{business_id}/managed_businesses` |  | Business: Admin |  |
| Business | `business.managed_businesses.create` | `POST /{business_id}/managed_businesses` |  | Business: Admin |  |
| Business | `business.openbridge_configurations.create` | `POST /{business_id}/openbridge_configurations` |  | Business: Admin |  |
| Business | `business.owned_ad_accounts.update` | `POST /{business_id}/owned_ad_accounts` |  | Business: Admin |  |
| Business | `business.owned_apps.update` | `POST /{business_id}/owned_apps` |  | Business: Admin |  |
| Business | `business.owned_pages.update` | `POST /{business_id}/owned_pages` |  | Business: Admin |  |
| Business | `business.owned_product_catalogs.create` | `POST /{business_id}/owned_product_catalogs` |  | Business: Admin |  |
| Business | `business.pages.delete` | `DELETE /{business_id}/pages` |  | Business: Admin |  |
| Business | `business.partner_premium_options.create` | `POST /{business_id}/partner_premium_options` |  | Business: Admin |  |
| Business | `business.pixel_tos.create` | `POST /{business_id}/pixel_tos` |  | Business: Admin |  |
| Business | `business.share_preverified_numbers.delete` | `DELETE /{business_id}/share_preverified_numbers` |  | Business: Admin |  |
| Business | `business.share_preverified_numbers.update` | `POST /{business_id}/share_preverified_numbers` |  | Business: Admin |  |
| Business | `business.system_user_access_tokens.create` | `POST /{business_id}/system_user_access_tokens` |  | Business: Admin |  |
| Business | `business.system_users.create` | `POST /{business_id}/system_users` |  | Business: Admin |  |
| BusinessAgreement | `business_agreement.update` | `POST /{business_agreement_id}` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.update` | `POST /{business_asset_group_id}` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.assigned_users.delete` | `DELETE /{business_asset_group_id}/assigned_users` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.assigned_users.update` | `POST /{business_asset_group_id}/assigned_users` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.contained_adaccounts.delete` | `DELETE /{business_asset_group_id}/contained_adaccounts` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.contained_adaccounts.update` | `POST /{business_asset_group_id}/contained_adaccounts` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.contained_applications.delete` | `DELETE /{business_asset_group_id}/contained_applications` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.contained_applications.update` | `POST /{business_asset_group_id}/contained_applications` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.contained_custom_conversions.delete` | `DELETE /{business_asset_group_id}/contained_custom_conversions` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.contained_custom_conversions.update` | `POST /{business_asset_group_id}/contained_custom_conversions` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.contained_instagram_accounts.delete` | `DELETE /{business_asset_group_id}/contained_instagram_accounts` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.contained_instagram_accounts.update` | `POST /{business_asset_group_id}/contained_instagram_accounts` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.contained_pages.delete` | `DELETE /{business_asset_group_id}/contained_pages` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.contained_pages.update` | `POST /{business_asset_group_id}/contained_pages` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.contained_pixels.delete` | `DELETE /{business_asset_group_id}/contained_pixels` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.contained_pixels.update` | `POST /{business_asset_group_id}/contained_pixels` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.contained_product_catalogs.delete` | `DELETE /{business_asset_group_id}/contained_product_catalogs` |  | Business: Admin |  |
| BusinessAssetGroup | `business_asset_group.contained_product_catalogs.update` | `POST /{business_asset_group_id}/contained_product_catalogs` |  | Business: Admin |  |
| BusinessAssetSharingAgreement | `business_asset_sharing_agreement.update` | `POST /{business_asset_sharing_agreement_id}` |  | Business: Admin |  |
| BusinessRoleRequest | `business_role_request.delete` | `DELETE /{business_role_request_id}` |  | Business: Admin | cannot be undone |
| BusinessRoleRequest | `business_role_request.update` | `POST /{business_role_request_id}` |  | Business: Admin |  |
| BusinessUser | `business_user.delete` | `DELETE /{business_user_id}` |  | Business: Admin | cannot be undone |
| BusinessUser | `business_user.update` | `POST /{business_user_id}` |  | Business: Admin |  |
| Campaign | `campaign.delete` | `DELETE /{campaign_id}` |  | Ad account: Manage campaigns | cannot be undone |
| Comment | `comment.delete` | `DELETE /{comment_id}` |  | Page: Community activity | cannot be undone |
| Comment | `comment.likes.delete` | `DELETE /{comment_id}/likes` |  | Page: Community activity |  |
| CustomAudience | `custom_audience.delete` | `DELETE /{custom_audience_id}` |  | Ad account: Manage campaigns | cannot be undone |
| CustomAudience | `custom_audience.adaccounts.delete` | `DELETE /{custom_audience_id}/adaccounts` |  | Business: Admin, Ad account: Manage ad account |  |
| CustomAudience | `custom_audience.adaccounts.update` | `POST /{custom_audience_id}/adaccounts` |  | Business: Admin, Ad account: Manage ad account |  |
| CustomAudience | `custom_audience.users.delete` | `DELETE /{custom_audience_id}/users` |  | Ad account: Manage campaigns |  |
| CustomConversion | `custom_conversion.delete` | `DELETE /{custom_conversion_id}` |  | Ad account: Manage campaigns | cannot be undone |
| EventSourceGroup | `event_source_group.update` | `POST /{event_source_group_id}` |  | Dataset: Manage |  |
| EventSourceGroup | `event_source_group.shared_accounts.update` | `POST /{event_source_group_id}/shared_accounts` |  | Business: Admin |  |
| ExtendedCredit | `extended_credit.extended_credit_invoice_groups.create` | `POST /{extended_credit_id}/extended_credit_invoice_groups` |  | Business: Finance editor, Business: Admin |  |
| ExtendedCredit | `extended_credit.owning_credit_allocation_configs.create` | `POST /{extended_credit_id}/owning_credit_allocation_configs` |  | Business: Finance editor, Business: Admin |  |
| ExtendedCredit | `extended_credit.whatsapp_credit_attach.update` | `POST /{extended_credit_id}/whatsapp_credit_attach` |  | Business: Finance editor, Business: Admin |  |
| ExtendedCredit | `extended_credit.whatsapp_credit_sharing.update` | `POST /{extended_credit_id}/whatsapp_credit_sharing` |  | Business: Finance editor, Business: Admin |  |
| ExtendedCredit | `extended_credit.whatsapp_credit_sharing_and_attach.create` | `POST /{extended_credit_id}/whatsapp_credit_sharing_and_attach` |  | Business: Finance editor, Business: Admin |  |
| ExtendedCreditAllocationConfig | `extended_credit_allocation_config.delete` | `DELETE /{extended_credit_allocation_config_id}` |  | Business: Finance editor, Business: Admin | cannot be undone |
| ExtendedCreditAllocationConfig | `extended_credit_allocation_config.update` | `POST /{extended_credit_allocation_config_id}` |  | Business: Finance editor, Business: Admin |  |
| ExtendedCreditInvoiceGroup | `extended_credit_invoice_group.delete` | `DELETE /{extended_credit_invoice_group_id}` |  | Business: Finance editor, Business: Admin | cannot be undone |
| ExtendedCreditInvoiceGroup | `extended_credit_invoice_group.update` | `POST /{extended_credit_invoice_group_id}` |  | Business: Finance editor, Business: Admin |  |
| ExtendedCreditInvoiceGroup | `extended_credit_invoice_group.ad_accounts.delete` | `DELETE /{extended_credit_invoice_group_id}/ad_accounts` |  | Business: Finance editor, Business: Admin |  |
| ExtendedCreditInvoiceGroup | `extended_credit_invoice_group.ad_accounts.update` | `POST /{extended_credit_invoice_group_id}/ad_accounts` |  | Business: Finance editor, Business: Admin |  |
| Flight | `flight.update` | `POST /{flight_id}` |  | Catalog: Manage catalog |  |
| HighDemandPeriod | `high_demand_period.delete` | `DELETE /{high_demand_period_id}` |  | Ad account: Manage campaigns | cannot be undone |
| HomeListing | `home_listing.delete` | `DELETE /{home_listing_id}` |  | Catalog: Manage catalog | cannot be undone |
| HomeListing | `home_listing.update` | `POST /{home_listing_id}` |  | Catalog: Manage catalog |  |
| Hotel | `hotel.delete` | `DELETE /{hotel_id}` |  | Catalog: Manage catalog | cannot be undone |
| Hotel | `hotel.update` | `POST /{hotel_id}` |  | Catalog: Manage catalog |  |
| IGComment | `ig_comment.delete` | `DELETE /{ig_comment_id}` |  | Instagram account: Community activity | cannot be undone |
| IGMedia | `ig_media.branded_content_partner_promote.create` | `POST /{ig_media_id}/branded_content_partner_promote` |  | Instagram account: Ads |  |
| IGMedia | `ig_media.partnership_ad_code.delete` | `DELETE /{ig_media_id}/partnership_ad_code` |  | Instagram account: Ads |  |
| IGMedia | `ig_media.partnership_ad_code.create` | `POST /{ig_media_id}/partnership_ad_code` |  | Instagram account: Ads |  |
| IGUser | `ig_user.authorized_adaccounts.update` | `POST /{ig_user_id}/authorized_adaccounts` |  | Business: Admin |  |
| IGUser | `ig_user.branded_content_ad_permissions.create` | `POST /{ig_user_id}/branded_content_ad_permissions` |  | Instagram account: Ads |  |
| IGUser | `ig_user.branded_content_tag_approval.delete` | `DELETE /{ig_user_id}/branded_content_tag_approval` |  | Instagram account: Ads |  |
| IGUser | `ig_user.branded_content_tag_approval.create` | `POST /{ig_user_id}/branded_content_tag_approval` |  | Instagram account: Ads |  |
| IGUser | `ig_user.instagram_backed_threads_user.create` | `POST /{ig_user_id}/instagram_backed_threads_user` |  | Instagram account: Ads |  |
| IGUser | `ig_user.welcome_message_flows.delete` | `DELETE /{ig_user_id}/welcome_message_flows` |  | Instagram account: Ads | cannot be undone |
| InstagramBusinessAsset | `instagram_business_asset.agencies.delete` | `DELETE /{instagram_business_asset_id}/agencies` |  | Business: Admin |  |
| InstagramBusinessAsset | `instagram_business_asset.agencies.update` | `POST /{instagram_business_asset_id}/agencies` |  | Business: Admin |  |
| InstagramBusinessAsset | `instagram_business_asset.assigned_users.delete` | `DELETE /{instagram_business_asset_id}/assigned_users` |  | Business: Admin |  |
| InstagramBusinessAsset | `instagram_business_asset.assigned_users.update` | `POST /{instagram_business_asset_id}/assigned_users` |  | Business: Admin |  |
| Lead | `lead.delete` | `DELETE /{lead_id}` |  | Page: Ads, Page: Leads access | cannot be undone |
| OpenBridgeConfiguration | `open_bridge_configuration.delete` | `DELETE /{open_bridge_configuration_id}` |  | Dataset: Manage | cannot be undone |
| OpenBridgeConfiguration | `open_bridge_configuration.update` | `POST /{open_bridge_configuration_id}` |  | Dataset: Manage |  |
| Page | `page.agencies.delete` | `DELETE /{page_id}/agencies` |  | Page: Full control, Business: Admin |  |
| Page | `page.agencies.update` | `POST /{page_id}/agencies` |  | Page: Full control, Business: Admin |  |
| Page | `page.assigned_users.delete` | `DELETE /{page_id}/assigned_users` |  | Page: Full control, Business: Admin |  |
| Page | `page.assigned_users.update` | `POST /{page_id}/assigned_users` |  | Page: Full control, Business: Admin |  |
| Page | `page.page_backed_instagram_accounts.create` | `POST /{page_id}/page_backed_instagram_accounts` |  | Page: Ads |  |
| Page | `page.subscribed_apps.delete` | `DELETE /{page_id}/subscribed_apps` |  | Page: Community activity |  |
| Page | `page.subscribed_apps.update` | `POST /{page_id}/subscribed_apps` |  | Page: Community activity |  |
| Page | `page.welcome_message_flows.delete` | `DELETE /{page_id}/welcome_message_flows` |  | Page: Ads | cannot be undone |
| PagePost | `page_post.delete` | `DELETE /{page_post_id}` |  | Page: Content | cannot be undone |
| ProductCatalog | `product_catalog.delete` | `DELETE /{product_catalog_id}` |  | Catalog: Manage catalog | cannot be undone |
| ProductCatalog | `product_catalog.update` | `POST /{product_catalog_id}` |  | Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.agencies.delete` | `DELETE /{product_catalog_id}/agencies` |  | Business: Admin |  |
| ProductCatalog | `product_catalog.agencies.update` | `POST /{product_catalog_id}/agencies` |  | Business: Admin |  |
| ProductCatalog | `product_catalog.assigned_users.delete` | `DELETE /{product_catalog_id}/assigned_users` |  | Business: Admin |  |
| ProductCatalog | `product_catalog.assigned_users.update` | `POST /{product_catalog_id}/assigned_users` |  | Business: Admin |  |
| ProductCatalog | `product_catalog.batch.create` | `POST /{product_catalog_id}/batch` |  | Catalog: Manage catalog | cannot be undone |
| ProductCatalog | `product_catalog.catalog_store.create` | `POST /{product_catalog_id}/catalog_store` |  | Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.categories.create` | `POST /{product_catalog_id}/categories` |  | Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.external_event_sources.delete` | `DELETE /{product_catalog_id}/external_event_sources` |  | Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.external_event_sources.update` | `POST /{product_catalog_id}/external_event_sources` |  | Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.geolocated_items_batch.create` | `POST /{product_catalog_id}/geolocated_items_batch` |  | Catalog: Manage catalog | cannot be undone |
| ProductCatalog | `product_catalog.home_listings.create` | `POST /{product_catalog_id}/home_listings` |  | Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.hotel_rooms_batch.create` | `POST /{product_catalog_id}/hotel_rooms_batch` |  | Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.hotels.create` | `POST /{product_catalog_id}/hotels` |  | Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.items_batch.update` | `POST /{product_catalog_id}/items_batch` |  | Catalog: Manage catalog | cannot be undone |
| ProductCatalog | `product_catalog.localized_items_batch.update` | `POST /{product_catalog_id}/localized_items_batch` |  | Catalog: Manage catalog | cannot be undone |
| ProductCatalog | `product_catalog.pricing_variables_batch.update` | `POST /{product_catalog_id}/pricing_variables_batch` |  | Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.product_feeds.create` | `POST /{product_catalog_id}/product_feeds` |  | Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.product_groups.create` | `POST /{product_catalog_id}/product_groups` |  | Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.products.create` | `POST /{product_catalog_id}/products` |  | Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.subscribed_apps.delete` | `DELETE /{product_catalog_id}/subscribed_apps` |  | Business: Admin |  |
| ProductCatalog | `product_catalog.update_generated_image_config.update` | `POST /{product_catalog_id}/update_generated_image_config` |  | Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.vehicles.create` | `POST /{product_catalog_id}/vehicles` |  | Catalog: Manage catalog |  |
| ProductCatalog | `product_catalog.version_items_batch.create` | `POST /{product_catalog_id}/version_items_batch` |  | Catalog: Manage catalog | cannot be undone |
| ProductCatalogActivitiesItem | `product_catalog_activities_item.delete` | `DELETE /{product_catalog_activities_item_id}` |  | Catalog: Manage catalog | cannot be undone |
| ProductCatalogAppsAndSoftwareItem | `product_catalog_apps_and_software_item.delete` | `DELETE /{product_catalog_apps_and_software_item_id}` |  | Catalog: Manage catalog | cannot be undone |
| ProductCatalogMediaTitle | `product_catalog_media_title.delete` | `DELETE /{product_catalog_media_title_id}` |  | Catalog: Manage catalog | cannot be undone |
| ProductCatalogProfessionalServicesItem | `product_catalog_professional_services_item.update` | `PUT /{product_catalog_professional_services_item_id}` |  | Catalog: Manage catalog |  |
| ProductFeed | `product_feed.delete` | `DELETE /{product_feed_id}` |  | Catalog: Manage catalog | cannot be undone |
| ProductFeed | `product_feed.update` | `POST /{product_feed_id}` |  | Catalog: Manage catalog |  |
| ProductFeed | `product_feed.rules.create` | `POST /{product_feed_id}/rules` |  | Catalog: Manage catalog |  |
| ProductFeed | `product_feed.supplementary_feed_assocs.update` | `POST /{product_feed_id}/supplementary_feed_assocs` |  | Catalog: Manage catalog |  |
| ProductFeed | `product_feed.upload_schedules.update` | `POST /{product_feed_id}/upload_schedules` |  | Catalog: Manage catalog |  |
| ProductFeed | `product_feed.uploads.create` | `POST /{product_feed_id}/uploads` |  | Catalog: Manage catalog |  |
| ProductFeedRule | `product_feed_rule.delete` | `DELETE /{product_feed_rule_id}` |  | Catalog: Manage catalog | cannot be undone |
| ProductFeedRule | `product_feed_rule.update` | `POST /{product_feed_rule_id}` |  | Catalog: Manage catalog |  |
| ProductGroup | `product_group.delete` | `DELETE /{product_group_id}` |  | Catalog: Manage catalog | cannot be undone |
| ProductGroup | `product_group.update` | `POST /{product_group_id}` |  | Catalog: Manage catalog |  |
| ProductGroup | `product_group.products.create` | `POST /{product_group_id}/products` |  | Catalog: Manage catalog |  |
| ProductItem | `product_item.delete` | `DELETE /{product_item_id}` |  | Catalog: Manage catalog | cannot be undone |
| ProductItem | `product_item.update` | `POST /{product_item_id}` |  | Catalog: Manage catalog |  |
| ProductSet | `product_set.delete` | `DELETE /{product_set_id}` |  | Catalog: Advertise | cannot be undone |
| PublisherBlockList | `publisher_block_list.delete` | `DELETE /{publisher_block_list_id}` |  | Ad account: Manage campaigns | cannot be undone |
| StoreCatalogSettings | `store_catalog_settings.delete` | `DELETE /{store_catalog_settings_id}` |  | Catalog: Manage catalog | cannot be undone |
| User | `user.businesses.delete` | `DELETE /{user_id}/businesses` |  | Business: Admin |  |
| User | `user.businesses.create` | `POST /{user_id}/businesses` |  | Business: Admin |  |
| User | `user.permissions.delete` | `DELETE /{user_id}/permissions` |  |  |  |
| Vehicle | `vehicle.update` | `POST /{vehicle_id}` |  | Catalog: Manage catalog |  |

## never

| Resource | Operation | Method and path | Lowered while | Roles | Note |
|---|---|---|---|---|---|
| (root) | `root.integrity_appeals_ads_eligibility.get.single_ad` | `GET /integrity/appeals/ads/eligibility` |  | Ad account: View performance | Held until a call confirms it: Meta's spec gives it no parameters. |
| (root) | `root.integrity_appeals_ads_status.get.single_ad` | `GET /integrity/appeals/ads/status` |  | Ad account: View performance | Held until a call confirms it: Meta's spec gives it no parameters. |
| AdAccount | `ad_account.agencies.list` | `GET /act_{ad_account_id}/agencies` |  | Ad account: Manage ad account | Only an admin-level task may read it, and Crewlet calls with none for a read made at once: a person reads it in Business Settings. |
| AdAccount | `ad_account.assigned_users.list` | `GET /act_{ad_account_id}/assigned_users` |  | Ad account: Manage ad account | Only an admin-level task may read it, and Crewlet calls with none for a read made at once: a person reads it in Business Settings. |
| AdAccount | `ad_account.creative_stickers.create` | `POST /act_{ad_account_id}/creative_stickers` |  | Ad account: Manage campaigns | Held until a call confirms it: Meta's spec gives it no parameters. |
| AdAccount | `ad_account.insights_feature_settings.create` | `POST /act_{ad_account_id}/insights/feature-settings` |  | Ad account: Manage campaigns | Held until a call confirms it: Meta's spec gives it no parameters. |
| AdAccount | `ad_account.message_campaign.create` | `POST /act_{ad_account_id}/message_campaign` |  | Ad account: Manage campaigns, Page: Messages | Meta limits the Marketing Message API for Messenger to tech providers that passed App Review, in listed countries; not offered in this release. |
| AdAccount | `ad_account.messages.create` | `POST /act_{ad_account_id}/messages` |  | Ad account: Manage campaigns, Page: Messages | Meta limits the Marketing Message API for Messenger to tech providers that passed App Review, in listed countries; not offered in this release. |
| AdAccount | `ad_account.naming_templates.create` | `POST /act_{ad_account_id}/naming_templates` |  | Ad account: Manage campaigns | Held until a call confirms it: Meta's spec gives it no parameters. |
| AdAccount | `ad_account.reachfrequencypredictions.create` | `POST /act_{ad_account_id}/reachfrequencypredictions` |  | Ad account: Manage campaigns | Reach and frequency buying is a reservation program Meta grants; not offered in this release. |
| AdAccount | `ad_account.subscribed_apps.list` | `GET /act_{ad_account_id}/subscribed_apps` |  | Ad account: Manage ad account | Only an admin-level task may read it, and Crewlet calls with none for a read made at once: a person reads it in Business Settings. |
| AdAccount | `ad_account.users.list` | `GET /act_{ad_account_id}/users` |  | Ad account: Manage ad account | Only an admin-level task may read it, and Crewlet calls with none for a read made at once: a person reads it in Business Settings. |
| AdsPixel | `ads_pixel.shadowtraffichelper.create` | `POST /{ads_pixel_id}/shadowtraffichelper` |  | Dataset: Manage | Held until a call confirms it: Meta's spec gives it no parameters. |
| AdsSubscription | `ads_subscription.subscriptions.list` | `GET /{id}/subscriptions` |  | Ad account: View performance | Held until a call confirms it: Meta's spec names no node it hangs off. |
| AdsSubscription | `ads_subscription.subscriptions_dynamic.delete` | `DELETE /{id}/subscriptions/{Dynamic}` |  | Ad account: Manage campaigns | Held until a call confirms it: Meta's spec names no node it hangs off. |
| BenchmarkAvailability | `benchmark_availability.insights_benchmark_availability.list` | `GET /{id}/insights/benchmark-availability` |  | Ad account: View performance | Held until a call confirms it: Meta's spec names no node it hangs off. |
| Business | `business.managed_partner_business_setup.create` | `POST /{business_id}/managed_partner_business_setup` |  | Business: Admin | Managed Partner Ads is for marketplaces Meta admits to the program; not offered in this release. |
| Business | `business.managed_partner_businesses.delete` | `DELETE /{business_id}/managed_partner_businesses` |  | Business: Admin | Managed Partner Ads is for marketplaces Meta admits to the program; not offered in this release. |
| Business | `business.managed_partner_businesses.create` | `POST /{business_id}/managed_partner_businesses` |  | Business: Admin | Managed Partner Ads is for marketplaces Meta admits to the program; not offered in this release. |
| Business | `business.onboard_partners_to_mm_lite.create` | `POST /{business_id}/onboard_partners_to_mm_lite` |  | Business: Admin | Meta documents this as the WhatsApp Business Solution Partner call that onboards client businesses to Marketing Messages Lite, using the partner app's system user token with advanced permissions. It is not available to an advertiser-side ads integration. |
| Business | `business.owned_businesses.delete` | `DELETE /{business_id}/owned_businesses` |  | Business: Admin | Child businesses need business_management granted in App Review; not offered in this release. |
| Business | `business.owned_businesses.create` | `POST /{business_id}/owned_businesses` |  | Business: Admin | Child businesses need business_management granted in App Review; not offered in this release. |
| Business | `business.self_certify_whatsapp_business.update` | `POST /{business_id}/self_certify_whatsapp_business` |  | Business: Admin | Meta restricts this endpoint to WhatsApp Select Solution and Premier Solution Partners (partner-led business verification of their onboarded clients). An ads integration that is not such a partner cannot call it. |
| CatalogGenericFeeds | `catalog_generic_feeds.uploads.list` | `GET /{id}/uploads` |  | Catalog: Advertise, Catalog: Manage catalog | Held until a call confirms it: Meta's spec names no node it hangs off. |
| CatalogGenericIngestionSessionErrors | `catalog_generic_ingestion_session_errors.errors.list` | `GET /{id}/errors` |  | Catalog: Advertise, Catalog: Manage catalog | Held until a call confirms it: Meta's spec names no node it hangs off. |
| CategoryBenchmarkCountries | `category_benchmark_countries.insights_category_benchmark_countries.list` | `GET /{id}/insights/category-benchmark/countries` |  | Ad account: View performance | Held until a call confirms it: Meta's spec names no node it hangs off. |
| CategoryBenchmarkSubcategories | `category_benchmark_subcategories.insights_category_benchmark_subcategories.list` | `GET /{id}/insights/category-benchmark/subcategories` |  | Ad account: View performance | Held until a call confirms it: Meta's spec names no node it hangs off. |
| CreativeDiagnostics | `creative_diagnostics.creative_diagnostics.create` | `POST /{id}/creative_diagnostics` |  | Ad account: View performance | Held until a call confirms it: Meta's spec names no node it hangs off. |
| CreativeGenerations | `creative_generations.creative_generations.list` | `GET /{id}/creative_generations` |  | Ad account: View performance | Held until a call confirms it: Meta's spec names no node it hangs off. |
| MyTopProducts | `my_top_products.insights_my_top_products.list` | `GET /{id}/insights/my-top-products` |  | Ad account: View performance | Held until a call confirms it: Meta's spec names no node it hangs off. |
| OpportunityScoreChangelog | `opportunity_score_changelog.opportunity_score_history.list` | `GET /{id}/opportunity_score_history` |  | Ad account: View performance | Held until a call confirms it: Meta's spec names no node it hangs off. |
| Page | `page.owned_product_catalogs.create` | `POST /{page_id}/owned_product_catalogs` |  | Page: Ads | Held until a call confirms it: Meta's spec gives it no parameters. |
| ProductCatalog | `product_catalog.activities.create` | `POST /{product_catalog_id}/activities` |  | Catalog: Manage catalog | Held until a call confirms it: Meta's spec gives it no parameters. |
| ProductCatalog | `product_catalog.apps_and_software.create` | `POST /{product_catalog_id}/apps_and_software` |  | Catalog: Manage catalog | Held until a call confirms it: Meta's spec gives it no parameters. |
| ProductCatalog | `product_catalog.articles_and_publications.create` | `POST /{product_catalog_id}/articles_and_publications` |  | Catalog: Manage catalog | Held until a call confirms it: Meta's spec gives it no parameters. |
| ProductCatalog | `product_catalog.marketplace_partner_deals_details.update` | `POST /{product_catalog_id}/marketplace_partner_deals_details` |  | Catalog: Manage catalog | Facebook Marketplace partner catalog calls are for Meta's partnership program; not offered in this release. |
| ProductCatalog | `product_catalog.marketplace_partner_sellers_details.update` | `POST /{product_catalog_id}/marketplace_partner_sellers_details` |  | Catalog: Manage catalog | Facebook Marketplace partner catalog calls are for Meta's partnership program; not offered in this release. |
| ProductCatalog | `product_catalog.marketplace_partner_signals.update` | `POST /{product_catalog_id}/marketplace_partner_signals` |  | Catalog: Manage catalog | Facebook Marketplace partner catalog calls are for Meta's partnership program; not offered in this release. |
| ProductCatalog | `product_catalog.media_titles.create` | `POST /{product_catalog_id}/media_titles` |  | Catalog: Manage catalog | Held until a call confirms it: Meta's spec gives it no parameters. |
| ProductCatalog | `product_catalog.professional_services.create` | `POST /{product_catalog_id}/professional_services` |  | Catalog: Manage catalog | Held until a call confirms it: Meta's spec gives it no parameters. |
| RTBDynamicPost | `rtb_dynamic_post.comments.create` | `POST /{rtb_dynamic_post_id}/comments` |  | Page: Community activity | Held until a call confirms it: Meta's spec gives it no parameters. |
| ShareOfVoice | `share_of_voice.insights_share_of_voice.list` | `GET /{id}/insights/share-of-voice` |  | Ad account: View performance | Held until a call confirms it: Meta's spec names no node it hangs off. |
