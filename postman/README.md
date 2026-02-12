# Postman – WhatsMiau API

## Import the collection

1. Open Postman.
2. **Import** → **File** → select `WhatsMiau-API.postman_collection.json`.
3. Set the collection variables (collection icon → **Variables**).

## Collection variables

| Variable     | Example                 | Purpose |
|-------------|--------------------------|---------|
| `baseUrl`   | `http://localhost:8080` | Router URL (production) or backend URL (local). |
| `apikey`    | `your-api-key`          | Value for the `apikey` header (Router and backend use the same API_KEY). |
| `instanceId`| `my-instance`           | Instance ID for routes that require it. |
| `webhookUrl`| `http://localhost:3456` | Reference only; the actual WEBHOOK_URL is set in the backend. |

## Authentication

The API (Router and backend) validates the header `apikey`. Set the variable `apikey` to your `API_KEY` value. If `API_KEY` is empty in `.env`, the backend does not require auth.

## Testing the Webhook URL

**WEBHOOK_URL** is not an endpoint you call from Postman. It is the URL **you** give to the backend; the backend **POSTs** to that URL when events occur (new messages, read receipts, session lost, etc.).

To test that your webhook receives events:

1. **Configure the backend:** in `.env` set for example:
   ```env
   WEBHOOK_URL=http://localhost:3456
   ```
2. **Start the test server** (it receives and logs what WhatsMiau sends):
   ```bash
   node test-webhook/server.js
   ```
   It listens on `http://localhost:3456`.
3. **Use the API:** create an instance, connect, send/receive messages. In the `test-webhook` terminal you will see the JSON for each event.

To receive webhooks from the internet (e.g. a cloud server), use a public URL (ngrok, tunnel, etc.) and set that URL in `WEBHOOK_URL`.

## Events sent by the webhook

| Event               | Description |
|---------------------|-------------|
| `MESSAGES_UPSERT`   | New message. |
| `MESSAGES_UPDATE`   | Status change (read, etc.). |
| `CONTACTS_UPSERT`   | Contact created or updated. |
| `CONNECTION_UPDATE` | Device connected (pairing/QR success). |
| `SESSION_LOST`      | WhatsApp session lost (when using Router/Redis). |
| `session.connected` | Sent at API startup (instance/phoneNumber empty) when `WEBHOOK_URL` is set, and when each device connects (instance, phoneNumber). |
