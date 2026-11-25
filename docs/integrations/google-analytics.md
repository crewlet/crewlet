# Google Analytics

Google Analytics lets agents **read and manage your Google Analytics 4 properties through engine tools**, as their own Google identity. Every method of the Google Analytics Data API, the Admin API (v1beta and v1alpha) and the Measurement Protocol is reachable; see [Every method, and its class](#every-method-and-its-class).

- **No token reaches an agent's box.** Each agent signs in as its own Google growth identity, a service account the engine makes in your identity project, and its tokens are minted inside the engine for an hour, with the narrowest scope each call accepts ([ADR-0172](../../adr/0172-a-google-growth-product-signs-in-as-each-seats-own-growth-identity-never-its-cloud-identity.md)).
- **Two ceilings, both kept.** The role you give the agent's identity in each property is Google's ceiling; the agent's level, its properties and each method's class are Crewlet's. A call needs both.
- **Account, access, key and irreversible changes are asked first,** one each, of an administrator at a fresh sign-in, never a delegate ([ADR-0171](../../adr/0171-spend-public-and-admin-acts-are-held-for-an-administrator-never-a-delegate.md)).

Nothing here receives events from Google Analytics.

## What agents get

An agent is on at one level across every property it is granted.

| An agent at | Can | Needs, on the property |
|---|---|---|
| `read` (the default) | Every report (standard, pivot, realtime, funnel, audience exports, report tasks), metadata, every setting read, change history and the access report where its role allows | Viewer or above. Change history needs Editor; the access report and access listing need Administrator |
| `operate` | Everything `read` can, plus reversible changes made at once and recorded: key events, custom dimensions and metrics, audiences, annotations, channel groups, event rules, data stream and reporting settings | Marketer for key events, audiences and attribution; Editor for the rest |
| `administer` | Everything `operate` can, plus asking an administrator to approve each account, access, link, key, deletion or other irreversible change | Editor; Administrator for access bindings |

Raising an agent to `operate` or `administer` asks for a fresh sign-in (step-up).

The roles Google Analytics gives, least first: **Viewer** (sees settings and data, runs every report), **Analyst** (adds annotations and shared explorations), **Marketer** (adds key events, audiences, event create rules and attribution; set in Google Analytics, since its API cannot grant it), **Editor** (every setting, stream, link, key and deletion, but not users), **Administrator** (adds users and access). **No Cost Metrics** and **No Revenue Metrics** are restrictions beside a role: those metrics read as zero.

## Connecting Crewlet

1. Connect [Google Cloud](google-cloud.md) first; no project needs preparing, and the form says whether it is connected. Turn Google Analytics on, then run the Google Cloud connection's Cloud Shell block again: it turns on the Analytics Data and Admin APIs in the identity project and makes the company reader ([Google's growth products sign in through this identity](google-cloud.md#googles-growth-products-sign-in-through-this-identity)).
2. Add the company reader's email, shown on the form, to each property at **Viewer**: Google Analytics, Admin, Property access management, Add users. Crewlet reads the company's figures for boards and the atlas as this identity.
3. Turn agents on from the Agents tab, each with its properties, and give each its identity ([below](#give-an-agent-its-own-identity)).

The integration's Settings tab then lists each connection and the properties it reaches, with the Google Analytics logo under them, linked to Google Analytics, as [Google's developer branding policy](https://developers.google.com/analytics/terms/branding-policy) asks of an application that shows Google Analytics data.

## Give an agent its own identity

An agent's identity at Google Analytics is its own Google growth identity, which Crewlet made; what is left is adding it to its properties. Its row on the Agents tab reads **Needs you** until the identity reaches each of them, and **Set up identity** in its box shows the steps, with the email ready to copy:

1. In Google Analytics, open **Admin**, choose the property under Property, and press **Property access management**.
2. Press **+**, then **Add users**, and paste the agent's email.
3. Pick the role its level needs, **Viewer** at `read` and **Editor** at `operate` and `administer`, and press **Add**. Access changes an `administer` agent asks for need **Administrator**.

**Add it for me.** With the company's own OAuth client (`oauth`, made as [Google Cloud's page describes](google-cloud.md#the-companys-own-oauth-client), which Google Play may share) and an Administrator's sign-in on the Settings tab, **Add it for me** binds the identity to each property at the level's role through the Admin API's access bindings, where it holds no role at or above it on the property or its account, keeping any No Cost Metrics or No Revenue Metrics restriction. Turning the agent off removes those bindings. The sign-in only adds, reads and removes agents' identities; no agent's call is made with it.

**The check, on every pass:** Crewlet signs in as the identity and lists the properties it reaches; one not listed is not added yet. With the admin sign-in it also reads the identity's role on each property and its account, which every property inherits: one below the level's is the wrong role, and Administrator below `administer` is a warning. The agent is ready once every property passes, and reads Needs you again if somebody removes it there; calls Google refuses meanwhile name what is missing.

## The agents' tools

| Tool | Class | What it does |
|---|---|---|
| `google_analytics_list_properties` | read | The accounts and properties the agent may reach, and the ones of its list its identity is not added to yet |
| `google_analytics_run_report` | read | A report on one property: dimensions and metrics over a date range, with Google's thresholding, sampling and other-row flags, the row count and the property's remaining quota |
| `google_analytics_run_realtime_report` | read | The property's last 30 minutes (60 on Analytics 360) |
| `google_analytics_mark_key_event` | operate | Marks an event as a key event |
| `google_analytics_read` | read | Any method that only reads, by its ID |
| `google_analytics_operate` | operate | Any reversible change, by its method's ID |
| `google_analytics_admin` | admin | Any account, access, link, key or irreversible change, by its method's ID: asked first |

A write whose class depends on what it changes is classed per call from its update mask and its new values, however the mask and the body spell a field (`displayName` or `display_name`, `updateMask` or `update_mask`): renaming a custom metric is `operate`, and taking off its revenue or cost restriction is `admin`. A field no rule names takes the method's highest class, and a `*` mask counts every field the resource holds, since it replaces them all. A call on the wrong tool for what it changes is refused with the tool that may make it.

**What a call may reach:**
- A property outside the agent's list is refused before Google is asked, whether the path names it or the body does: the account a property is created in, a roll-up's sources, the subproperty a filter applies to.
- A call on an account is made only where every property of that account is on the agent's list, since it reaches all of them.
- A write that names no property or account is refused, except opening a new account, which reaches none that exists.
- A listing is cut to the agent's properties.

**How a change is made:** a change made at once, and an approved one once its answer is on the access log, is made under its property's or account's lease, reading its target again first, and sends nothing where the target moved since it was read or approved. A change that failed before its request left (its lease, the read again, its context's end) is recorded as failed and told to the agent as nothing changed; one whose request got no answer from Google is recorded as an unknown outcome, never as not made.

**What an agent reads:** every value Google answers with is fenced as somebody else's words, since anyone can write into a property (page titles, referrers, search terms, campaign names, event parameters) and property users write names and descriptions. A Measurement Protocol secret's value and a webhook's channel token are withheld from every answer; a Measurement Protocol call names its stream secret by resource name (`query.api_secret_name`), held to the agent's properties, and the engine fills in the value only as the call is sent, so no approval, record or error holds it.

## Configuration

```yaml
integrations:
  google_analytics:
    enabled: true
    properties: ["123456789"]   # optional: the properties agents may be given; absent is every one an identity reaches
    oauth:                      # optional: the client an Administrator signs in as, for Add it for me
      client_id: "123456789012-abc.apps.googleusercontent.com"
      client_secret: ${GOOGLE_OAUTH_CLIENT_SECRET}
    approvers: [founder]        # optional: who an admin act is routed to first; only an administrator answers
roles:
  - handle: growth-lead
    integrations:
      google_analytics: {enabled: true, level: read, properties: ["123456789"]}
```

The block needs `integrations.google_cloud` on with its provisioning block. Its one secret is the optional OAuth client's.

## Every method, and its class

Each row is one method the engine offers, with its class, the least role Google asks of the identity, and the scope minted for it. The table is held to the engine's catalog by a test, both ways.

| Method | Resource | Call | Class | Least role | Scope |
|---|---|---|---|---|---|
| `analyticsdata.properties.batchRunPivotReports` | properties | `POST /v1beta/properties/{propertiesId}:batchRunPivotReports` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.batchRunReports` | properties | `POST /v1beta/properties/{propertiesId}:batchRunReports` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.chat` | properties | `POST /v1alpha/properties/{propertiesId}:chat` | read | Viewer (property) | analytics.chatbot.read |
| `analyticsdata.properties.checkCompatibility` | properties | `POST /v1beta/properties/{propertiesId}:checkCompatibility` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.getMetadata` | properties | `GET /v1beta/properties/{propertiesId}/metadata` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.getPropertyQuotasSnapshot` | properties | `GET /v1alpha/properties/{propertiesId}/propertyQuotasSnapshot` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.runFunnelReport` | properties | `POST /v1alpha/properties/{propertiesId}:runFunnelReport` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.runPivotReport` | properties | `POST /v1beta/properties/{propertiesId}:runPivotReport` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.runRealtimeReport` | properties | `POST /v1beta/properties/{propertiesId}:runRealtimeReport` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.runReport` | properties | `POST /v1beta/properties/{propertiesId}:runReport` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.audienceExports.list` | properties.audienceExports | `GET /v1beta/properties/{propertiesId}/audienceExports` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.audienceExports.get` | properties.audienceExports | `GET /v1beta/properties/{propertiesId}/audienceExports/{audienceExportsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.audienceExports.create` | properties.audienceExports | `POST /v1beta/properties/{propertiesId}/audienceExports` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.audienceExports.query` | properties.audienceExports | `POST /v1beta/properties/{propertiesId}/audienceExports/{audienceExportsId}:query` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.audienceLists.list` | properties.audienceLists | `GET /v1alpha/properties/{propertiesId}/audienceLists` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.audienceLists.get` | properties.audienceLists | `GET /v1alpha/properties/{propertiesId}/audienceLists/{audienceListsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.audienceLists.create` | properties.audienceLists | `POST /v1alpha/properties/{propertiesId}/audienceLists` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.audienceLists.query` | properties.audienceLists | `POST /v1alpha/properties/{propertiesId}/audienceLists/{audienceListsId}:query` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.recurringAudienceLists.list` | properties.recurringAudienceLists | `GET /v1alpha/properties/{propertiesId}/recurringAudienceLists` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.recurringAudienceLists.get` | properties.recurringAudienceLists | `GET /v1alpha/properties/{propertiesId}/recurringAudienceLists/{recurringAudienceListsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.recurringAudienceLists.create` | properties.recurringAudienceLists | `POST /v1alpha/properties/{propertiesId}/recurringAudienceLists` | admin | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.reportTasks.list` | properties.reportTasks | `GET /v1alpha/properties/{propertiesId}/reportTasks` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.reportTasks.get` | properties.reportTasks | `GET /v1alpha/properties/{propertiesId}/reportTasks/{reportTasksId}` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.reportTasks.create` | properties.reportTasks | `POST /v1alpha/properties/{propertiesId}/reportTasks` | read | Viewer (property) | analytics.readonly |
| `analyticsdata.properties.reportTasks.query` | properties.reportTasks | `POST /v1alpha/properties/{propertiesId}/reportTasks/{reportTasksId}:query` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.accountSummaries.list` | accountSummaries | `GET /v1beta/accountSummaries` | read | any role on the account or one of its properties | analytics.readonly |
| `analyticsadmin.accounts.list` | accounts | `GET /v1beta/accounts` | read | any role | analytics.readonly |
| `analyticsadmin.accounts.get` | accounts | `GET /v1beta/accounts/{accountsId}` | read | Viewer (account) | analytics.readonly |
| `analyticsadmin.accounts.patch` | accounts | `PATCH /v1beta/accounts/{accountsId}` | admin | Editor (account) | analytics.edit |
| `analyticsadmin.accounts.getDataSharingSettings` | accounts | `GET /v1beta/accounts/{accountsId}/dataSharingSettings` | read | Viewer (account) | analytics.readonly |
| `analyticsadmin.accounts.provisionAccountTicket` | accounts | `POST /v1beta/accounts:provisionAccountTicket` | admin | any signed-in Google identity | analytics.edit |
| `analyticsadmin.accounts.runAccessReport` | accounts | `POST /v1beta/accounts/{accountsId}:runAccessReport` | read | Administrator (account) | analytics.readonly |
| `analyticsadmin.accounts.searchChangeHistoryEvents` | accounts | `POST /v1beta/accounts/{accountsId}:searchChangeHistoryEvents` | read | Editor (account) | analytics.edit |
| `analyticsadmin.accounts.delete` | accounts | `DELETE /v1beta/accounts/{accountsId}` | admin | Editor (account) | analytics.edit |
| `analyticsadmin.properties.list` | properties | `GET /v1beta/properties` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.get` | properties | `GET /v1beta/properties/{propertiesId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.create` | properties | `POST /v1beta/properties` | admin | Editor (account) | analytics.edit |
| `analyticsadmin.properties.patch` | properties | `PATCH /v1beta/properties/{propertiesId}` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.acknowledgeUserDataCollection` | properties | `POST /v1beta/properties/{propertiesId}:acknowledgeUserDataCollection` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.createRollupProperty` | properties | `POST /v1alpha/properties:createRollupProperty` | admin | Editor (account) and Editor on each source property | analytics.edit |
| `analyticsadmin.properties.getAttributionSettings` | properties | `GET /v1alpha/properties/{propertiesId}/attributionSettings` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.getDataRetentionSettings` | properties | `GET /v1beta/properties/{propertiesId}/dataRetentionSettings` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.getGoogleSignalsSettings` | properties | `GET /v1alpha/properties/{propertiesId}/googleSignalsSettings` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.getReportingIdentitySettings` | properties | `GET /v1alpha/properties/{propertiesId}/reportingIdentitySettings` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.getUserProvidedDataSettings` | properties | `GET /v1alpha/properties/{propertiesId}/userProvidedDataSettings` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.provisionSubproperty` | properties | `POST /v1alpha/properties:provisionSubproperty` | admin | Editor (account) and Editor on the source property | analytics.edit |
| `analyticsadmin.properties.runAccessReport` | properties | `POST /v1beta/properties/{propertiesId}:runAccessReport` | read | Administrator (property) | analytics.readonly |
| `analyticsadmin.properties.submitUserDeletion` | properties | `POST /v1alpha/properties/{propertiesId}:submitUserDeletion` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.updateAttributionSettings` | properties | `PATCH /v1alpha/properties/{propertiesId}/attributionSettings` | operate | Marketer (property) | analytics.edit |
| `analyticsadmin.properties.updateDataRetentionSettings` | properties | `PATCH /v1beta/properties/{propertiesId}/dataRetentionSettings` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.updateGoogleSignalsSettings` | properties | `PATCH /v1alpha/properties/{propertiesId}/googleSignalsSettings` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.updateReportingIdentitySettings` | properties | `PATCH /v1alpha/properties/{propertiesId}/reportingIdentitySettings` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.delete` | properties | `DELETE /v1beta/properties/{propertiesId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.accounts.accessBindings.list` | accounts.accessBindings | `GET /v1alpha/accounts/{accountsId}/accessBindings` | read | Administrator (account) | analytics.manage.users.readonly |
| `analyticsadmin.accounts.accessBindings.get` | accounts.accessBindings | `GET /v1alpha/accounts/{accountsId}/accessBindings/{accessBindingsId}` | read | Administrator (account) | analytics.manage.users.readonly |
| `analyticsadmin.accounts.accessBindings.batchGet` | accounts.accessBindings | `GET /v1alpha/accounts/{accountsId}/accessBindings:batchGet` | read | Administrator (account) | analytics.manage.users.readonly |
| `analyticsadmin.accounts.accessBindings.create` | accounts.accessBindings | `POST /v1alpha/accounts/{accountsId}/accessBindings` | admin | Administrator (account) | analytics.manage.users |
| `analyticsadmin.accounts.accessBindings.batchCreate` | accounts.accessBindings | `POST /v1alpha/accounts/{accountsId}/accessBindings:batchCreate` | admin | Administrator (account) | analytics.manage.users |
| `analyticsadmin.accounts.accessBindings.patch` | accounts.accessBindings | `PATCH /v1alpha/accounts/{accountsId}/accessBindings/{accessBindingsId}` | admin | Administrator (account) | analytics.manage.users |
| `analyticsadmin.accounts.accessBindings.batchUpdate` | accounts.accessBindings | `POST /v1alpha/accounts/{accountsId}/accessBindings:batchUpdate` | admin | Administrator (account) | analytics.manage.users |
| `analyticsadmin.accounts.accessBindings.batchDelete` | accounts.accessBindings | `POST /v1alpha/accounts/{accountsId}/accessBindings:batchDelete` | admin | Administrator (account) | analytics.manage.users |
| `analyticsadmin.accounts.accessBindings.delete` | accounts.accessBindings | `DELETE /v1alpha/accounts/{accountsId}/accessBindings/{accessBindingsId}` | admin | Administrator (account) | analytics.manage.users |
| `analyticsadmin.properties.accessBindings.list` | properties.accessBindings | `GET /v1alpha/properties/{propertiesId}/accessBindings` | read | Administrator (property) | analytics.manage.users.readonly |
| `analyticsadmin.properties.accessBindings.get` | properties.accessBindings | `GET /v1alpha/properties/{propertiesId}/accessBindings/{accessBindingsId}` | read | Administrator (property) | analytics.manage.users.readonly |
| `analyticsadmin.properties.accessBindings.batchGet` | properties.accessBindings | `GET /v1alpha/properties/{propertiesId}/accessBindings:batchGet` | read | Administrator (property) | analytics.manage.users.readonly |
| `analyticsadmin.properties.accessBindings.create` | properties.accessBindings | `POST /v1alpha/properties/{propertiesId}/accessBindings` | admin | Administrator (property) | analytics.manage.users |
| `analyticsadmin.properties.accessBindings.batchCreate` | properties.accessBindings | `POST /v1alpha/properties/{propertiesId}/accessBindings:batchCreate` | admin | Administrator (property) | analytics.manage.users |
| `analyticsadmin.properties.accessBindings.patch` | properties.accessBindings | `PATCH /v1alpha/properties/{propertiesId}/accessBindings/{accessBindingsId}` | admin | Administrator (property) | analytics.manage.users |
| `analyticsadmin.properties.accessBindings.batchUpdate` | properties.accessBindings | `POST /v1alpha/properties/{propertiesId}/accessBindings:batchUpdate` | admin | Administrator (property) | analytics.manage.users |
| `analyticsadmin.properties.accessBindings.batchDelete` | properties.accessBindings | `POST /v1alpha/properties/{propertiesId}/accessBindings:batchDelete` | admin | Administrator (property) | analytics.manage.users |
| `analyticsadmin.properties.accessBindings.delete` | properties.accessBindings | `DELETE /v1alpha/properties/{propertiesId}/accessBindings/{accessBindingsId}` | admin | Administrator (property) | analytics.manage.users |
| `analyticsadmin.properties.adSenseLinks.list` | properties.adSenseLinks | `GET /v1alpha/properties/{propertiesId}/adSenseLinks` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.adSenseLinks.get` | properties.adSenseLinks | `GET /v1alpha/properties/{propertiesId}/adSenseLinks/{adSenseLinksId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.adSenseLinks.create` | properties.adSenseLinks | `POST /v1alpha/properties/{propertiesId}/adSenseLinks` | admin | Editor (property) plus access to the AdSense account | analytics.edit |
| `analyticsadmin.properties.adSenseLinks.delete` | properties.adSenseLinks | `DELETE /v1alpha/properties/{propertiesId}/adSenseLinks/{adSenseLinksId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.audiences.list` | properties.audiences | `GET /v1alpha/properties/{propertiesId}/audiences` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.audiences.get` | properties.audiences | `GET /v1alpha/properties/{propertiesId}/audiences/{audiencesId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.audiences.create` | properties.audiences | `POST /v1alpha/properties/{propertiesId}/audiences` | operate | Marketer (property) | analytics.edit |
| `analyticsadmin.properties.audiences.patch` | properties.audiences | `PATCH /v1alpha/properties/{propertiesId}/audiences/{audiencesId}` | operate | Marketer (property) | analytics.edit |
| `analyticsadmin.properties.audiences.archive` | properties.audiences | `POST /v1alpha/properties/{propertiesId}/audiences/{audiencesId}:archive` | admin | Marketer (property) | analytics.edit |
| `analyticsadmin.properties.bigQueryLinks.list` | properties.bigQueryLinks | `GET /v1alpha/properties/{propertiesId}/bigQueryLinks` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.bigQueryLinks.get` | properties.bigQueryLinks | `GET /v1alpha/properties/{propertiesId}/bigQueryLinks/{bigQueryLinksId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.bigQueryLinks.create` | properties.bigQueryLinks | `POST /v1alpha/properties/{propertiesId}/bigQueryLinks` | admin | Editor (property) plus OWNER (or the listed IAM permissions) on the Cloud project | analytics.edit |
| `analyticsadmin.properties.bigQueryLinks.patch` | properties.bigQueryLinks | `PATCH /v1alpha/properties/{propertiesId}/bigQueryLinks/{bigQueryLinksId}` | operate or admin by field | Editor (property) | analytics.edit |
| `analyticsadmin.properties.bigQueryLinks.delete` | properties.bigQueryLinks | `DELETE /v1alpha/properties/{propertiesId}/bigQueryLinks/{bigQueryLinksId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.calculatedMetrics.list` | properties.calculatedMetrics | `GET /v1alpha/properties/{propertiesId}/calculatedMetrics` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.calculatedMetrics.get` | properties.calculatedMetrics | `GET /v1alpha/properties/{propertiesId}/calculatedMetrics/{calculatedMetricsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.calculatedMetrics.create` | properties.calculatedMetrics | `POST /v1alpha/properties/{propertiesId}/calculatedMetrics` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.calculatedMetrics.patch` | properties.calculatedMetrics | `PATCH /v1alpha/properties/{propertiesId}/calculatedMetrics/{calculatedMetricsId}` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.calculatedMetrics.delete` | properties.calculatedMetrics | `DELETE /v1alpha/properties/{propertiesId}/calculatedMetrics/{calculatedMetricsId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.channelGroups.list` | properties.channelGroups | `GET /v1alpha/properties/{propertiesId}/channelGroups` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.channelGroups.get` | properties.channelGroups | `GET /v1alpha/properties/{propertiesId}/channelGroups/{channelGroupsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.channelGroups.create` | properties.channelGroups | `POST /v1alpha/properties/{propertiesId}/channelGroups` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.channelGroups.patch` | properties.channelGroups | `PATCH /v1alpha/properties/{propertiesId}/channelGroups/{channelGroupsId}` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.channelGroups.delete` | properties.channelGroups | `DELETE /v1alpha/properties/{propertiesId}/channelGroups/{channelGroupsId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.conversionEvents.list` | properties.conversionEvents | `GET /v1beta/properties/{propertiesId}/conversionEvents` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.conversionEvents.get` | properties.conversionEvents | `GET /v1beta/properties/{propertiesId}/conversionEvents/{conversionEventsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.conversionEvents.create` | properties.conversionEvents | `POST /v1beta/properties/{propertiesId}/conversionEvents` | operate | Marketer (property) | analytics.edit |
| `analyticsadmin.properties.conversionEvents.patch` | properties.conversionEvents | `PATCH /v1beta/properties/{propertiesId}/conversionEvents/{conversionEventsId}` | operate | Marketer (property) | analytics.edit |
| `analyticsadmin.properties.conversionEvents.delete` | properties.conversionEvents | `DELETE /v1beta/properties/{propertiesId}/conversionEvents/{conversionEventsId}` | admin | Marketer (property) | analytics.edit |
| `analyticsadmin.properties.customDimensions.list` | properties.customDimensions | `GET /v1beta/properties/{propertiesId}/customDimensions` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.customDimensions.get` | properties.customDimensions | `GET /v1beta/properties/{propertiesId}/customDimensions/{customDimensionsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.customDimensions.create` | properties.customDimensions | `POST /v1beta/properties/{propertiesId}/customDimensions` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.customDimensions.patch` | properties.customDimensions | `PATCH /v1beta/properties/{propertiesId}/customDimensions/{customDimensionsId}` | operate or admin by field | Editor (property) | analytics.edit |
| `analyticsadmin.properties.customDimensions.archive` | properties.customDimensions | `POST /v1beta/properties/{propertiesId}/customDimensions/{customDimensionsId}:archive` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.customMetrics.list` | properties.customMetrics | `GET /v1beta/properties/{propertiesId}/customMetrics` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.customMetrics.get` | properties.customMetrics | `GET /v1beta/properties/{propertiesId}/customMetrics/{customMetricsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.customMetrics.create` | properties.customMetrics | `POST /v1beta/properties/{propertiesId}/customMetrics` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.customMetrics.patch` | properties.customMetrics | `PATCH /v1beta/properties/{propertiesId}/customMetrics/{customMetricsId}` | operate or admin by field | Editor (property) | analytics.edit |
| `analyticsadmin.properties.customMetrics.archive` | properties.customMetrics | `POST /v1beta/properties/{propertiesId}/customMetrics/{customMetricsId}:archive` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.list` | properties.dataStreams | `GET /v1beta/properties/{propertiesId}/dataStreams` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.dataStreams.get` | properties.dataStreams | `GET /v1beta/properties/{propertiesId}/dataStreams/{dataStreamsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.dataStreams.create` | properties.dataStreams | `POST /v1beta/properties/{propertiesId}/dataStreams` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.patch` | properties.dataStreams | `PATCH /v1beta/properties/{propertiesId}/dataStreams/{dataStreamsId}` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.getDataRedactionSettings` | properties.dataStreams | `GET /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/dataRedactionSettings` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.dataStreams.getEnhancedMeasurementSettings` | properties.dataStreams | `GET /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/enhancedMeasurementSettings` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.dataStreams.getGlobalSiteTag` | properties.dataStreams | `GET /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/globalSiteTag` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.dataStreams.updateDataRedactionSettings` | properties.dataStreams | `PATCH /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/dataRedactionSettings` | operate or admin by field | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.updateEnhancedMeasurementSettings` | properties.dataStreams | `PATCH /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/enhancedMeasurementSettings` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.delete` | properties.dataStreams | `DELETE /v1beta/properties/{propertiesId}/dataStreams/{dataStreamsId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.displayVideo360AdvertiserLinkProposals.list` | properties.displayVideo360AdvertiserLinkProposals | `GET /v1alpha/properties/{propertiesId}/displayVideo360AdvertiserLinkProposals` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.displayVideo360AdvertiserLinkProposals.get` | properties.displayVideo360AdvertiserLinkProposals | `GET /v1alpha/properties/{propertiesId}/displayVideo360AdvertiserLinkProposals/{displayVideo360AdvertiserLinkProposalsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.displayVideo360AdvertiserLinkProposals.create` | properties.displayVideo360AdvertiserLinkProposals | `POST /v1alpha/properties/{propertiesId}/displayVideo360AdvertiserLinkProposals` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.displayVideo360AdvertiserLinkProposals.approve` | properties.displayVideo360AdvertiserLinkProposals | `POST /v1alpha/properties/{propertiesId}/displayVideo360AdvertiserLinkProposals/{displayVideo360AdvertiserLinkProposalsId}:approve` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.displayVideo360AdvertiserLinkProposals.cancel` | properties.displayVideo360AdvertiserLinkProposals | `POST /v1alpha/properties/{propertiesId}/displayVideo360AdvertiserLinkProposals/{displayVideo360AdvertiserLinkProposalsId}:cancel` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.displayVideo360AdvertiserLinkProposals.delete` | properties.displayVideo360AdvertiserLinkProposals | `DELETE /v1alpha/properties/{propertiesId}/displayVideo360AdvertiserLinkProposals/{displayVideo360AdvertiserLinkProposalsId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.displayVideo360AdvertiserLinks.list` | properties.displayVideo360AdvertiserLinks | `GET /v1alpha/properties/{propertiesId}/displayVideo360AdvertiserLinks` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.displayVideo360AdvertiserLinks.get` | properties.displayVideo360AdvertiserLinks | `GET /v1alpha/properties/{propertiesId}/displayVideo360AdvertiserLinks/{displayVideo360AdvertiserLinksId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.displayVideo360AdvertiserLinks.create` | properties.displayVideo360AdvertiserLinks | `POST /v1alpha/properties/{propertiesId}/displayVideo360AdvertiserLinks` | admin | Editor (property) plus access to the Display & Video 360 advertiser | analytics.edit |
| `analyticsadmin.properties.displayVideo360AdvertiserLinks.patch` | properties.displayVideo360AdvertiserLinks | `PATCH /v1alpha/properties/{propertiesId}/displayVideo360AdvertiserLinks/{displayVideo360AdvertiserLinksId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.displayVideo360AdvertiserLinks.delete` | properties.displayVideo360AdvertiserLinks | `DELETE /v1alpha/properties/{propertiesId}/displayVideo360AdvertiserLinks/{displayVideo360AdvertiserLinksId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.expandedDataSets.list` | properties.expandedDataSets | `GET /v1alpha/properties/{propertiesId}/expandedDataSets` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.expandedDataSets.get` | properties.expandedDataSets | `GET /v1alpha/properties/{propertiesId}/expandedDataSets/{expandedDataSetsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.expandedDataSets.create` | properties.expandedDataSets | `POST /v1alpha/properties/{propertiesId}/expandedDataSets` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.expandedDataSets.patch` | properties.expandedDataSets | `PATCH /v1alpha/properties/{propertiesId}/expandedDataSets/{expandedDataSetsId}` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.expandedDataSets.delete` | properties.expandedDataSets | `DELETE /v1alpha/properties/{propertiesId}/expandedDataSets/{expandedDataSetsId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.firebaseLinks.list` | properties.firebaseLinks | `GET /v1beta/properties/{propertiesId}/firebaseLinks` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.firebaseLinks.create` | properties.firebaseLinks | `POST /v1beta/properties/{propertiesId}/firebaseLinks` | admin | Editor (property) plus owner rights on the Firebase project | analytics.edit |
| `analyticsadmin.properties.firebaseLinks.delete` | properties.firebaseLinks | `DELETE /v1beta/properties/{propertiesId}/firebaseLinks/{firebaseLinksId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.googleAdsLinks.list` | properties.googleAdsLinks | `GET /v1beta/properties/{propertiesId}/googleAdsLinks` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.googleAdsLinks.create` | properties.googleAdsLinks | `POST /v1beta/properties/{propertiesId}/googleAdsLinks` | admin | Editor (property) plus administrative access on the Google Ads account | analytics.edit |
| `analyticsadmin.properties.googleAdsLinks.patch` | properties.googleAdsLinks | `PATCH /v1beta/properties/{propertiesId}/googleAdsLinks/{googleAdsLinksId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.googleAdsLinks.delete` | properties.googleAdsLinks | `DELETE /v1beta/properties/{propertiesId}/googleAdsLinks/{googleAdsLinksId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.keyEvents.list` | properties.keyEvents | `GET /v1beta/properties/{propertiesId}/keyEvents` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.keyEvents.get` | properties.keyEvents | `GET /v1beta/properties/{propertiesId}/keyEvents/{keyEventsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.keyEvents.create` | properties.keyEvents | `POST /v1beta/properties/{propertiesId}/keyEvents` | operate | Marketer (property) | analytics.edit |
| `analyticsadmin.properties.keyEvents.patch` | properties.keyEvents | `PATCH /v1beta/properties/{propertiesId}/keyEvents/{keyEventsId}` | operate | Marketer (property) | analytics.edit |
| `analyticsadmin.properties.keyEvents.delete` | properties.keyEvents | `DELETE /v1beta/properties/{propertiesId}/keyEvents/{keyEventsId}` | admin | Marketer (property) | analytics.edit |
| `analyticsadmin.properties.reportingDataAnnotations.list` | properties.reportingDataAnnotations | `GET /v1alpha/properties/{propertiesId}/reportingDataAnnotations` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.reportingDataAnnotations.get` | properties.reportingDataAnnotations | `GET /v1alpha/properties/{propertiesId}/reportingDataAnnotations/{reportingDataAnnotationsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.reportingDataAnnotations.create` | properties.reportingDataAnnotations | `POST /v1alpha/properties/{propertiesId}/reportingDataAnnotations` | operate | Analyst (property) | analytics.edit |
| `analyticsadmin.properties.reportingDataAnnotations.patch` | properties.reportingDataAnnotations | `PATCH /v1alpha/properties/{propertiesId}/reportingDataAnnotations/{reportingDataAnnotationsId}` | operate | Analyst (property) | analytics.edit |
| `analyticsadmin.properties.reportingDataAnnotations.delete` | properties.reportingDataAnnotations | `DELETE /v1alpha/properties/{propertiesId}/reportingDataAnnotations/{reportingDataAnnotationsId}` | admin | Analyst (property) | analytics.edit |
| `analyticsadmin.properties.rollupPropertySourceLinks.list` | properties.rollupPropertySourceLinks | `GET /v1alpha/properties/{propertiesId}/rollupPropertySourceLinks` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.rollupPropertySourceLinks.get` | properties.rollupPropertySourceLinks | `GET /v1alpha/properties/{propertiesId}/rollupPropertySourceLinks/{rollupPropertySourceLinksId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.rollupPropertySourceLinks.create` | properties.rollupPropertySourceLinks | `POST /v1alpha/properties/{propertiesId}/rollupPropertySourceLinks` | admin | Editor (property) on the roll-up and the source property | analytics.edit |
| `analyticsadmin.properties.rollupPropertySourceLinks.delete` | properties.rollupPropertySourceLinks | `DELETE /v1alpha/properties/{propertiesId}/rollupPropertySourceLinks/{rollupPropertySourceLinksId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.searchAds360Links.list` | properties.searchAds360Links | `GET /v1alpha/properties/{propertiesId}/searchAds360Links` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.searchAds360Links.get` | properties.searchAds360Links | `GET /v1alpha/properties/{propertiesId}/searchAds360Links/{searchAds360LinksId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.searchAds360Links.create` | properties.searchAds360Links | `POST /v1alpha/properties/{propertiesId}/searchAds360Links` | admin | Editor (property) plus access to the Search Ads 360 advertiser | analytics.edit |
| `analyticsadmin.properties.searchAds360Links.patch` | properties.searchAds360Links | `PATCH /v1alpha/properties/{propertiesId}/searchAds360Links/{searchAds360LinksId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.searchAds360Links.delete` | properties.searchAds360Links | `DELETE /v1alpha/properties/{propertiesId}/searchAds360Links/{searchAds360LinksId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.subpropertyEventFilters.list` | properties.subpropertyEventFilters | `GET /v1alpha/properties/{propertiesId}/subpropertyEventFilters` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.subpropertyEventFilters.get` | properties.subpropertyEventFilters | `GET /v1alpha/properties/{propertiesId}/subpropertyEventFilters/{subpropertyEventFiltersId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.subpropertyEventFilters.create` | properties.subpropertyEventFilters | `POST /v1alpha/properties/{propertiesId}/subpropertyEventFilters` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.subpropertyEventFilters.patch` | properties.subpropertyEventFilters | `PATCH /v1alpha/properties/{propertiesId}/subpropertyEventFilters/{subpropertyEventFiltersId}` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.subpropertyEventFilters.delete` | properties.subpropertyEventFilters | `DELETE /v1alpha/properties/{propertiesId}/subpropertyEventFilters/{subpropertyEventFiltersId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.subpropertySyncConfigs.list` | properties.subpropertySyncConfigs | `GET /v1alpha/properties/{propertiesId}/subpropertySyncConfigs` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.subpropertySyncConfigs.get` | properties.subpropertySyncConfigs | `GET /v1alpha/properties/{propertiesId}/subpropertySyncConfigs/{subpropertySyncConfigsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.subpropertySyncConfigs.patch` | properties.subpropertySyncConfigs | `PATCH /v1alpha/properties/{propertiesId}/subpropertySyncConfigs/{subpropertySyncConfigsId}` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.eventCreateRules.list` | properties.dataStreams.eventCreateRules | `GET /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/eventCreateRules` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.dataStreams.eventCreateRules.get` | properties.dataStreams.eventCreateRules | `GET /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/eventCreateRules/{eventCreateRulesId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.dataStreams.eventCreateRules.create` | properties.dataStreams.eventCreateRules | `POST /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/eventCreateRules` | operate | Marketer (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.eventCreateRules.patch` | properties.dataStreams.eventCreateRules | `PATCH /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/eventCreateRules/{eventCreateRulesId}` | operate | Marketer (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.eventCreateRules.delete` | properties.dataStreams.eventCreateRules | `DELETE /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/eventCreateRules/{eventCreateRulesId}` | admin | Marketer (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.eventEditRules.list` | properties.dataStreams.eventEditRules | `GET /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/eventEditRules` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.dataStreams.eventEditRules.get` | properties.dataStreams.eventEditRules | `GET /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/eventEditRules/{eventEditRulesId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.dataStreams.eventEditRules.create` | properties.dataStreams.eventEditRules | `POST /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/eventEditRules` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.eventEditRules.patch` | properties.dataStreams.eventEditRules | `PATCH /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/eventEditRules/{eventEditRulesId}` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.eventEditRules.reorder` | properties.dataStreams.eventEditRules | `POST /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/eventEditRules:reorder` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.eventEditRules.delete` | properties.dataStreams.eventEditRules | `DELETE /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/eventEditRules/{eventEditRulesId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.measurementProtocolSecrets.list` | properties.dataStreams.measurementProtocolSecrets | `GET /v1beta/properties/{propertiesId}/dataStreams/{dataStreamsId}/measurementProtocolSecrets` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.dataStreams.measurementProtocolSecrets.get` | properties.dataStreams.measurementProtocolSecrets | `GET /v1beta/properties/{propertiesId}/dataStreams/{dataStreamsId}/measurementProtocolSecrets/{measurementProtocolSecretsId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.dataStreams.measurementProtocolSecrets.create` | properties.dataStreams.measurementProtocolSecrets | `POST /v1beta/properties/{propertiesId}/dataStreams/{dataStreamsId}/measurementProtocolSecrets` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.measurementProtocolSecrets.patch` | properties.dataStreams.measurementProtocolSecrets | `PATCH /v1beta/properties/{propertiesId}/dataStreams/{dataStreamsId}/measurementProtocolSecrets/{measurementProtocolSecretsId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.measurementProtocolSecrets.delete` | properties.dataStreams.measurementProtocolSecrets | `DELETE /v1beta/properties/{propertiesId}/dataStreams/{dataStreamsId}/measurementProtocolSecrets/{measurementProtocolSecretsId}` | admin | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.sKAdNetworkConversionValueSchema.list` | properties.dataStreams.sKAdNetworkConversionValueSchema | `GET /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/sKAdNetworkConversionValueSchema` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.dataStreams.sKAdNetworkConversionValueSchema.get` | properties.dataStreams.sKAdNetworkConversionValueSchema | `GET /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/sKAdNetworkConversionValueSchema/{sKAdNetworkConversionValueSchemaId}` | read | Viewer (property) | analytics.readonly |
| `analyticsadmin.properties.dataStreams.sKAdNetworkConversionValueSchema.create` | properties.dataStreams.sKAdNetworkConversionValueSchema | `POST /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/sKAdNetworkConversionValueSchema` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.sKAdNetworkConversionValueSchema.patch` | properties.dataStreams.sKAdNetworkConversionValueSchema | `PATCH /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/sKAdNetworkConversionValueSchema/{sKAdNetworkConversionValueSchemaId}` | operate | Editor (property) | analytics.edit |
| `analyticsadmin.properties.dataStreams.sKAdNetworkConversionValueSchema.delete` | properties.dataStreams.sKAdNetworkConversionValueSchema | `DELETE /v1alpha/properties/{propertiesId}/dataStreams/{dataStreamsId}/sKAdNetworkConversionValueSchema/{sKAdNetworkConversionValueSchemaId}` | admin | Editor (property) | analytics.edit |
| `mp.collect` | mp | `POST /mp/collect` | admin | none at call time; secret holder | a stream secret |
| `mp.debug_collect` | mp | `POST /debug/mp/collect` | read | none | a stream secret |


**Notes on the classing:**
- `recurringAudienceLists.create` only reads data, but no method stops or deletes the standing job it makes, so it is `admin`.
- No method publishes, and none spends money through Google Analytics itself. A BigQuery link's exports bill the linked Google Cloud project, with no amount a spending cap could hold, so turning one on, or taking an event off its exclusions, is `admin` (billing setup, asked first at a fresh sign-in); turning one off, or excluding another event, is a brake made at once (`operate`).
- Methods behind an Analytics 360 property or a limited rollout are offered; Google answers 400 or 403 where the property lacks them.

## What the atlas shows

Each property the company narrows agents to (`integrations.google_analytics.properties`) is an account with the `analytics` capability, keyed by its ID; a company that names none shows Google Analytics as one account ([the atlas](../concepts/atlas.md)).

## Google Analytics' limits worth knowing

- The Data API spends tokens per property: 200,000 a day and 40,000 an hour on a standard property. A report's answer carries what is left, and a refusal says when quotas free up (hourly ones within the hour, daily ones at midnight Pacific).
- Reports hide small numbers under Google's privacy thresholds and fold rows into `(other)`; the answer says so.
