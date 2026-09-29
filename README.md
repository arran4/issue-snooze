# issue-snooze

`issue-snooze` is a Go daemon that watches GitHub issue and pull-request comments for `@snooze <date/time/relative>` and posts a reminder when the snooze expires.

GitHub App authentication is the primary deployment mode. A legacy Personal Access Token (PAT) mode is retained for development and compatibility, but App mode never falls back to a PAT when App credentials are configured.

## GitHub App registration

Create the App at <https://github.com/settings/apps/new>.

1. Choose an App name and homepage URL.
2. Set the webhook URL to the public HTTPS endpoint for this daemon, ending in `/webhook`.
3. Generate a strong webhook secret and keep it out of source control.
4. Grant **Issues: Read & write** and **Pull requests: Read & write**.
5. Subscribe to **Issue comment**, **Installation**, **Installation repositories**, **Repository**, and **Installation target**.
6. Create the App, note its App ID, generate a private key, and install it on the desired repositories. Selected-repository installations are supported.

The daemon creates an installation-scoped GitHub client from each webhook installation ID. Installation transports and short-lived access tokens are cached only in memory; tokens/JWTs are not persisted in SQLite. Restarting reconstructs authentication from App credentials and the installation IDs stored with pending reminders.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `GITHUB_APP_ID` | unset | Numeric GitHub App ID. |
| `GITHUB_APP_PRIVATE_KEY_FILE` | unset | Path to the App private-key PEM file. |
| `WEBHOOK_SECRET_FILE` | unset | Preferred path to a file containing the webhook secret. |
| `WEBHOOK_SECRET` | unset | Webhook secret value when a file is not used. |
| `GITHUB_TOKEN_FILE` | unset | Legacy PAT file; only used when App mode is not configured. |
| `GITHUB_TOKEN` | unset | Legacy PAT value; only used when App mode is not configured. |
| `BOT_COMMAND` | `@snooze` | Comment prefix that starts a snooze command. |
| `PORT` | `8080` | HTTP listen port. |
| `DATABASE_FILE` | `snooze.db` | SQLite database path. |

App mode requires `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY_FILE`, and a webhook secret. Partial App configuration is rejected. If both App and PAT credentials are supplied, App mode is used exclusively.

## Timezones

For a snooze command, the daemon looks up the comment author's GitHub profile location and treats it as an IANA timezone name when possible.

- A valid IANA timezone such as `Australia/Melbourne` is used directly.
- A missing, invalid, or unavailable (`404`) location falls back to UTC.
- `401`, `403`, `429`, server failures, and network failures do not silently fall back to UTC; the webhook returns a server error so GitHub can retry rather than persisting the wrong time.
- A date without an explicit time defaults to 09:00 in the resolved timezone.

## Webhook and lifecycle behavior

App-mode webhook signatures are mandatory. Invalid or missing signatures are rejected with HTTP 400. Snooze-creating `issue_comment` deliveries also require `X-GitHub-Delivery`; the delivery ID is recorded transactionally with the snooze so redelivery cannot create a second row. Edited/deleted comments and bot-authored comments are ignored.

Lifecycle reconciliation is installation-scoped:

- **Installation deleted:** cached auth and all pending reminders for that installation are removed.
- **Installation suspended:** reminders remain queued and cached auth is discarded.
- **Installation unsuspended / new permissions accepted:** cached auth is discarded so the next request obtains fresh credentials.
- **Repository removed:** reminders for that repository and installation are removed.
- **Repository renamed/transferred:** stored owner/name values are updated from the webhook's old identity to the new identity for that installation.
- **Installation target renamed:** stored repository-owner values are updated for that installation.
- **Reminder delivery receives `401`/`403`:** the reminder stays queued and that installation's cached transport is evicted.

If a lifecycle payload lacks the old identity needed for safe reconciliation, the handler returns an error rather than silently leaving stale rows.

## Reminder delivery guarantee

Pending reminders use an SQLite claim owner plus a renewable lease. A worker that loses ownership or cannot renew cancels its in-flight GitHub request.

Delivery is **at least once / best effort**, not exactly once. Posting the GitHub comment and deleting the SQLite row cannot be atomic. If GitHub accepts the comment but local completion fails, the row remains and a later retry can create a duplicate reminder.

## Docker Compose

Tagged releases are configured to publish `ghcr.io/arran4/issue-snooze`. Do not assume a desired tag exists; check GitHub Packages and build locally when necessary.

```yaml
services:
  issue-snooze:
    image: ghcr.io/arran4/issue-snooze:latest
    ports:
      - "8080:8080"
    volumes:
      - ./data:/data
      - ./app-private-key.pem:/run/secrets/github_app_private_key:ro
      - ./webhook-secret:/run/secrets/webhook_secret:ro
    environment:
      GITHUB_APP_ID: "123456"
      GITHUB_APP_PRIVATE_KEY_FILE: /run/secrets/github_app_private_key
      WEBHOOK_SECRET_FILE: /run/secrets/webhook_secret
      DATABASE_FILE: /data/snooze.db
    restart: unless-stopped
```

## Native installation

CGO and SQLite support are required because the daemon uses `github.com/mattn/go-sqlite3`.

```bash
go install github.com/arran4/issue-snooze/cmd/issue-snooze@latest
export GITHUB_APP_ID=123456
export GITHUB_APP_PRIVATE_KEY_FILE=/path/to/app-private-key.pem
export WEBHOOK_SECRET_FILE=/path/to/webhook-secret
export DATABASE_FILE=/var/lib/issue-snooze/snooze.db
issue-snooze run
```

## Persistence, upgrades, backups, and smoke testing

Pending reminders and processed webhook delivery IDs are stored in SQLite. Schema migrations run at startup and fail startup if they cannot be applied. The background checker runs once immediately at startup and then once per minute. Back up `DATABASE_FILE` before upgrades using an SQLite-safe backup procedure.

A persistence smoke test should exercise an actual restart:

1. Start with a persistent database path.
2. Create a reminder due a few minutes in the future and verify it is stored.
3. Stop the daemon before it is due.
4. Restart with the same database file.
5. Verify the reminder is eventually attempted after its due time.

## Inspecting and redelivering webhooks

In the GitHub App settings, open **Advanced / Recent deliveries**. Select a delivery to inspect the request, response status, and body, and use **Redeliver** to retry it. A redelivery of an already-successful snooze command with the same delivery ID should return success without inserting another reminder.

## Legacy PAT mode

For local development or compatibility, create a fine-grained PAT with the repository access and minimum Issues/Pull requests permissions needed to read comments and post reminders, configure an `issue_comment` repository webhook, and start with `GITHUB_TOKEN` or `GITHUB_TOKEN_FILE`. PAT mode is separate from App mode and is not the recommended production deployment path.

## Troubleshooting

- **HTTP 400:** check webhook signature, payload, delivery ID, and snooze syntax.
- **HTTP 500:** inspect App authentication, permission/rate-limit, GitHub API, and SQLite errors, then redeliver when appropriate.
- **Repeated failure after permissions change:** check the App installation state and permissions; cached installation auth is evicted on authorization failures and relevant lifecycle events.
