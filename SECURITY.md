# Security policy

## Private reporting

Report vulnerabilities privately to **support@sentinelite.com**. Include the affected version, OS/build type, a description of the impact, and synthetic reproduction steps where possible. Please allow time for investigation and a coordinated fix before public disclosure. Do not open a public issue containing exploit details or credentials.

Never attach cookies, access/refresh tokens, raw HAR captures, signed storage URLs, one-time codes, private account payloads, or credential/state files to public issues. Do not send those secrets by email either; use a synthetic example and ask how to share any necessary evidence safely. If a credential has already been exposed, revoke or rotate it with the provider; removing the post or file is not sufficient.

## Trust boundaries

- `maker` is an unofficial Maker web-protocol client, not an approved Elgato OAuth client. Imported cookies authorize access to your Maker account; treat them as passwords.
- Native macOS builds store Maker credentials in Keychain. Linux and no-cgo macOS builds require an explicit private file. Windows secure storage and ACL-protected state are being finalized; do not assume Unix mode bits protect Windows files. See [Windows testing](docs/WINDOWS-TESTING.md).
- Local plans, locks, hosted grants and artifact mappings may contain private business metadata or paths. Keep them outside source control and shared storage. See [local-state locations](README.md#private-local-state).
- Hosted OAuth login is separate from Maker cookie import. Explicit `cloud connect` / `mcp connect` transfers the selected Maker session to the chosen service after consent; that service can use and renew it.
- Upload never submits. Explicit submission and other mutations have command-specific approval boundaries. An uncertain result must be reconciled, not blindly retried or bypassed by deleting state.

Development must use synthetic credentials and localhost `httptest` endpoints for mutation tests. No browser automation, live uploads, server-action POSTs or submissions are permitted for testing. A real release requires the owner's explicit instruction. See [AGENTS.md](AGENTS.md).

## Versions and dependencies

Before using a release, check its release notes and any published security advisories. No long-term support or backport window is promised for this initial public release. Maintainers should run the checks in [CONTRIBUTING.md](CONTRIBUTING.md) and the pre-publication history/secret review in [docs/RELEASING.md](docs/RELEASING.md).

Copyright 2026 SENTINELITE. Licensed under [Apache-2.0](LICENSE).
