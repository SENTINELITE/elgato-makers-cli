# Adapter compatibility policy

Separately built services (for example a hosted connector) may import two public
Go packages from this module. Everything under `internal/` is private and may
change in any release.

| Package | Role | Version marker |
| --- | --- | --- |
| `pkg/connect` | Session handling and read-only Maker queries | `connect.ContractVersion` |
| `pkg/workflows/v1` | Durable, approval-bound business workflows | the `v1` import path and `v1.Version` |

`pkg/discountplan` is a helper for the workflow package. It follows the same
rules as `pkg/connect`.

## Versioning rules

- `connect.ContractVersion` is a semantic version for the exported API of
  `pkg/connect`. Adding an exported identifier raises the minor number. Removing
  or renaming an identifier, or changing a signature, field, constant value or
  documented behavior, raises the major number.
- A major change to `pkg/connect` is released with the CLI's next major version
  (or the next minor version while the CLI is below 1.0.0) and is called out in
  the release notes.
- `pkg/workflows/v1` stays source compatible for as long as it exists. Breaking
  changes go into a new `pkg/workflows/v2` package; `v1` is kept for at least one
  CLI minor release after `v2` ships and its removal is announced beforehand.
- Behavior that protects users does not count as a breaking change: durable
  uncertainty guards, approval binding, sequential media handling and refusal of
  blind retries may become stricter in any release.

## What is pinned

`pkg/connect/api_test.go` compares every exported declaration of `pkg/connect`
(plus the members of aliased internal types) with
`pkg/connect/testdata/api.golden`. Any difference fails the test, so an
accidental break cannot ship unnoticed.

For an intentional change:

1. Decide whether the change is additive or breaking and update
   `ContractVersion` to match.
2. Run `go test ./pkg/connect -run TestExportedAPIIsPinned -update-api`.
3. Review the golden file diff, then commit it together with the change.

Members of aliased internal types are pinned by name and signature only, so
their behavior is covered by the ordinary tests.

## Consuming the adapter

- Depend on a released tag, never an untagged branch, and pin it in `go.mod`.
- Check `connect.ContractVersion` at startup if your service must refuse an
  unexpected major version.
- A normal build of this module never needs a private repository, and nothing
  here imports a host service.
