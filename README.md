# issue-snooze

`issue-snooze` is a Go daemon that watches GitHub issue and pull-request comments for `@snooze <date/time/relative>` and posts a reminder when the snooze expires.

The primary deployment model is a **GitHub App**. A legacy PAT mode remains available for development and compatibility.

## Register a GitHub App

1. Open <https://github.com/settings/apps/new>.
2. Choose an App name and homepage URL.
3. Set the webhook URL to the public HTTPS endpoint for this daemon, for example `https://bot.example.com/webhook`.
4. Generate a strong webhook secret and keep it outside the repository.
5. Grant the minimum permissions used by the daemon:
   - **Issues:** Read & write.
   - **Pull requests:** Read & write.
6. Subscribe to:
   - **Issue comment**
   - **Installation**
   - **Installation repositories**
   - **Repository**
7. Create the App, record its **App ID**, and generate a private key.
8. Install the App on the repositories it should manage. Selected-repository installations are supported.

The lifecycle subscriptions are used to clean up removed installations/repositories and reconcile repository/account renames and transfers without crossing installation boundaries.

## Configuration

| Variable | Description |
|---|---|
| `GITHUB_APP_ID` | Positive GitHub App ID. |
| `GITHUB_APP_PRIVATE_KEY_FILE` | Path to the App private-key PEM file. |
| `WEBHOOK_SECRET_FILE` | Path to a file containing the webhook secret. Preferred for containers. |
| `WEBHOOK_SECRET` | Webhook secret value when a secret file is not used. |
| `BOT_COMMAND` | Command prefix. Defaults to `@snooze`. |
| `DATABASE_FILE` | SQLite database path. Defaults to `snooze.db`. |
| `PORT` | HTTP port. Defaults to `8080`. |
| `GITHUB_TOKEN_FILE` | Legacy PAT file path. Used only when App mode is not configured. |
| `GITHUB_TOKEN` | Legacy PAT value. Used only when App mode is not configured. |

If any App configuration is supplied, App mode must be complete: App ID, private-key file, and webhook secret are all required. Supplying both complete App credentials and a PAT selects App mode; the PAT is not used as an App-auth fallback.

Secrets, App JWTs, and installation access tokens are not persisted in SQLite. Installation transports are cached in memory and recreated after restart or relevant installation lifecycle changes.

## Run natively

```bash
go install github.com/arran4/issue-snooze/cmd/issue-snooze@latest
export GITHUB_APP_ID=123456
export GITHUB_APP_PRIVATE_KEY_FILE=/path/to/app-private-key.pem
export WEBHOOK_SECRET_FILE=/path/to/webhook-secret
export DATABASE_FILE=/var/lib/issue-snooze/snooze.db
issue-snooze run
```

SQLite uses CGO through `github.com/mattn/go-sqlite3`, so native builds require a working C compiler and SQLite-compatible build environment.

## Container deployment

Release automation is configured to publish release images to `ghcr.io/arran4/issue-snooze` when a release is run. Pull-request CI does **not** publish images. Verify the desired release/package exists before depending on a prebuilt tag; do not assume `latest` exists for an unreleased revision.

Example Compose configuration:

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

The container entry point runs `issue-snooze run`. Keep `/data` writable and persistent.

## Webhook verification and delivery handling

App mode requires a webhook secret. Invalid or missing signatures are rejected with HTTP 400. A snooze-producing webhook also requires `X-GitHub-Delivery`; the delivery ID is recorded transactionally with the snooze so a redelivery does not create a second reminder.

To inspect webhook failures or test redelivery, open the GitHub App's settings and inspect its recent webhook deliveries. Redeliver the same delivery and verify the daemon returns success without creating another snooze. Logs report duplicate delivery IDs as ignored.

Edited/deleted issue comments are ignored; only newly created comments are candidates for `@snooze`. Bot-authored comments are ignored to avoid feedback loops.

## Time zones

For a snooze command, the daemon looks up the sender's GitHub profile location and attempts to load it as an IANA time-zone name.

- A valid IANA location is used for date parsing.
- A missing location, an invalid/unloadable location, or a user lookup returning HTTP 404 falls back to UTC.
- Authentication/permission failures (401/403), rate limiting (429), network errors, and server failures are treated as processing errors. The webhook returns 5xx and the snooze is not persisted with a guessed time.

If predictable local-time parsing matters, use an IANA value such as `Australia/Melbourne` in the GitHub profile location field or include an unambiguous time in the command.

## Reminder delivery and retries

Pending reminders are stored in SQLite. The background checker runs on its configured interval (currently one minute); it does not perform an extra immediate pass merely because the daemon restarted.

Workers claim reminders with an owner token and a renewable lease. Failed GitHub posts release the current owner's claim so the reminder can be retried. Installation suspension or authorization failure preserves the reminder and clears cached installation authentication so a later retry can obtain fresh credentials.

Delivery is **at least once / best effort**, not exactly once. Posting a GitHub comment and committing local completion in SQLite cannot be one atomic transaction. If GitHub accepts a comment but the daemon crashes or cannot record local completion, a later retry can post a duplicate reminder.

## Lifecycle behavior

- **Installation deleted:** delete reminders for that installation and evict cached installation auth.
- **Installation suspended:** preserve reminders, evict cached installation auth, and allow later retry.
- **Installation unsuspended / new permissions accepted:** evict cached auth so the next operation obtains fresh installation credentials.
- **Repositories removed from an installation:** delete reminders only for that repository within that installation.
- **Repository rename/transfer:** update the stored owner/name from the old identity in the webhook `changes` payload to the new identity, scoped to the installation.
- **Installation target/account rename:** update stored repository owners only within that installation.

If a rename/transfer event does not contain the old identity needed to reconcile safely, the handler returns an error rather than silently leaving known stale mappings.

## Persistence smoke test

1. Start the daemon with a persistent `DATABASE_FILE`.
2. Create a snooze several minutes into the future and confirm it is present in SQLite/logs.
3. Stop the daemon **before** the due time.
4. Restart it with the same database file.
5. Leave it running through the due time plus one checker interval.
6. Confirm exactly one normal reminder is posted in the non-failure case and the row is removed afterward.

Also redeliver the original webhook from GitHub and confirm no second snooze row is created for the same delivery ID.

## Backups and upgrades

Back up the SQLite database before upgrades. Schema migrations are applied at startup. A migration failure aborts initialization rather than silently continuing with a partially upgraded schema.

## Legacy PAT mode

If App mode is not configured, the daemon can use a PAT:

1. Create a fine-grained PAT with the minimum repository access needed to read issue/PR comments and post replies.
2. Configure a repository webhook for `Issue comment` events and point it at `/webhook`.
3. Set `GITHUB_TOKEN` or `GITHUB_TOKEN_FILE` and the webhook secret.

PAT mode is a compatibility/development path; it is not used as a fallback when App mode is configured.
