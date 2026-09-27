# issue-snooze

A Go-based GitHub bot daemon that monitors registered repositories for `@snooze <date/time/relative>` comments.

This project operates primarily as a **GitHub App**. It supports multiple installations and repositories without crossing authorization boundaries.

## Registration as a GitHub App

To use `issue-snooze`, you must first register it as a GitHub App on your account or organization.

1. Navigate to **Settings > Developer Settings > GitHub Apps**.
2. Click **New GitHub App**.
3. **App name:** Give your app a unique name (e.g., `My Issue Snoozer`).
4. **Homepage URL:** The URL of your repository or website.
5. **Webhook URL:** The HTTPS endpoint where this daemon will be hosted (e.g., `https://bot.example.com/webhook`).
6. **Webhook Secret:** Generate a strong random string and provide it here. You will need this for the `WEBHOOK_SECRET` configuration.
7. **Permissions:**
   - **Issues:** `Read & write` (to read comments and post replies).
   - **Pull Requests:** `Read & write` (to interact with PR comments).
8. **Subscribe to events:**
   - `Issue comment`
9. Click **Create GitHub App**.
10. Once created, note your **App ID** near the top of the general settings page.
11. Scroll down and click **Generate a private key**. A `.pem` file will download to your computer.
12. Navigate to **Install App** in the left sidebar to install the App on your desired repositories.

## Deployment

Deploying the bot requires the credentials generated during the registration process.

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
  snooze-bot:
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

### Go Install

To run the daemon natively without Docker:

```bash
go install github.com/arran4/issue-snooze@latest
export GITHUB_APP_ID=123456
export GITHUB_APP_PRIVATE_KEY_FILE=/path/to/key.pem
export WEBHOOK_SECRET=mysecret
snooze-bot run
```

## Legacy PAT Deployment

If you are not using a GitHub App, you can configure the bot with a Personal Access Token (PAT). Generate a fine-grained token with Issues/PR Read & Write access and configure your repository webhook manually.
Then provide the `GITHUB_TOKEN` or `GITHUB_TOKEN_FILE` environment variable instead of the App ID and Private Key.
