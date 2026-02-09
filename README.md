# WhatsMiau

![logo-whatsmiau](logo.png)

WhatsMiau is a backend service for WhatsApp, built with Go. It uses the Whatsmeow library to connect to WhatsApp and provides an HTTP API to send and receive messages.

[Community Whatsapp (BR)](https://chat.whatsapp.com/FXMrTY552nOBFXU71Be8Zh)
## About The Project

This project provides a robust, scalable, and production-ready solution for integrating WhatsApp functionalities into your applications. It is extremely lightweight, consuming very little memory, making it ideal for resource-constrained environments.

It's designed to be compatible with the Evolution API, making it a flexible choice for developers familiar with that ecosystem.

## Features

- **Lightweight & Efficient:** Optimized for low memory consumption.
- **Production Ready:** Stable and reliable for use in production environments.
- **WhatsApp Integration:** Connects to WhatsApp to send and receive messages.
- **HTTP API:** Exposes an HTTP API for easy integration with other services.
- **Redis Support:** Uses Redis for session storage and caching.
- **SQLite Database:** Utilizes SQLite for persistent data storage.
- **Environment-based Configuration:** Easily configure the application using environment variables.
- **Structured Logging:** Implements structured logging with Zap for better monitoring and debugging.

## Getting Started

To get a local copy up and running follow these simple steps.

### Prerequisites

- Go 1.24 or higher
- Redis
- SQLite

### Installation

1. Clone the repo
   ```sh
   git clone https://github.com/verbeux-ai/whatsmiau.git
   ```
2. Install Go packages
   ```sh
   go mod tidy
   ```
3. Set up your environment variables by copying `.env.example` to `.env` and filling in the required values.
   ```sh
   cp .env.example .env
   ```
4. Run the application
   ```sh
   go run main.go
   ```

## Running with Docker (Router + Backend + Redis)

Run the full stack locally (same as production: Router + Backend + Redis):

1. **Bring up and test in one go:**
   ```sh
   make test-stack
   ```
   (runs `docker-compose up -d --build`, waits for services, then runs list/create/status against the router.)

2. **Or manually:** `docker-compose up -d --build`, then:
   ```sh
   curl -H "apikey: local-apikey" http://localhost:8080/v1/instance
   curl -X POST -H "apikey: local-apikey" -H "Content-Type: application/json" \
     -d '{"instanceName":"my-instance"}' http://localhost:8080/v1/instance
   ```

- Router: `http://localhost:8080` (envs in `docker-compose.yml`: `API_KEY=local-apikey`, `REDIS_URL=redis:6379`).
- Backend (direct): `http://localhost:8081` (optional; normally you use the router).
- Redis: `localhost:6379` (for local `go run`).

**View logs:** `docker-compose logs -f`  
**Stop:** `docker-compose down`

To run **without Docker** (backend only): install Redis, copy `env.local.example` to `.env`, then `go run main.go`. To run the router as well: `go run ./cmd/router/` in another terminal (use the same `.env` or set `REDIS_URL`, `API_KEY`).

## AWS deployment (single API with Router + ECS)

To run a single public API that routes requests to the correct backend using Redis (min 1 backend, scale when CPU/RAM > 80%, SQLite per backend, webhook per env, session loss cleanup and notification):

- See **[docs/deploy-aws.md](docs/deploy-aws.md)** for environment variables and step-by-step deploy.
- **Scripts:** `scripts/aws-create-stack.sh`, `scripts/aws-update-stack.sh`, `scripts/aws-delete-stack.sh`, `scripts/aws-push-prod.sh`. Copy `.env.production.example` to `.env.production` and set `API_KEY`, `WEBHOOK_URL`, and `ECR_REGISTRY` (or `AWS_ACCOUNT_ID`).
- **Makefile:** `make infra-create` (first time), `make push-prod` (build + push images + roll out new code), `make infra-update`, `make infra-destroy`.
- **Templates:** `cloudformation/template.yaml` (VPC, Redis, ECS router + backend, ALB, auto scaling).

## Docker Image

Official Docker images are available on Docker Hub.

- **Latest stable release:** `impedr029/whatsmiau:vX.Y.Z` [(see versions)](https://github.com/verbeux-ai/whatsmiau/tags)
- **Development version:** `impedr029/whatsmiau:develop`

You can pull the latest stable image with (example):
```sh
docker pull impedr029/whatsmiau:vX.Y.Z
```

Or the development image with:
```sh
docker pull impedr029/whatsmiau:develop
```

## Configuration

The application is configured using environment variables. The following variables are available:

| Variable | Description | Default |
| --- | --- | --- |
| `PORT` | The port the server will run on. | `8080` |
| `DEBUG_MODE` | Enable or disable debug mode. | `false` |
| `DEBUG_WHATSMEOW` | Enable or disable debug mode for Whatsmeow. | `false` |
| `REDIS_URL` | The URL of the Redis server. | `localhost:6379` |
| `REDIS_PASSWORD` | The password for the Redis server. | `` |
| `REDIS_TLS` | Enable or disable TLS for Redis. | `false` |
| `API_KEY` | The API key to protect the service. | `` |
| `WEBHOOK_URL` | (Optional) Single URL for all webhook events; overrides per-instance webhook when set. | `` |
| `BACKEND_PUBLIC_URL` | (Optional) URL where this instance is reachable; used for Redis routing when deployed behind the router. | `` |
| `DIALECT_DB` | The database dialect to use (`sqlite3` or `postgres`). | `sqlite3` |
| `DB_URL` | The database connection URL. For ECS use `file:/app/data/data.db?_foreign_keys=on`. | `file:data.db?_foreign_keys=on` |
| `GCS_ENABLED` | Enable or disable Google Cloud Storage. | `false` |
| `GCS_BUCKET` | The GCS bucket name. | `whatsmiau` |
| `GCS_URL` | The GCS URL. | `https://storage.googleapis.com` |
| `GCL_APP_NAME` | The GCL application name. | `whatsmiau-br-1` |
| `GCL_ENABLED` | Enable or disable Google Cloud Logging. | `false` |
| `GCL_PROJECT_ID` | The GCL project ID. | `` |
| `EMITTER_BUFFER_SIZE` | The emitter buffer size. | `2048` |
| `HANDLER_SEMAPH-ORE_SIZE` | The handler semaphore size. | `512` |
| `PROXY_ADDRESSES` | A comma-separated list of proxy addresses. Example: `SOCKS5://user:pass@host:port,HTTP://host:port` | `` |
| `PROXY_STRATEGY` | The strategy to use when selecting a proxy from the list (`RANDOM`). | `RANDOM` |
| `PROXY_NO_MEDIA` | If set to `true`, media will not be sent through the proxy. | `false` |

## Versioning

We use [SemVer](http://semver.org/) for versioning. For the versions available, see the [tags on this repository](https://github.com/verbeux-ai/whatsmiau/tags).

## Compatibility

This API is designed to be compatible with the Evolution API. This means that you can use clients and tools designed for the Evolution API with this project.

It exclusively supports webhooks in the Evolution API format, offering two distinct approaches for their implementation, providing flexibility for different use cases.

## Migration from Evolution API

WhatsMiau is designed to be a lightweight, drop-in replacement for the Evolution API. If you are running WhatsMiau on the same host and port as your previous Evolution API instance, migration is seamless.

Since WhatsMiau maintains compatibility with the Evolution API's routes, you only need to stop your Evolution API server and start the WhatsMiau server. No changes to your existing API calls are necessary.

### Example

For instance, if you were sending a text message using a `curl` command to an Evolution API server running on `localhost:8080`, the exact same command will work with WhatsMiau.

**Before (Evolution API):**
```bash
curl -X POST 'http://localhost:8080/message/sendText/my-instance' \
-H 'Content-Type: application/json' \
-H 'apikey: YOUR_API_KEY' \
-d ".{\"number\": \"1234567890\",\"textMessage\": {\"text\": \"Hello from Evolution API!\"}}"
```

**After (WhatsMiau):**

Simply point your application to the WhatsMiau server URL. The same request will be handled by WhatsMiau:
```bash
curl -X POST 'http://localhost:8080/v1/message/sendText/my-instance' \
-H 'Content-Type: application/json' \
-H 'apikey: YOUR_API_KEY' \
-d ".{\"number\": \"1234567890\",\"textMessage\": {\"text\": \"Hello from WhatsMiau!\"}}"
```

## API Routes

Same Pattern: https://www.postman.com/agenciadgcode/evolution-api/overview
| Method | Path                                      | Description                 |
|--------|-------------------------------------------|-----------------------------|
| GET    | /v1                                       | API info and version        |
| POST   | /v1/instance                            | Create a new instance       |
| GET    | /v1/instance                            | List all instances          |
| POST   | /v1/instance/:id/connect                | Connect to an instance      |
| POST   | /v1/instance/:id/logout                 | Logout from an instance     |
| DELETE | /v1/instance/:id                        | Delete an instance          |
| GET    | /v1/instance/:id/status                 | Get instance status         |
| POST   | /v1/instance/:instance/message/text     | Send a text message         |
| POST   | /v1/instance/:instance/message/audio    | Send an audio message       |
| POST   | /v1/instance/:instance/message/document | Send a document             |
| POST   | /v1/instance/:instance/message/image    | Send an image message       |
| POST   | /v1/instance/:instance/chat/presence    | Send chat presence          |
| POST   | /v1/instance/:instance/chat/read-messages| Mark messages as read       |
| POST   | /v1/instance/:instance/chat/whatsapp-numbers| Check if a number is on WhatsApp |

### Evolution API Compatibility Routes

| Method | Path                               | Description                 |
|--- |--- |--- |
| POST   | /v1/instance/create                | Create a new instance       |
| GET    | /v1/instance/fetchInstances        | List all instances          |
| GET    | /v1/instance/connect/:id           | Connect to an instance      |
| GET    | /v1/instance/connect/:id/image     | Get connection QR as image |
| GET    | /v1/instance/connectionState/:id   | Get instance status         |
| DELETE | /v1/instance/logout/:id            | Logout from an instance     |
| DELETE | /v1/instance/delete/:id            | Delete an instance          |
| PUT    | /v1/instance/update/:id            | Update an instance          |
| POST   | /v1/message/sendText/:instance     | Send a text message         |
| POST   | /v1/message/sendWhatsAppAudio/:instance | Send an audio message       |
| POST   | /v1/message/sendMedia/:instance    | Send a media message        |
| POST   | /v1/message/sendReaction/:instance | Send a reaction to a message |
| POST   | /v1/chat/markMessageAsRead/:instance | Mark messages as read       |
| POST   | /v1/chat/sendPresence/:instance    | Send chat presence          |
| POST   | /v1/chat/whatsappNumbers/:instance | Check if a number is on WhatsApp |

## Supported Events

The application can send webhook events for the following actions:

| Event                | Description                                                         |
|----------------------|---------------------------------------------------------------------|
| `MESSAGES_UPSERT`   | Triggered when a new message is received.                           |
| `MESSAGES_UPDATE`   | Triggered when a message status changes (e.g., read).                 |
| `CONTACTS_UPSERT`   | Triggered when a contact is created or updated.                     |
| `CONNECTION_UPDATE` | Triggered when the device connects (pairing/QR success).            |
| `SESSION_LOST`      | Triggered when the WhatsApp session is lost (Router/Redis setup).    |

When `WEBHOOK_URL` is set, the API sends a **`ready`** event (no subscription needed) once at startup when the server has finished initializing and is about to listen.

**Note:** Only events you subscribe to (in `webhook.events` when creating/updating the instance) are sent. API errors (e.g. "instance already exists", "context deadline exceeded") and internal library logs (e.g. "Successfully paired") are not webhook events and are never posted to the webhook URL.

### Media (images, audio, documents, video) in webhooks

For `messages.upsert` events, media messages include:

- **Raw data from WhatsApp** (what the library receives): inside `data.message.imageMessage` (or `audioMessage`, `documentMessage`, `videoMessage`) you get `url`, `mediaKey`, `directPath`, `mimetype`, `caption`, etc. That media is **encrypted**; the URL is WhatsApp’s CDN and requires decryption with `mediaKey` (handled by the backend when it downloads).
- **Decoded file** (image/audio/document/video already decrypted):
  - If **`WEBHOOK_URL`** is set, the backend decodes media and adds **`decodedBase64`** in the same object (e.g. `data.message.imageMessage.decodedBase64`). You can decode it to get the file:
    ```js
    const buffer = Buffer.from(data.message.imageMessage.decodedBase64, 'base64');
    require('fs').writeFileSync('image.jpg', buffer);
    ```
  - If the instance has **`webhook.base64: true`**, you also get `data.message.base64` and `data.message.imageMessage.decodedBase64`.
  - If **GCS storage** is configured, you get **`decodedMediaUrl`** (e.g. `data.message.imageMessage.decodedMediaUrl`) with a public URL to the downloaded file, and optionally `data.message.mediaUrl`.

So: use **`data.message.imageMessage`** for the raw WA payload, and **`data.message.imageMessage.decodedBase64`** or **`data.message.imageMessage.decodedMediaUrl`** to obtain the actual image file (when `WEBHOOK_URL` or webhook.base64 / storage is configured).


## Did you like project?
Donate: https://buy.stripe.com/8x28wI5vKfPbe9b8ih1VK0f