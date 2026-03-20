# Flomation Launch

Launch service for Flomation Automate — receives external events and triggers workflow execution.

## Overview

Flomation Launch is a Go HTTP service that acts as the ingress layer for the Flomation automation platform. It accepts incoming webhooks, QR code scans, form submissions, image-load tracking pixels, and other trigger types, then forwards them to the Automate service for workflow execution. Triggers are persisted in PostgreSQL and identified by UUID.

## Prerequisites

- Go 1.26.1+
- PostgreSQL
- A running instance of Flomation Automate (for trigger execution)
- (Optional) Google OAuth2 credentials for Google Drive integration

## Installation

```bash
# Clone the repository
git clone <repo-url> && cd launch

# Install dependencies
go mod download

# Copy and edit the configuration file
cp config.json.example config.json
# Edit config.json with your database and service settings

# Run the service
go run flomation.app/automate/launch/cmd
```

## Configuration

The service reads a `config.json` file from the working directory on startup.

### Database

| Field | Type | Description | Required |
|-------|------|-------------|----------|
| `hostname` | string | PostgreSQL host | Yes |
| `port` | int | PostgreSQL port | Yes |
| `username` | string | Database user | Yes |
| `password` | string | Database password | Yes |
| `database` | string | Database name | Yes |
| `encryption_key` | string | Key for data encryption | Yes |
| `max_idle_connections` | int | Max idle DB connections | No |
| `max_open_connections` | int | Max open DB connections | No |
| `ssl_mode_override` | string | Override PostgreSQL SSL mode | No |

### HTTP

| Field | Type | Description | Required | Default |
|-------|------|-------------|----------|---------|
| `address` | string | Bind address | Yes | — |
| `port` | int | Listen port | Yes | — |

### Automate Service

| Field | Type | Description | Required |
|-------|------|-------------|----------|
| `url` | string | Base URL of the Automate service | Yes |
| `key` | string | API key for Automate service | No |

### Google (Optional)

| Field | Type | Description | Required |
|-------|------|-------------|----------|
| `client_id` | string | Google OAuth2 client ID | No |
| `client_secret` | string | Google OAuth2 client secret | No |
| `credentials_file` | string | Path to Google credentials file | No |

Example `config.json`:

```json
{
  "database": {
    "hostname": "localhost",
    "port": 5432,
    "username": "flomation",
    "password": "secret",
    "database": "launch",
    "encryption_key": "your-encryption-key",
    "max_idle_connections": 5,
    "max_open_connections": 10
  },
  "http": {
    "address": "0.0.0.0",
    "port": 8080
  },
  "automate": {
    "url": "http://localhost:9000",
    "key": "your-api-key"
  }
}
```

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/version` | Returns service version, build date, and git hash |
| `GET/POST` | `/webhook/:id` | Receives webhook payloads and fires the associated trigger |
| `GET` | `/qr/:id` | Fires a QR code trigger |
| `GET` | `/form/:id` | Renders a dynamic form for the trigger |
| `POST` | `/form/:id` | Submits form data and fires the trigger |
| `GET` | `/image/:id` | Returns a 1x1 tracking pixel and fires the image trigger |
| `POST` | `/trigger/:id` | Creates or updates a trigger |
| `GET` | `/google/credential` | Google OAuth2 callback handler |

All trigger IDs are UUIDs. The service validates the trigger type matches the endpoint before firing.

## Trigger Types

| Type | Constant | Description |
|------|----------|-------------|
| `manual` | `TriggerTypeManual` | Manually invoked |
| `schedule` | `TriggerTypeScheduled` | Time-based schedule |
| `qr` | `TriggerTypeQR` | QR code scan |
| `image` | `TriggerTypeImage` | Invisible pixel tracking |
| `email` | `TriggerTypeEmail` | Email-based trigger |
| `telegram` | `TriggerTypeTelegram` | Telegram message trigger |
| `form` | `TriggerTypeForm` | HTML form submission |
| `webhook` | `TriggerTypeWebhook` | HTTP webhook |
| `git-poll` | `TriggerTypeGitPoll` | Git repository polling |

## Usage

**Fire a webhook trigger:**

```bash
curl -X POST http://localhost:8080/webhook/550e8400-e29b-41d4-a716-446655440000 \
  -H "Content-Type: application/json" \
  -d '{"event": "deployment", "status": "success"}'
```

**Fire a QR trigger:**

```bash
curl http://localhost:8080/qr/550e8400-e29b-41d4-a716-446655440000
```

**Embed a tracking pixel in HTML:**

```html
<img src="http://localhost:8080/image/550e8400-e29b-41d4-a716-446655440000" width="1" height="1" />
```

**Check service version:**

```bash
curl http://localhost:8080/version
```

## Development

```bash
# Run locally
go run flomation.app/automate/launch/cmd

# Run tests with coverage
make test

# Run linters (goimports, golangci-lint, vet, gosec, govulncheck)
make lint

# Build for all platforms (linux, darwin, windows × amd64/arm64/arm)
make build
```

Binaries are output to `dist/` and bundled into `build.zip`.

## Docker

Built on Alpine Linux. Runs as a non-root `flomation` user.

```bash
docker build --build-arg BINARY_FILE=dist/flomation-launch-amd64-linux-1.0.dev -t flomation-launch .
docker run -v $(pwd)/config.json:/config.json flomation-launch
```

## Project Structure

```
cmd/
  main.go                  Entry point
internal/
  config/                  Configuration loader (config.json)
  http/                    Gin HTTP server, routes, and handlers
  trigger/                 Trigger business logic and Automate API client
  persistence/             PostgreSQL access layer and migrations
  google/                  Google Drive OAuth2 integration
  git/poll/                Git repository polling (stub)
  version/                 Build version injection
  assets/                  Embedded static files and HTML templates
types.go                   Trigger type constants and domain model
Makefile                   Build, lint, and test targets
Dockerfile                 Container image definition
```

## Licence

MIT — see [LICENCE.md](LICENCE.md).
