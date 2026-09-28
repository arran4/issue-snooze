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
   *(Note: Subscribing to these lifecycle events is required so the daemon knows when it has been uninstalled, suspended, removed, transferred, or renamed allowing it to clean up old reminders safely without leaving orphans).*
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
All pending snoozes are durably stored in an SQLite database (default: `snooze.db`). You can safely restart or upgrade the daemon at any time. When the application boots up, the background checker will immediately process any snoozes whose target times have passed. We strongly recommend routinely backing up your `snooze.db` file.

### Troubleshooting
Check the container logs or standard output if snoozes are not triggering. Common problems:
- `Invalid GitHub App configuration`: Ensure `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY_FILE`, and `WEBHOOK_SECRET` are all populated correctly.
- `UNIQUE constraint failed`: This is an expected log when GitHub re-delivers an identical webhook payload; the bot correctly ignores these.

### Smoke Tests
Once installed, create a test issue and a test Pull Request on a repository where the App is active.
- Leave a comment: `@snooze in 1 minute`
- Verify the daemon log acknowledges the snooze command.
- Wait 1 minute.
- Verify the bot replies tagging your username.
- Restart the daemon and observe SQLite persistence handling retry.

### Environment Variables

| Variable | Description |
|---|---|
| `GITHUB_APP_ID` | The App ID of your GitHub App. |
| `GITHUB_APP_PRIVATE_KEY_FILE` | Path to the `.pem` file containing your App's private key. |
| `WEBHOOK_SECRET` | The webhook secret you configured. (Alternatively: `WEBHOOK_SECRET_FILE`). |
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
    environment:
      - GITHUB_APP_ID=123456
      - GITHUB_APP_PRIVATE_KEY_FILE=/run/secrets/github_app_private_key
      - WEBHOOK_SECRET=your_webhook_secret
      - DATABASE_FILE=/data/snooze.db
    restart: unless-stopped
```

Place your generated `.pem` file in the same directory as `app-private-key.pem`.
Run the service in the background:
```bash
docker-compose up -d
```
*(Note: As of writing, there may be no official pre-built image pushed to ghcr.io/arran4/issue-snooze. You can optionally build the container locally by uncommenting the `build` directives.)*

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
