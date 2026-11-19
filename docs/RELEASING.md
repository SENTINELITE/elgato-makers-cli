# Releasing

Every push to `main` builds the five native targets and a tested npm tarball
as GitHub Actions artifacts. The version comes from the CLI source default.
Use **Actions → Build and release → Run workflow** to build an explicit
SemVer for testing. Both build-only routes leave GitHub Releases and npm untouched.
Tag a reviewed commit to publish its GitHub download. Publish the npm package
from that same release tarball.

## Versioning

- Versions follow semantic versioning and tags look like `v1.2.3`. A suffix such
  as `-rc.1` marks a pre-release.
- While the CLI is below 1.0.0, a minor bump may include breaking changes; say so
  in the release notes.
- `pkg/connect` carries its own `ContractVersion` and `pkg/workflows/v1` its own
  path version. Follow [ADAPTER.md](ADAPTER.md) when either changes, and make sure
  the CLI version moves accordingly.
- Untagged builds report `0.9.0`, including outside a Git
  checkout. `make build` uses the version tag on the current commit when one is
  present and marks a dirty tagged build with `-dirty`. Override it with
  `make build VERSION=0.9.0-rc.1`. The release workflow sets the version from
  the pushed tag.

## Before tagging

1. Work from a clean, reviewed commit on the protected release branch. Required
   checks: `CI` passing and a review of the diff since the last tag.
2. Run locally:

   ```sh
   go vet ./...
   go test -race ./...
   go list -deps ./... | grep -i stream-deck-makers-mcp   # must print nothing
   ```

3. Run the secret check below.
4. Confirm `pkg/connect/testdata/api.golden` and `connect.ContractVersion` match
   any adapter change.

## Secret check

Run this on the full history before the first public push and before every
release. A pattern hit or an unexpected filename blocks the release.

```sh
# Tooling, if installed (preferred):
gitleaks detect --source . --log-opts="--all"

# Built-in fallback: suspicious file names anywhere in history ...
git log --all --name-only --format= | sort -u | grep -E '\.(har|key|pem|enc|cookies)$|(^|/)\.env|credentials'

# ... and secret-shaped content in every commit.
git grep -nIE 'BEGIN [A-Z ]*PRIVATE KEY|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{36}|xox[abprs]-[A-Za-z0-9-]{10,}|X-Amz-Signature=[0-9a-f]{32,}' $(git rev-list --all)
```

Also search history for real account identifiers (organization and product IDs,
product names, e-mail addresses) that should not be public. Deleting a file in a
later commit does not remove it from history. If something real was committed,
either publish a fresh history without it or rewrite history before pushing, and
rotate any credential that was exposed instead of only removing it.

Release builds use no repository secrets. The only token is the workflow's
`GITHUB_TOKEN`, and the `publish` job alone holds write and attestation
permissions. Keep infrastructure credentials in the hosting provider's secret
settings, never in the repository.

## Tagging and publishing

```sh
git tag -s v1.2.3 -m "v1.2.3"     # use -a if you do not sign tags
git push origin v1.2.3
```

The **Build and release** workflow vets and tests the tagged commit, runs
`govulncheck`, builds the binaries and npm tarball, then installs the tarball
offline on all five native targets. The version from the tag is embedded in
each binary, its filename, and the npm manifest. The workflow attaches the
tested files, SHA-256 checksums, and build provenance to the GitHub release.
Pre-1.0.0 versions and tags with a pre-release suffix are marked pre-release
on GitHub.

## npm publishing

The package is `@sentinelite/elgato-makers-cli` and installs the `maker`
command. The tarball bundles all five native binaries and has no dependencies
or install-time scripts.

For the first publish, download the tarball and `SHA256SUMS` from the matching
GitHub release, then run:

```sh
tag=v0.9.0
gh release download "$tag" --repo SENTINELITE/elgato-makers-cli \
  --pattern '*.tgz' --pattern SHA256SUMS
shasum -a 256 -c --ignore-missing SHA256SUMS
npm login
npm publish "./sentinelite-elgato-makers-cli-${tag#v}.tgz" --access public --ignore-scripts
```

Then add GitHub Actions as the npm package's [trusted publisher](https://docs.npmjs.com/trusted-publishers/).
Set the GitHub owner to `SENTINELITE`, repository to `elgato-makers-cli`, and
workflow filename to `npm-publish.yml`. Allow direct publishing. For later
versions, dispatch **Publish CLI to npm** from `main` with the existing release
tag. The workflow verifies the release checksum, build attestation, package name, and version
before publishing with OIDC. Plain versions go to npm's `latest` tag.
SemVer pre-release suffixes go to `next`.

## Toolchain

Release and CI builds pin the Go compiler explicitly (`go-version: '1.27.4'`
in the workflows, currently the newest patch on the newest supported release
line). This is deliberate and separate from the `go 1.25` directive in
`go.mod`: that directive is only the minimum language/toolchain version a
consumer needs to build the module — `pkg/connect` and `pkg/workflows/v1` are
consumed as a library by sibling projects, so the minimum stays low and no
`toolchain` directive is added. The pinned compiler is what produces shipped
binaries, and `govulncheck` runs under the same pin because it analyzes that
toolchain's standard library. When bumping the pin, update both workflows and
pick the newest patch release on a supported line (see
https://go.dev/doc/devel/release). govulncheck itself is pinned to
`golang.org/x/vuln/cmd/govulncheck@v1.10.0`.

Verify a downloaded binary:

```sh
shasum -a 256 -c SHA256SUMS --ignore-missing
gh attestation verify maker_1.2.3_linux_amd64 --repo SENTINELITE/elgato-makers-cli
```

macOS binaries are built with cgo for the Keychain adapter. Linux and Windows
binaries are built without cgo and need `--credentials-file`.

## Rollback

- Never move or reuse a published tag. Consumers pin tags and checksums.
- If a release is bad, mark the GitHub release as a pre-release or delete it so it
  is no longer offered, then publish a fixed patch version (`v1.2.4`). Add a
  `retract` directive to `go.mod` for a version that Go users must avoid, and
  release the retraction in a new tag.
- If a secret was published, treat it as compromised: rotate it first, then
  remove the release and rewrite history only if needed.
- Hosted services that pin the adapter should move back to the last good tag in
  their own `go.mod`; no change to this repository is needed for that.
