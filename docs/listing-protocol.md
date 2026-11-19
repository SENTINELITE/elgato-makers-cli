# Complete listing workflow

The `maker listing` commands combine product creation, package analysis, listing details, images and review submission. They use HTTP without browser automation. The bindings were derived from observed Maker Console requests and its public JavaScript; current action IDs and taxonomy IDs are discovered at runtime. This is an unofficial integration and may stop when Maker changes its protocol.

## Commands

Copy `examples/listing.example.json` to your release directory and replace the example paths, name, pricing, language/category choices and links. Paths are relative to the JSON file. Supply an already built, signed and packaged `.streamDeckPlugin`; this workflow does not build or notarize plugins.

```sh
# Offline: checks the JSON, package, text and images. No saved state or authentication.
maker listing validate --file ./listing.json

# Preferred direct path: creates/reuses the SKU, stages everything, submits for review.
maker listing submit --file ./listing.json

# Explicit unattended approval of the same production flow:
maker listing submit --file ./listing.json --yes
```

Interactive submission asks two questions, both defaulting to No, before any product/upload/detail/media mutation. It shows the reviewed identity, version, price, publication choice and digest. Blank input, No, EOF or interruption cancels. The CLI prints the local plan ID before connecting. Noninteractive submission requires `--yes` or `-y`; it is available only on submission, not as a global setting.

You do not need a separate draft command. The remote service still needs a product record before it can attach files and create a reviewed version. A failure after that point may leave an incomplete product in Maker Console; the saved plan retains its IDs for recovery.

To set the gallery order before preparing or uploading:

```sh
maker listing reorder --file ./listing.json --order 3,1,2 --output ./reordered.json
maker listing validate --file ./reordered.json
```

Indices refer to the current gallery array, start at 1, and must form a complete permutation with no duplicates or omissions. Without `--output`, reordered configuration JSON goes to stdout; with it, the CLI atomically creates a new private file and refuses to overwrite anything. Artifact paths are emitted as absolute paths so a different output directory does not break them. This command is offline and changes no remote assets or saved plans. If a pristine prepared plan already owns this plugin UUID, discard that plan before preparing the reordered file.

For a staged workflow:

```sh
maker listing prepare --file ./listing.json
maker listing show <listing-id>
maker listing upload <listing-id>
maker listing submit <listing-id>
```

`prepare` is offline and saves private local state. `upload` performs production staging writes but never requests review. Submission retains the explicit `auto_publish` choice: true requests publication after approval; false requires later manual publication.

## Configuration

`schema_version` must be 1. Organization comes from existing CLI configuration or explicit `org_id` in the JSON; conflicting `--org` is rejected. The package UUID and version bind the operation. `name` and description use the supported Maker editor subset documented in README. Description must contain 250 to 1500 editor characters; release notes to 1500; name to 50 UTF-16 units.

Pricing requires `type` (`free` or `paid`), decimal-string `amount` and `currency` (`USD`). A free listing uses `"0"`; a paid listing requires a positive amount. Decimal strings avoid floating-point price conversion. This integration currently sends Maker's observed once-only price plan.

`languages` and `categories` accept the current option names or IDs. They are resolved from Maker's taxonomy before creating anything. Compatibility tags such as OS and dial support come from server analysis of the staged package and preserve existing tags outside the language/type groups.

`links` maps current Maker category labels to HTTPS URLs. Observed labels include Support, Setup Guide, Privacy Policy, Legal, Community, Feedback, Demo and Related Download; the current page is authoritative. Omitting `links` or setting it to null preserves existing links. An explicit empty object requests clearing them. A supplied map replaces the link list in deterministic label order.

For an existing product, set `product_id` and optionally `variant_id` (required when variant discovery is ambiguous). The current name, description and UUID must match the prepared listing. Arbitrary name/description editing is not implemented. Published products cannot switch between free and paid through this workflow. A pending review, duplicate version, conflicting UUID, ambiguous gallery slot or surplus remote gallery image stops the operation. Surplus images are not silently deleted. Use `release` commands when updating only the package and notes.

## Requirements and limits

Media checks follow the Maker Console file controls observed November 2, 2026: PNG/JPG images, MP4 gallery videos, and at least three gallery items. Icon and thumbnail remain separately required slots, and the same local file may serve as the thumbnail and one gallery image. Gallery entries must be distinct within the gallery itself. The [published product guidelines](https://docs.elgato.com/guidelines/products/) describe PNG artwork, while the current Console accepts JPEG too; this CLI follows the accepted Console formats. Content accuracy, legibility and plugin behavior still need review and Elgato approval. Build and validate the plugin package with the official Stream Deck tools first.

## Images and ordering

Marketplace listing images may be PNG or JPEG. Gallery items may also be supported MP4 videos. The CLI validates every file before network access, then rechecks the reviewed hashes before mutations.

| Asset | Required dimensions | Maximum file size |
| --- | --- | --- |
| Icon | 288 x 288 | 2,097,152 bytes (2 MiB) |
| Thumbnail | 1920 x 960 | 5,242,880 bytes (5 MiB) |
| Gallery images, within 3 to 10 total gallery items | 1920 x 960 each | 10,485,760 bytes each (10 MiB) |
| Gallery video | 1920 x 1080 | 52,428,800 bytes (50 MiB) |

`gallery` array order is the display order, starting at 1. Upload is sequential by default and the only supported mode. You may specify `"gallery_upload_mode": "sequential"` or `--gallery-upload-mode sequential` with a JSON-file command. Each asset's storage upload, media association and local checkpoint finish before the next starts. Existing unique slots of the same type use PATCH; empty slots use POST. Replacing an image with a video or a video with an image in an occupied remote slot is blocked before staging because the observed Console flow creates another item and deletes the old one. No standalone remote reorder/delete-and-recreate operation is implemented. MP4 counts as one gallery item, just like an image. Icon and thumbnail do not support video. Files are validated by their contents as well as their extensions; renaming a different container to .mp4 or a JPEG to .png fails.

## Recovery and feedback

Successful results and plan data go to stdout as JSON; `--minify` makes compact JSON. Failed mutation commands print a concise checkpoint summary and recovery guidance to stderr; request `--json` or `--minify` for the failed plan on stdout. Progress, elapsed heartbeats, plan/resume guidance and successful write-command completion messages go to stderr. Errors return a nonzero exit code and never print a false success message. No cookies or signed URLs are included in the plan.

```sh
maker listing status <listing-id>
maker listing show <listing-id>
# After status and only when the state allows a safe retry:
maker listing submit <listing-id>
```

If you cancelled or a preflight check failed before any remote write, retire the pristine local plan before fixing files and preparing again:

```sh
maker listing discard <listing-id>
```

Discard preserves the local record and releases its UUID lockout; it does not delete a remote SKU. Plans with child draft/release records or any uncertain/completed write cannot be discarded through this command. A discarded plan cannot upload or submit.

If you deleted an unfinished draft in Maker Console, retire its saved local listing before starting a renamed or rebuilt replacement:

```sh
maker listing retire <listing-id>
```

This command makes only read requests to verify the recorded product is absent from a complete catalog, then marks the parent and child draft plans retired locally. It never deletes a remote product. It refuses live products, unknown identities, or evidence of package/details/media/submission writes. For the current rendered catalog, it requires the recognized grid/card structure with matching organization/product links and explicit single-page pagination. Multi-page or unfamiliar layouts block absence verification. Retired records remain available for inspection and cannot upload or submit.

If creation is uncertain and has no acknowledgment or product/variant IDs, use the separate operator-reviewed recovery command:

```sh
maker listing recover-creation <listing-id>
```

It requires an interactive terminal and two default-No confirmations. There is no `--yes` or `-y` bypass. No, blank input, EOF or Ctrl-C cancels. It then uses read-only requests to obtain two stable, complete catalogs, resolving missing UUID evidence through exact product details. Matching names or UUIDs, unresolved identities, malformed/incomplete catalogs or a changed catalog stop recovery. The current rendered catalog must be a recognized unfiltered single page.

Recovery means you explicitly accept that the earlier request's result is unknown. Catalog absence only describes current visibility; it does not prove the request never created anything. On success, the parent and child journals are retained as retired records with the check time, catalog digest and product count. It never creates, edits, deletes, uploads or submits remotely. It refuses any acknowledged or identified creation, package, details, media or submission evidence. An interrupted local commit can resume this recovery and recheck the catalog; completed recovery is inspectable offline.

After successful recovery, start a new `maker listing submit --file ./listing.json` invocation when ready. The old retired ID cannot submit again. Recovery does not prepare or submit the replacement automatically. A later-visible conflicting product will still block new creation through the normal identity guards.

A successful `createProduct` acknowledgment and its exact product/variant IDs are saved before catalog reconciliation. A newly created empty draft may temporarily have no Stream Deck UUID. Only a matching acknowledged creation receipt permits that missing UUID; conflicting UUIDs or unrelated drafts remain blocked. Old ambiguous plans without an acknowledgment are not silently promoted.

Resume using the saved ID, not another `--file` invocation. Confirmed steps are skipped. Local files and prepared configuration are immutable; changed content is rejected. An unresolved plan blocks another plan for the same organization/plugin UUID to avoid duplicate work. Creation failures report only a recognized safe failure category; raw provider responses, messages and transport URLs are never printed. If the create request has no acknowledgment or target IDs, current catalog absence alone does not resolve its outcome. Such plans stay blocked during normal submission and cannot use the known-product retirement command. The separate `recover-creation` command above requires explicit acceptance of that uncertainty.

A package failure proven to occur before its storage PUT can be retried. An uncertain upload, metadata mutation, media association or submission is blocked until evidence resolves it. `status` performs read-only reconciliation and never replays writes. If Maker does not expose enough evidence to identify the changed metadata/image, status leaves the plan blocked; manual inspection or a future verified reconciliation path is required. An unchanged media ID alone does not prove an image PATCH succeeded.

## Observed wire sequence

1. Read creation capabilities, extension schema and language/type/link taxonomy. Validate known-target pricing and gallery shape.
2. Create or adopt product through the dynamic `createProduct` server action at `/{org}/create/plugins`; save product and variant IDs immediately.
3. Apply requested pricing, language/type tags and links, then read back the exact USD cents, monetization and requested details. Stop before package upload if they do not match.
4. Request a fresh package presigned URL; PUT the exact archive without Maker cookies; invoke `uploadAux` at the versions route.
5. Read `/api/upload-aux/manifest/{folder}` (observed `{"manifest": {...}}` envelope; legacy direct manifest also supported), and `/api/upload-aux/tags/{folder}`.
6. Apply the General editor's metadata-only `updateProduct` action on the product route with resolved tags and links. Verify the existing price before dispatch; omit pricing fields so a second price is not created. The creation wizard's `updateProductDetails` action is reserved for initial pricing on a recognized empty free draft. Paid price mismatches remain blocked.
7. For icon, thumbnail, then gallery 1 through N: GET a fresh `/api/presigned-url?file=media/...`; PUT bytes; POST or PATCH the product/variant media route. `file_type` is `video` for MP4 and `media` for images. Plugin icon wire type is `app-icon`. Association `s3_file_id` omits the leading `media/`.
8. Recheck files/identity/version guards and the fresh staging manifest; verify pricing and every exact media acknowledgment/order again before each submission; invoke the current `createVersion` action with package references, release notes, compatibility and publication choice. Confirm review state through the release manager.

MP4 validation uses a bounded pure Go container/sample-metadata reader, without requiring ffprobe at runtime. It checks video track dimensions, timing, sample layout and recognized AVC/H.264 or HEVC/H.265 metadata. It does not decode compressed video frames. Fragmented or encrypted MP4s, codecs other than AVC/HEVC, unsupported transforms and ambiguous multi-video-track containers fail with an error; export a standard nonfragmented 1920x1080 clip instead. No transcoding or resizing occurs automatically.

The workflow checkpoints each mutation before and after dispatch with private atomic state files and exclusive locks. Presigned query strings, session credentials and private account responses are excluded from documentation and fixtures.

## Validation boundary

Unit, race and synthetic HTTP integration tests exercise creation, package staging, details, image order, submission, confirmations, read-only status and uncertain-failure behavior. Development tests use localhost servers and synthetic credentials. A live attempt created an unfinished draft, then stopped before pricing and upload because its newly created variant had a null UUID. That draft was deleted by the owner. Recovery tests cover this response shape, but a complete live end-to-end submission remains unverified; local tests do not prove provider acceptance.

## Multipart action ordering

Maker's Next.js streaming server-action decoder snapshots referenced FormData when it receives the root argument part `0`. Every `_1_...` field must precede `0:[null,"$K1"]` or the corresponding root argument model. The browser encoder appends root `0` last. Writing a Go map directly into the multipart writer made this order random, causing required fields after `0` to be omitted by the streaming decoder.

The shared action encoder now writes non-root fields in deterministic order, then root `0` last. This covers product creation, upload analysis, listing details and version submission. Indexed field names and values are preserved. The separate deletion encoder already writes root last.

A local experiment with the official Next15.5.18 compiled Busboy and React decoder, matching Maker's observed deployment, reproduced the captured failure: root before a synthetic name yielded six fields and a missing name; root last yielded all seven. A regression test parses parts as a stream and snapshots fields at root arrival. Parsing the complete request before checking fields would miss this bug. These are offline protocol checks; live provider acceptance still requires an owner-run attempt.

## Description read-back

Maker may remove the final newline from a submitted description. Remote listing identity checks allow trailing CR/LF differences only. Leading whitespace, trailing spaces, internal whitespace or line endings, markup and text changes still fail verification. Local description files, child-plan inputs, reviewed hashes and checksums remain byte-exact. This permits an acknowledged creation to reconcile and resume its existing saved ID without creating another product or altering the reviewed copy.

## Partial listing-details updates

The creation wizard may save tags and links before failing to create a price. HTTP 200 and `success:false` do not prove that no mutation occurred. Such updates remain unknown until `listing status` reads back the target identity, paid/free state, authoritative once-only USD amount, requested tags and links. A complete match checkpoints details as done without replaying the update. Mismatched or unrecognized evidence remains blocked. Resuming a reconciled plan skips completed creation, package upload and details updates.

The General editor exposes separate actions with the same exported `updateProduct` name for About and Details. Details discovery must identify its tags/additional-links contract within the current module; a name match alone is insufficient. Action hashes and module numbers are deployment-specific and are not persisted.

## Action reference boundaries

Minified scripts can declare several `createServerReference` calls in one comma-separated statement. Discovery binds the hash and exported name within a single call; it must not cross the closing parenthesis into a neighboring function. The same bounded matching applies to release, draft, details and deletion discovery. Unsupported call shapes and conflicting references stop preflight.

The Stream Deck version validator must return a boolean `is_valid`. A response such as `available:false` belongs to a different availability contract and is never accepted as version validation. Missing or incorrectly typed validation fields are reported separately from an explicit version rejection. Recognized preflight causes are reported without arbitrary provider text, credentials or transport URLs. Before `createVersion` dispatch, any failure retains the uploaded state and completed media checkpoints.

## Submission receipts and canonical versions

A successful `createVersion` response can return an acknowledgment ID different from the version ID rendered in the same response. The CLI preserves the acknowledgment separately. A canonical version ID may be bound only from that response's exact product/variant snapshot, with the expected plugin UUID and one matching version. Unsupported or conflicting evidence leaves the outcome unknown; an unrelated global version record cannot establish the binding.

Maker can shorten numeric versions: `1.0.0.0` is displayed as `1`. Comparisons allow omitted trailing zero components, with strict unsigned decimal syntax and at most four components. Other version differences remain distinct, and duplicate equivalent remote versions remain conflicts. Version summaries are enriched only from detailed rows with the exact anchored version ID; conflicting details fail readback.

Existing uncertain plans never silently replace a stored ID with another version ID. After independently verifying that a captured successful acknowledgment refers to a canonical review version, explicit local recovery is available:

```sh
maker --state-dir /path/to/release-plans release recover-receipt <release-id> \
  --receipt-id <stored-acknowledgment-id> --version-id <canonical-version-id>
```

This command uses a read-only client. It requires the exact stored receipt and verifies the target variant, unique equivalent version, review status, release notes and automatic-publication choice before changing the local journal. Only trailing CR/LF differences in release notes are accepted. The receipt remains recorded. Mismatches remain unknown; no upload or submission is retried. For a complete listing, use its nested release-plan directory, then run `maker listing status <listing-id>` to refresh the parent checkpoint.

## Request reuse and diagnostics

The HTTP routes, mutation methods, action payloads, multipart ordering, sequential media uploads and confirmation boundaries are unchanged. Optimization does not hardcode action IDs or skip conflict detection.

Each client keeps up to128 successful same-origin public script bodies, bounded to32MiB. Only `/_next/static/` JavaScript filenames ending in a content hash are eligible. Discovery still reads the current route to select deployment-specific script URLs and validates every body, including cached bodies. Unhashed scripts and failed requests are fetched again; a new hashed URL is a new entry. Cache entries do not persist across commands.

Read-only preflight and final readiness checks can share a bounded page snapshot within one context phase. Only exact Maker product, versions, catalog and plugin-creation HTML routes qualify. Auth/session, presigned URL, upload manifest/tag and storage requests never use the page cache. The phase is bounded to32entries/16MiB and does not carry across confirmations or verification phases. Any dispatched non-GET request invalidates its phase before and after the request, including failed or uncertain writes; earlier in-flight reads cannot repopulate the invalidated cache.

Standalone upload verification and fresh pre-submission verification remain separate. Pricing, plugin UUID, uploaded manifest, version ownership and every media acknowledgment are still checked at their existing boundaries. The full synthetic submission test retains the exact mutation order, downloads its hashed script once and enforces a request budget.

`--verbose` writes aggregate request counts, Maker/storage totals, HTTP failures, cache hits, response-body bytes and summed HTTP time to stderr on success or failure. Session refresh and deletion's specialized HTTP paths are included. Statistics never contain URLs, headers, cookies, tokens or payloads. JSON stdout remains valid. Default listing status reads the already-reconciled child journal for a short review/checkpoint summary; full saved data remains available through `--json`, `--minify` or `listing show`.
