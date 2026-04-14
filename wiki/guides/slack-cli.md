---
title: "Guide: Slack CLI"
type: guide
updated: 2026-04-12
sources:
  - guides/slack-cli/README.md
related:
  - "[[slack-cli-expert]]"
---

# Guide: Slack CLI

Reference documentation for the Slack CLI — app lifecycle management, trigger operations, datastore CRUD, and environment variable management.

## Overview

The Slack CLI guide provides reference documentation for `slack-cli-expert`. It covers the official Slack CLI commands for building, deploying, and managing Slack apps on the Slack Platform, including authentication workflows and manifest validation.

## Key Topics

- **App Lifecycle**: `slack create`, `slack run` (local dev), `slack deploy`, `slack delete`
- **Authentication**: `slack login`, `slack logout`, `slack auth list` — workspace authorization
- **Triggers**: Create, update, delete event triggers via `--trigger-def` files
- **Datastore**: `slack datastore put/get/query/bulk-put/bulk-delete` operations
- **Environment Variables**: `slack env add/remove/list` for deployed app configuration
- **Diagnostics**: `slack doctor` (system check), `slack manifest validate` (pre-deployment validation)
- **Collaborators**: `slack collaborators add/remove/list` for app access management

## Relationships

- **Agent**: [[slack-cli-expert]] primary consumer

## Sources

- `guides/slack-cli/README.md`
