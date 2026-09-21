# Hermes calendar

`calendar_client.py` is a Python standard-library Google Calendar client; `calendar/SKILL.md` makes it discoverable in Hermes. It runs in both agent and WebUI without a browser or MCP changes.

Install the script as `$HERMES_HOME/calendar_client.py` and skill as `$HERMES_HOME/skills/calendar/SKILL.md` in the shared persistent Hermes home volume. Store a Google Desktop OAuth client as `calendar_client_secret.json` (0600). Run `auth-url`, approve events and calendar-list read scopes, then exchange the full redirect URL using `auth-code --redirect-file`. Token and pending PKCE verifier files are atomically written with 0600 permissions. Do not commit credentials, tokens, redirect URLs or source message contents.

Containers must use their own HERMES_HOME. Shared-volume installations must run both Hermes processes with the same numeric UID so atomic 0600 token refresh remains private and accessible. Different runtime UIDs require separate private credential homes. Verify access as each actual runtime user. A skill refresh/new conversation may be needed to refresh the catalog. Existing conversations can load the skill explicitly.

Manual writes use ETag conditional PATCH and caller-supplied event IDs. No attendee emails are sent. Recurrence-series updates, all-day conversion and deletes are deliberately unsupported. Run `python3 -m unittest discover -s deploy/hermes -p 'test_*.py'`.

Automatic calendar registration is provided separately by the Go collector (`CALENDAR_AUTOMATION_ENABLED=true`). See `../ubuntu1-stack/README.md` for deployment and cutoff semantics.
