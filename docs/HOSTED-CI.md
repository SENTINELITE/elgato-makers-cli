# Hosted delivery and GitHub CI

Hosted MCP OAuth and Maker login are separate. The CLI sends only its scoped hosted access token to the hosted API. It never transfers Maker cookies through these commands.

## Local listing workflow

```sh
maker mcp login --write
maker mcp listing prepare --file listing.json --json
maker mcp action request --workflow WORKFLOW_ID --action submit --upload-mode direct --json
# Open approval_url and complete both owner confirmations.
maker mcp action execute --workflow WORKFLOW_ID --operation OPERATION_ID --json
```

The listing uses the existing CLI JSON format and local paths. It is validated before uploading. Package and artwork bytes go to authenticated private artifact storage for independent server validation. The hosted listing receives opaque artifact IDs and inline Markdown. A private mapping under the CLI configuration directory keeps the exact original file hashes and paths on this computer.

`maker mcp upload --file plugin.streamDeckPlugin --kind package --json` uploads a single validated artifact. Other kinds are `icon`, `thumbnail`, and `gallery`.

Direct delivery asks the local CLI to perform the final signed PUT to the exact Elgato production S3 bucket. Signed URLs remain in memory and are never printed or stored. Every lease checks the artifact ID, SHA256, size, expiry, and local file hash. Storage requests carry no hosted authorization or Maker cookies and follow no redirects. A failed PUT or uncertain acknowledgment stops execution. Inspect and reconcile the existing operation before starting another attempt. Polling stops after 30 minutes or cancellation. Already dispatched provider work may continue.

A grant saved before `--write` remains read-only. Logout and log in with `--write` to request new browser consent. Refresh rotates the existing grant without requesting broader scopes. Long interactive delivery refreshes that same locked saved client/grant before status, lease and completion requests as expiry approaches. CI uses separate short-lived policy tokens.

## Set up CI without an MCP client

The hosted CI API supports standalone setup through `maker cloud login --write` and `maker ci policy`. On a service supporting these routes, run:

```sh
maker cloud login --write --server https://makers.sentinelite.com
maker ci policy create --file /absolute/path/preflight-policy.json --open --wait
maker ci policy list
```

Create sends your bounded policy JSON to the owner REST API and prints a browser approval URL. The owner must complete both confirmations. `--wait` observes approval, applies the policy once and returns its ID; it never treats terminal input as browser approval. Without waiting, resume with `maker ci policy status --operation ID` and `maker ci policy apply --operation ID`. To revoke a policy, run `maker ci policy disable --policy ID --open --wait`, which also requires both confirmations.

The website alternative is https://makers.sentinelite.com/ci. It defaults to validation only, saves the exact request for review and applies CI metadata after both confirmations. The resulting ID appears on the CI page. Neither setup path submits a plugin. Existing MCP policy tools remain available for users who prefer them.

The JSON uses `repository` (owner/repo), `repository_id` (numeric GitHub ID as a string), `ref` (exact refs/heads/main), `workflow_ref` (owner/repo/.github/workflows/FILE.yml@refs/heads/main), optional `environment`, and existing `product_id`, `variant_id`, `uuid`. Choose `auto_publish` explicitly, `preflight_only: true` for validation, `expiration_days` (1–365), and optional `default_release_notes`. A release-capable policy requires explicit owner review; do not silently broaden a preflight policy.

Copy the policy ID from the successful browser confirmation or the hosted CI policies page. For automation, add `--json` to get the full ID in the CLI receipt.

Policy setup uses HTTPS REST, not MCP tool calls. Hosted OAuth credentials remain scoped to this CLI's saved grant, shared with compatible `maker mcp login` commands. `maker cloud status` uses the read-only REST policy list. The GitHub runner uses OIDC instead of your owner OAuth token. Failed or uncertain apply requests are never automatically repeated; retain the operation ID and inspect its status before continuing. The same owner/client/grant must request and apply an operation.

`maker ci preflight` and `maker ci release` are the preferred runner spellings. The legacy `maker mcp ci` group remains compatible and uses the same HTTPS endpoints.

## Choose where to validate and release

| Run | Command | Authority |
| --- | --- | --- |
| Pull request or local build | `maker inspect dist/plugin.streamDeckPlugin` and the project's normal tests | Local archive reads. Omit hosted credentials and GitHub OIDC permission on PRs. |
| Trusted branch validation | `maker ci preflight` | Scoped hosted package upload, validation and Maker reads. A `preflight_only` policy cannot obtain a release token. |
| Production | `maker ci release` | The standing owner-approved policy allows qualifying runs to queue or submit an update for the existing bound product. |

Both hosted commands require a GitHub Actions runner with OIDC permission and matching policy. Preflight returns `ready` or `queued_for_review` with the package version, SHA-256 and corresponding review state (`clear` or `in_review`). Both results include `provider_writes: false`. `queued_for_review` is a preflight result. Preflight creates no release job and performs no Elgato writes. The host saves the uploaded package in private artifact storage for validation. On a PR, local inspection supplies no readback from the Marketplace.

```sh
maker mcp ci preflight --file dist/plugin.streamDeckPlugin \
  --policy "$MAKER_CI_PREFLIGHT_POLICY" \
  --server https://makers.sentinelite.com --json

maker mcp ci release --file dist/plugin.streamDeckPlugin \
  --policy "$MAKER_CI_POLICY" \
  --server https://makers.sentinelite.com --json
```

Add `--notes RELEASE_NOTES.md` to use a reviewed file from the checked-out commit. Omission uses the notes saved in the approved policy if present. An invalid or empty explicit notes file fails validation even when a default is configured. The effective notes become part of the saved release. The owner also reviews the policy's `auto_publish` setting, which controls publication **after Elgato approval**.

## Configure the example

1. Start with the [example workflow](../.github/workflows/maker-hosted-release.example.yml). Use trusted, pinned action revisions and a trusted CLI binary. Replace the build placeholders with your project's real pipeline, including its production signing steps. Choose runners that support those steps. Protect `main`, restrict each GitHub environment to its intended branch and review changes to the release workflow.
2. In an MCP client with `maker:write` consent, call `prepare_ci_deployment`. Bind the policy to the exact repository name and numeric ID, branch ref, workflow ref, product, variant and package UUID. Bind the signed GitHub `environment` claim to the environment name in the workflow. Approve a policy with `preflight_only: true` for validation and a separate policy for production. For each policy, call `request_ci_deployment_enable_approval`, complete both browser confirmations, then call `execute_workflow`.
3. Set repository variables `MAKER_CI_REPOSITORY` to `OWNER/REPO` and `MAKER_CI_PREFLIGHT_REF` to the approved full branch ref, such as `refs/heads/release-validation`. Keep the workflow on the repository's default branch for manual dispatch and use the reviewed copy on the validation branch. Set `MAKER_CI_PREFLIGHT_POLICY` for the `maker-preflight` environment and `MAKER_CI_POLICY` for `maker-production`. Policy IDs are identifiers. GitHub OIDC supplies the credential for each run.
4. Set `MAKER_CI_PREFLIGHT_ENABLED=true` at the repository level when ready for validation. Dispatch the workflow on the exact preflight ref. The example's production job requires the exact `refs/heads/main` ref, a `push` or `workflow_dispatch` event, and the separate `MAKER_CI_RELEASE_ENABLED=true` repository variable. Set the release variable only when the production build, signing, environment and standing policy have been reviewed.

The release policy fields are `repository` (name), `repository_id` (numeric ID as a string), `ref`, `workflow_ref`, `product_id`, `variant_id`, `uuid`, and an explicit Boolean `auto_publish`. Set `environment`, and optionally `expiration_days` and `default_release_notes`. Set `preflight_only: true` for the validation policy. The workflow ref includes the filename and branch, such as `OWNER/REPO/.github/workflows/maker-release.yml@refs/heads/main`.

The example jobs are off by default. GitHub can load filenames ending in `.example.yml`; the job conditions and failing placeholders are what keep the template from executing unconfigured. Fork and pull-request code should stay in a separate local-validation workflow without hosted release permissions.

## Queue and recovery

The default server upload mode can retain a release while a different version of the same variant is in Elgato review. A `queued` release result returns immediately with a durable operation ID. The CLI polls when the release is running and exits nonzero on reported failure or uncertainty. The service periodically rechecks the variant and may submit the newest qualifying queued release when review clears. The queue is bounded by policy and artifact expiry. Existing reviews are never withdrawn or deleted automatically. `--upload-mode direct` is available for immediate CLI-led S3 delivery and requires the review slot to be clear. When using direct mode with an older host, supply an explicit notes file.

Use the hosted MCP tool `list_ci_deployments` to inspect the policy's pending release and use the saved operation ID to track dispatched work. Check the hosted operation and Maker status before a retry, especially after a transport failure or missing acknowledgment. Saved job status and Marketplace review status are separate, and a saved submission receipt does not establish approval or publication. Branch protection controls what can enter the approved workflow. GitHub job concurrency alone cannot cancel an already accepted Marketplace write.

## Verification

Synthetic localhost tests cover scoped CI authorization and fake-provider delivery. Real GitHub OIDC claims and protected environments, deployed-policy isolation, and Elgato review and publication require acceptance on those systems. The standalone CLI builds and runs without a hosted service.
