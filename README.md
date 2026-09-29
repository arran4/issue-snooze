# issue-snooze

A Go-based GitHub bot daemon that monitors registered repositories for `@snooze <date/time/relative>` comments.

This project operates primarily as a **GitHub App**. It supports multiple installations and repositories without crossing authorization boundaries.

## Registration as a GitHub App

To use `issue-snooze`, you must first register it as a GitHub App on your account or organization.

1. Navigate directly to the GitHub App registration page: [https://github.com/settings/apps/new](https://github.com/settings/apps/new).
2. **App name:** Give your app a unique name (e.g., `My Issue Snoozer`).
3. **Homepage URL:** The URL of your repository or website.
4. **Webhook URL:** The HTTPS endpoint where this daemon will be hosted (e.g., `https://bot.example.com/webhook`).
5. **Webhook Secret:** Generate a strong random string and provide it here. You will need this for the `WEBHOOK_SECRET` configuration. See "Webhook Verification" below.
6. **Permissions:**
   - **Issues:** `Read & write` (to read comments and post replies).
   - **Pull Requests:** `Read & write` (to interact with PR comments).
7. **Subscribe to events:**
   - `Issue comment`
   - `Installation`
   - `Installation repositories`
   - `Repository`
   - `Installation target`
   *(These lifecycle events let the daemon evict stale installation credentials, remove reminders when repository access is removed, and reconcile repository/account renames and transfers without crossing installation boundaries.)*
8. Click **Create GitHub App**.
9. Once created, note your **App ID** near the top of the general settings page.
10. Scroll down and click **Generate a private key**. A `.pem` file will download to your computer. Store this securely.
11. Navigate to **Install App** in the left sidebar to install the App on your desired repositories.

## Deployment

Deploying the bot requires the credentials generated during the registration process.

## Operational Guidance

### Webhook Verification & Reverse Proxies
The daemon must validate the signatures of incoming GitHub Webhooks. Do not disable or omit the `WEBHOOK_SECRET` in App Mode. The bot uses standard GitHub payload signature validation.
Ensure you expose the bot behind an HTTPS reverse proxy (such as Nginx, Traefik, or Cloudflare Tunnels) mapping port `8080` (or your configured `PORT`) to the internet.

### Secret and Key Rotation
If you suspect your webhook secret or private key `.pem` file has been compromised:
1. Generate a new private key or webhook secret in your GitHub App settings.
2. Update the environment variables/secret files for your deployment.
3. Restart the `issue-snooze` container/daemon.

### Restarting, Upgrades, and Backups
Pending snoozes are stored in SQLite (default: `snooze.db`). Keep that file on persistent storage and back it up before upgrades. The background checker runs on its configured interval (the daemon currently starts it with a one-minute interval), so an overdue reminder may wait until the next check after restart rather than being processed immediately.

### Troubleshooting
Check the container logs or standard output if snoozes are not triggering. Common problems:
- `Invalid GitHub App configuration`: Ensure `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY_FILE`, and `WEBHOOK_SECRET` are all populated correctly.
- `Ignoring duplicate webhook delivery`: This is an expected log when GitHub re-delivers an identical webhook payload. Delivery ID tracking ensures comments are only processed once.

### Timezone and retry behaviour
For relative or timezone-less commands, the daemon looks up the GitHub user's public profile location and treats it as an IANA timezone name. Missing, invalid, or unavailable (404) locations fall back to UTC. Authentication/permission failures (401/403), rate limiting (429), network failures, and 5xx responses are treated as processing errors so GitHub can retry the webhook instead of silently storing a reminder at the wrong time.

Webhook delivery IDs are stored transactionally with new snoozes, so a redelivered webhook does not create a second reminder. Reminder delivery itself is best-effort/at-least-once: if GitHub accepts a reminder comment but the local completion/delete step fails, a later retry can create a duplicate comment because the remote post and SQLite update cannot be atomic.

### Smoke Tests
Once installed, test both issue and pull-request comments on a repository where the App is active.
1. Leave `@snooze in 2 minutes` and confirm the daemon logs the command.
2. Stop the daemon before the reminder becomes due.
3. Restart it using the same SQLite file.
4. Wait for the next checker interval and verify exactly one reminder reply is posted.
5. In the GitHub App's webhook delivery view, choose the original delivery and use **Redeliver**. Confirm the daemon reports the duplicate delivery and does not create a second snooze.

For PAT-mode repository webhooks, use **Repository Settings -> Webhooks -> Recent Deliveries -> Redeliver** instead.

### Environment Variables

| Variable | Description |
|---|---|
| `GITHUB_APP_ID` | The App ID of your GitHub App. |
| `GITHUB_APP_PRIVATE_KEY_FILE` | Path to the `.pem` file containing your App's private key. |
| `WEBHOOK_SECRET_FILE` | Path to the file containing your webhook secret (preferred over `WEBHOOK_SECRET`). |
| `WEBHOOK_SECRET` | Inline webhook secret. Ignored when `WEBHOOK_SECRET_FILE` is readable. |
| `GITHUB_TOKEN_FILE` | Legacy PAT file path. Used only when App mode is not configured. |
| `GITHUB_TOKEN` | Legacy PAT value. Used only when App mode is not configured. |
| `BOT_COMMAND` | Custom bot prefix (defaults to `@snooze`). |
| `DATABASE_FILE` | Path to the SQLite database (defaults to `snooze.db`). |
| `PORT` | Port to listen on (defaults to `8080`). |

*(Note: `GITHUB_TOKEN` is still supported for legacy Personal Access Token deployments, but a GitHub App deployment must not require it.)*

### Docker Compose

The recommended way to deploy is using Docker Compose. Create a `docker-compose.yml` file:

```yaml
version: '3.8'

services:
  issue-snooze:
    # If a pre-built image is not published, build it locally with:
    # build:
    #   context: .
    #   dockerfile: Dockerfile.goreleaser
    image: ghcr.io/arran4/issue-snooze:latest
    ports:
      - "8080:8080"
    volumes:
      - ./data:/data
      - ./app-private-key.pem:/run/secrets/github_app_private_key:ro
      - ./webhook-secret.txt:/run/secrets/webhook_secret:ro
    environment:
      - GITHUB_APP_ID=123456
      - GITHUB_APP_PRIVATE_KEY_FILE=/run/secrets/github_app_private_key
      - WEBHOOK_SECRET_FILE=/run/secrets/webhook_secret
      - DATABASE_FILE=/data/snooze.db
    restart: unless-stopped
```

Place your generated `.pem` file at `app-private-key.pem` and put the webhook secret text in `webhook-secret.txt`.
Run the service in the background:
```bash
docker-compose up -d
```
The release workflow is responsible for publishing `ghcr.io/arran4/issue-snooze` tags. Do not assume `latest` exists before a release has successfully published it; uncomment the local `build` directives when deploying from source.

### Go Install

To run the daemon natively without Docker:

```bash
go install github.com/arran4/issue-snooze/cmd/issue-snooze@latest
export GITHUB_APP_ID=123456
export GITHUB_APP_PRIVATE_KEY_FILE=/path/to/key.pem
export WEBHOOK_SECRET=mysecret
issue-snooze run
```

## Legacy PAT Deployment

If you are not using a GitHub App, you can configure the bot with a Personal Access Token (PAT).
1. Generate a fine-grained PAT granting **Issues/Pull Requests (Read/Write)** access for the required repositories.
2. Manually register a repository webhook on each repository you wish to monitor, subscribing to `Issue comment` events, pointing to your bot's HTTPS endpoint.
3. Start the daemon using `GITHUB_TOKEN` or `GITHUB_TOKEN_FILE` instead of the App configuration variables.
