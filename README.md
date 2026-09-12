# YNAB vs Bank Compare

A small local web app that reconciles a YNAB account against a bank or
credit-card CSV export. It shows two lists: transactions in the bank export
that have no match in YNAB, and transactions in YNAB that have no match in the
export.

Any bank's export layout can be described with a **mapping file** — a small
JSON file that says which CSV column holds the date, the amount, and the
description. Mappings are created and edited in the app and saved to a folder
of your choosing.

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

The server starts on <http://localhost:8080/>, opens your default browser, and
logs which bank-CSV and mapping folders it's using.

## 4. Use it

**Step 1** — Paste your Personal Access Token and confirm the Budget ID, then
click **Load accounts**.

**Step 2** — Choose:

- the YNAB account to reconcile,
- a bank CSV, either from the dropdown (files found in the server's CSV
  folder) or by uploading one — an upload always wins if you do both,
- the **column mapping** for that CSV (see below), or leave it on
  *Auto-detect*,
- a **date tolerance** in days (default 2). Bank and YNAB entries match when
  the amounts are equal and the dates fall within this many days of each
  other, which absorbs the lag between a transaction posting at the bank and
  clearing in YNAB.

**Results** — The comparison is scoped to the date range of the CSV itself, so
YNAB transactions outside that window are never reported as missing. Each bank
entry is paired with the unmatched YNAB entry of the same amount whose date is
closest, so repeated identical amounts don't mismatch.

## Mapping files

A mapping tells the app which CSV column feeds which YNAB field:

| YNAB field | Comes from |
| --- | --- |
| Date | the **date column** |
| Amount | the **amount column**, or the **debit** and **credit** columns combined |
| Payee (shown for eyeballing) | the **description column** |

Column names are matched ignoring case and surrounding whitespace, and a UTF-8
byte-order mark on the first header is stripped, so `"Posting Date"`,
`"posting date"` and `" POSTING DATE "` all refer to the same column.

### Editing mappings in the app

The mapping dropdown on step 2 is followed by four buttons:

- **Edit selected mapping** — open it in the editor below.
- **Copy selected mapping** — start a new mapping from an existing one. This is
  how you adapt a built-in, since built-ins can't be overwritten.
- **New mapping** — start from an empty form.
- **Delete selected mapping** — remove the JSON file. Your CSVs are untouched.

Saving writes `<mapping name>.json` into the mapping folder and reports the
full path it wrote to. Renaming a mapping moves its file.

### Where mappings are stored

Set `MAPPING_DIR` to keep mappings outside the project directory:

```sh
export MAPPING_DIR="$HOME/Documents/ynab-mappings"
```

It defaults to `./mappings`, created on first save. The mapping name becomes
the filename with everything that isn't a letter or digit collapsed to a dash,
so "Chase Freedom" saves as `chase-freedom.json`.

### The two amount layouts

**One column with `+`/`-` values.** Name the amount column and you're done:

```csv
Transaction Date,Posting Date,Reference Number,Amount,Description
5/1/2026,5/1/2026,REF1,-40.00,EXAMPLE STORE
5/25/2026,5/25/2026,REF2,500.00,ONLINE PAYMENT
```

YNAB wants money out to be negative. If your bank writes purchases as
*positive* numbers instead, tick **Flip the sign** and they'll be negated on
the way in.

**Two columns, debit and credit.** Name both columns and say what each one
means. The default is debit = money out, credit = money in:

```csv
Status,Date,Description,Debit,Credit
Cleared,09/01/2026,EXAMPLE STORE,100.00,
Cleared,09/03/2026,PAYMENT THANK YOU,,-250.00
```

Only the *magnitude* of each cell is used, so an export that already writes
credits as negative (Citi, above) and one that writes both columns as plain
positive numbers (common for `Withdrawal`/`Deposit` pairs) both come out right
with the defaults. Flip a column to the opposite meaning if your bank labels
them backwards from your account's perspective.

Amount cells tolerate blanks, `$`, thousands separators, and accounting-style
negatives, so `$1,234.56` and `(99.00)` both parse.

### Dates

Dates are auto-detected from a list of common layouts: `2006-01-02`,
`01/02/2006`, `1/2/2006`, `01-02-2006`, `2006/01/02`, `Jan 2, 2006`,
`January 2, 2006`, `02 Jan 2006`, and RFC 3339.

Set **Date layout** on the mapping only if your bank writes day-first dates
(`02/01/2026` for 1 February), which the guesser would otherwise read as
February 1. Use Go's reference date to spell the format out — `02/01/2006` for
day/month/year.

### The file format

The editor is just a front end for this; you can hand-write or version these
files if you prefer:

```json
{
  "name": "Chase Freedom",
  "description": "Chase card export",
  "dateColumn": "Transaction Date",
  "descriptionColumn": "Description",
  "amountMode": "single",
  "amountColumn": "Amount",
  "invertAmount": false,
  "dateLayout": ""
}
```

```json
{
  "name": "Local Credit Union",
  "dateColumn": "Post Date",
  "descriptionColumn": "Memo",
  "amountMode": "debit_credit",
  "debitColumn": "Withdrawal",
  "creditColumn": "Deposit",
  "debitSign": -1,
  "creditSign": 1
}
```

`amountMode` is `"single"` or `"debit_credit"`. `debitSign` and `creditSign`
are `-1` (money out) or `1` (money in).

### Built-in mappings

Two mappings ship with the app and need no file: **Sam's Club** (single signed
`Amount` column) and **Citi** (`Debit`/`Credit` pair). They can be used and
copied but not edited or deleted.

*Auto-detect* tries both built-ins against the header row, then falls back to
any recognizable date column paired with an `Amount` column. When it can't work
the file out, the error lists the columns it actually found so you know what to
put in a mapping.

## Configuration

All of these are optional environment variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `YNAB_TOKEN` | — | Pre-fills the token so you don't paste it each run. The form field becomes optional when this is set. |
| `YNAB_BUDGET_ID` | `last-used` | Default Budget ID shown on the first screen. |
| `BANK_CSV_DIR` | `.` | Folder scanned for `*.csv` files to populate the dropdown. |
| `MAPPING_DIR` | `mappings` | Folder holding the mapping JSON files. |
| `PORT` | `8080` | Port to listen on. |
| `NO_BROWSER` | — | Set to any value to stop the app from opening a browser at startup. |

Example:

```sh
# macOS / Linux
export YNAB_TOKEN="your-token-here"
export BANK_CSV_DIR="$HOME/Downloads/bank-exports"
export MAPPING_DIR="$HOME/Documents/ynab-mappings"
go run .
```

```powershell
# Windows PowerShell
$env:YNAB_TOKEN = "your-token-here"
$env:BANK_CSV_DIR = "$HOME\Downloads\bank-exports"
$env:MAPPING_DIR = "$HOME\Documents\ynab-mappings"
go run .
```

## Tests

```sh
go test ./...
```

`match_test.go` covers both amount layouts, sign flipping, messy currency
formatting, and case-insensitive column matching.

## A note on your data

Bank CSV exports contain your real transaction history, and some include your
name or per-transaction reference numbers. `.gitignore` excludes `*.csv`,
`*.CSV`, and `*.exe` for that reason. Run `git status` before your first commit
to confirm none of them are staged.

Mapping files hold only column names, so they're safe to commit or share.
