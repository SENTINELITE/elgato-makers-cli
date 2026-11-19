# Contributing

Read [AGENTS.md](AGENTS.md) before changing code or documentation. This is an unofficial client of an observed web protocol; local tests do not prove that a production mutation will be accepted.

## Build and check

Use Go 1.25 or newer. Native macOS Keychain support requires cgo and Xcode Command Line Tools. The module must build from a clean public checkout without private repositories or hosted-service imports.

```sh
go build ./cmd/maker
go vet ./...
go test -race ./...
```

On macOS/Linux, `make build`, `make vet`, and `make test` are equivalent project entry points; `make build` injects the Git-derived version into `bin/maker`. On Windows, use the Go commands directly; the build output is `maker.exe`. Race tests require a supported race-detector toolchain/platform. Report any check you could not run instead of claiming it passed.

## Safety and fixtures

- No browser automation.
- No production uploads, server-action POSTs or submissions during development or testing. Live checks may only read. A real release requires explicit instruction from the owner.
- Every mutation test uses localhost `httptest` endpoints and synthetic credentials.
- Never put raw HAR contents, cookies, tokens, signed S3 query strings or personal account payloads into source, tools, logs, fixtures, documentation or issues. Keep raw captures and credentials outside the repository. Reduce privately inspected evidence to reviewed synthetic contract fixtures.
- Do not change endpoints, payloads, multipart ordering, sequential media handling or uncertainty guards without a reviewed contract capture.
- Preserve explicit write boundaries: upload never submits; `release submit` is the package-release submission command with two default-No confirmations or its own `--yes/-y`. The separate listing submission flow has its own confirmation boundary. Never introduce global approval or automatic mutation retries.
- Preserve durable uncertainty journals and read-only status reconciliation. Missing evidence must not silently authorize a retry.

## Public adapter contracts

Follow [docs/ADAPTER.md](docs/ADAPTER.md) for every public API change.

- `pkg/connect` currently declares `ContractVersion = "1.1.0"`. Its exported declarations (including aliased members) are pinned by `pkg/connect/testdata/api.golden`; the API pin test rejects unversioned changes.
- Additive exported changes require a minor contract bump; incompatible API or documented-behavior changes require a major bump under the adapter policy. For an intentional change, update the marker, run `go test ./pkg/connect -run TestExportedAPIIsPinned -update-api`, and review the golden diff with the implementation.
- `pkg/workflows/v1` uses a versioned import path and must remain source compatible. Breaking changes belong in a new versioned package, with the retention rules in the adapter policy.
- `pkg/discountplan` follows the same compatibility rules as `pkg/connect`. `internal/` is not a public adapter surface.
- Pin consumers to released tags. Never import a private or host-service repository into this module.

## Changes and reports

Match the surrounding style and comment density. Include focused synthetic regression tests for protocol or safety changes. Describe behavior changes, validation performed, and remaining provider/platform acceptance limits. Documentation examples must use real registered commands/flags and synthetic IDs/paths/codes. Do not claim unfinished hosted CI/CD or Windows secure-storage work is accepted.

Use [SECURITY.md](SECURITY.md) for vulnerability reports. General support and private security contact: **support@sentinelite.com**.

Contributions are provided under the repository's [Apache License, Version 2.0](LICENSE). Preserve SENTINELITE license attribution.
