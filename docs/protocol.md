# Observed Maker protocol

The adapter implements the October 9, 2026 web flow. This is a captured internal web protocol, not a documented public Maker API. Credentials and signed URLs belong outside source, fixtures, and diagnostics.

## Read routes

- `GET /{organization}/products` supplies the organization catalog.
- `GET /{organization}/products/{product}/versions` supplies product, variant and version data.
- `GET /api/upload-aux/manifest/{folder}` and `/tags/{folder}` describe an uploaded archive.

Responses can contain Next.js React Server Component records embedded in HTML. The parser resolves references and length-prefixed text records, recognizes the observed variant UUID field `streamdeckid`, and rejects malformed version/variant records. Products prefer `type` and fall back to the observed `extension_name` string.

## Authentication

The capture used Cookie authentication rather than an Authorization bearer header. The importer permits `__Host-kc_at`, `kc_user`, and `__Secure-kc_rt`. Their inclusion does not establish the minimum necessary set. Requests and cookies are pinned to the exact Maker HTTPS origin. Redirects are rejected; signed storage uploads use a separate cookie-free client.

The native macOS store uses Security.framework directly. Explicit private files support other builds. Server-issued authentication-cookie updates are retained and persisted for later commands. A newly scoped server cookie replaces an imported cookie of the same authentication name.

## Upload sequence

1. Discover the current named `uploadAux`, `getIsSdVersionValid`, and `createVersion` references in linked Next.js bundles. Action IDs are not fixed constants.
2. `GET /api/presigned-url?file={organization}-{timestamp}.streamDeckPlugin` returns `presignedUrl`.
3. PUT the exact archive bytes with Content-Length to the approved storage host/path. The captured PUT had no Content-Type or Maker Cookie header. The observed signed URL expired after 60 seconds.
4. POST the versions route using `uploadAux` and multipart fields `_1_s3_link_id` and `0=[null,"$K1"]`.
5. Require an application-level success and folder ID; read the manifest and tags. Compare the server manifest with the reviewed local manifest.

Staging uploads are not durable review drafts. A failure may leave unused staged storage, but the upload command cannot call `createVersion`.

## Explicit submission

`getIsSdVersionValid` receives the minimum Stream Deck version in its JSON argument. Only after its validation succeeds does `createVersion` receive the captured multipart structure:

```text
_1_version
_1_release_notes
_1_release_notesPlainTextLength
_1_is_ready_for_publish
_1_variant_id
_1_id
_1_org_id
_1_file_s3_id
_1_folder_id
_1_min_app_version
_1_streamdeck_id
_1_os_versions[i][name]
_1_os_versions[i][number]
0=[{},"$K1"]
```

HTTP 200 alone does not establish success. The adapter validates the action response, saves a returned version ID, and reads versions again to confirm the result. Before dispatching `createVersion`, errors carry a typed not-submitted marker. After dispatch, failures are ambiguous and cannot trigger automatic submission retries.

The release manager stores uncertainty before calling the adapter, surviving interruption. Only a known not-dispatched result restores the upload checkpoint. This distinction is covered by synthetic transport and preflight failure tests.

## Evidence boundary

The captured product/version response was parsed offline against the observed product IDs, variant UUID and version records. The full CLI request sequence is exercised against localhost fixtures. The current Go client has not yet completed a fresh authenticated catalog read or a live upload/submission. Those checks belong to the next authorized release.

## Draft product creation

The observed createProduct action is independent of createVersion. The creation route is `/{organization}/create/plugins`. Named action references rotate with builds. The plugin extension ID is resolved from structured page data using exact `id` and `slug: plugins`; unrecognized page shape fails closed unless an explicit verified extension ID was supplied. Conflicting data always fails. This generic discovery shape is covered by synthetic Flight frames and references but awaits a fresh live schema check.

The create form uses `_1_name`, `_1_description`, `_1_descriptionPlainTextLength`, `_1_organization_id`, `_1_extension`, `_1_extension_slug`, `_1_streamDeckId`, and `0=[null,"$K1"]`. Success requires productId and variantId. This request does not need an archive or a staged upload ID. The initial wizard upload is a metadata convenience.

Draft plans checkpoint uncertainty before dispatch and returned IDs before verification. Product lookup requires UUID evidence, exact name, Draft status and one identified variant; it can resolve missing catalog details with product reads. No catalog match after dispatch remains unknown. A successful creation/adoption does not edit pricing, media or description.

## Session renewal

GET `/api/auth/session` was observed issuing 15-minute access cookies. Before a command, an absent or nearly expired access token plus usable refresh cookie triggers one renewal. Explicit auth refresh uses the same route. A temporary cookie jar validates server-issued future access cookies before committing any rotation. The CLI persists renewal before continuing, and ordinary response rotations on command completion. No action is automatically replayed after a request failure.

The maximum refresh-token/session lifetime is unknown. Server revocation and rotation can require fresh browser authentication. There is no background task to extend a session and no indefinite-session guarantee.


## Observed product deletion

Captured 2026-11-02T01:44:19-08:00 after explicit user confirmation at Maker's irreversible-deletion dialog. Example Product draft 00000000-0000-4000-8000-0000000000b1 was deleted once through the browser. No plugin was submitted or published; local package/source files remain intact.

- POST the product's General URL `/<organization>/products/<product>`.
- Dynamically discover named `deleteProductAction` from current linked bundles, including the catalog page or shared product-header chunk. Never hardcode the captured action ID.
- Multipart `_1_organization_id`, `_1_product_id`, and `0=[{"success":false},"$K1"]`.
- Observed response: HTTP303, content type text/x-component. CDP did not retain a readable body across navigation. Existing shared request helper rejects all redirects, so a future delete adapter must explicitly handle this action outcome without following arbitrary credential-bearing redirects or treating any redirect as success.
- UI navigated to the organization catalog and showed the complete remaining list without Example Product. Verify catalog absence after deletion; HTTP303 alone is not proof.
- Detailed sanitized evidence: delete-product-capture.json. No cookies or tokens are recorded. This capture is the contract for the 0.3.0 draft-only CLI deletion adapter. Development tests only use synthetic targets; the adapter is not live-destructive-tested again.
- CLI design should default to draft-only, read exact ID/name/status before two confirmations or an explicit destructive flag, checkpoint uncertainty, and reconcile with read-only catalog requests before any retry. No automatic destructive retry.


## Analytics reads

Verified read-only via browser CDP: GET `/api/organizations/{organization}/analytics?start=YYYY-MM-DD&end=YYYY-MM-DD`. Date endpoints are inclusive; custom October1-7 returned HTTP200 with seven dates. Response keys are time_range, totals_in_range, totals_per_day, products. Organization and product totals contain revenue/adoptions; product days and organization daily rows contain date/revenue/adoptions. Products contain product_id,name,extension,app_icon,thumbnail,publish_date,is_free,totals_in_range,days.

Overview and Products share the full response. Free/Paid/All uses product.is_free locally and does not issue another analytics request. Product/type filtering and per-day grouping are derivable from the same rows. Comparison requests the immediately preceding inclusive period separately. Revenue is integer USD cents, not a proven payout/profit figure; adoptions is product units, not unique users.

UI daily-history cutoff: October1,2025. All time requested start2025-10-01/end2026-11-01; do not claim complete older history. The UI All time label showed September30 due to a boundary discrepancy; retain API date strings unchanged. Current range/all/pricing settings were restored after inspection. See analytics-protocol.json for credential-free schema evidence. Real data export is private outside Git under the owner's Downloads folder. Analytics commands are implemented in 0.3.0: maker analytics defaults to overview/all available daily dates/all pricing; overview|products|daily subcommands support --start/--end, --type all|free|paid, --product, --extension, --compare-previous, --json or --format csv. A live no-query probe returned HTTP400 with an empty body, so the CLI supplies the observed history start and yesterday by default.


## Version 0.3.0 safeguards

Deletion discovers deleteProductAction, posts once with the captured envelope, does not follow redirects, accepts only explicit200 application success or303 acknowledgement, and then requires absence from a strict catalog schema. The checkpoint manager rechecks identity/status, serializes local operations and preserves unknown outcomes. CLI confirmations are two default-No prompts or --yes plus exact --confirm-name. All current delete dispatch tests are synthetic, not production.

Analytics validates response schema, integer cents and inclusive dates. Unfiltered server totals/daily figures are preserved; filtering aggregates selected product rows. Auth imports remain private and cookies are never printed.
