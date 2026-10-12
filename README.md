# Budgetlane

A local, single-user budget dashboard built with Go, SQLite, and Plaid Sandbox. Connect a test bank, incrementally sync transactions, inspect monthly spending by category, and save monthly category budgets. Demo mode works without credentials and uses the same database and sync pipeline as Sandbox mode.

## Run

Requires Go 1.25+ and a C compiler (SQLite uses CGO; on macOS install Xcode Command Line Tools if needed).

```sh
go mod download
go run .
```

Open http://localhost:8080. The default demo database is `demo.db`. Click **Sync transactions**: the first click modifies a purchase and replaces a pending charge with a posted transaction. Later clicks leave totals unchanged. Demo transaction dates use the month when the database was created; delete `demo.db` to reseed a new month.

To build an executable locally:

```sh
go build -o budgetlane .
./budgetlane
```

## Connect Plaid Sandbox

Create developer credentials at https://dashboard.plaid.com/ and use **Sandbox** credentials. Keep them in your terminal environment; do not paste them into the frontend or commit them. `.env.example` documents configuration but is not automatically loaded.

```sh
export APP_MODE=sandbox
export PLAID_CLIENT_ID='your-client-id'
export PLAID_SECRET='your-sandbox-secret'
export TOKEN_ENCRYPTION_KEY="$(openssl rand -base64 32)"
go run .
```

Retain the encryption key securely across restarts: changing it makes existing stored access tokens unreadable. Sandbox defaults to a separate `sandbox.db`, avoiding synthetic demo data mixing with Plaid data. Set `DB_PATH` to override the database location. Do not use the same database for both modes.

Click **Connect Sandbox bank** and choose a test institution using Plaid's documented test credentials. After connection, the app requests a sync; if history is still preparing, click **Sync transactions** again shortly afterward. Link is loaded from Plaid's CDN only in Sandbox mode. This app supports US/non-OAuth Sandbox test institutions; OAuth redirect handling is outside this MVP.

Only `https://sandbox.plaid.com` is used. There is no production API mode and no money movement.

## Architecture

```
Browser → Go HTTP server → Plaid Sandbox REST API
                    ↓
                  SQLite
```

- Embedded HTML/CSS/JavaScript frontend: no Node build or frontend dependencies.
- Standard-library Go HTTP adapter for Link token creation, public token exchange, and transaction sync; requests use context cancellation and a 20-second timeout.
- `items`: encrypted server-side access token and persisted cursor.
- `accounts`: names, account types, and currency returned by sync.
- `transactions`: unique Plaid transaction ID, date, merchant, minor-unit amount, currency, pending status, and personal finance categories.
- `budgets`: limits scoped by month, category, and currency.
- `sync_runs`: successful/failed sync history and update counts.

### Sync correctness

Fetch every page into a staged update before writing. Apply added/modified rows with upserts, remove deleted rows, then persist the final cursor and success record in **one SQLite transaction**. Failed requests do not change transactions or the cursor. A `TRANSACTIONS_SYNC_MUTATION_DURING_PAGINATION` response discards the staged batch and restarts from the original persisted cursor, up to three attempts. A mutex serializes connection changes and syncs; duplicate updates cannot create duplicate transaction rows.

A multi-bank sync commits each Item independently. If a later bank fails, earlier successful banks remain synced and the activity log shows that outcome.

### Calculation rules

- Posted positive outflows count as spending.
- Pending transactions, transfers, loan payments (including credit-card payments), and inflows do not count.
- Refunds are displayed as negative amounts but are not netted against gross spending.
- Missing categories are `UNCATEGORIZED`.
- Currencies are displayed separately; there is no currency conversion.
- Remaining budget equals limits minus spending **within budgeted categories**. Unbudgeted spending is disclosed separately.
- Monetary values use integer hundredths; this MVP is intended for USD Sandbox data. Non-two-decimal currencies are not supported for accurate budgeting.
- Transaction dates determine the month; this is not an account-balance or cash-flow dashboard.

## API

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/dashboard?month=YYYY-MM` | Transactions, totals, budgets, accounts, and sync activity |
| POST | `/api/link-token` | Create a Sandbox Link token |
| POST | `/api/exchange` | Exchange `{ "public_token": "…" }` and store encrypted token |
| POST | `/api/sync` | Sync all connected Sandbox Items or advance the demo scenario |
| POST | `/api/budgets` | Save `{ "month":"2026-10", "category":"FOOD_AND_DRINK", "currency":"USD", "amount":30000 }` |

POST requests require `Content-Type: application/json`. Budget amounts are integer hundredths. A limit of zero is valid. Server binds only to `127.0.0.1`; this is a local project without authentication, not a public multi-user service. Access tokens are encrypted with AES-256-GCM and database permissions are restricted to the owner. Financial data in SQLite is not otherwise encrypted. Do not expose this server publicly.

## Validation

```sh
go test -race ./...
go vet ./...
```

Tests cover replay/upsert behavior, modifications and removals, multi-page syncing, failed-page rollback, pagination mutation restart, database write rollback, spending rules, budget input validation, and cross-origin mutation rejection.

Live Plaid integration requires your Sandbox credentials and must be validated separately. Unit tests use deterministic page responses; the bundled demo is not a live Plaid connection.

## Two-minute demo

1. Show demo mode and explain that transactions are synthetic.
2. Walk through posted spending, category totals, and budgets.
3. Set a monthly category limit and show its progress bar update.
4. Find the pending Corner market charge. Click sync: it becomes posted; a grocery amount changes.
5. Click sync again: transaction count and totals stay stable.
6. Show the sync history and explain atomic cursor persistence and pagination restart.
7. If configured, switch to Sandbox and demonstrate the actual Link flow.

## References

- https://plaid.com/docs/quickstart/
- https://plaid.com/docs/api/link/
- https://plaid.com/docs/api/products/transactions/
- https://plaid.com/docs/sandbox/test-credentials/
