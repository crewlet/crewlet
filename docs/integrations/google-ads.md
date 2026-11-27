# Google Ads

Google Ads lets agents **read and manage your Google Ads accounts through engine tools**, as their own Google identity, with **spend held to caps you set per account**. Every method of the Google Ads API (v26) is reachable; see [Every method, and its class](#every-method-and-its-class).

- **No token reaches an agent's box.** Each agent signs in as its own Google growth identity, a service account the engine makes in your identity project, and its tokens are minted inside the engine ([ADR-0172](../../adr/0172-a-google-growth-product-signs-in-as-each-seats-own-growth-identity-never-its-cloud-identity.md)). Google retired developer tokens, so none is asked for.
- **Two ceilings, both kept.** The access level you give the agent's identity on each account is Google's ceiling; the agent's level, its accounts and each call's class are Crewlet's. A call needs both.
- **Spending, publishing ad text, removals, users, links and billing are asked first,** one each, of an administrator at a fresh sign-in, never a delegate ([ADR-0171](../../adr/0171-spend-public-and-admin-acts-are-held-for-an-administrator-never-a-delegate.md)). A spend is held to the account's caps before anyone is asked and again when it is made.

Nothing here receives events from Google Ads.

## What agents get

An agent is on at one level across every account it is granted.

| An agent at | Can | Needs, on the account |
|---|---|---|
| `read` (the default) | Every report and GAQL query, planning and keyword ideas, recommendations, change history, billing reads | Read-only or above |
| `operate` | Everything `read` can, plus changes that spend nothing more, made at once and recorded: pausing, lowering budgets and bids, adding negatives, labels, audiences, conversions, experiments' setup | Standard |
| `administer` | Everything `operate` can, plus asking an administrator to approve each change that can raise spend (enabling, raising a budget or a bid, widening reach), publishes or retracts ad text, removes, or touches users, links, billing and the account | Standard; Admin for users, links and billing |

Raising an agent to `operate` or `administer` asks for a fresh sign-in (step-up). An agent holds only its level's tools, and each tool that takes a method's ID says what lies above it. A method above the agent's level, named on a tool it holds, is refused before Google Ads is called, naming the method's class, the tool and level that make it, whether an administrator approves each call, and that an administrator raises the level on the Agents tab, asked in words (`request_access` asks for no level). A method Crewlet does not offer is refused as one no level reaches.

Google Ads' access levels, least first: **Email only** and **Billing** (no API method an agent calls), **Read-only** (reads everything, changes nothing), **Standard** (every change to campaigns, ads, budgets, bids, audiences and conversions, not users or links), **Admin** (users, links, billing and account budgets). A manager account's access reaches its client accounts through it.

## Connecting Crewlet

1. Connect [Google Cloud](google-cloud.md) first; no project needs preparing, and the form says whether it is connected. Turn Google Ads on: Crewlet turns on the Google Ads API in the identity project and makes the company reader itself, with no block to run ([Google's growth products sign in through this identity](google-cloud.md#googles-growth-products-sign-in-through-this-identity)).
2. Apply for Explorer or Basic access to the Google Ads API on the identity project's API page; access levels and quotas belong to the project.
3. Add the company reader's email, shown on the form, to each account at **Read-only**: Google Ads, Admin, Access and security, Users.
4. Set each account's spending caps (below). An account without both caps refuses every spend call; reads, operate calls and pausing still work.
5. Turn agents on from the Agents tab, each with its accounts, and give each its identity ([below](#give-an-agent-its-own-identity)).

## Give an agent its own identity

An agent's identity at Google Ads is its own Google growth identity, which Crewlet made; what is left is an Admin adding it, since Google Ads adds a user by API only as an invitation a mailbox accepts. The agent's **Accounts** list what the company reader sees, each by its name under its manager, and take a ten-digit customer ID for one it cannot see yet. A new identity reads **Getting ready** for up to about two minutes, until Google lets a person add it. Then its row reads **Needs you** until the identity reaches each of its accounts, and **Set up identity** in its box shows the steps, with the email ready to copy:

1. In Google Ads, open the account, or the manager account its accounts are reached through (`manager`), then **Admin**, **Access and security**.
2. On the **Users** tab, press **+** and paste the agent's email.
3. Pick the access level its level needs, **Read-only** at `read` and **Standard** at `operate` and `administer`, and press **Add account**. Users, links and billing an `administer` agent asks for need **Admin**, which Google Ads gives a service account only by raising it after it is added.

Added once to a manager account, the identity reaches every account under it; one email reaches at most 20 accounts directly.

**The check, on every pass:** Crewlet signs in as the identity and reads each account, through its manager where it has one; an account Google refuses is not added yet. It then reads the identity's own access level as a user of the account it was added to, where Google Ads lets it: one below the level's is the wrong role, and Admin below `administer` is a warning. Where Google Ads does not let it read its level, the row reads **Role not checked**, never In sync, and says which level to set. The agent is ready once every account passes, and reads Needs you again if somebody removes it there; calls Google refuses meanwhile name what is missing.

## The agents' tools

| Tool | Class | What it does |
|---|---|---|
| `google_ads_list_accounts` | read | The accounts the agent may reach, with each one's name, currency, time zone, status and manager |
| `google_ads_query` | read | A GAQL query on one account: campaigns, ad groups, ads, keywords, search terms, assets and their metrics |
| `google_ads_pause` | operate | Pauses campaigns, ad groups, ads, keywords, criteria, asset groups or asset links |
| `google_ads_set_budget` | spend | Sets a campaign budget's daily amount: asked first, held to the caps |
| `google_ads_read` | read | Any method that only reads, by its ID |
| `google_ads_operate` | operate | Any change that spends nothing more, by its method's ID |
| `google_ads_publish` | public | Any change that publishes or retracts ad text or assets: asked first |
| `google_ads_spend` | spend | Any change that can raise spend: asked first, held to the caps |
| `google_ads_admin` | admin | Any removal, user, link, billing or account change: asked first |

**A mutate is classed by every operation it holds.** Each create, update (by its field mask) and remove is classed by its service's rule, against what the account holds now: enabling, raising a budget, a bid or a target, widening a network, a date range, a match type or targeting, and removing a negative or an exclusion are `spend`; pausing, lowering and adding a negative are `operate`; new or changed ad text, links of assets that show, and policy exemptions are `public`; removing campaigns, ad groups and other containers, links, users and billing are `admin`. A change the engine cannot tell up from down (a value it could not read, a list it cannot compare, an enum given as anything but the name a rule lowers on, such as its number or another case) is classed as the highest its rule allows. A value set where the account holds none is a raise, except a ceiling or a target whose absence is no limit (a bid ceiling, a maximize-conversions target CPA, a target ROAS); a criterion's `negative` is what it is in the account, never what an update sends. Google takes a field by its camelCase or snake_case name, and so do the rules; a body naming one field both ways with two values is refused. A request of several operations is the highest of them, and a call on the wrong tool for what it changes is refused with the tool that may make it. Staging operations into a batch job is classed by them as though they were made then. Running the job is `admin`: Google gives no read of a job's operations and the access log keeps them only as digests, so nothing can class them, or hold them to the caps, as the account stands when the job runs. An approved run binds the job's operation count when it was asked, so one staged since makes it ask again.

**What a call may reach:**
- A call acts in the account its path names or, for a planning method whose path names none, its body's `customer_id`. An account outside the agent's list is refused before Google is asked, and so is a body that names another account's resource, or another account itself (`customers/{id}`), anywhere in it, or a `customer_id` other than the path's. The one exception is the field a method exists to name another account by, which may be outside the agent's list too: a manager's new client (`customerClientLinks.mutate`), the manager a client moves to (`moveManagerLink`) and the account a product link joins (`productLinks.create`), each `admin`.
- The accessible-accounts listing is cut to the agent's accounts.

**How an approved change is made:** the administrator's answer is recorded first, and the change is made only on the call's one answer: a call that lapsed or was denied is never made. Every write holds its account's one lease while the engine checks the agent's accounts, reads the targets again, holds a spend to the caps and sends it, so two changes to one account never pass on the same reading.

**What an agent reads:** every value Google answers with is fenced as somebody else's words, since the public types search terms, other businesses name placements and assets, consumers write leads, and Google generates ad text.

## Spending caps

Each account has two caps, in its own currency:
- **per_change**: the most one approved change may add to the account's daily capacity.
- **daily**: the ceiling on that daily capacity after any approved change.

The daily capacity is the daily budget of every enabled, unended campaign, a shared budget counted once, and a total budget as what is left of it over its remaining days, rounded up. A spend is checked against both caps before an administrator is asked, so nobody is asked to allow what would be refused, and checked again when it is made, under the account's write lease, so two approved raises cannot both pass the daily cap. A change that adds nothing (a broader match, a raised bid) is still asked: the class comes from the change, never its amount.

What a call adds is measured whatever else it holds:
- A call that spends is held to the caps even where it comes to `admin`: a budget raise beside an ad group removed is asked as an account change, with its figures, and refused over a cap.
- Every budget the call names is read, so a budget no campaign uses yet counts once a campaign is put on it.
- An amount is read as the rules read it, a JSON number or a decimal string, and one that is no whole number of micros is refused.
- Graduating an experiment counts each experiment campaign as serving on the budget its mapping names, and a budget recommendation applied with its new amount is that budget's change.
- A method whose request names no amount a cap could hold is `admin` ([ADR-0171](../../adr/0171-spend-public-and-admin-acts-are-held-for-an-administrator-never-a-delegate.md)): see the notes below the method table. The caps bound the budgets agents may set; Google may spend up to twice a campaign's daily budget on one day, and no more than 30.4 times it over a month.

## Configuration

```yaml
integrations:
  google_ads:
    enabled: true
    customers:                       # optional: the accounts agents may be given; absent is every one an identity reaches directly
      - {id: "1234567890"}
      - {id: "2345678901", manager: "9876543210"}   # reached through this manager account (login-customer-id)
    spend:
      accounts:
        - {account: "1234567890", currency: EUR, per_change: "50.00", daily: "200.00"}
    approvers: [founder]             # optional: who an act is routed to first; only an administrator answers
roles:
  - handle: growth-lead
    integrations:
      google_ads: {enabled: true, level: administer, customers: ["1234567890"]}
```

The block needs `integrations.google_cloud` on with its provisioning block. It holds no secret.

## Every method, and its class

Each row is one method the engine offers, with its class (or the classes its rule allows, by each operation it holds) and the least access level Google asks of the identity. Every method takes the `adwords` scope. The table is held to the engine's catalog by a test, both ways.

| Method | Service | Call | Class | Least access level |
|---|---|---|---|---|
| `googleads.customers.accountBudgetProposals.mutate` | AccountBudgetProposalService | `POST /v26/customers/{customersId}/accountBudgetProposals:mutate` | operate, spend or admin by operation (FR-ACCOUNT-BUDGET) | Admin |
| `googleads.customers.accountLinks.create` | AccountLinkService | `POST /v26/customers/{customersId}/accountLinks:create` | admin | Admin |
| `googleads.customers.accountLinks.mutate` | AccountLinkService | `POST /v26/customers/{customersId}/accountLinks:mutate` | admin | Admin |
| `googleads.customers.adGroupAdLabels.mutate` | AdGroupAdLabelService | `POST /v26/customers/{customersId}/adGroupAdLabels:mutate` | operate | Standard |
| `googleads.customers.adGroupAds.mutate` | AdGroupAdService | `POST /v26/customers/{customersId}/adGroupAds:mutate` | operate, public or spend by operation (FR-AD-GROUP-AD) | Standard |
| `googleads.customers.adGroupAds.removeAutomaticallyCreatedAssets` | AdGroupAdService | `POST /v26/customers/{customersId}/adGroupAds/{adGroupAdsId}:removeAutomaticallyCreatedAssets` | public | Standard |
| `googleads.customers.adGroupAssets.mutate` | AdGroupAssetService | `POST /v26/customers/{customersId}/adGroupAssets:mutate` | operate or public by operation (FR-ASSET-LINK) | Standard |
| `googleads.customers.adGroupAssetSets.mutate` | AdGroupAssetSetService | `POST /v26/customers/{customersId}/adGroupAssetSets:mutate` | public or spend by operation (FR-ASSET-SET-LINK) | Standard |
| `googleads.customers.adGroupBidModifiers.mutate` | AdGroupBidModifierService | `POST /v26/customers/{customersId}/adGroupBidModifiers:mutate` | operate or spend by operation (FR-BID-MODIFIER) | Standard |
| `googleads.customers.AdGroupCriterionCustomizers.mutate` | AdGroupCriterionCustomizerService | `POST /v26/customers/{customersId}/AdGroupCriterionCustomizers:mutate` | public | Standard |
| `googleads.customers.adGroupCriterionLabels.mutate` | AdGroupCriterionLabelService | `POST /v26/customers/{customersId}/adGroupCriterionLabels:mutate` | operate | Standard |
| `googleads.customers.adGroupCriteria.mutate` | AdGroupCriterionService | `POST /v26/customers/{customersId}/adGroupCriteria:mutate` | operate, public, spend or admin by operation (FR-AD-GROUP-CRITERION) | Standard |
| `googleads.customers.adGroupCustomizers.mutate` | AdGroupCustomizerService | `POST /v26/customers/{customersId}/adGroupCustomizers:mutate` | public | Standard |
| `googleads.customers.adGroupLabels.mutate` | AdGroupLabelService | `POST /v26/customers/{customersId}/adGroupLabels:mutate` | operate | Standard |
| `googleads.customers.adGroups.mutate` | AdGroupService | `POST /v26/customers/{customersId}/adGroups:mutate` | operate, public, spend or admin by operation (FR-AD-GROUP) | Standard |
| `googleads.customers.adParameters.mutate` | AdParameterService | `POST /v26/customers/{customersId}/adParameters:mutate` | public | Standard |
| `googleads.customers.ads.mutate` | AdService | `POST /v26/customers/{customersId}/ads:mutate` | operate or public by operation (FR-AD) | Standard |
| `googleads.customers.assetGenerations.generateText` | AssetGenerationService | `POST /v26/customers/{customersId}/assetGenerations:generateText` | read | Read only |
| `googleads.customers.assetGenerations.generateImages` | AssetGenerationService | `POST /v26/customers/{customersId}/assetGenerations:generateImages` | read | Read only |
| `googleads.customers.assetGroupAssets.mutate` | AssetGroupAssetService | `POST /v26/customers/{customersId}/assetGroupAssets:mutate` | operate or public by operation (FR-ASSET-LINK) | Standard |
| `googleads.customers.assetGroupListingGroupFilters.mutate` | AssetGroupListingGroupFilterService | `POST /v26/customers/{customersId}/assetGroupListingGroupFilters:mutate` | operate or spend by operation (FR-LISTING-GROUP) | Standard |
| `googleads.customers.assetGroups.mutate` | AssetGroupService | `POST /v26/customers/{customersId}/assetGroups:mutate` | operate, public, spend or admin by operation (FR-ASSET-GROUP) | Standard |
| `googleads.customers.assetGroupSignals.mutate` | AssetGroupSignalService | `POST /v26/customers/{customersId}/assetGroupSignals:mutate` | operate, public or spend by operation (FR-ASSET-GROUP-SIGNAL) | Standard |
| `googleads.customers.assets.mutate` | AssetService | `POST /v26/customers/{customersId}/assets:mutate` | operate or public by operation (FR-ASSET) | Standard |
| `googleads.customers.assetSetAssets.mutate` | AssetSetAssetService | `POST /v26/customers/{customersId}/assetSetAssets:mutate` | operate, public or spend by operation (FR-ASSET-SET-MEMBER) | Standard |
| `googleads.customers.assetSets.mutate` | AssetSetService | `POST /v26/customers/{customersId}/assetSets:mutate` | operate, public or admin by operation (FR-ASSET-SET) | Standard |
| `googleads.customers.generateInsightsFinderReport` | AudienceInsightsService | `POST /v26/customers/{customersId}:generateInsightsFinderReport` | read | Read only |
| `googleads.customers.searchAudienceInsightsAttributes` | AudienceInsightsService | `POST /v26/customers/{customersId}:searchAudienceInsightsAttributes` | read | Read only |
| `googleads.audienceInsights.listInsightsEligibleDates` | AudienceInsightsService | `POST /v26/audienceInsights:listInsightsEligibleDates` | read | Read only |
| `googleads.customers.generateAudienceCompositionInsights` | AudienceInsightsService | `POST /v26/customers/{customersId}:generateAudienceCompositionInsights` | read | Read only |
| `googleads.customers.generateAudienceDefinition` | AudienceInsightsService | `POST /v26/customers/{customersId}:generateAudienceDefinition` | read | Read only |
| `googleads.customers.generateSuggestedTargetingInsights` | AudienceInsightsService | `POST /v26/customers/{customersId}:generateSuggestedTargetingInsights` | read | Read only |
| `googleads.customers.generateAudienceOverlapInsights` | AudienceInsightsService | `POST /v26/customers/{customersId}:generateAudienceOverlapInsights` | read | Read only |
| `googleads.customers.generateTargetingSuggestionMetrics` | AudienceInsightsService | `POST /v26/customers/{customersId}:generateTargetingSuggestionMetrics` | read | Read only |
| `googleads.customers.audiences.mutate` | AudienceService | `POST /v26/customers/{customersId}/audiences:mutate` | operate | Standard |
| `googleads.customers.removeCampaignAutomaticallyCreatedAsset` | AutomaticallyCreatedAssetRemovalService | `POST /v26/customers/{customersId}:removeCampaignAutomaticallyCreatedAsset` | public | Standard |
| `googleads.customers.batchJobs.mutate` | BatchJobService | `POST /v26/customers/{customersId}/batchJobs:mutate` | operate or admin by operation (FR-BATCH) | Standard |
| `googleads.customers.batchJobs.listResults` | BatchJobService | `GET /v26/customers/{customersId}/batchJobs/{batchJobsId}:listResults` | read | Read only |
| `googleads.customers.batchJobs.run` | BatchJobService | `POST /v26/customers/{customersId}/batchJobs/{batchJobsId}:run` | admin | Standard |
| `googleads.customers.batchJobs.addOperations` | BatchJobService | `POST /v26/customers/{customersId}/batchJobs/{batchJobsId}:addOperations` | operate, public, spend or admin by operation (FR-MUTATE) | Standard |
| `googleads.listBenchmarksAvailableDates` | BenchmarksService | `POST /v26:listBenchmarksAvailableDates` | read | Read only |
| `googleads.listBenchmarksLocations` | BenchmarksService | `POST /v26:listBenchmarksLocations` | read | Read only |
| `googleads.listBenchmarksProducts` | BenchmarksService | `POST /v26:listBenchmarksProducts` | read | Read only |
| `googleads.listBenchmarksSources` | BenchmarksService | `POST /v26:listBenchmarksSources` | read | Read only |
| `googleads.customers.generateBenchmarksMetrics` | BenchmarksService | `POST /v26/customers/{customersId}:generateBenchmarksMetrics` | read | Read only |
| `googleads.customers.biddingDataExclusions.mutate` | BiddingDataExclusionService | `POST /v26/customers/{customersId}/biddingDataExclusions:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.customers.biddingSeasonalityAdjustments.mutate` | BiddingSeasonalityAdjustmentService | `POST /v26/customers/{customersId}/biddingSeasonalityAdjustments:mutate` | operate, spend or admin by operation (FR-SEASONALITY) | Standard |
| `googleads.customers.biddingStrategies.mutate` | BiddingStrategyService | `POST /v26/customers/{customersId}/biddingStrategies:mutate` | operate, spend or admin by operation (FR-BIDDING-STRATEGY) | Standard |
| `googleads.customers.billingSetups.mutate` | BillingSetupService | `POST /v26/customers/{customersId}/billingSetups:mutate` | admin | Admin |
| `googleads.customers.suggestBrands` | BrandSuggestionService | `POST /v26/customers/{customersId}:suggestBrands` | read | Read only |
| `googleads.customers.campaignAssets.mutate` | CampaignAssetService | `POST /v26/customers/{customersId}/campaignAssets:mutate` | operate or public by operation (FR-ASSET-LINK) | Standard |
| `googleads.customers.campaignAssetSets.mutate` | CampaignAssetSetService | `POST /v26/customers/{customersId}/campaignAssetSets:mutate` | public or spend by operation (FR-ASSET-SET-LINK) | Standard |
| `googleads.customers.campaignBidModifiers.mutate` | CampaignBidModifierService | `POST /v26/customers/{customersId}/campaignBidModifiers:mutate` | operate or spend by operation (FR-BID-MODIFIER) | Standard |
| `googleads.customers.campaignBudgets.mutate` | CampaignBudgetService | `POST /v26/customers/{customersId}/campaignBudgets:mutate` | operate, spend or admin by operation (FR-BUDGET) | Standard |
| `googleads.customers.campaignConversionGoals.mutate` | CampaignConversionGoalService | `POST /v26/customers/{customersId}/campaignConversionGoals:mutate` | operate | Standard |
| `googleads.customers.campaignCriteria.mutate` | CampaignCriterionService | `POST /v26/customers/{customersId}/campaignCriteria:mutate` | operate, spend or admin by operation (FR-CAMPAIGN-CRITERION) | Standard |
| `googleads.customers.campaignCustomizers.mutate` | CampaignCustomizerService | `POST /v26/customers/{customersId}/campaignCustomizers:mutate` | public | Standard |
| `googleads.customers.campaignDrafts.mutate` | CampaignDraftService | `POST /v26/customers/{customersId}/campaignDrafts:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.customers.campaignDrafts.promote` | CampaignDraftService | `POST /v26/customers/{customersId}/campaignDrafts/{campaignDraftsId}:promote` | admin | Standard |
| `googleads.customers.campaignDrafts.listAsyncErrors` | CampaignDraftService | `GET /v26/customers/{customersId}/campaignDrafts/{campaignDraftsId}:listAsyncErrors` | read | Read only |
| `googleads.customers.CampaignGoalConfigs.mutate` | CampaignGoalConfigService | `POST /v26/customers/{customersId}/CampaignGoalConfigs:mutate` | operate, public or spend by operation (FR-GOAL) | Standard |
| `googleads.customers.campaignGroups.mutate` | CampaignGroupService | `POST /v26/customers/{customersId}/campaignGroups:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.customers.campaignLabels.mutate` | CampaignLabelService | `POST /v26/customers/{customersId}/campaignLabels:mutate` | operate | Standard |
| `googleads.customers.campaigns.mutate` | CampaignService | `POST /v26/customers/{customersId}/campaigns:mutate` | operate, public, spend or admin by operation (FR-CAMPAIGN) | Standard |
| `googleads.customers.campaigns.enablePMaxBrandGuidelines` | CampaignService | `POST /v26/customers/{customersId}/campaigns:enablePMaxBrandGuidelines` | public | Standard |
| `googleads.customers.campaignSharedSets.mutate` | CampaignSharedSetService | `POST /v26/customers/{customersId}/campaignSharedSets:mutate` | operate or spend by operation (FR-SHARED-SET-LINK) | Standard |
| `googleads.customers.generateCreatorInsights` | ContentCreatorInsightsService | `POST /v26/customers/{customersId}:generateCreatorInsights` | read | Read only |
| `googleads.customers.generateTrendingInsights` | ContentCreatorInsightsService | `POST /v26/customers/{customersId}:generateTrendingInsights` | read | Read only |
| `googleads.customers.conversionActions.mutate` | ConversionActionService | `POST /v26/customers/{customersId}/conversionActions:mutate` | operate, spend or admin by operation (FR-CONVERSION-ACTION) | Standard |
| `googleads.customers.uploadConversionAdjustments` | ConversionAdjustmentUploadService | `POST /v26/customers/{customersId}:uploadConversionAdjustments` | operate | Standard |
| `googleads.customers.conversionCustomVariables.mutate` | ConversionCustomVariableService | `POST /v26/customers/{customersId}/conversionCustomVariables:mutate` | operate | Standard |
| `googleads.customers.conversionGoalCampaignConfigs.mutate` | ConversionGoalCampaignConfigService | `POST /v26/customers/{customersId}/conversionGoalCampaignConfigs:mutate` | operate | Standard |
| `googleads.customers.uploadClickConversions` | ConversionUploadService | `POST /v26/customers/{customersId}:uploadClickConversions` | operate | Standard |
| `googleads.customers.uploadCallConversions` | ConversionUploadService | `POST /v26/customers/{customersId}:uploadCallConversions` | operate | Standard |
| `googleads.customers.conversionValueRules.mutate` | ConversionValueRuleService | `POST /v26/customers/{customersId}/conversionValueRules:mutate` | operate, spend or admin by operation (FR-VALUE-RULE) | Standard |
| `googleads.customers.conversionValueRuleSets.mutate` | ConversionValueRuleSetService | `POST /v26/customers/{customersId}/conversionValueRuleSets:mutate` | operate, spend or admin by operation (FR-VALUE-RULE) | Standard |
| `googleads.customers.customAudiences.mutate` | CustomAudienceService | `POST /v26/customers/{customersId}/customAudiences:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.customers.customConversionGoals.mutate` | CustomConversionGoalService | `POST /v26/customers/{customersId}/customConversionGoals:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.customers.customInterests.mutate` | CustomInterestService | `POST /v26/customers/{customersId}/customInterests:mutate` | operate | Standard |
| `googleads.customers.customerAssets.mutate` | CustomerAssetService | `POST /v26/customers/{customersId}/customerAssets:mutate` | operate or public by operation (FR-ASSET-LINK) | Standard |
| `googleads.customers.customerAssetSets.mutate` | CustomerAssetSetService | `POST /v26/customers/{customersId}/customerAssetSets:mutate` | public or spend by operation (FR-ASSET-SET-LINK) | Standard |
| `googleads.customers.customerBudgetNetworkSettings.mutate` | CustomerBudgetNetworkSettingService | `POST /v26/customers/{customersId}/customerBudgetNetworkSettings:mutate` | operate or spend by operation (FR-BUDGET-NETWORK) | Standard |
| `googleads.customers.customerClientLinks.mutate` | CustomerClientLinkService | `POST /v26/customers/{customersId}/customerClientLinks:mutate` | admin | Admin on the manager account |
| `googleads.customers.customerConversionGoals.mutate` | CustomerConversionGoalService | `POST /v26/customers/{customersId}/customerConversionGoals:mutate` | operate | Standard |
| `googleads.customers.CustomerCustomizers.mutate` | CustomerCustomizerService | `POST /v26/customers/{customersId}/CustomerCustomizers:mutate` | public | Standard |
| `googleads.customers.customerLabels.mutate` | CustomerLabelService | `POST /v26/customers/{customersId}/customerLabels:mutate` | operate | Standard on the manager |
| `googleads.customers.customerManagerLinks.mutate` | CustomerManagerLinkService | `POST /v26/customers/{customersId}/customerManagerLinks:mutate` | admin | Admin |
| `googleads.customers.customerManagerLinks.moveManagerLink` | CustomerManagerLinkService | `POST /v26/customers/{customersId}/customerManagerLinks:moveManagerLink` | admin | Admin on the manager account |
| `googleads.customers.customerNegativeCriteria.mutate` | CustomerNegativeCriterionService | `POST /v26/customers/{customersId}/customerNegativeCriteria:mutate` | operate or spend by operation (FR-NEGATIVE) | Standard |
| `googleads.customers.mutate` | CustomerService | `POST /v26/customers/{customersId}:mutate` | operate, spend or admin by operation (FR-CUSTOMER) | Standard |
| `googleads.customers.listAccessibleCustomers` | CustomerService | `GET /v26/customers:listAccessibleCustomers` | read | Any Google Ads user |
| `googleads.customers.createCustomerClient` | CustomerService | `POST /v26/customers/{customersId}:createCustomerClient` | admin | Admin on the manager account |
| `googleads.customers.customerSkAdNetworkConversionValueSchemas.mutate` | CustomerSkAdNetworkConversionValueSchemaService | `POST /v26/customers/{customersId}/customerSkAdNetworkConversionValueSchemas:mutate` | operate | Standard |
| `googleads.customers.customerUserAccessInvitations.mutate` | CustomerUserAccessInvitationService | `POST /v26/customers/{customersId}/customerUserAccessInvitations:mutate` | admin | Admin |
| `googleads.customers.customerUserAccesses.mutate` | CustomerUserAccessService | `POST /v26/customers/{customersId}/customerUserAccesses:mutate` | admin | Admin |
| `googleads.customers.customizerAttributes.mutate` | CustomizerAttributeService | `POST /v26/customers/{customersId}/customizerAttributes:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.customers.dataLinks.create` | DataLinkService | `POST /v26/customers/{customersId}/dataLinks:create` | admin | Admin |
| `googleads.customers.dataLinks.remove` | DataLinkService | `POST /v26/customers/{customersId}/dataLinks:remove` | admin | Admin |
| `googleads.customers.dataLinks.update` | DataLinkService | `POST /v26/customers/{customersId}/dataLinks:update` | admin | Admin |
| `googleads.customers.experimentArms.mutate` | ExperimentArmService | `POST /v26/customers/{customersId}/experimentArms:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.customers.experiments.mutate` | ExperimentService | `POST /v26/customers/{customersId}/experiments:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.customers.experiments.endExperiment` | ExperimentService | `POST /v26/customers/{customersId}/experiments/{experimentsId}:endExperiment` | admin | Standard |
| `googleads.customers.experiments.listExperimentAsyncErrors` | ExperimentService | `GET /v26/customers/{customersId}/experiments/{experimentsId}:listExperimentAsyncErrors` | read | Read only |
| `googleads.customers.experiments.graduateExperiment` | ExperimentService | `POST /v26/customers/{customersId}/experiments/{experimentsId}:graduateExperiment` | spend | Standard |
| `googleads.customers.experiments.scheduleExperiment` | ExperimentService | `POST /v26/customers/{customersId}/experiments/{experimentsId}:scheduleExperiment` | admin | Standard |
| `googleads.customers.experiments.promoteExperiment` | ExperimentService | `POST /v26/customers/{customersId}/experiments/{experimentsId}:promoteExperiment` | admin | Standard |
| `googleads.geoTargetConstants.suggest` | GeoTargetConstantService | `POST /v26/geoTargetConstants:suggest` | read | Read only |
| `googleads.customers.Goals.mutate` | GoalService | `POST /v26/customers/{customersId}/Goals:mutate` | operate, public or spend by operation (FR-GOAL) | Standard |
| `googleads.googleAdsFields.get` | GoogleAdsFieldService | `GET /v26/googleAdsFields/{googleAdsFieldsId}` | read | Any authenticated user |
| `googleads.googleAdsFields.search` | GoogleAdsFieldService | `POST /v26/googleAdsFields:search` | read | Any authenticated user |
| `googleads.customers.googleAds.search` | GoogleAdsService | `POST /v26/customers/{customersId}/googleAds:search` | read | Read only |
| `googleads.customers.googleAds.searchStream` | GoogleAdsService | `POST /v26/customers/{customersId}/googleAds:searchStream` | read | Read only |
| `googleads.customers.googleAds.mutate` | GoogleAdsService | `POST /v26/customers/{customersId}/googleAds:mutate` | operate, public, spend or admin by operation (FR-MUTATE) | Standard |
| `googleads.customers.startIdentityVerification` | IdentityVerificationService | `POST /v26/customers/{customersId}:startIdentityVerification` | admin | Admin |
| `googleads.customers.getIdentityVerification` | IdentityVerificationService | `GET /v26/customers/{customersId}/getIdentityVerification` | read | Read only |
| `googleads.incentives.fetchIncentive` | IncentiveService | `GET /v26/incentives:fetchIncentive` | read | Any authenticated user |
| `googleads.customers.incentives.applyIncentive` | IncentiveService | `POST /v26/customers/{customersId}/incentives/{incentivesId}:applyIncentive` | admin | Admin |
| `googleads.customers.invoices.list` | InvoiceService | `GET /v26/customers/{customersId}/invoices` | read | Read only |
| `googleads.customers.keywordPlanAdGroupKeywords.mutate` | KeywordPlanAdGroupKeywordService | `POST /v26/customers/{customersId}/keywordPlanAdGroupKeywords:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.customers.keywordPlanAdGroups.mutate` | KeywordPlanAdGroupService | `POST /v26/customers/{customersId}/keywordPlanAdGroups:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.customers.keywordPlanCampaignKeywords.mutate` | KeywordPlanCampaignKeywordService | `POST /v26/customers/{customersId}/keywordPlanCampaignKeywords:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.customers.keywordPlanCampaigns.mutate` | KeywordPlanCampaignService | `POST /v26/customers/{customersId}/keywordPlanCampaigns:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.customers.generateKeywordIdeas` | KeywordPlanIdeaService | `POST /v26/customers/{customersId}:generateKeywordIdeas` | read | Read only |
| `googleads.customers.generateKeywordHistoricalMetrics` | KeywordPlanIdeaService | `POST /v26/customers/{customersId}:generateKeywordHistoricalMetrics` | read | Read only |
| `googleads.customers.generateAdGroupThemes` | KeywordPlanIdeaService | `POST /v26/customers/{customersId}:generateAdGroupThemes` | read | Read only |
| `googleads.customers.generateKeywordForecastMetrics` | KeywordPlanIdeaService | `POST /v26/customers/{customersId}:generateKeywordForecastMetrics` | read | Read only |
| `googleads.customers.keywordPlans.mutate` | KeywordPlanService | `POST /v26/customers/{customersId}/keywordPlans:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.keywordThemeConstants.suggest` | KeywordThemeConstantService | `POST /v26/keywordThemeConstants:suggest` | read | Read only |
| `googleads.customers.labels.mutate` | LabelService | `POST /v26/customers/{customersId}/labels:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.customers.checkLocalServicesEligibility` | LocalServicesEligibilityService | `POST /v26/customers/{customersId}:checkLocalServicesEligibility` | read | Read only |
| `googleads.customers.localServices.appendLeadConversation` | LocalServicesLeadService | `POST /v26/customers/{customersId}/localServices:appendLeadConversation` | public | Standard |
| `googleads.customers.localServicesLeads.provideLeadFeedback` | LocalServicesLeadService | `POST /v26/customers/{customersId}/localServicesLeads/{localServicesLeadsId}:provideLeadFeedback` | admin | Standard |
| `googleads.customers.multiPartyAuthReview.resolve` | MultiPartyAuthReviewService | `POST /v26/customers/{customersId}/multiPartyAuthReview:resolve` | admin | Admin |
| `googleads.customers.offlineUserDataJobs.create` | OfflineUserDataJobService | `POST /v26/customers/{customersId}/offlineUserDataJobs:create` | operate | Standard |
| `googleads.customers.offlineUserDataJobs.addOperations` | OfflineUserDataJobService | `POST /v26/customers/{customersId}/offlineUserDataJobs/{offlineUserDataJobsId}:addOperations` | operate | Standard |
| `googleads.customers.offlineUserDataJobs.run` | OfflineUserDataJobService | `POST /v26/customers/{customersId}/offlineUserDataJobs/{offlineUserDataJobsId}:run` | operate | Standard |
| `googleads.customers.paymentsAccounts.list` | PaymentsAccountService | `GET /v26/customers/{customersId}/paymentsAccounts` | read | Read only |
| `googleads.customers.productLinkInvitations.create` | ProductLinkInvitationService | `POST /v26/customers/{customersId}/productLinkInvitations:create` | admin | Admin |
| `googleads.customers.productLinkInvitations.update` | ProductLinkInvitationService | `POST /v26/customers/{customersId}/productLinkInvitations:update` | admin | Admin |
| `googleads.customers.productLinkInvitations.remove` | ProductLinkInvitationService | `POST /v26/customers/{customersId}/productLinkInvitations:remove` | admin | Admin |
| `googleads.customers.productLinks.create` | ProductLinkService | `POST /v26/customers/{customersId}/productLinks:create` | admin | Admin |
| `googleads.customers.productLinks.remove` | ProductLinkService | `POST /v26/customers/{customersId}/productLinks:remove` | admin | Admin |
| `googleads.generateConversionRates` | ReachPlanService | `POST /v26:generateConversionRates` | read | Read only |
| `googleads.listPlannableLocations` | ReachPlanService | `POST /v26:listPlannableLocations` | read | Read only |
| `googleads.listPlannableProducts` | ReachPlanService | `POST /v26:listPlannableProducts` | read | Read only |
| `googleads.customers.generateReachForecast` | ReachPlanService | `POST /v26/customers/{customersId}:generateReachForecast` | read | Read only |
| `googleads.listPlannableUserLists` | ReachPlanService | `POST /v26:listPlannableUserLists` | read | Read only |
| `googleads.listPlannableUserInterests` | ReachPlanService | `POST /v26:listPlannableUserInterests` | read | Read only |
| `googleads.customers.recommendations.apply` | RecommendationService | `POST /v26/customers/{customersId}/recommendations:apply` | operate, public, spend or admin by operation (FR-RECOMMENDATION) | Standard |
| `googleads.customers.recommendations.dismiss` | RecommendationService | `POST /v26/customers/{customersId}/recommendations:dismiss` | operate | Standard |
| `googleads.customers.recommendations.generate` | RecommendationService | `POST /v26/customers/{customersId}/recommendations:generate` | read | Read only |
| `googleads.customers.recommendationSubscriptions.mutateRecommendationSubscription` | RecommendationSubscriptionService | `POST /v26/customers/{customersId}/recommendationSubscriptions:mutateRecommendationSubscription` | operate, public, spend or admin by operation (FR-REC-SUBSCRIPTION) | Standard |
| `googleads.customers.remarketingActions.mutate` | RemarketingActionService | `POST /v26/customers/{customersId}/remarketingActions:mutate` | operate | Standard |
| `googleads.customers.reservations.quoteCampaigns` | ReservationService | `POST /v26/customers/{customersId}/reservations:quoteCampaigns` | read | Standard |
| `googleads.customers.reservations.bookCampaigns` | ReservationService | `POST /v26/customers/{customersId}/reservations:bookCampaigns` | admin | Standard |
| `googleads.customers.generateShareablePreviews` | ShareablePreviewService | `POST /v26/customers/{customersId}:generateShareablePreviews` | read | Read only |
| `googleads.customers.sharedCriteria.mutate` | SharedCriterionService | `POST /v26/customers/{customersId}/sharedCriteria:mutate` | operate or spend by operation (FR-NEGATIVE-LIST) | Standard |
| `googleads.customers.sharedSets.mutate` | SharedSetService | `POST /v26/customers/{customersId}/sharedSets:mutate` | operate or spend by operation (FR-SHARED-SET) | Standard |
| `googleads.customers.smartCampaignSettings.getSmartCampaignStatus` | SmartCampaignSettingService | `GET /v26/customers/{customersId}/smartCampaignSettings/{smartCampaignSettingsId}:getSmartCampaignStatus` | read | Read only |
| `googleads.customers.smartCampaignSettings.mutate` | SmartCampaignSettingService | `POST /v26/customers/{customersId}/smartCampaignSettings:mutate` | public | Standard |
| `googleads.customers.smartCampaignSettings.generatePMaxDraftCampaign` | SmartCampaignSettingService | `POST /v26/customers/{customersId}/smartCampaignSettings/{smartCampaignSettingsId}:generatePMaxDraftCampaign` | operate | Standard |
| `googleads.customers.suggestSmartCampaignBudgetOptions` | SmartCampaignSuggestService | `POST /v26/customers/{customersId}:suggestSmartCampaignBudgetOptions` | read | Read only |
| `googleads.customers.suggestSmartCampaignAd` | SmartCampaignSuggestService | `POST /v26/customers/{customersId}:suggestSmartCampaignAd` | read | Read only |
| `googleads.customers.suggestKeywordThemes` | SmartCampaignSuggestService | `POST /v26/customers/{customersId}:suggestKeywordThemes` | read | Read only |
| `googleads.customers.thirdPartyAppAnalyticsLinks.regenerateShareableLinkId` | ThirdPartyAppAnalyticsLinkService | `POST /v26/customers/{customersId}/thirdPartyAppAnalyticsLinks/{thirdPartyAppAnalyticsLinksId}:regenerateShareableLinkId` | admin | Admin |
| `googleads.customers.suggestTravelAssets` | TravelAssetSuggestionService | `POST /v26/customers/{customersId}:suggestTravelAssets` | read | Read only |
| `googleads.customers.uploadUserData` | UserDataService | `POST /v26/customers/{customersId}:uploadUserData` | operate | Standard |
| `googleads.customers.userListCustomerTypes.mutate` | UserListCustomerTypeService | `POST /v26/customers/{customersId}/userListCustomerTypes:mutate` | operate | Standard |
| `googleads.customers.userLists.mutate` | UserListService | `POST /v26/customers/{customersId}/userLists:mutate` | operate or admin by operation (FR-ENTITY) | Standard |
| `googleads.media.upload` | YouTubeVideoUploadService | `POST /v26/customers/{customersId}/youTubeVideoUploads:create` | public | Standard |
| `googleads.customers.youTubeVideoUploads.update` | YouTubeVideoUploadService | `POST /v26/customers/{customersId}/youTubeVideoUploads:update` | public | Standard |
| `googleads.customers.youTubeVideoUploads.remove` | YouTubeVideoUploadService | `POST /v26/customers/{customersId}/youTubeVideoUploads:remove` | public | Standard |
| `googleads.customers.operations.cancel` | google.longrunning.Operations | `POST /v26/customers/{customersId}/operations/{operationsId}:cancel` | operate | Standard |
| `googleads.customers.operations.delete` | google.longrunning.Operations | `DELETE /v26/customers/{customersId}/operations/{operationsId}` | admin | Standard |
| `googleads.customers.operations.get` | google.longrunning.Operations | `GET /v26/customers/{customersId}/operations/{operationsId}` | read | Read only |
| `googleads.customers.operations.list` | google.longrunning.Operations | `GET /v26/customers/{customersId}/operations` | read | Read only |
| `googleads.customers.operations.wait` | google.longrunning.Operations | `POST /v26/customers/{customersId}/operations/{operationsId}:wait` | read | Read only |


**Notes on the classing:**
- Nothing is offered at `never`: no method is technically impossible or forbidden by Google's terms. Methods behind an allowlist (reservations, incentives, benchmarks, new Customer Match and click-conversion adopters) are offered; Google answers 403 where the account is not allowlisted.
- A method whose request names no amount a cap could hold is `admin`, not `spend`: running a batch job, promoting a draft, scheduling or promoting an experiment, booking reservations, applying an incentive (its terms move money) and lead feedback (a lead's credit). Graduating an experiment is `spend`, held to the caps by the budgets its mappings name.
- A recommendation applied is its type's class. A type that may set a budget is `spend` only where the request names the new amount (`campaignBudget.newBudgetAmountMicros`), held to the caps on the budget the recommendation targets, and `admin` otherwise; subscribing to such a type is `admin`, since Google picks the amount each time it applies one.
- A conversion value rule, and a conversion action's value settings, are `spend` where they make a conversion worth more, as goals are: value bidding bids more.
- Removes are terminal in Google Ads, so removing a container is `admin`; removing an ad or an asset link retracts published content and is `public`.

## What the atlas shows

Each account the company names (`integrations.google_ads.customers`) is an account with the `advertising` capability, keyed by its customer ID; a company that names none shows Google Ads as one account ([the atlas](../concepts/atlas.md)).

## Google Ads' limits worth knowing

- The daily operations limit belongs to the identity project's API access level (2,880 a day on Explorer against production accounts, 15,000 on Basic, unlimited on Standard), counted over a sliding 24 hours; a search is one operation however many pages it reads.
- An account budget changes at most once in 12 hours, and only on monthly invoicing.
