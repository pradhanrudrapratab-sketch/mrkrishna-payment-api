# MrKrishna UPI Payment API

Go + SQLite payment-link and FamPay email-reconciliation service.

## Render health endpoint
`GET /health` returns exactly `200 OK` and plain-text `ok`. It does not query SQLite, Gmail, payment state, metrics, analytics, or tracking. It only confirms that the HTTP server responds.

## Stack
- Go and `net/http`
- SQLite via `modernc.org/sqlite` (pure Go)
- Gmail IMAP via `go-imap`
- QR generation via `go-qrcode`

## Endpoints
- `GET /health` - minimal Render liveness endpoint.
- `GET /` - API information.
- `GET /link?value=500` - create transaction, payment URL, QR URL.
- `GET /qr?value=500` - create independent QR-only transaction.
- `GET /pay/:txnid` - hosted payment page.
- `GET /qr/:txnid` - QR-only page.
- `GET /cheque?value=TXN` - read status; if pending, synchronously check Gmail.
- `POST /api/payments/:txnid/confirm` - schedule a Gmail check after 60 seconds.
- `GET /api/qr/:txnid` - QR image.

## Local run
Copy `.env.example` to `.env`, fill values, then:
```sh
go mod tidy
go run .
```

## Render
The included `render.yaml` configures a Go web service and `/health` health check. It mounts a persistent disk at `/var/data`; set the secret environment variables in Render and set `BASE_URL` to the actual Render URL. A persistent disk is necessary because ephemeral storage can lose SQLite data during redeploys.

## Transaction IDs
`MKYYYYMMDDXXXX`, with four cryptographically random digits. SQLite enforces uniqueness.

## Gmail / FamPay verification
The backend fetches the latest 10 inbox messages and accepts only the exact `FAMPAY_OFFICIAL_EMAIL` sender. It requires an exact generated transaction reference and exact amount match before setting `PAID`.

**Production blocker:** The parser is generic and has not been validated against a real FamPay notification. Inspect a genuine FamPay email and adapt parsing to its actual sender, amount and transaction-reference format. If the notification does not include the generated reference, do not fall back to amount-only matching; it cannot safely identify which payment was made. Exact sender matching alone is not cryptographic proof; validate SPF/DKIM/DMARC or use an official payment verification API/webhook if available.

## Timers
- No customer confirmation: automatic Gmail check after 10 minutes.
- Customer confirms: Gmail check scheduled after 60 seconds.
- At 30 minutes: final Gmail check before marking a still-pending payment FAILED.
- Gmail/network errors do not cause failure; the scheduler retries later.
- Timers are stored in SQLite.

## Before live payments
This is a functional starter, not yet production-hardened. It does not include API-key authentication, rate limiting, admin tools, cryptographic email-authentication validation, or a confirmed FamPay-specific parser. `/cheque` performs a synchronous IMAP fetch for pending transactions and can be slow. The checkout page polls `/cheque`; add rate limiting before public deployment. Late payments after FAILED need a reconciliation workflow.
