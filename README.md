# Family Guy

A small family web app written in Go: shared shopping list, to-do list and documents, with email + password login. It is built for phone browsers.

## Run

```bash
go run .
```

Then open http://localhost:8080. To reach it from your phone on the same Wi-Fi, use `http://<your-computer-ip>:8080`.

Options (flag or environment variable):

| Flag              | Env              | Default     |
|-------------------|------------------|-------------|
| `-addr`           | `ADDR`           | `:8080`     |
| `-db`             | `DB_PATH`        | `family.db` |
| `-uploads`        | `UPLOADS_DIR`    | `uploads`   |
| `-secure-cookies` | `SECURE_COOKIES=1` | off — turn on when served over HTTPS |

## How families work

Registration is invite-only. Only the very first account on an empty database can sign up without a code; it creates the family. The **Family** tab shows the invite code, and everyone else must enter it when they register, or open `/register?invite=CODE`. Everyone in a family sees and edits the same lists and documents.

## Documents

The **Docs** tab takes PDF, Word (DOC, DOCX), ODT, RTF and TXT files up to 20 MB each. Files are stored in the uploads directory under random names; their real names are kept in the database. Back up both `family.db` and `uploads/` together.

## Structure

- `main.go` – flags and HTTP server
- `app.go` – routes, template rendering
- `auth.go` – register, login, logout, sessions (bcrypt passwords, hashed session tokens in SQLite)
- `lists.go` – shopping list, to-do list, family page
- `documents.go` – upload, download, rename, delete documents
- `swim.go` – Kinder Schwimmschule: 20 classes to mark as done
- `favorites.go` – "Often bought" items for the shopping list
- `history.go` – "Finish shopping" and the shopping history
- `push.go` – push notifications ("Notify …" on the shopping list)
- `db.go` – SQLite schema (pure Go driver, no cgo)
- `templates/`, `static/` – HTML, CSS and a small optional JS file, embedded into the binary

## Notifications

On the **Family** tab, each person taps **Turn on notifications** on their own phone. Then the shopping list shows a **🔔 Notify <name>** button for everyone who did; it sends the current list as a push notification. On iPhone this works only when the app is opened from the Home Screen icon (iOS 16.4+). The push keys are created on first start and stored in the database.

## Deploy

After the server is set up once (user, `/opt/family-guy`, systemd service, cloudflared):

```bash
./deploy.sh          # test, build, upload, back up the database, restart, check
./deploy.sh --fast   # same, without running the tests
```

It uses `milos@darkstar` and checks `https://family.milosm.com`; override with `DEPLOY_HOST` / `DEPLOY_URL`. If the new version doesn't start, the previous one is restored. Database backups (last 10) are in `/var/lib/family-guy/backups`.

## Build a single binary

```bash
go build -o family-guy .
```

To build for a Linux server: `GOOS=linux GOARCH=amd64 go build -o family-guy .`
# family-guy
