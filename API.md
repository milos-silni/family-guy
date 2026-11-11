# Family Guy JSON API (v1)

The API the Android app uses. It runs in the same server as the web app, under `/api/v1`, against the same database, so the web and the app always show the same lists.

- Base URL: `https://family.milosm.com/api/v1` (locally `http://<computer-ip>:8080/api/v1`)
- Requests with a body send `Content-Type: application/json`, except the uploads, which are `multipart/form-data`.
- Responses are JSON. Times are RFC 3339 in UTC, for example `"2026-11-11T16:31:30Z"`.
- Errors look like `{"error": "Wrong email or password."}`. The message is written for people, so the app can show it as it is.
- URLs in responses (`photo_url`, `download_url`) are paths. Put the server origin in front, and send the `Authorization` header with them too.

## Authentication

Log in (or register) to get a token, store it (for example in EncryptedSharedPreferences or DataStore), and send it with every request:

```
Authorization: Bearer <token>
```

A token is valid for 90 days after it was last used, so an app in regular use stays logged in. Any `401` means the token is no longer valid: show the login screen.

| Status | Meaning |
|---|---|
| 200 / 201 | OK / created |
| 204 | OK, no body |
| 400 | Malformed request (bad JSON, missing upload field) |
| 401 | Not logged in, or the token expired |
| 404 | No such thing in your family |
| 413 | Upload too large |
| 422 | Input rejected; show `error` to the user |
| 503 | Feature not set up on the server (Android push) |

### `GET /` — server info (no login)

```json
{"name": "family-guy", "api_version": 1, "fcm_available": true}
```

Use it to check a server URL the user typed, and to decide whether to offer notifications.

### `POST /auth/login` (no login)

```json
{"email": "milos@example.com", "password": "secret123"}
```

`200`:

```json
{
  "token": "Xq3…",
  "expires_at": "2027-02-09T16:31:30Z",
  "user": {"id": 1, "email": "milos@example.com", "name": "Miloš", "family_id": 1}
}
```

`401` for a wrong email or password.

### `POST /auth/register` (no login)

```json
{"email": "vera@example.com", "name": "Vera", "password": "secret123", "invite_code": "AB3KX7PQ"}
```

`201` with the same body as login. `422` with a message when something is wrong: invalid email, name missing or over 50 characters, password under 8 or over 72 characters, unknown invite code, email already used. Only the very first account on an empty server may leave out `invite_code`.

### `POST /auth/logout`

Optional body `{"fcm_token": "…"}` also stops notifications to this phone. `204`.

### `GET /me`

```json
{"user": {"id": 1, "email": "milos@example.com", "name": "Miloš", "family_id": 1}}
```

### `GET /family`

```json
{
  "name": "Miloš's family",
  "invite_code": "AB3KX7PQ",
  "members": [
    {"id": 1, "name": "Miloš", "email": "milos@example.com", "notifications": true, "is_me": true},
    {"id": 2, "name": "Vera", "email": "vera@example.com", "notifications": false, "is_me": false}
  ]
}
```

`notifications` is true when the member turned notifications on in a browser or in the app. Those members (other than yourself) are the ones to offer "Notify …" for.

## Shopping list

A shopping item:

```json
{
  "id": 12,
  "name": "mleko",
  "quantity": "2L",
  "bought": false,
  "not_found": false,
  "photo_url": "/api/v1/shopping/photos/4f…e1.jpg",
  "added_by": "Vera",
  "created_at": "2026-11-11T16:31:30Z"
}
```

`photo_url` is `null` when there is no photo. `added_by` is `""` if that account was removed. An item is either open, `bought` or `not_found`, never both.

### `GET /shopping`

```json
{"items": [ …items… ], "open_count": 3, "bought_count": 1, "not_found_count": 0}
```

Items come in display order: open first, then not found, then bought; oldest first within each group.

### `POST /shopping/items`

`{"name": "mleko", "quantity": "2L"}`. `quantity` is optional. Name is up to 100 characters, quantity up to 20; longer input is cut. `201` with the item, `422` when the name is empty.

### `PATCH /shopping/items/{id}`

Send only the fields to change: `name`, `quantity`, `bought`, `not_found`. `{"bought": true}` also clears `not_found`, and `{"not_found": true}` also clears `bought`. `200` with the updated item, `404` if the item is gone, `422` for an empty name or both flags true.

### `DELETE /shopping/items/{id}`

`204`, also when the item was already deleted.

### `PUT /shopping/items/{id}/photo`

Multipart form, image in field `photo`: JPEG, PNG or WebP, up to 10 MB. Shrink photos on the phone first (for example to 1600 px, JPEG quality 80). `200` with the item, `422` for an unsupported or too large image.

### `DELETE /shopping/items/{id}/photo`

Removes the photo. `200` with the item.

### `GET /shopping/photos/{name}`

The image bytes. Needs the `Authorization` header (with Coil, add it in the `ImageLoader`'s OkHttp client). File names are never reused, so photos can be cached for good.

### `POST /shopping/finish`

"Finish shopping": bought items move to the history, not-found items stay on the list as open again. `{"bought": 4}`. With nothing bought, nothing happens and `bought` is 0.

### `POST /shopping/notify/{user_id}`

Sends the open shopping list as a notification to all devices of that family member. `{"sent": 2}` is the number of devices reached. With `0`, tell the user the other person needs to turn notifications on again. `404` for someone outside your family.

## Often bought (favorites)

A favorite: `{"id": 3, "name": "jaja", "quantity": "10 kom", "on_list": false}`. `on_list` is true while the same name (any case) is on the shopping list and not bought.

### `GET /shopping/favorites`

`{"favorites": [ … ]}`

### `POST /shopping/favorites`

Add one or more:

```json
{"items": [{"name": "sir", "quantity": "0,5 kg"}]}
```

or paste text, one item per line, parsed like the web form (it strips bullets and checkboxes and picks up a trailing amount such as "4 kom" or "2L"):

```json
{"text": "- jaja 10 kom\n- hleb"}
```

A name that already exists (any case) gets the new quantity. `200` with all favorites, like `GET`.

### `DELETE /shopping/favorites/{id}`

`204`.

### `POST /shopping/favorites/{id}/add-to-list`, `POST /shopping/favorites/add-all-to-list`

Put favorites on the shopping list, skipping any already on it. `{"added": 2}`.

## History

### `GET /shopping/history`

The last 50 finished trips, newest first:

```json
{
  "trips": [{
    "id": 7,
    "finished_at": "2026-11-10T18:02:11Z",
    "finished_by": "Vera",
    "bought":    [{"id": 41, "name": "mleko", "quantity": "2L", "photo_url": null, "on_list": false}],
    "not_found": [{"id": 42, "name": "kvasac", "quantity": "", "photo_url": null, "on_list": true}]
  }]
}
```

### `POST /shopping/history/items/{id}/add-to-list`

Puts that item (with its photo) back on the list unless it is already there. `{"added": 1}` or `{"added": 0}`.

## To-do

A to-do: `{"id": 5, "title": "Call the plumber", "done": false, "added_by": "Miloš", "created_at": "…"}`

| Request | Body | Response |
|---|---|---|
| `GET /todos` | | `{"todos": [...], "open_count": 2, "done_count": 1}`; open first |
| `POST /todos` | `{"title": "…"}` (up to 200 characters) | `201` to-do; `422` if empty |
| `PATCH /todos/{id}` | `{"title"?, "done"?}` | `200` to-do; `404` |
| `DELETE /todos/{id}` | | `204` |
| `POST /todos/clear-done` | | `{"deleted": 3}` |

## Documents

A document:

```json
{
  "id": 9, "name": "Tax 2026.pdf", "kind": "PDF", "size": 183422,
  "added_by": "Miloš", "created_at": "…",
  "download_url": "/api/v1/documents/9/download"
}
```

| Request | Body | Response |
|---|---|---|
| `GET /documents` | | `{"documents": [...]}`, newest first |
| `POST /documents` | multipart, one or more `file` fields | `201 {"documents": [new ones]}` |
| `GET /documents/{id}/download` | | the file, with `Content-Type` and `Content-Disposition: attachment; filename=…` |
| `PATCH /documents/{id}` | `{"name": "Tax 2026"}` | `200` document. The extension is always kept |
| `DELETE /documents/{id}` | | `204` |

Uploads accept PDF, DOC, DOCX, ODT, RTF and TXT, up to 20 MB each and 100 MB per request. Files are checked by content, so a renamed file is rejected with `422` and a message naming it. When several files go in one request, the ones before a rejected file are already saved, so reload the list after an error.

## Swim classes (Kinder Schwimmschule)

### `GET /swim`

```json
{
  "total": 20, "done_count": 3, "next": 4,
  "classes": [
    {"number": 1, "done": true, "done_at": "2026-10-02T15:00:00Z", "done_by": "Vera"},
    {"number": 4, "done": false, "done_at": null, "done_by": ""}
  ]
}
```

`next` is the first class not done yet, or `null` when all are done.

### `PUT /swim/classes/{n}`

`{"done": true}` or `{"done": false}` for class 1–20. Repeating the same call does nothing more. `200` with the whole `GET /swim` body.

### `POST /swim/reset`

Marks all classes not done. `200` with the `GET /swim` body.

## Push notifications (Firebase Cloud Messaging)

The server sends to the app through FCM. It needs to be set up once on both sides:

1. Create a Firebase project and add the Android app to it (package name). Put `google-services.json` in the app.
2. In Firebase console → Project settings → Service accounts, create a private key. Copy the JSON file to the server and start the server with `-fcm-credentials /path/key.json` (or `FCM_CREDENTIALS=/path/key.json`). Then `GET /` answers `"fcm_available": true`.

In the app:

- Ask for the `POST_NOTIFICATIONS` permission (Android 13+), get the token with `FirebaseMessaging.getInstance().token`, and send it:
  - `POST /push/fcm` with `{"token": "…"}`: `204`. `503` while the server has no FCM key.
  - Send it again from `onNewToken`. A token belongs to whoever registered it last.
- To turn notifications off: `DELETE /push/fcm` with `{"token": "…"}`, or pass `fcm_token` to logout.
- Create a notification channel with id `shopping`. Messages look like this:

```json
{
  "notification": {"title": "Vera: 3 items to buy", "body": "mleko, hleb, jaja"},
  "data": {"type": "shopping_list", "url": "/shopping"},
  "android": {"priority": "HIGH", "collapse_key": "shopping", "notification": {"channel_id": "shopping", "tag": "shopping"}}
}
```

  A newer ping replaces the older notification. Use `data.type` to open the shopping screen when it is tapped.

Tokens that FCM reports as unregistered are deleted on the next send.

## Notes for the Android client

- Allow cleartext HTTP only for local testing (`network_security_config` for your computer's IP). In production use HTTPS.
- Nothing pushes live changes to the app. Reload a screen when it opens, on pull-to-refresh, and after each change. Every change endpoint returns the updated object, so you can update the UI from the response.
- The API ignores JSON fields it doesn't know, so it is safe to send extra fields. Breaking changes will move to `/api/v2`.
