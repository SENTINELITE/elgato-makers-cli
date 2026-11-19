# Contributor and agent guide

`maker` is a Go CLI (project name Elgato Makers CLI) for reading Maker products and preparing Stream Deck releases over plain HTTP. These rules apply to people and coding agents alike.

## Safety rules

- No browser automation.
- No production uploads, server-action POSTs or submissions during development or testing. Live checks may only read.
- All mutation tests use localhost `httptest` endpoints and synthetic credentials.
- Never put raw HAR contents, cookies, tokens, signed S3 query strings or personal account payloads in source, tools, logs, fixtures or docs. Private capture inputs may be inspected in memory and reduced to reviewed synthetic contract fixtures. Keep credentials and raw captures outside the repository.
- A live release needs the person's explicit instruction.

## Contract rules

- Production submission is a separate, explicit `release submit` command: two default-No interactive confirmations, or `--yes`/`-y` on that command only.
- Upload never submits.
- An ambiguous submission result blocks blind retry and requires status reconciliation.
- Do not change endpoints, payloads, multipart ordering, sequential media handling or uncertainty guards unless a reviewed contract capture requires it.

## Adapter and build rules

- `pkg/connect` and `pkg/workflows/v1` are the public adapter surface. Follow `docs/ADAPTER.md`; the API pin test fails on any unversioned change.
- This module must build from a clean public checkout. Never import a private or host-service repository.
- Match the surrounding style and comment density. Run `go vet ./...` and `go test -race ./...` before submitting a change.
