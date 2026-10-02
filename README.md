# Music backend

The existing `GET /` (and `HEAD /`) redirects to https://ducnguynx.codes/music and records access in `qr_access.log`. `POST /api/song-requests` accepts anonymous song requests and stores them in SQLite. No public endpoint exposes submissions or visitor information.

## Run

Requires Go 1.25 or newer. SQLite uses the pure Go `modernc.org/sqlite` driver; no C compiler is needed for production builds.

```sh
DATABASE_PATH=./data/music.db \
go run .
```

The server listens on port 8080. `DATABASE_PATH` defaults to `/app/data/music.db`; the access log is written alongside the database. Persist `/app/data` when running the Docker image. The image uses the default container root user, preserving the original deployment's ownership setup. With the default rootless Podman user mapping, container root maps to the host user running Podman; mounted directories must be writable by that host user. Run one application instance per database file.

## Frontend contract

```js
const response = await fetch('https://your-backend.example/api/song-requests', {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ song_name: 'Một bài hát' }),
});
const result = await response.json();
// Check response.ok; result.error contains a user-readable failure message.
```

A successful request returns HTTP 201:

```json
{"id":1,"song_name":"Một bài hát","created_at":"2026-10-01T15:00:00Z"}
```

Only `song_name` is accepted. Titles must contain 1–200 Unicode characters after trimming and collapsing spaces. Unicode titles, apostrophes, punctuation, and emoji are allowed. Control characters, Unicode format characters, and invalid UTF-8 are rejected. The request must be a single JSON object, have `Content-Type: application/json`, and fit within 4096 bytes. Unknown fields and trailing JSON are rejected.

Error responses have the form `{"error":"message"}`:

| Status | Meaning |
| --- | --- |
| 400 | Invalid JSON or title |
| 405 | Unsupported HTTP method |
| 409 | Same normalized title submitted by this IP within 10 minutes |
| 413 | Request body too large |
| 415 | Incorrect content type |
| 429 | Rate limit exceeded; respect `Retry-After` |
| 503 | Database unavailable |

The API allows browser requests from any origin (`Access-Control-Allow-Origin: *`) and supports OPTIONS preflight, so a separately hosted frontend needs no origin configuration. `Retry-After` is exposed for frontend code to read. No cookies or credentials are required. CORS provides browser compatibility; the SQLite submission quotas and request limits enforce the abuse controls.

## Stored information and proxy trust

Each row in `song_requests` contains `id`, `song_name`, `ip`, `country`, `user_agent` (up to 512 characters), and `created_at` (UTC Unix seconds). These identify a network client, not a verified person. Country is `Unknown` unless trusted Cloudflare metadata is available; the backend does not perform IP geolocation.

By default IP comes from the TCP peer and all forwarding headers are ignored. If deployed behind Cloudflare, configure `TRUSTED_PROXY_CIDRS` with the actual trusted proxy address ranges. Only requests whose immediate peer belongs to one of those CIDRs may supply `CF-Connecting-IP` and `CF-IPCountry`. `X-Forwarded-For` is never used.

If another reverse proxy sits between Cloudflare and this server, trust that proxy only when it restricts upstream access to Cloudflare and overwrites or removes client-supplied Cloudflare headers. Do not configure all addresses as trusted. Incorrect proxy configuration lets callers bypass IP limits and falsify metadata. Without proxy configuration, all users behind a reverse proxy share its IP and rate limit.

## Abuse controls and input safety

- At most 5 POST attempts per IP per minute, including malformed submissions, and 100 POST attempts globally per minute. These short-term counters reset on restart and use bounded memory.
- At most 5 accepted submissions per IP per hour, enforced transactionally using SQLite, including across restarts.
- At most 100 accepted submissions globally in any rolling hour, across all IPs. This SQLite limit survives restarts and is checked in the same transaction as the insert. Once full, submissions return HTTP 429 with `Retry-After` indicating when the oldest submission leaves the window. Invalid or rejected submissions do not consume this quota.
- Duplicate requests from one IP within 10 minutes return 409. Titles are compared exactly after whitespace normalization; case and spelling variants remain distinct.
- SQL values use placeholders. A song title is stored as plain text rather than interpreted as SQL or HTML. When displaying it in the frontend, use normal framework text interpolation or `textContent`; avoid `innerHTML`.
- Request/header size bounds, HTTP timeouts, and a private database with no public listing endpoint reduce resource consumption and data exposure.

These limits are a baseline for a small public service. IP limits can affect people sharing a network and can be bypassed using multiple IPs. For sustained bot traffic, add a server-verified challenge (such as Turnstile), authentication, or edge rate limits. IP addresses and user agents are personal data; choose a retention period, remove old rows as appropriate, and describe collection in the frontend privacy notice. No automatic retention policy is applied.

## Verify

```sh
GOCACHE=/tmp/music-backend-go-cache go test -race ./...
go vet ./...
```
