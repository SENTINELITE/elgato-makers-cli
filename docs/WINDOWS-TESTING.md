# Windows owner smoke test

Use a normal, non-administrator PowerShell session. This guide does not authorize production mutations: **no upload, create, deactivate, delete, listing submission, release submission or CI release**. Do not run a hosted write flow. All IDs and paths below are examples.

## Storage gate

The Windows implementation uses an explicit `--credentials-file` store. It creates private files, directories and lock files with a Windows DACL protected from inheritance. Created objects grant access to the current user, LocalSystem and built-in Administrators. The credential store checks its parent directory and any existing credential file before use, rejects reparse points such as junctions and symlinks, and rejects access grants to other principals. Replacement writes a private temporary file in the same directory before replacing the checked destination. Some error messages still use the Unix labels 0700 or 0600. On Windows the checks use the ACL.

Windows host acceptance is still pending. Use the exact reviewed and checksum-verified binary for the offline checks below, then inspect its ACLs and synthetic credential replacement. Stop before real cookie import or hosted sign-in if a check fails. Keep the scratch directory on the current user's local drive and outside shared or synchronized folders.

## Install and offline checks

Download the matching Windows release asset when available. Compare its SHA256 to `SHA256SUMS` using `Get-FileHash`; use `Expand-Archive` only if the asset is a ZIP (a raw `.exe` needs no extraction). Substitute the real downloaded filename; do not copy the illustrative name literally.

```powershell
Get-FileHash "$HOME\Downloads\maker-windows.zip" -Algorithm SHA256
Expand-Archive "$HOME\Downloads\maker-windows.zip" -DestinationPath "$HOME\bin\maker-test"
# Place the extracted maker.exe in this directory if the archive nests it.
$env:Path = "$HOME\bin\maker-test;$env:Path"
Get-Command maker
maker --version
maker --help
maker auth --help
maker release submit --help
```

The PATH change applies only to this PowerShell process. For later sessions, add the executable's directory to your user PATH through Windows Environment Variables. From a source checkout with Go 1.25+, an alternative is `go build -trimpath -o maker.exe ./cmd/maker`, then `.\maker.exe --version`.

### Verify private storage offline

Use a fresh scratch path under `%LocalAppData%`. Let the CLI create it so the test exercises its directory ACL. The helper checks ACL metadata without reading file contents. It accepts inherited rules on an input file when all grants are limited to the trusted principals. For CLI-created paths, require protected ACLs as well.

```powershell
$MakerSid = ([System.Security.Principal.WindowsIdentity]::GetCurrent()).User.Value
$TrustedSids = @($MakerSid, 'S-1-5-18', 'S-1-5-32-544')
function Assert-MakerPrivatePath {
    param([string]$LiteralPath, [switch]$RequireProtected)

    $item = Get-Item -LiteralPath $LiteralPath -Force -ErrorAction Stop
    if ($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) {
        throw 'The storage path is a reparse point.'
    }
    $acl = Get-Acl -LiteralPath $LiteralPath -ErrorAction Stop
    if ($acl.GetOwner([System.Security.Principal.SecurityIdentifier]).Value -notin $TrustedSids) {
        throw 'The storage path has an unexpected owner.'
    }
    if ($RequireProtected -and -not $acl.AreAccessRulesProtected) {
        throw 'The CLI-created ACL is not protected from inheritance.'
    }
    $allows = @($acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]) |
        Where-Object { $_.AccessControlType -eq 'Allow' })
    if ($allows.Count -eq 0 -or @($allows | Where-Object {
        $_.IdentityReference.Value -notin $TrustedSids
    }).Count -gt 0) {
        throw 'The storage path lacks a restricted allow list.'
    }
}

$SmokeRoot = Join-Path $env:LOCALAPPDATA ('MakerSmoke-' + [guid]::NewGuid().ToString('N'))
$SmokeConfig = Join-Path $SmokeRoot 'config.json'
$SyntheticStore = Join-Path $SmokeRoot 'synthetic\session.json'
maker --config "$SmokeConfig" config init --org 00000000-0000-4000-8000-000000000001
if ($LASTEXITCODE -ne 0) { throw 'Synthetic config creation failed.' }
Assert-MakerPrivatePath -LiteralPath $SmokeRoot -RequireProtected
Assert-MakerPrivatePath -LiteralPath $SmokeConfig -RequireProtected

foreach ($label in @('initial', 'replacement')) {
    "Cookie: __Host-kc_at=synthetic-smoke-$label" |
        maker --config "$SmokeConfig" --credentials-file "$SyntheticStore" auth import --stdin
    if ($LASTEXITCODE -ne 0) { throw "Synthetic import failed during $label." }
    Assert-MakerPrivatePath -LiteralPath (Split-Path -Parent $SyntheticStore) -RequireProtected
    Assert-MakerPrivatePath -LiteralPath $SyntheticStore -RequireProtected
}
$Doctor = maker --config "$SmokeConfig" --credentials-file "$SyntheticStore" doctor | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or $Doctor.credential_store -ne 'explicit private file' -or
    $Doctor.credentials -ne 'available') {
    throw 'The synthetic store could not be read.'
}
maker --config "$SmokeConfig" --credentials-file "$SyntheticStore" auth forget
if ($LASTEXITCODE -ne 0 -or (Test-Path -LiteralPath $SyntheticStore)) {
    throw 'Synthetic credential removal failed.'
}
```

`doctor` runs locally. Use the synthetic cookie values only for this offline check.

Use only synthetic artifacts for the remaining offline commands:

```powershell
maker inspect C:\maker-test\com.example.plugin.streamDeckPlugin
maker listing validate --file C:\maker-test\listing.json
```

Inspection needs a valid synthetic package; listing validation needs a complete synthetic configuration and local media matching the [listing requirements](listing-protocol.md). These commands do not upload. An intentionally invalid package/configuration should return a nonzero `$LASTEXITCODE`, not a success message. Without valid fixtures, record those positive checks as skipped. From a checkout, run `go vet ./...` and `go test -race ./...` with a supported Windows race-detector toolchain; the tests use synthetic local servers, not real mutation endpoints.

## Real read-only checks (only after the storage gate passes)

After the offline storage checks pass, sign into Maker in your regular browser. Save a Cookie header or cookie JSON as `maker-cookies.json` directly in the checked scratch directory. Keep its contents out of PowerShell commands, logs and transcripts. Set the organization ID privately.

```powershell
$CookieFile = Join-Path $SmokeRoot 'maker-cookies.json'
$Store = Join-Path $SmokeRoot 'credentials\session.json'
Assert-MakerPrivatePath -LiteralPath $CookieFile
maker --config "$SmokeConfig" --credentials-file "$Store" auth import --cookie-file "$CookieFile"
if ($LASTEXITCODE -ne 0) { throw 'Maker cookie import failed.' }
Assert-MakerPrivatePath -LiteralPath (Split-Path -Parent $Store) -RequireProtected
Assert-MakerPrivatePath -LiteralPath $Store -RequireProtected
Remove-Item -LiteralPath $CookieFile
maker --config "$SmokeConfig" config init --org YOUR_ORGANIZATION_ID
maker --config "$SmokeConfig" --credentials-file "$Store" auth check
maker --config "$SmokeConfig" --credentials-file "$Store" products
maker --config "$SmokeConfig" --credentials-file "$Store" analytics -d 7
```

Run the commands individually and check `$LASTEXITCODE` immediately after each. Stop on failure. Keep account results private. Read-only business queries may renew and persist session cookies. The credential lock uses the same path as `$Store` with `.lock` appended.

The default config path is `%AppData%\Elgato Makers CLI\config.json`. The sibling `releases\` directory contains workflow state and `mcp\` contains hosted credentials and mappings. `--config` keeps these directories under the scratch location in this test. `--state-dir` can move workflow state. Keep using the same explicit `--credentials-file` for Maker-authenticated commands on Windows.

### Renewal and local removal

Import must include the scoped `__Secure-kc_rt` cookie for renewal. Copy from a successful `/api/auth/session` request or scoped cookie JSON if ordinary page cookies omit it.

```powershell
maker --config "$SmokeConfig" --credentials-file "$Store" auth refresh
maker --config "$SmokeConfig" --credentials-file "$Store" auth check
```

Then allow normal access expiry and rerun `auth check` to check automatic renewal. Do not edit token timestamps or report token values. If the server session is expired/revoked, sign into Maker again and reimport privately; indefinite renewal is not promised. When finished, `maker --config "$SmokeConfig" --credentials-file "$Store" auth forget` removes the selected local credentials, not the browser/server session.

## Optional hosted login (also gated on secure state)

```powershell
maker --config "$SmokeConfig" mcp login --server https://makers.sentinelite.com --no-browser
# Open the authorization URL manually and complete Google/hosted consent.
if ($LASTEXITCODE -ne 0) { throw 'Hosted sign-in failed.' }
$HostedState = Join-Path $SmokeRoot 'mcp'
Assert-MakerPrivatePath -LiteralPath $HostedState -RequireProtected
$HostedFiles = @(Get-ChildItem -LiteralPath $HostedState -Filter 'mcp-*.json' -File)
if ($HostedFiles.Count -ne 1) { throw 'Expected one hosted credential file in scratch state.' }
Assert-MakerPrivatePath -LiteralPath $HostedFiles[0].FullName -RequireProtected
maker --config "$SmokeConfig" mcp status --server https://makers.sentinelite.com
maker --config "$SmokeConfig" mcp logout --server https://makers.sentinelite.com
```

This requires a reachable hosted deployment and a local callback allowed by Windows/browser policy. Do not disable security controls to make it work. Hosted OAuth is separate from Maker cookies; login does not connect a Maker account. Do not run `mcp connect` / `cloud connect` during this smoke test: those explicitly transfer a Maker session. Do not request `--write`. Logout revokes this CLI's hosted grant, not a remote Maker connection. Failed refresh guards require logout/fresh login, never manual guard deletion or refresh-token retry.

Report binary version, Windows version/architecture, command names, pass/fail and sanitized error categories to the owner. Do not attach raw HARs, cookies, tokens, signed URLs, state files or personal account output. Private security reports: **support@sentinelite.com**.
