# issue-snooze

A Go-based GitHub bot daemon that monitors registered repositories for `@snooze <date/time/relative>` comments.

## Installation / Run

### Docker
```bash
docker pull ghcr.io/issue-snooze/snooze-bot:latest
docker run -d \
  -e GITHUB_TOKEN=your_github_token \
  -e WEBHOOK_SECRET=your_webhook_secret \
  -p 8080:8080 \
  -v /path/to/data:/data \
  ghcr.io/issue-snooze/snooze-bot:latest
```

Or pass secrets via files using `-e GITHUB_TOKEN_FILE=/run/secrets/token`

### Go Install
```bash
go install github.com/issue-snooze/snooze-bot@latest
```