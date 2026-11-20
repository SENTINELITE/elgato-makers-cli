# Elgato Makers CLI

`maker` is an **unofficial** Go client for the Elgato Maker web protocol. It reads products and analytics and prepares Stream Deck Marketplace releases over HTTP using manually imported browser cookies. It is **not an approved Elgato OAuth client** and is not affiliated with or endorsed by Elgato. Changes to Maker's internal web protocol may stop it working.

Normal CLI use needs only your Maker session, not a private MCP service. Optional hosted Google/OAuth sign-in authorizes a separate hosted MCP connection; it does not authenticate the CLI to Elgato or import Maker cookies.

## Install and supported platforms

Once published, the npm package installs `maker` with Node.js 20 or later:

```sh
npm install -g @sentinelite/elgato-makers-cli
maker --version
```

Use a binary matching your OS and architecture from the [releases page](https://github.com/SENTINELITE/elgato-makers-cli/releases), when published. Verify its checksum against the accompanying `SHA256SUMS`. Do not assume an unsigned binary is notarized or that hosted delivery has been accepted by the provider.

| Platform | Build/install | Maker credential storage and support boundary |
| --- | --- | --- |
| macOS | Native Go build with cgo and Xcode Command Line Tools | Security.framework Keychain by default. An awake, unlocked session may be needed for authorization prompts. |
| Linux | Go build or matching release binary | Explicit `--credentials-file`; dedicated directory mode 0700 and regular file mode 0600. No default Keychain store. |
| Windows | Go build or matching `maker.exe`; owner smoke testing required | Windows secure credential storage and ACL-protected state are being finalized. Do not treat authenticated Windows use as verified; see [Windows testing](docs/WINDOWS-TESTING.md). |

Source builds require Go 1.25 or newer and no private repositories. On macOS install Xcode Command Line Tools first. From a checkout on macOS or Linux:

```sh
make build
make install
export PATH="$(go env GOPATH)/bin:$PATH"
maker --version
```

Persist that PATH entry in your shell profile. `make install INSTALL_DIR="$HOME/.local/bin"` selects another directory; add it to PATH instead. To install a downloaded executable, place it in a directory on PATH and, on macOS/Linux, make it executable with `chmod +x maker`.

On Windows, use PowerShell from a checkout (no Make required):

```powershell
go build -trimpath -o maker.exe ./cmd/maker
.\maker.exe --version
```

For a persistent install, place `maker.exe` in a user-owned directory such as `$HOME\bin` and add it to your **user** PATH through Windows Environment Variables. Reopen PowerShell and run `Get-Command maker` and `maker --version`. See the [PowerShell smoke-test guide](docs/WINDOWS-TESTING.md) before importing credentials.

A `CGO_ENABLED=0` macOS build has no Keychain adapter; select a private file explicitly. Plain `go build` reports the development version. `make build` uses a release tag on the current commit when available. See [releasing](docs/RELEASING.md) for maintainer procedures.

## Import and renew a Maker session

1. Sign into `https://maker.elgato.com` in your normal browser.
2. In DevTools Network, select a successful Maker request and copy only its **Cookie header value**, or export cookie JSON privately. Do not use a complete copied cURL command.
3. Import through stdin or a private file, never a cookie-valued command argument. Keep import files outside the repository and remove them after import.

macOS:

```sh
pbpaste | maker auth import --stdin
maker config init --org 00000000-0000-4000-8000-000000000001
maker auth check
maker products
```

Linux (also the explicit file-store option on macOS):

```sh
install -d -m 700 "$HOME/.local/share/maker-private"
maker --credentials-file "$HOME/.local/share/maker-private/session.json" \
  auth import --cookie-file /absolute/private/path/cookies.json
maker config init --org 00000000-0000-4000-8000-000000000001
maker --credentials-file "$HOME/.local/share/maker-private/session.json" auth check
```

Use the same `--credentials-file` on every subsequent authenticated command. No default Linux credential file is selected automatically. The importer accepts a Cookie header, an array of cookie objects, or `{"cookies":[...]}`. Only recognized Maker authentication cookies are retained; scope and expiry metadata in cookie JSON are supported. Never paste credentials into issues, chat, logs or Git.

Automatic renewal needs `__Secure-kc_rt`. A normal page request may omit this path-scoped cookie; use a successful `/api/auth/session` request or cookie JSON retaining its scope. Before network commands, a missing access token or expiry within 60 seconds triggers renewal when a usable refresh cookie exists. Successful renewal is persisted before continuing; other server-issued rotations are saved even if a later step fails. JWT expiry is scheduling information, not proof of authentication.

```sh
maker doctor          # local diagnostics; no network
maker auth check      # read-only organization access check; may renew cookies
maker auth refresh    # explicitly request session renewal
maker auth forget     # remove credentials from the selected local store
```

Maker controls expiration and revocation. There is no background keepalive or indefinite-session guarantee. Browser and CLI refresh credentials may rotate together. If renewal fails, sign into Maker again and reimport fresh cookies. `auth forget` is local removal, not remote session revocation; it does not delete review plans. There is no separate `auth logout` command.

Troubleshooting:

- Keychain unavailable: use the native cgo build on an awake, unlocked Mac and review its normal authorization prompt. Non-macOS/no-cgo builds need an explicit store.
- Private-file permission error on Unix: use a dedicated real directory (0700) and regular file (0600), not a shared or symlinked location. Do not weaken permission checks.
- Missing organization: run `config init --org` or use `--org` for the command.
- Session lock: commands sharing a store are serialized. Verify the owning process has stopped before removing a reported stale lock.
- Mutation failed or timed out: inspect its saved status first. Renewing authentication is not permission to repeat an uncertain write.

## Inspect and read

All IDs, aliases, paths and codes below are synthetic examples. Replace them with your own reviewed targets.

```sh
maker inspect ./dist/com.example.plugin.streamDeckPlugin
maker products
maker product show --product 00000000-0000-4000-8000-000000000002
maker config bind example --product 00000000-0000-4000-8000-000000000002 \
  --uuid com.example.plugin
maker versions --product example
maker notes --product example --count 2
maker analytics -d 30
maker analytics products monthly --range this-year --type paid --format csv > product-months.csv
maker analytics daily -d 30 --json
```

Binding verifies the product through read-only requests; specify `--variant` when multiple variants exist. Aliases bind organization, product, variant and expected plugin UUID. Versions and notes preserve provider order.

Analytics supports overview, products, daily, weekly, monthly and summary views; `--type all|free|paid`, product/extension filters, relative or explicit dates, and `--compare-previous`. Choose one date selector: `--days/-d`, `--months/-m`, `--range`, or `--start`/`--end`. Dates are inclusive. Defaults use available daily history (currently starting 2025-10-01) through yesterday, not guaranteed lifetime history. Earlier starts are clipped with a warning. Free/paid filtering uses product monetization, not the effective purchase price.

Revenue is integer USD cents in JSON/CSV. The displayed 70% creator share assumes a 30% fee; it is a local estimate, not a payout statement. Adoptions are product units, not unique users. `--json` writes structured results, `--minify` compact JSON, and `--verbose` aggregate HTTP statistics without request URLs or credentials. Progress/warnings/completion go to stderr. Check exit status before using an export; failures may leave partial redirected files.

## Prepare, upload and explicitly submit

Build, sign and package your plugin with the appropriate Stream Deck tools first. This CLI takes an existing `.streamDeckPlugin`; it does not build, sign or notarize it.

```sh
maker release prepare --product example \
  --file ./dist/com.example.plugin.streamDeckPlugin \
  --notes ./release-notes.md --no-auto-publish
maker release show RELEASE_ID
```

Preparation saves a local review record and performs read-only target checks; add `--offline` to use an existing configured alias without network or credentials. The reviewed package and notes are hashed. Choose exactly one of `--no-auto-publish` or `--auto-publish`. Automatic publication means **after Elgato approval**, not automatic submission.

Supported notes include paragraphs with line breaks, headings, lists with supported nesting, emphasis, inline code and links. See [release-note Markdown](docs/RELEASE-NOTES.md) for indentation, examples and validation limits.

**The following commands write to production. Run them only for a release you explicitly intend to send.**

```sh
maker release upload RELEASE_ID
maker release show RELEASE_ID
maker release submit RELEASE_ID
maker release status RELEASE_ID
```

Within the package-only `release` workflow, **`release submit` is the only submission path**. Upload stages and validates the package; **upload never submits**. Submission asks two questions, both defaulting to No. Blank input, No, EOF or interruption cancels. Noninteractive submission refuses unless explicitly authorized:

```sh
maker release submit RELEASE_ID --yes
```

`-y` is equivalent. This flag belongs to that command, not a global setting. Files, identity and publication choice are rechecked before dispatch. Changed reviewed files require a new plan. Upload and submission check that the selected variant has no duplicate version, no different version in review and no unrecognized lifecycle in its version list.

An ambiguous result becomes `submission_unknown` and blocks blind retry, including a new plan for the same target. Run `release status RELEASE_ID` for read-only reconciliation. Missing proof keeps the operation blocked. Do not edit/delete state to force a retry. Status never replays writes.

### Other explicit write workflows

The separate complete-listing workflow also has an explicit **`listing submit`** command; it is not a read or upload alias. See [listing configuration and recovery](docs/listing-protocol.md) and [example JSON](examples/listing.example.json):

```sh
maker listing validate --file ./listing.json
maker listing prepare --file ./listing.json
maker listing show LISTING_ID
# Production staging writes; never submits:
maker listing upload LISTING_ID
# Production submission; two default-No confirmations, or its own --yes/-y:
maker listing submit LISTING_ID
maker listing status LISTING_ID
```

`listing submit --file ./listing.json` is a combined flow with confirmation before production writes. Listing uploads handle icon, thumbnail and gallery sequentially in reviewed order. Existing names/descriptions must match; surplus remote gallery assets are not silently removed. Resume a saved ID after status reconciliation rather than preparing another operation.

`product draft prepare/show/create/status` supports initial draft creation/adoption without review submission. `product delete` is irreversible and draft-only: it requires two default-No confirmations, or its own `--yes` plus exact `--confirm-name`. These commands do not bypass release submission guards.

## Discounts

```sh
maker discounts list
maker discounts list --status scheduled
maker discounts show 00000000-0000-4000-8000-000000000003
maker discounts prepare --file ./discount.json
# Production monetary changes; review before confirming:
maker discounts create PLAN_DIGEST
maker discounts status PLAN_DIGEST
maker discounts deactivate 00000000-0000-4000-8000-000000000003 --confirm-code EXAMPLE20
```

Preparation is offline and creates an immutable 24-hour plan. The supported write shapes are all-products percentage, all-products fixed USD in whole dollars, and percentage for one selected product. See [discount plan fields and examples](docs/DISCOUNTS.md).

Creation and deactivation each require two default-No confirmations or their own explicit `--yes`/`-y`. Deactivation also requires the exact current maker-code suffix. Reconcile uncertain creation with `discounts status` before attempting another change.

## Optional hosted connection

```sh
maker mcp login --server https://makers.sentinelite.com
maker mcp status --server https://makers.sentinelite.com
maker mcp logout --server https://makers.sentinelite.com
```

Hosted Google sign-in/OAuth is separate from Maker cookie authentication. Login uses browser consent and PKCE; `--no-browser` prints an authorization URL for manual opening and still waits for a loopback callback. Login/status/logout do not transfer Maker cookies. Hosted MCP runs remotely; there is no local start/stop daemon. A service must be deployed and reachable at the selected origin.

Only the explicit `maker mcp connect` / `maker cloud connect` one-time-link flow transfers your selected Maker session to a service, after consent. Review its exact origin and organization: the recipient can use and renew that session. Hosted logout removes/revokes this CLI's OAuth grant, not the remote Maker connection; disconnect Maker on the hosted account page. An interrupted OAuth refresh requires logout and fresh login, not deletion of its guard.

Hosted CI policies can be set up at https://makers.sentinelite.com/ci or with `maker cloud login --write` and `maker ci policy create --file policy.json --open --wait`, without an MCP client. Review and confirm the policy in your signed-in browser. Setup changes only CI permissions.

Optional hosted CI offers package preflight with no Elgato writes and owner-approved automatic update submissions. When a version is in review, the hosted service can queue the newest eligible release for that variant. See [hosted delivery and CI](docs/HOSTED-CI.md) for exact branch setup, default notes, queue behavior and verification. The CLI runs standalone without the hosted service.

## Private local state

| Platform | Default configuration base |
| --- | --- |
| macOS | `~/Library/Application Support/Elgato Makers CLI/` |
| Linux | `$XDG_CONFIG_HOME/Elgato Makers CLI/`, or `~/.config/Elgato Makers CLI/` |
| Windows | `%AppData%\Elgato Makers CLI\` (secure storage/state acceptance pending) |

Under that base: `config.json` holds organization/aliases; `releases/` holds review plans, draft/listing/discount records and uncertainty journals; `mcp/` holds hosted OAuth credentials (`mcp-<server hash>.json`), refresh guards and private delivery mappings. Plans/mappings may contain private paths and business metadata even though they omit cookies and signed URLs. Keep the whole state tree private and out of source control, shared folders and public backups.

Maker cookies are separate: native macOS Keychain service `com.sentinelite.elgato-makers-cli`, account `maker.elgato.com`, or the exact path you select with `--credentials-file`. File-store session locks use that path plus `.lock`. The default-store lock is `Elgato Makers CLI/session.lock` under the OS user cache directory: `~/Library/Caches` on macOS, `$XDG_CACHE_HOME` or `~/.cache` on Linux, and `%LocalAppData%` on Windows.

`--config` moves the configuration base (including hosted state); `--state-dir` overrides workflow state. Unix credential/OAuth stores enforce private permissions (0700 directories/0600 files). Those mode bits are not a Windows ACL guarantee.

## Features and verification limits

| Area | Current behavior / limitation |
| --- | --- |
| Catalog and analytics | Read-only queries, aliases, date grouping and exports; dependent on observed provider schemas/history. |
| Package releases | Reviewed local plans, separate upload/submit, read-only reconciliation and durable uncertainty guards. |
| Complete listings | Draft identity, pricing/details and ordered media workflow; not arbitrary existing name/description editing or gallery deletion. |
| Discounts | Read, reviewed all-products percentage or fixed USD creation, one-product percentage creation, and deactivation. |
| Shared Go adapter | `pkg/connect` contract 1.2.0 and `pkg/workflows/v1`; no MCP dependency. See [adapter policy](docs/ADAPTER.md). |
| Hosted delivery / CI | Optional preflight, owner-approved submission and review queue. Real GitHub OIDC and Elgato acceptance are separate. |

| Verification layer | What it establishes |
| --- | --- |
| Standalone CLI | Builds independently of private MCP; historical read-only analytics/session-renewal checks are not a guarantee for a new session or platform. |
| Shared adapter | Exported API pin and versioning policy; not acceptance by an independently deployed service. |
| Hosted MCP | Optional OAuth/delivery client integration; deployment, consent and remote policy require separate verification. |
| Local tests | Synthetic credentials and localhost `httptest` exercise protocol, confirmations, ordering and failure recovery; no production mutations. |
| Real provider acceptance | Requires a separately authorized owner run. A complete listing submission, real GitHub OIDC/policy delivery and Marketplace acceptance are not established by local tests. |

### Future work (not implemented or not yet accepted)

- Discount editing, reactivation, deletion, and write shapes beyond those in the [discount plan guide](docs/DISCOUNTS.md).
- Arbitrary listing name/description edits and broader gallery removal/replacement.
- Dedicated CLI commands for ProductDetails/ProductMedia adapters and optional MCP 0.16 wrappers.
- Real GitHub OIDC and hosted policy/delivery acceptance.
- Finalized Windows secure credential storage, ACL-protected state and owner acceptance.
- Homebrew distribution and macOS code signing/notarization.

## Contributing, security and license

See [CONTRIBUTING.md](CONTRIBUTING.md), [AGENTS.md](AGENTS.md), [CHANGELOG.md](CHANGELOG.md), and the [protocol notes](docs/protocol.md). Build checks are `go vet ./...` and `go test -race ./...`. Development must not perform live mutations.

The CLI has no telemetry, crash reporting or update check. Commands contact Maker, the selected hosted service, and storage endpoints needed for explicit uploads. Keep credentials, raw captures, signed URLs and personal account payloads out of public reports. Report vulnerabilities privately to **support@sentinelite.com**; see [SECURITY.md](SECURITY.md). General support: support@sentinelite.com.

Licensed under the [Apache License, Version 2.0](LICENSE). Copyright 2026 SENTINELITE.
