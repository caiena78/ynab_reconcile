# YNAB vs Bank Compare

A small local web app that reconciles a YNAB account against a bank or
credit-card CSV export. It shows two lists: transactions in the bank export
that have no match in YNAB, and transactions in YNAB that have no match in the
export.

Everything runs on your own machine. The only network call is a read-only
request to the YNAB API.

## Prerequisites

- Go 1.23 or newer (`go version` to check)
- A YNAB account

## 1. Get a YNAB Personal Access Token

The app authenticates to YNAB with a Personal Access Token (PAT).

1. Sign in at <https://app.ynab.com>.
2. Click your email address in the lower-left corner → **Account Settings**.
3. Scroll to **Developer Settings** and click it.
4. Under **Personal Access Tokens**, click **New Token**.
5. Re-enter your YNAB password and click **Generate**.
6. Copy the token immediately — YNAB shows it exactly once. If you lose it,
   revoke it and generate a new one.

A few things worth knowing about the token:

- It grants full read *and* write access to every budget on your account.
  This app only ever issues `GET` requests, but treat the token like a
  password anyway.
- It does not expire, so revoke it from the same screen when you're done
  using it, or if it's ever exposed.
- Do not commit it. The app never writes it to disk.

## 2. Find your Budget ID (optional)

You can leave the Budget ID as `last-used` and YNAB will use whichever budget
you opened most recently. That's the right choice if you only have one budget.

To target a specific budget, open it in the YNAB web app and copy the UUID
from the URL:

```
https://app.ynab.com/a1b2c3d4-5678-90ab-cdef-1234567890ab/budget
                     ^--------------- this is the Budget ID ---------------^
```

## 3. Build and run

```sh
go build -o ynabv2.exe .
./ynabv2.exe
```

Or run it without building a binary:

```sh
go run .
```

The server starts on <http://localhost:8080/> and opens your default browser
automatically.

## 4. Use it

**Step 1** — Paste your Personal Access Token and confirm the Budget ID, then
click **Load accounts**.

**Step 2** — Choose:

- the YNAB account to reconcile,
- a bank CSV, either from the dropdown (files found in the server's CSV
  folder) or by uploading one — an upload always wins if you do both,
- a **date tolerance** in days (default 2). Bank and YNAB entries match when
  the amounts are equal and the dates fall within this many days of each
  other, which absorbs the lag between a transaction posting at the bank and
  clearing in YNAB.

**Results** — The comparison is scoped to the date range of the CSV itself, so
YNAB transactions outside that window are never reported as missing. Each bank
entry is paired with the unmatched YNAB entry of the same amount whose date is
closest, so repeated identical amounts don't mismatch.

## Configuration

All of these are optional environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `YNAB_TOKEN` | — | Pre-fills the token so you don't paste it each run. The form field becomes optional when this is set. |
| `YNAB_BUDGET_ID` | `last-used` | Default Budget ID shown on the first screen. |
| `BANK_CSV_DIR` | `.` | Folder scanned for `*.csv` files to populate the dropdown. |
| `PORT` | `8080` | Port to listen on. |
| `NO_BROWSER` | — | Set to any value to stop the app from opening a browser at startup. |

Example:

```sh
# macOS / Linux
export YNAB_TOKEN="your-token-here"
export BANK_CSV_DIR="$HOME/Downloads/bank-exports"
go run .
```

```powershell
# Windows PowerShell
$env:YNAB_TOKEN = "your-token-here"
$env:BANK_CSV_DIR = "$HOME\Downloads\bank-exports"
go run .
```

## Supported CSV formats

The format is auto-detected from the header row.

**Sam's Club style** — needs `Posting Date`, `Amount` (signed), `Description`:

```csv
Transaction Date,Posting Date,Reference Number,Amount,Description
5/1/2026,5/1/2026,REF123,-40.00,EXAMPLE STORE
```

**Citi style** — needs `Date`, `Description`, `Debit`, `Credit`:

```csv
Status,Date,Description,Debit,Credit
Cleared,09/01/2026,EXAMPLE STORE,100.00,
```

Citi's `Debit` column is an unsigned purchase amount and `Credit` is already
negative, so the combined amount is computed as `-(debit + credit)`. That
yields negative values for purchases and positive values for refunds and
payments, matching YNAB's sign convention for credit-card accounts.

Date parsing is lenient and accepts `2006-01-02`, `01/02/2006`, `1/2/2006`,
`Jan 2, 2006`, RFC 3339, and several other common layouts.

If your bank exports some other shape, the fastest path is to rename the
columns in a spreadsheet to match one of the two formats above.

## A note on your data

Bank CSV exports contain your real transaction history, and some include your
name or per-transaction reference numbers. `.gitignore` excludes `*.csv`,
`*.CSV`, and `*.exe` for that reason. Run `git status` before your first commit
to confirm none of them are staged.
