---
title: slack-cli-expert
type: agent
updated: 2026-04-12
sources:
  - .claude/agents/slack-cli-expert.md
related:
  - "[[infra-docker-expert]]"
  - "[[mgr-gitnerd]]"
---

# slack-cli-expert

Slack CLI developer for Slack app management, deployment, triggers, datastore CRUD, and workspace automation using the official Slack Platform.

## Overview

`slack-cli-expert` manages the full Slack app lifecycle via the official Slack CLI tool. The workflow always begins with `slack doctor` (system diagnostics) and `slack auth list` (workspace authorization) before any operations — ensuring the environment is correctly configured before attempting deployment.

The agent references `guides/slack-cli/` for command details and validates manifests before deployment.

## Key Details

- **Model**: sonnet | **Effort**: medium | **Domain**: universal
- **References**: docs.slack.dev/tools/slack-cli/, `guides/slack-cli/`

### Capabilities

1. App lifecycle: create, run (local dev), deploy, delete
2. Authentication: login, logout, workspace auth management
3. Triggers: create, update, delete event triggers via trigger-def files
4. Datastore: CRUD operations, bulk put/delete
5. Environment variables: add, remove, list for deployed apps
6. Collaborators: add/remove/list app collaborators
7. Diagnostics: `slack doctor`, `slack manifest validate`

### Required Pre-flight

```bash
slack doctor          # system check
slack auth list       # workspace authorization
slack manifest validate  # before any deployment
```

## Relationships

- **Deployment infrastructure**: [[infra-docker-expert]] for containerized Slack app backends
- **CI/CD**: [[mgr-gitnerd]] for version-controlled Slack app deployment workflows

## Sources

- `.claude/agents/slack-cli-expert.md`
