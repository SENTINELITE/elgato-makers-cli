# Changelog

## 0.9.1

- Match the hosted MCP's consolidated account, workflow and approval tools.
- Track server-started operations after signed-in browser confirmation and retain direct file delivery.

## Unreleased: standalone hosted CI setup

- Add `maker cloud login/status/logout` aliases over the existing hosted OAuth credentials.
- Add `maker ci policy create/list/status/apply/disable` using the HTTPS owner API, with optional browser opening and approval waiting.
- Add top-level `maker ci preflight/release`; keep `maker mcp ci` compatible.
- Preserve trusted browser confirmations and stop on uncertain apply outcomes without retrying.

## Initial public release

The initial public source release includes:

- Standalone `maker` CLI using HTTP and imported Maker session cookies, with session renewal and local credential removal.
- Catalog, product, version, release-note and analytics reads, including date grouping and JSON/CSV exports.
- Local package inspection and reviewed release plans, separate staging/upload and explicit review submission, and read-only status reconciliation with durable uncertainty guards. Existing variants are guarded against duplicate versions, pending review and unknown lifecycle states.
- Release-note validation matches the Maker editor for standalone line breaks and supported nested lists.
- Draft creation/adoption, guarded draft-only deletion, and a complete-listing workflow with sequential artwork/media staging.
- Discount reads, reviewed all-products percentage and whole-dollar USD discounts, percentage discounts for one selected product, and guarded deactivation.
- Public `pkg/connect` adapter contract 1.2.0 and versioned `pkg/workflows/v1` workflows, independent of private hosted services.
- Optional hosted OAuth, artifact delivery, CI preflight without Elgato writes, and policy-authorized CI releases with approved default notes and a queue that keeps the newest eligible update during review.
- Synthetic local protocol and safety tests. These are not proof of complete live listing submission, real GitHub OIDC delivery or Marketplace acceptance.

Windows secure credential storage, ACL-protected state and owner acceptance remain in progress. Homebrew distribution and macOS signing/notarization are not included. See [README.md](README.md) for platform and feature limits.

This is an unofficial web-protocol client, not an approved Elgato OAuth integration. Licensed under [Apache-2.0](LICENSE), copyright 2026 SENTINELITE. Support: support@sentinelite.com.
