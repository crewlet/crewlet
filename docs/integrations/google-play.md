# Google Play

Google Play lets agents **read and manage your Google Play apps through engine tools**, as their own Google identity: reviews and replies, store listings and images, releases and staged rollouts, testers, in-app products, subscriptions, prices and offers, orders and refunds, vitals, the monthly reports, and the developer account's users. Every method of the Google Play Android Developer API, the Play Developer Reporting API and the monthly reports bucket is reachable; see [Every method, and its class](#every-method-and-its-class).

- **No token reaches an agent's box.** Each agent signs in as its own Google growth identity, a service account the engine makes in your identity project, and its tokens are minted inside the engine for an hour ([ADR-0172](../../adr/0172-a-google-growth-product-signs-in-as-each-seats-own-growth-identity-never-its-cloud-identity.md)).
- **Two ceilings, both kept.** The Play Console permissions you give the agent's identity on each app are Google's ceiling; the agent's level, its apps and each method's class are Crewlet's. A call needs both. Play has a single scope for everything, so the permissions you grant are the vendor's only bound.
- **What customers or testers see is asked first:** a review reply, a listing, an image, a release reaching users, a price or an offer, one ask each (up to ten replies in one), of an administrator at a fresh sign-in, never a delegate. So are refunds, and user, access, key, tax and irreversible changes ([ADR-0171](../../adr/0171-spend-public-and-admin-acts-are-held-for-an-administrator-never-a-delegate.md)).

Nothing here receives events from Google Play.

## What agents get

An agent is on at one level across every app it is granted.

| An agent at | Can | Needs, on the app |
|---|---|---|
| `read` (the default) | Reviews, listings, tracks and releases, products, subscriptions and offers, vitals, error reports and anomalies, and the monthly reports of its apps | View app information. Orders and purchases need View financial data; the users list needs Admin |
| `operate` | Everything `read` can, plus changes made at once and recorded: draft releases, halting a rollout, tester groups, new closed tracks, draft products, base plans and offers, mapping files, an app recovery draft. It also asks an administrator to approve each reply, listing or image change, release, price, offer state, Data safety declaration and refund | The permission of each act: Reply to reviews, Manage store presence, Release to testing tracks, Release to production, Manage testing tracks, Manage policy declarations, Manage orders and subscriptions |
| `administer` | Everything `operate` can, plus asking an administrator to approve users and permissions, app signing keys, tax settings, deletions, purchase acknowledgement and consumption, subscription cancels, deferrals and plan changes, and external transaction records | Admin for users and permissions; the permission of each act otherwise |

Raising an agent to `operate` or `administer` asks for a fresh sign-in (step-up).

The permissions Play Console gives, each on chosen apps or on all of them: **Admin** (every permission, plus users), **View app information** (added to every other permission; all-apps also downloads bulk reports), **View app quality information**, **View financial data**, **Manage orders and subscriptions**, **Manage store presence**, **Release to production**, **Release to testing tracks**, **Manage testing tracks and tester lists**, **Reply to reviews**, **Manage policy declarations**, **Edit and delete draft apps**, and on the whole account **Create and publish private apps**. The account owner is no permission and no agent needs it; a third-party app store enrolled in Google's program calls its review and catalog methods as its own service account. Games, deep links and connected apps permissions reach no method of these APIs.

## Connecting Crewlet

1. Connect [Google Cloud](google-cloud.md) first; no project needs preparing, and the form says whether it is connected. Turn Google Play on: Crewlet turns on the Google Play Android Developer and Developer Reporting APIs in the identity project and makes the company reader itself, with no block to run ([Google's growth products sign in through this identity](google-cloud.md#googles-growth-products-sign-in-through-this-identity)).
2. Invite the company reader's email, shown on the form, in Play Console, Users and permissions, with **View app information** for all apps (and View financial data for the financial reports). Name each developer account's monthly reports bucket: Download reports, Copy Cloud Storage URI, without `gs://`. Crewlet reads the company's figures and the monthly reports as this identity.
3. Turn agents on from the Agents tab, each with its apps, and give each its identity ([below](#give-an-agent-its-own-identity)).

Building and uploading releases stays with [Expo](expo.md) or your pipeline: Crewlet promotes, rolls out and halts the version codes they upload. It can upload a store image, an APK, a bundle or a mapping file itself, from an artifact, as an approved act.

## Give an agent its own identity

An agent's identity at Google Play is its own Google growth identity, which Crewlet made; what is left is adding it in Play Console. Its row on the Agents tab reads **Needs you** until the identity reaches each of its apps, and **Set up identity** in its box shows the steps, with the email ready to copy:

1. In Play Console, open **Users and permissions** and press **Invite new users**.
2. Paste the agent's email.
3. Under **App permissions**, add the agent's apps and tick what its level needs: at `read`, **View app information (read-only)**; at `operate`, also **Reply to reviews**, **Manage store presence**, **Release to testing tracks**, **Manage testing tracks and edit tester lists** and **Release to production**; at `administer`, also **Manage orders and subscriptions**. Leave Account permissions empty and press **Invite user**.

**Add it for me.** With the company's own OAuth client (`oauth`, made as [Google Cloud's page describes](google-cloud.md#the-companys-own-oauth-client), which Google Analytics may share) and a Play Console admin's sign-in on the Settings tab, **Add it for me** takes those steps through Play's users and grants API, in the developer account `developer_accounts` names that holds each app, with exactly the level's permissions and keeping any the identity holds already. Turning the agent off takes the identity out of those accounts. The sign-in only adds, reads and removes agents' identities; no agent's call is made with it.

**The check, on every pass:** Crewlet signs in as the identity and reads one review of each app; an app Play refuses is not added yet. With the admin sign-in it also reads the identity's permissions on each app: fewer than the level needs is the wrong role, naming what is missing, and Admin below `administer` is a warning. Play can take up to two days to apply new permissions; while the admin sign-in shows the identity added and Play does not let it in yet, the box says so. The agent is ready once every app passes, and reads Needs you again if somebody removes it there; calls Google refuses meanwhile name what is missing.

## The agents' tools

| Tool | Class | What it does |
|---|---|---|
| `google_play_list_apps` | read | The apps the agent may reach, and the ones of its list its identity does not see yet |
| `google_play_list_reviews` | read | An app's reviews with text, newest first, translated where asked: Google answers only the last 7 days |
| `google_play_read_listing` | read | The store listing in every language, the contact details, and the images of a language and type |
| `google_play_read_tracks` | read | Every track and its releases: version codes, status, rollout fraction, notes |
| `google_play_query_vitals` | read | Android vitals by day or hour: crashes, ANRs, slow starts and rendering, wakeups, wakelocks, memory, error counts |
| `google_play_read_report` | read | The monthly reports in the bucket (installs, ratings, crashes, store performance, reviews; sales and earnings at `administer`), listed or read with rows decoded |
| `google_play_halt_release` | operate | Halts a staged rollout, every other release kept |
| `google_play_reply_to_reviews` | public | Replies to 1 to 10 reviews of one app, each at most 350 characters: asked first |
| `google_play_update_listing` | public | One language's title, short and full description or video, and the contact details, in one commit: asked first |
| `google_play_update_images` | public | Store images of one language and type, uploaded from an artifact or removed, in one commit: asked first |
| `google_play_release` | public | Rolls out version codes to a fraction of users or all, raises or completes a rollout, resumes a halted one, or changes a live release's notes or countries: asked first |
| `google_play_read` | read | Any method that only reads, by its ID; `androidpublisher.edits.validate` with `steps` checks changes in an edit the engine discards |
| `google_play_operate` | operate | Any reversible change, by its method's ID |
| `google_play_publish` | public | Any change customers or testers see, by its method's ID: asked first |
| `google_play_spend` | spend | A refund, a subscription revoked with a refund, or a refund request approved: asked first |
| `google_play_admin` | admin | Any user, access, key, tax or irreversible change, by its method's ID: asked first |

A write whose class depends on its target is classed per call from the target as read now: a release that only drafts or halts is `operate` and one that reaches users `public`; a change to a product, base plan or offer still in draft is `operate` and to a live one `public`; a tax setting is `admin`; approving a refund request is `spend` and declining it `admin`. A call on the wrong tool for what it changes is refused with the tool that may make it.

**Edits are the engine's.** Listing, image, release, tester and file changes go through a Play edit, which any other commit to the app invalidates. The engine opens the edit, stages the changes, validates them and commits, all inside one call and under the app's write lease, and discards the edit on any failure; an agent never names an edit. Several changes ride one commit as `steps` of `androidpublisher.edits.commit`. A commit never cancels changes a person sent for review unless the approved call says so (`changesInReviewBehavior`), and one with `changesNotSentForReview` is `operate`, since nothing reaches users until a person sends it from Play Console.

**What a call may reach:**
- An app outside the agent's list is refused before Google is asked.
- Users and permissions of a developer account need the account named in `developer_accounts` and an agent that reaches every app the company names. An account-wide report (sales, earnings, invoices, subscription statistics) covers every app of the account, listed by the company or not, so it also needs an agent at `administer`.
- The monthly reports are read as the company's reader, which needs access to all apps no agent should hold, and cut to the reports of the agent's apps.

**What an agent reads:** every value Google answers with is fenced as somebody else's words: reviews and their authors, listings anyone with Play Console access wrote, error reports, buyer details and the reports' rows. A downloaded APK is never handed to an agent. A file to upload is named as `artifact:<artifact id>/<path>`, and the ask shows its digest, which binds the approval: a file changed since is not sent.

## Configuration

```yaml
integrations:
  google_play:
    enabled: true
    apps: ["com.example.app"]                    # optional: the apps agents may be given; absent is every one an identity sees
    developer_accounts: ["1234567890123456789"]   # optional: accounts whose users and permissions may be asked about
    reports_buckets: ["pubsite_prod_rev_0123456789"]  # optional: each account's monthly reports bucket
    oauth:                                       # optional: the client a Play Console admin signs in as, for Add it for me
      client_id: "123456789012-abc.apps.googleusercontent.com"
      client_secret: ${GOOGLE_OAUTH_CLIENT_SECRET}
    approvers: [founder]                         # optional: who a held act is routed to first; only an administrator answers
roles:
  - handle: mobile-lead
    integrations:
      google_play: {enabled: true, level: read, apps: ["com.example.app"]}
```

The block needs `integrations.google_cloud` on with its provisioning block. Its one secret is the optional OAuth client's.

## Every method, and its class

Each row is one method the engine offers, with its class (and the class its target's state or a field gives it), the least Play Console permission it needs, and the scope minted for it. The table is held to the engine's catalog by a test, both ways. Google removed `purchases.subscriptions.get`, `.refund` and `.revoke` from its discovery document; `purchases.subscriptionsv2.get`, `orders.refund` and `purchases.subscriptionsv2.revoke` replace them here. Three rows are held stricter than the coverage matrix allows, since the engine does not read the state that would lower them: `purchases.subscriptionsv2.defer` with `validateOnly` is `admin`, `apprecovery.addTargeting` on a draft action is `public`, an offers `batchUpdate` creating a draft offer is `public`, and a `subscriptions.patch` creating a subscription (`allowMissing`) is `public`. A state rule lowers a call only on its target as read: a target that cannot be read keeps the stricter class.

| Method | Resource | Call | Class | Least permission | Scope |
|---|---|---|---|---|---|
| `androidpublisher.applications.dataSafety` | applications | `POST /androidpublisher/v3/applications/{packageName}/dataSafety` | public | Manage policy declarations | androidpublisher |
| `androidpublisher.applications.deviceTierConfigs.create` | applications.deviceTierConfigs | `POST /androidpublisher/v3/applications/{packageName}/deviceTierConfigs` | operate | Release to testing tracks | androidpublisher |
| `androidpublisher.applications.deviceTierConfigs.get` | applications.deviceTierConfigs | `GET /androidpublisher/v3/applications/{packageName}/deviceTierConfigs/{deviceTierConfigId}` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.applications.deviceTierConfigs.list` | applications.deviceTierConfigs | `GET /androidpublisher/v3/applications/{packageName}/deviceTierConfigs` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.applications.tracks.releases.list` | applications.tracks.releases | `GET /androidpublisher/v3/applications/{applicationsId}/tracks/{tracksId}/releases` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.apprecovery.addTargeting` | apprecovery | `POST /androidpublisher/v3/applications/{packageName}/appRecoveries/{appRecoveryId}:addTargeting` | public | Release to production | androidpublisher |
| `androidpublisher.apprecovery.cancel` | apprecovery | `POST /androidpublisher/v3/applications/{packageName}/appRecoveries/{appRecoveryId}:cancel` | public | Release to production | androidpublisher |
| `androidpublisher.apprecovery.create` | apprecovery | `POST /androidpublisher/v3/applications/{packageName}/appRecoveries` | operate | Release to production | androidpublisher |
| `androidpublisher.apprecovery.deploy` | apprecovery | `POST /androidpublisher/v3/applications/{packageName}/appRecoveries/{appRecoveryId}:deploy` | public | Release to production | androidpublisher |
| `androidpublisher.apprecovery.list` | apprecovery | `GET /androidpublisher/v3/applications/{packageName}/appRecoveries` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.appsigning.enrollApp` | appsigning | `POST /androidpublisher/v3/applications/{name}/appSigning:enrollApp` | admin | Release to production | androidpublisher |
| `androidpublisher.appsigning.rotateAppSigningKey` | appsigning | `POST /androidpublisher/v3/applications/{name}/appSigning:rotateAppSigningKey` | admin | Release to production | androidpublisher |
| `androidpublisher.appstoreappsreview.createappstorehostedapp` | appstoreappsreview | `POST /androidpublisher/v3/appstore/{appStorePackageName}/apps:create` | operate | Third-party app store (program) | androidpublisher |
| `androidpublisher.appstoreappsreview.updateappstorehostedapp` | appstoreappsreview | `POST /androidpublisher/v3/appstore/{appStorePackageName}/apps:update` | public | Third-party app store (program) | androidpublisher |
| `androidpublisher.appstoreappsreview.updateappstorehostedapppublishstatus` | appstoreappsreview | `POST /androidpublisher/v3/appstore/{appStorePackageName}/apps/{packageName}:updateAppStoreHostedAppPublishStatus` | public | Third-party app store (program) | androidpublisher |
| `androidpublisher.appstoreappsreview.uploadapk` | appstoreappsreview | `POST /upload/androidpublisher/v3/appstore/{appStorePackageName}/apps/{packageName}/apks:upload` | public | Third-party app store (program) | androidpublisher |
| `androidpublisher.appstoreappsreview.uploadappstoreapppolicydeclarationfile` | appstoreappsreview | `POST /upload/androidpublisher/v3/appstore/{appStorePackageName}/apps/{packageName}/policyDeclarationFiles:upload` | operate | Third-party app store (program) | androidpublisher |
| `androidpublisher.appstoreappsreview.uploadimage` | appstoreappsreview | `POST /upload/androidpublisher/v3/appstore/{appStorePackageName}/apps/{packageName}/images:upload` | public | Third-party app store (program) | androidpublisher |
| `androidpublisher.appstorecatalog.recentappviews.get` | appstorecatalog.recentappviews | `GET /androidpublisher/v3/appstorecatalog/{appStorePackageName}/recentAppViews/{playAppPackageName}` | read | Third-party app store (program) | androidpublisher |
| `androidpublisher.appstorecatalog.recentupdateevents.list` | appstorecatalog.recentupdateevents | `GET /androidpublisher/v3/appstorecatalog/{appStorePackageName}/recentUpdateEvents` | read | Third-party app store (program) | androidpublisher |
| `androidpublisher.edits.commit` | edits | `POST /androidpublisher/v3/applications/{packageName}/edits/{editId}:commit` | public, operate while stages operate changes only | Manage store presence or Release to testing tracks or Release to production or Manage testing tracks | androidpublisher |
| `androidpublisher.edits.delete` | edits | `DELETE /androidpublisher/v3/applications/{packageName}/edits/{editId}` | operate | View app information (any permission); made by the engine around each edit | androidpublisher |
| `androidpublisher.edits.get` | edits | `GET /androidpublisher/v3/applications/{packageName}/edits/{editId}` | read | View app information (any permission); made by the engine around each edit | androidpublisher |
| `androidpublisher.edits.insert` | edits | `POST /androidpublisher/v3/applications/{packageName}/edits` | operate | View app information (any permission); made by the engine around each edit | androidpublisher |
| `androidpublisher.edits.validate` | edits | `POST /androidpublisher/v3/applications/{packageName}/edits/{editId}:validate` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.edits.apks.addexternallyhosted` | edits.apks | `POST /androidpublisher/v3/applications/{packageName}/edits/{editId}/apks/externallyHosted` | public | Create private apps | androidpublisher |
| `androidpublisher.edits.apks.list` | edits.apks | `GET /androidpublisher/v3/applications/{packageName}/edits/{editId}/apks` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.edits.apks.upload` | edits.apks | `POST /upload/androidpublisher/v3/applications/{packageName}/edits/{editId}/apks` | public | Release to testing tracks | androidpublisher |
| `androidpublisher.edits.bundles.list` | edits.bundles | `GET /androidpublisher/v3/applications/{packageName}/edits/{editId}/bundles` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.edits.bundles.upload` | edits.bundles | `POST /upload/androidpublisher/v3/applications/{packageName}/edits/{editId}/bundles` | public | Release to testing tracks | androidpublisher |
| `androidpublisher.edits.countryavailability.get` | edits.countryavailability | `GET /androidpublisher/v3/applications/{packageName}/edits/{editId}/countryAvailability/{track}` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.edits.deobfuscationfiles.upload` | edits.deobfuscationfiles | `POST /upload/androidpublisher/v3/applications/{packageName}/edits/{editId}/apks/{apkVersionCode}/deobfuscationFiles/{deobfuscationFileType}` | operate | Release to testing tracks | androidpublisher |
| `androidpublisher.edits.details.get` | edits.details | `GET /androidpublisher/v3/applications/{packageName}/edits/{editId}/details` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.edits.details.patch` | edits.details | `PATCH /androidpublisher/v3/applications/{packageName}/edits/{editId}/details` | public | Manage store presence | androidpublisher |
| `androidpublisher.edits.details.update` | edits.details | `PUT /androidpublisher/v3/applications/{packageName}/edits/{editId}/details` | public | Manage store presence | androidpublisher |
| `androidpublisher.edits.expansionfiles.get` | edits.expansionfiles | `GET /androidpublisher/v3/applications/{packageName}/edits/{editId}/apks/{apkVersionCode}/expansionFiles/{expansionFileType}` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.edits.expansionfiles.patch` | edits.expansionfiles | `PATCH /androidpublisher/v3/applications/{packageName}/edits/{editId}/apks/{apkVersionCode}/expansionFiles/{expansionFileType}` | public | Release to testing tracks | androidpublisher |
| `androidpublisher.edits.expansionfiles.update` | edits.expansionfiles | `PUT /androidpublisher/v3/applications/{packageName}/edits/{editId}/apks/{apkVersionCode}/expansionFiles/{expansionFileType}` | public | Release to testing tracks | androidpublisher |
| `androidpublisher.edits.expansionfiles.upload` | edits.expansionfiles | `POST /upload/androidpublisher/v3/applications/{packageName}/edits/{editId}/apks/{apkVersionCode}/expansionFiles/{expansionFileType}` | public | Release to testing tracks | androidpublisher |
| `androidpublisher.edits.images.delete` | edits.images | `DELETE /androidpublisher/v3/applications/{packageName}/edits/{editId}/listings/{language}/{imageType}/{imageId}` | public | Manage store presence | androidpublisher |
| `androidpublisher.edits.images.deleteall` | edits.images | `DELETE /androidpublisher/v3/applications/{packageName}/edits/{editId}/listings/{language}/{imageType}` | public | Manage store presence | androidpublisher |
| `androidpublisher.edits.images.list` | edits.images | `GET /androidpublisher/v3/applications/{packageName}/edits/{editId}/listings/{language}/{imageType}` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.edits.images.upload` | edits.images | `POST /upload/androidpublisher/v3/applications/{packageName}/edits/{editId}/listings/{language}/{imageType}` | public | Manage store presence | androidpublisher |
| `androidpublisher.edits.listings.delete` | edits.listings | `DELETE /androidpublisher/v3/applications/{packageName}/edits/{editId}/listings/{language}` | public | Manage store presence | androidpublisher |
| `androidpublisher.edits.listings.deleteall` | edits.listings | `DELETE /androidpublisher/v3/applications/{packageName}/edits/{editId}/listings` | public | Manage store presence | androidpublisher |
| `androidpublisher.edits.listings.get` | edits.listings | `GET /androidpublisher/v3/applications/{packageName}/edits/{editId}/listings/{language}` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.edits.listings.list` | edits.listings | `GET /androidpublisher/v3/applications/{packageName}/edits/{editId}/listings` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.edits.listings.patch` | edits.listings | `PATCH /androidpublisher/v3/applications/{packageName}/edits/{editId}/listings/{language}` | public | Manage store presence | androidpublisher |
| `androidpublisher.edits.listings.update` | edits.listings | `PUT /androidpublisher/v3/applications/{packageName}/edits/{editId}/listings/{language}` | public | Manage store presence | androidpublisher |
| `androidpublisher.edits.testers.get` | edits.testers | `GET /androidpublisher/v3/applications/{packageName}/edits/{editId}/testers/{track}` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.edits.testers.patch` | edits.testers | `PATCH /androidpublisher/v3/applications/{packageName}/edits/{editId}/testers/{track}` | operate | Manage testing tracks | androidpublisher |
| `androidpublisher.edits.testers.update` | edits.testers | `PUT /androidpublisher/v3/applications/{packageName}/edits/{editId}/testers/{track}` | operate | Manage testing tracks | androidpublisher |
| `androidpublisher.edits.tracks.create` | edits.tracks | `POST /androidpublisher/v3/applications/{packageName}/edits/{editId}/tracks` | operate | Manage testing tracks | androidpublisher |
| `androidpublisher.edits.tracks.get` | edits.tracks | `GET /androidpublisher/v3/applications/{packageName}/edits/{editId}/tracks/{track}` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.edits.tracks.list` | edits.tracks | `GET /androidpublisher/v3/applications/{packageName}/edits/{editId}/tracks` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.edits.tracks.patch` | edits.tracks | `PATCH /androidpublisher/v3/applications/{packageName}/edits/{editId}/tracks/{track}` | public, operate while drafts or halts only | Release to testing tracks or Release to production | androidpublisher |
| `androidpublisher.edits.tracks.update` | edits.tracks | `PUT /androidpublisher/v3/applications/{packageName}/edits/{editId}/tracks/{track}` | public, operate while drafts or halts only | Release to testing tracks or Release to production | androidpublisher |
| `androidpublisher.externaltransactions.createexternaltransaction` | externaltransactions | `POST /androidpublisher/v3/applications/{applicationsId}/externalTransactions` | admin | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.externaltransactions.getexternaltransaction` | externaltransactions | `GET /androidpublisher/v3/applications/{applicationsId}/externalTransactions/{externalTransactionsId}` | read | View financial data | androidpublisher |
| `androidpublisher.externaltransactions.refundexternaltransaction` | externaltransactions | `POST /androidpublisher/v3/applications/{applicationsId}/externalTransactions/{externalTransactionsId}:refund` | admin | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.generatedapks.download` | generatedapks | `GET /androidpublisher/v3/applications/{packageName}/generatedApks/{versionCode}/downloads/{downloadId}:download` | read | Release to production | androidpublisher |
| `androidpublisher.generatedapks.list` | generatedapks | `GET /androidpublisher/v3/applications/{packageName}/generatedApks/{versionCode}` | read | Release to production | androidpublisher |
| `androidpublisher.grants.create` | grants | `POST /androidpublisher/v3/developers/{developersId}/users/{usersId}/grants` | admin | Admin | androidpublisher |
| `androidpublisher.grants.delete` | grants | `DELETE /androidpublisher/v3/developers/{developersId}/users/{usersId}/grants/{grantsId}` | admin | Admin | androidpublisher |
| `androidpublisher.grants.patch` | grants | `PATCH /androidpublisher/v3/developers/{developersId}/users/{usersId}/grants/{grantsId}` | admin | Admin | androidpublisher |
| `androidpublisher.inappproducts.batchDelete` | inappproducts | `POST /androidpublisher/v3/applications/{packageName}/inappproducts:batchDelete` | admin | Manage store presence | androidpublisher |
| `androidpublisher.inappproducts.batchGet` | inappproducts | `GET /androidpublisher/v3/applications/{packageName}/inappproducts:batchGet` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.inappproducts.batchUpdate` | inappproducts | `POST /androidpublisher/v3/applications/{packageName}/inappproducts:batchUpdate` | public, admin for managedProductTaxesAndComplianceSettings | Manage store presence | androidpublisher |
| `androidpublisher.inappproducts.delete` | inappproducts | `DELETE /androidpublisher/v3/applications/{packageName}/inappproducts/{sku}` | admin | Manage store presence | androidpublisher |
| `androidpublisher.inappproducts.get` | inappproducts | `GET /androidpublisher/v3/applications/{packageName}/inappproducts/{sku}` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.inappproducts.insert` | inappproducts | `POST /androidpublisher/v3/applications/{packageName}/inappproducts` | public, operate while product inactive, admin for managedProductTaxesAndComplianceSettings | Manage store presence | androidpublisher |
| `androidpublisher.inappproducts.list` | inappproducts | `GET /androidpublisher/v3/applications/{packageName}/inappproducts` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.inappproducts.patch` | inappproducts | `PATCH /androidpublisher/v3/applications/{packageName}/inappproducts/{sku}` | public, operate while product inactive, admin for managedProductTaxesAndComplianceSettings | Manage store presence | androidpublisher |
| `androidpublisher.inappproducts.update` | inappproducts | `PUT /androidpublisher/v3/applications/{packageName}/inappproducts/{sku}` | public, operate while product inactive, admin for managedProductTaxesAndComplianceSettings | Manage store presence | androidpublisher |
| `androidpublisher.internalappsharingartifacts.uploadapk` | internalappsharingartifacts | `POST /upload/androidpublisher/v3/applications/internalappsharing/{packageName}/artifacts/apk` | public | Release to testing tracks | androidpublisher |
| `androidpublisher.internalappsharingartifacts.uploadbundle` | internalappsharingartifacts | `POST /upload/androidpublisher/v3/applications/internalappsharing/{packageName}/artifacts/bundle` | public | Release to testing tracks | androidpublisher |
| `androidpublisher.monetization.convertRegionPrices` | monetization | `POST /androidpublisher/v3/applications/{packageName}/pricing:convertRegionPrices` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.batchDelete` | monetization.onetimeproducts | `POST /androidpublisher/v3/applications/{packageName}/oneTimeProducts:batchDelete` | admin | Manage store presence | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.batchGet` | monetization.onetimeproducts | `GET /androidpublisher/v3/applications/{packageName}/oneTimeProducts:batchGet` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.batchUpdate` | monetization.onetimeproducts | `POST /androidpublisher/v3/applications/{packageName}/oneTimeProducts:batchUpdate` | public, admin for taxAndComplianceSettings | Manage store presence | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.delete` | monetization.onetimeproducts | `DELETE /androidpublisher/v3/applications/{packageName}/oneTimeProducts/{productId}` | admin | Manage store presence | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.get` | monetization.onetimeproducts | `GET /androidpublisher/v3/applications/{packageName}/oneTimeProducts/{productId}` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.list` | monetization.onetimeproducts | `GET /androidpublisher/v3/applications/{packageName}/oneTimeProducts` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.patch` | monetization.onetimeproducts | `PATCH /androidpublisher/v3/applications/{packageName}/onetimeproducts/{productId}` | public, operate while no purchase option active, admin for taxAndComplianceSettings | Manage store presence | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.purchaseOptions.batchDelete` | monetization.onetimeproducts.purchaseOptions | `POST /androidpublisher/v3/applications/{packageName}/oneTimeProducts/{productId}/purchaseOptions:batchDelete` | admin | Manage store presence | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.purchaseOptions.batchUpdateStates` | monetization.onetimeproducts.purchaseOptions | `POST /androidpublisher/v3/applications/{packageName}/oneTimeProducts/{productId}/purchaseOptions:batchUpdateStates` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.purchaseOptions.offers.activate` | monetization.onetimeproducts.purchaseOptions.offers | `POST /androidpublisher/v3/applications/{packageName}/oneTimeProducts/{productId}/purchaseOptions/{purchaseOptionId}/offers/{offerId}:activate` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.purchaseOptions.offers.batchDelete` | monetization.onetimeproducts.purchaseOptions.offers | `POST /androidpublisher/v3/applications/{packageName}/oneTimeProducts/{productId}/purchaseOptions/{purchaseOptionId}/offers:batchDelete` | admin | Manage store presence | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.purchaseOptions.offers.batchGet` | monetization.onetimeproducts.purchaseOptions.offers | `POST /androidpublisher/v3/applications/{packageName}/oneTimeProducts/{productId}/purchaseOptions/{purchaseOptionId}/offers:batchGet` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.purchaseOptions.offers.batchUpdate` | monetization.onetimeproducts.purchaseOptions.offers | `POST /androidpublisher/v3/applications/{packageName}/oneTimeProducts/{productId}/purchaseOptions/{purchaseOptionId}/offers:batchUpdate` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.purchaseOptions.offers.batchUpdateStates` | monetization.onetimeproducts.purchaseOptions.offers | `POST /androidpublisher/v3/applications/{packageName}/oneTimeProducts/{productId}/purchaseOptions/{purchaseOptionId}/offers:batchUpdateStates` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.purchaseOptions.offers.cancel` | monetization.onetimeproducts.purchaseOptions.offers | `POST /androidpublisher/v3/applications/{packageName}/oneTimeProducts/{productId}/purchaseOptions/{purchaseOptionId}/offers/{offerId}:cancel` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.purchaseOptions.offers.deactivate` | monetization.onetimeproducts.purchaseOptions.offers | `POST /androidpublisher/v3/applications/{packageName}/oneTimeProducts/{productId}/purchaseOptions/{purchaseOptionId}/offers/{offerId}:deactivate` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.onetimeproducts.purchaseOptions.offers.list` | monetization.onetimeproducts.purchaseOptions.offers | `GET /androidpublisher/v3/applications/{packageName}/oneTimeProducts/{productId}/purchaseOptions/{purchaseOptionId}/offers` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.monetization.subscriptions.archive` | monetization.subscriptions | `POST /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}:archive` | never | Deprecated in discovery: 'subscription archiving is not supported.' Technically impossible; do not expose. | none |
| `androidpublisher.monetization.subscriptions.batchGet` | monetization.subscriptions | `GET /androidpublisher/v3/applications/{packageName}/subscriptions:batchGet` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.monetization.subscriptions.batchUpdate` | monetization.subscriptions | `POST /androidpublisher/v3/applications/{packageName}/subscriptions:batchUpdate` | public, admin for taxAndComplianceSettings | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.create` | monetization.subscriptions | `POST /androidpublisher/v3/applications/{packageName}/subscriptions` | operate | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.delete` | monetization.subscriptions | `DELETE /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}` | admin | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.get` | monetization.subscriptions | `GET /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.monetization.subscriptions.list` | monetization.subscriptions | `GET /androidpublisher/v3/applications/{packageName}/subscriptions` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.monetization.subscriptions.patch` | monetization.subscriptions | `PATCH /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}` | public, operate while no base plan active, admin for taxAndComplianceSettings | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.activate` | monetization.subscriptions.basePlans | `POST /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans/{basePlanId}:activate` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.batchMigratePrices` | monetization.subscriptions.basePlans | `POST /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans:batchMigratePrices` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.batchUpdateStates` | monetization.subscriptions.basePlans | `POST /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans:batchUpdateStates` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.deactivate` | monetization.subscriptions.basePlans | `POST /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans/{basePlanId}:deactivate` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.delete` | monetization.subscriptions.basePlans | `DELETE /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans/{basePlanId}` | admin | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.migratePrices` | monetization.subscriptions.basePlans | `POST /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans/{basePlanId}:migratePrices` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.offers.activate` | monetization.subscriptions.basePlans.offers | `POST /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans/{basePlanId}/offers/{offerId}:activate` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.offers.batchGet` | monetization.subscriptions.basePlans.offers | `POST /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans/{basePlanId}/offers:batchGet` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.offers.batchUpdate` | monetization.subscriptions.basePlans.offers | `POST /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans/{basePlanId}/offers:batchUpdate` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.offers.batchUpdateStates` | monetization.subscriptions.basePlans.offers | `POST /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans/{basePlanId}/offers:batchUpdateStates` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.offers.create` | monetization.subscriptions.basePlans.offers | `POST /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans/{basePlanId}/offers` | operate | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.offers.deactivate` | monetization.subscriptions.basePlans.offers | `POST /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans/{basePlanId}/offers/{offerId}:deactivate` | public | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.offers.delete` | monetization.subscriptions.basePlans.offers | `DELETE /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans/{basePlanId}/offers/{offerId}` | admin | Manage store presence | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.offers.get` | monetization.subscriptions.basePlans.offers | `GET /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans/{basePlanId}/offers/{offerId}` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.offers.list` | monetization.subscriptions.basePlans.offers | `GET /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans/{basePlanId}/offers` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.monetization.subscriptions.basePlans.offers.patch` | monetization.subscriptions.basePlans.offers | `PATCH /androidpublisher/v3/applications/{packageName}/subscriptions/{productId}/basePlans/{basePlanId}/offers/{offerId}` | public, operate while offer in draft | Manage store presence | androidpublisher |
| `androidpublisher.orders.batchget` | orders | `GET /androidpublisher/v3/applications/{packageName}/orders:batchGet` | read | View financial data | androidpublisher |
| `androidpublisher.orders.get` | orders | `GET /androidpublisher/v3/applications/{packageName}/orders/{orderId}` | read | View financial data | androidpublisher |
| `androidpublisher.orders.refund` | orders | `POST /androidpublisher/v3/applications/{packageName}/orders/{orderId}:refund` | spend | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.orders.reviewrefund` | orders | `POST /androidpublisher/v3/applications/{packageName}/orders/{orderId}:reviewrefund` | admin, spend while approving the refund | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.purchases.products.acknowledge` | purchases.products | `POST /androidpublisher/v3/applications/{packageName}/purchases/products/{productId}/tokens/{token}:acknowledge` | admin | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.purchases.products.consume` | purchases.products | `POST /androidpublisher/v3/applications/{packageName}/purchases/products/{productId}/tokens/{token}:consume` | admin | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.purchases.products.get` | purchases.products | `GET /androidpublisher/v3/applications/{packageName}/purchases/products/{productId}/tokens/{token}` | read | View financial data | androidpublisher |
| `androidpublisher.purchases.productsv2.getproductpurchasev2` | purchases.productsv2 | `GET /androidpublisher/v3/applications/{packageName}/purchases/productsv2/tokens/{token}` | read | View financial data | androidpublisher |
| `androidpublisher.purchases.subscriptions.acknowledge` | purchases.subscriptions | `POST /androidpublisher/v3/applications/{packageName}/purchases/subscriptions/{subscriptionId}/tokens/{token}:acknowledge` | admin | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.purchases.subscriptions.cancel` | purchases.subscriptions | `POST /androidpublisher/v3/applications/{packageName}/purchases/subscriptions/{subscriptionId}/tokens/{token}:cancel` | admin | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.purchases.subscriptions.defer` | purchases.subscriptions | `POST /androidpublisher/v3/applications/{packageName}/purchases/subscriptions/{subscriptionId}/tokens/{token}:defer` | admin | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.purchases.subscriptionsv2.activateoffertargeting` | purchases.subscriptionsv2 | `POST /androidpublisher/v3/applications/{packageName}/purchases/subscriptionsv2/tokens/{purchaseToken}:activateOfferTargeting` | public | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.purchases.subscriptionsv2.cancel` | purchases.subscriptionsv2 | `POST /androidpublisher/v3/applications/{packageName}/purchases/subscriptionsv2/tokens/{token}:cancel` | admin | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.purchases.subscriptionsv2.cancelplantransition` | purchases.subscriptionsv2 | `POST /androidpublisher/v3/applications/{packageName}/purchases/subscriptionsv2/tokens/{token}:cancelPlanTransition` | operate | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.purchases.subscriptionsv2.deactivateoffertargeting` | purchases.subscriptionsv2 | `POST /androidpublisher/v3/applications/{packageName}/purchases/subscriptionsv2/tokens/{purchaseToken}:deactivateOfferTargeting` | public | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.purchases.subscriptionsv2.defer` | purchases.subscriptionsv2 | `POST /androidpublisher/v3/applications/{packageName}/purchases/subscriptionsv2/tokens/{token}:defer` | admin | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.purchases.subscriptionsv2.get` | purchases.subscriptionsv2 | `GET /androidpublisher/v3/applications/{packageName}/purchases/subscriptionsv2/tokens/{token}` | read | View financial data | androidpublisher |
| `androidpublisher.purchases.subscriptionsv2.revoke` | purchases.subscriptionsv2 | `POST /androidpublisher/v3/applications/{packageName}/purchases/subscriptionsv2/tokens/{token}:revoke` | spend | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.purchases.subscriptionsv2.scheduleplantransition` | purchases.subscriptionsv2 | `POST /androidpublisher/v3/applications/{packageName}/purchases/subscriptionsv2/tokens/{token}:schedulePlanTransition` | admin | Manage orders and subscriptions | androidpublisher |
| `androidpublisher.purchases.voidedpurchases.list` | purchases.voidedpurchases | `GET /androidpublisher/v3/applications/{packageName}/purchases/voidedpurchases` | read | View financial data | androidpublisher |
| `androidpublisher.reviews.get` | reviews | `GET /androidpublisher/v3/applications/{packageName}/reviews/{reviewId}` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.reviews.list` | reviews | `GET /androidpublisher/v3/applications/{packageName}/reviews` | read | View app information (any permission) | androidpublisher |
| `androidpublisher.reviews.reply` | reviews | `POST /androidpublisher/v3/applications/{packageName}/reviews/{reviewId}:reply` | public | Reply to reviews | androidpublisher |
| `androidpublisher.systemapks.variants.create` | systemapks.variants | `POST /androidpublisher/v3/applications/{packageName}/systemApks/{versionCode}/variants` | operate | Release to production | androidpublisher |
| `androidpublisher.systemapks.variants.download` | systemapks.variants | `GET /androidpublisher/v3/applications/{packageName}/systemApks/{versionCode}/variants/{variantId}:download` | read | Release to production | androidpublisher |
| `androidpublisher.systemapks.variants.get` | systemapks.variants | `GET /androidpublisher/v3/applications/{packageName}/systemApks/{versionCode}/variants/{variantId}` | read | Release to production | androidpublisher |
| `androidpublisher.systemapks.variants.list` | systemapks.variants | `GET /androidpublisher/v3/applications/{packageName}/systemApks/{versionCode}/variants` | read | Release to production | androidpublisher |
| `androidpublisher.users.create` | users | `POST /androidpublisher/v3/developers/{developersId}/users` | admin | Admin | androidpublisher |
| `androidpublisher.users.delete` | users | `DELETE /androidpublisher/v3/developers/{developersId}/users/{usersId}` | admin | Admin | androidpublisher |
| `androidpublisher.users.list` | users | `GET /androidpublisher/v3/developers/{developersId}/users` | read | Admin | androidpublisher |
| `androidpublisher.users.patch` | users | `PATCH /androidpublisher/v3/developers/{developersId}/users/{usersId}` | admin | Admin | androidpublisher |
| `playdeveloperreporting.anomalies.list` | anomalies | `GET /v1beta1/apps/{appsId}/anomalies` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.apps.fetchReleaseFilterOptions` | apps | `GET /v1beta1/apps/{appsId}:fetchReleaseFilterOptions` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.apps.search` | apps | `GET /v1beta1/apps:search` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.anonrssandswapmemoryusage.get` | vitals.anonrssandswapmemoryusage | `GET /v1beta1/apps/{appsId}/anonRssAndSwapMemoryUsageMetricSet` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.anonrssandswapmemoryusage.query` | vitals.anonrssandswapmemoryusage | `POST /v1beta1/apps/{appsId}/anonRssAndSwapMemoryUsageMetricSet:query` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.anrrate.get` | vitals.anrrate | `GET /v1beta1/apps/{appsId}/anrRateMetricSet` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.anrrate.query` | vitals.anrrate | `POST /v1beta1/apps/{appsId}/anrRateMetricSet:query` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.bitmapmemoryusage.get` | vitals.bitmapmemoryusage | `GET /v1beta1/apps/{appsId}/bitmapMemoryUsageMetricSet` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.bitmapmemoryusage.query` | vitals.bitmapmemoryusage | `POST /v1beta1/apps/{appsId}/bitmapMemoryUsageMetricSet:query` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.crashrate.get` | vitals.crashrate | `GET /v1beta1/apps/{appsId}/crashRateMetricSet` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.crashrate.query` | vitals.crashrate | `POST /v1beta1/apps/{appsId}/crashRateMetricSet:query` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.errors.counts.get` | vitals.errors.counts | `GET /v1beta1/apps/{appsId}/errorCountMetricSet` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.errors.counts.query` | vitals.errors.counts | `POST /v1beta1/apps/{appsId}/errorCountMetricSet:query` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.errors.issues.search` | vitals.errors.issues | `GET /v1beta1/apps/{appsId}/errorIssues:search` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.errors.reports.search` | vitals.errors.reports | `GET /v1beta1/apps/{appsId}/errorReports:search` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.excessivewakeuprate.get` | vitals.excessivewakeuprate | `GET /v1beta1/apps/{appsId}/excessiveWakeupRateMetricSet` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.excessivewakeuprate.query` | vitals.excessivewakeuprate | `POST /v1beta1/apps/{appsId}/excessiveWakeupRateMetricSet:query` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.lmkrate.get` | vitals.lmkrate | `GET /v1beta1/apps/{appsId}/lmkRateMetricSet` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.lmkrate.query` | vitals.lmkrate | `POST /v1beta1/apps/{appsId}/lmkRateMetricSet:query` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.slowrenderingrate.get` | vitals.slowrenderingrate | `GET /v1beta1/apps/{appsId}/slowRenderingRateMetricSet` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.slowrenderingrate.query` | vitals.slowrenderingrate | `POST /v1beta1/apps/{appsId}/slowRenderingRateMetricSet:query` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.slowstartrate.get` | vitals.slowstartrate | `GET /v1beta1/apps/{appsId}/slowStartRateMetricSet` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.slowstartrate.query` | vitals.slowstartrate | `POST /v1beta1/apps/{appsId}/slowStartRateMetricSet:query` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.stuckbackgroundwakelockrate.get` | vitals.stuckbackgroundwakelockrate | `GET /v1beta1/apps/{appsId}/stuckBackgroundWakelockRateMetricSet` | read | View app information (any permission) | playdeveloperreporting |
| `playdeveloperreporting.vitals.stuckbackgroundwakelockrate.query` | vitals.stuckbackgroundwakelockrate | `POST /v1beta1/apps/{appsId}/stuckBackgroundWakelockRateMetricSet:query` | read | View app information (any permission) | playdeveloperreporting |
| `storage.objects.bulkRestore` | objects | `POST /storage/v1/b/{bucket}/o/bulkRestore` | never | The monthly reports bucket is Google's and Play Console grants only read access to it (support.google.com/googleplay/android-developer/answer/6135870): a write cannot be made. | none |
| `storage.objects.compose` | objects | `POST /storage/v1/b/{destinationBucket}/o/{destinationObject}/compose` | never | The monthly reports bucket is Google's and Play Console grants only read access to it (support.google.com/googleplay/android-developer/answer/6135870): a write cannot be made. | none |
| `storage.objects.copy` | objects | `POST /storage/v1/b/{sourceBucket}/o/{sourceObject}/copyTo/b/{destinationBucket}/o/{destinationObject}` | never | The monthly reports bucket is Google's and Play Console grants only read access to it (support.google.com/googleplay/android-developer/answer/6135870): a write cannot be made. | none |
| `storage.objects.delete` | objects | `DELETE /storage/v1/b/{bucket}/o/{object}` | never | The monthly reports bucket is Google's and Play Console grants only read access to it (support.google.com/googleplay/android-developer/answer/6135870): a write cannot be made. | none |
| `storage.objects.get` | objects | `GET /storage/v1/b/{bucket}/o/{object}` | read | View app information or View financial data, all apps (company reader) | devstorage.read_only (company reader) |
| `storage.objects.getIamPolicy` | objects | `GET /storage/v1/b/{bucket}/o/{object}/iam` | read | View app information or View financial data, all apps (company reader) | devstorage.read_only (company reader) |
| `storage.objects.insert` | objects | `POST /storage/v1/b/{bucket}/o` | never | The monthly reports bucket is Google's and Play Console grants only read access to it (support.google.com/googleplay/android-developer/answer/6135870): a write cannot be made. | none |
| `storage.objects.list` | objects | `GET /storage/v1/b/{bucket}/o` | read | View app information or View financial data, all apps (company reader) | devstorage.read_only (company reader) |
| `storage.objects.move` | objects | `POST /storage/v1/b/{bucket}/o/{sourceObject}/moveTo/o/{destinationObject}` | never | The monthly reports bucket is Google's and Play Console grants only read access to it (support.google.com/googleplay/android-developer/answer/6135870): a write cannot be made. | none |
| `storage.objects.patch` | objects | `PATCH /storage/v1/b/{bucket}/o/{object}` | never | The monthly reports bucket is Google's and Play Console grants only read access to it (support.google.com/googleplay/android-developer/answer/6135870): a write cannot be made. | none |
| `storage.objects.restore` | objects | `POST /storage/v1/b/{bucket}/o/{object}/restore` | never | The monthly reports bucket is Google's and Play Console grants only read access to it (support.google.com/googleplay/android-developer/answer/6135870): a write cannot be made. | none |
| `storage.objects.rewrite` | objects | `POST /storage/v1/b/{sourceBucket}/o/{sourceObject}/rewriteTo/b/{destinationBucket}/o/{destinationObject}` | never | The monthly reports bucket is Google's and Play Console grants only read access to it (support.google.com/googleplay/android-developer/answer/6135870): a write cannot be made. | none |
| `storage.objects.setIamPolicy` | objects | `PUT /storage/v1/b/{bucket}/o/{object}/iam` | never | The monthly reports bucket is Google's and Play Console grants only read access to it (support.google.com/googleplay/android-developer/answer/6135870): a write cannot be made. | none |
| `storage.objects.testIamPermissions` | objects | `GET /storage/v1/b/{bucket}/o/{object}/iam/testPermissions` | read | View app information or View financial data, all apps (company reader) | devstorage.read_only (company reader) |
| `storage.objects.update` | objects | `PUT /storage/v1/b/{bucket}/o/{object}` | never | The monthly reports bucket is Google's and Play Console grants only read access to it (support.google.com/googleplay/android-developer/answer/6135870): a write cannot be made. | none |
| `storage.objects.viewFullContext` | objects | `GET /storage/v1/b/{bucket}/o/{object}/viewFullContext` | read | View app information or View financial data, all apps (company reader) | devstorage.read_only (company reader) |

## What the atlas shows

Each app the company narrows agents to (`integrations.google_play.apps`) is an account with the `app_delivery` capability, keyed by its package name; a company that names none shows Google Play as one account ([the atlas](../concepts/atlas.md)).

## Google Play's limits worth knowing

- 3,000 requests a minute for each of Google's buckets (publishing, purchases, orders and others); per app, 200 review reads an hour and 2,000 replies a day. A refusal says so; a read waits out a short limit, a write is never sent twice.
- Reviews are answered for the last 7 days only. Older ones are in the monthly reviews report.
- The monthly reports are posted within 3 to 7 days of each day, in monthly files; the statistics files are UTF-16, which the engine decodes.
- An app with no upload yet is invisible to the API: submit a first release with Expo or Play Console.
- Methods of Google's programs (third-party app stores, external transactions, managed private apps, self-hosted app signing keys) answer a refusal until the account is enrolled; they stay offered.
