# Resumen de eventos emitidos al webhook

Todos los eventos se envían por **POST** al `WEBHOOK_URL` (env) o a la URL del webhook de la instancia, con **Content-Type: application/json**.

Los eventos que se emiten son exactamente estos tres:

| `event`              | Descripción |
|----------------------|-------------|
| `session.connected` | API lista (instance/phoneNumber vacíos) o dispositivo conectado (instance, phoneNumber) |
| `session.lost`       | Sesión perdida / teardown |
| `messages.upsert`    | Mensaje recibido o enviado por API (solo chats 1:1) |

---

## Envoltura común

- **session.connected** y **session.lost:** campos en la raíz (`instance`, `phoneNumber`, `date_time`, `event`); no llevan `data`.
- **messages.upsert:** formato plano en la raíz (`instance`, `phoneNumber`, `fromMe`, `message`, `date_time`, `event`); no lleva `data`.

---

## 1. `session.connected`

**Cuándo:**
- Al arrancar la API (con `WEBHOOK_URL` en env): se envía con `instance` y `phoneNumber` vacíos.
- Al conectar un dispositivo (pairing o reconexión): se envía con `instance` y `phoneNumber` del dispositivo.

**Formato:**

Arranque de la API:
```json
{
  "instance": "",
  "phoneNumber": "",
  "date_time": "2025-02-12T10:00:00Z",
  "event": "session.connected"
}
```

Dispositivo conectado:
```json
{
  "instance": "<instance_id>",
  "phoneNumber": "5493512275498",
  "date_time": "2025-02-12T10:00:00Z",
  "event": "session.connected"
}
```

`phoneNumber` es la parte del JID del dispositivo antes de `@` (p. ej. `5493512275498@s.whatsapp.net` → `5493512275498`).

---

## 2. `session.lost`

**Cuándo:** Al hacer teardown de la instancia (desconexión, logout, etc.).

**Formato:**
```json
{
  "instance": "<instance_id>",
  "phoneNumber": "",
  "date_time": "2025-02-12T10:00:00Z",
  "event": "session.lost"
}
```

---

## 3. `messages.upsert`

**Cuándo:**
- Mensajes entrantes en chats 1:1 (no grupos, canales ni broadcast), si la instancia tiene webhook con `MESSAGES_UPSERT` o `WEBHOOK_URL` global.
- Mensajes enviados por la API (texto, audio, documento, imagen, etc.) en las mismas condiciones.

**Formato:** Plano en la raíz, sin `data`. Igual para entrantes y salientes. El campo `message` es un objeto con dos propiedades; si no hay texto o no hay archivo, se envía `null` en la correspondiente.

```json
{
  "instance": "<instance_id>",
  "phoneNumber": "5493512275498",
  "fromMe": false,
  "message": {
    "message": "Texto o caption",
    "fileBase64": null
  },
  "date_time": "2025-02-12T10:00:00Z",
  "event": "messages.upsert"
}
```

- **phoneNumber:** número del contacto (jid sin `@s.whatsapp.net`).
- **fromMe:** `true` si el mensaje lo enviamos nosotros (por API o desde el dispositivo); `false` si lo recibimos.
- **message.message:** texto del mensaje o caption de imagen/video/documento; `null` si no hay texto.
- **message.fileBase64:** base64 del archivo/imagen/audio/video; `null` si no hay archivo.

Solo texto:
```json
"message": { "message": "Hola", "fileBase64": null }
```

Solo archivo/imagen (sin caption):
```json
"message": { "message": null, "fileBase64": "/9j/4AAQSkZJRg..." }
```

Imagen o documento con caption:
```json
"message": { "message": "Mi foto", "fileBase64": "/9j/4AAQSkZJRg..." }
```

Reacción, contacto, lista (sin texto ni archivo):
```json
"message": { "message": null, "fileBase64": null }
```
