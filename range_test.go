package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubBudget serves an account list and a transaction list, so runComparison
// can be exercised end to end without a real budget.
func stubBudget(t *testing.T, txns []map[string]any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/transactions"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"transactions": txns},
			})
		case strings.HasSuffix(r.URL.Path, "/categories"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"category_groups": []map[string]any{{
					"name": "Needs", "hidden": false, "deleted": false,
					"categories": []map[string]any{
						{"id": "cat-1", "name": "Groceries", "hidden": false, "deleted": false},
					},
				}}},
			})
		case strings.HasSuffix(r.URL.Path, "/accounts"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"accounts": []map[string]any{
					{"id": "acct", "name": "Example Rewards Card", "closed": false},
				}},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	old := baseURL
	baseURL = srv.URL
	t.Cleanup(func() { baseURL = old; srv.Close() })
}

func ynabTxn(id, date string, milli int64) map[string]any {
	return map[string]any{
		"id": id, "date": date, "amount": milli,
		"payee_name": "Example Store", "cleared": "cleared", "approved": true,
	}
}

const chaseCSV = `Transaction Date,Post Date,Description,Category,Type,Amount,Memo
03/07/2026,03/07/2026,EXAMPLE MKT*G7H8I9,Shopping,Sale,-12.34,
02/24/2026,02/25/2026,EXAMPLE STORE*J1K2L3,Shopping,Sale,-56.78,
`

// A YNAB transaction dated past the newest CSV row is still within tolerance
// and must be reported as a date mismatch, not as missing from YNAB — that is
// exactly the row the user needs an update button for.
func TestDateMismatchJustOutsideCSVRange(t *testing.T) {
	stubBudget(t, []map[string]any{
		ynabTxn("newer", "2026-03-08", -12340),
		ynabTxn("older", "2026-02-24", -56780),
	})

	got, err := runComparison("tok", "bud", "acct", "", 2, []byte(chaseCSV), "chase.csv")
	if err != nil {
		t.Fatalf("runComparison: %v", err)
	}

	if len(got.MissingFromYnab) != 0 {
		t.Errorf("MissingFromYnab = %+v, want none", got.MissingFromYnab)
	}
	if len(got.DateMismatches) != 1 {
		t.Fatalf("DateMismatches = %d, want 1", len(got.DateMismatches))
	}
	m := got.DateMismatches[0]
	if m.YnabID != "newer" || m.BankDate != "2026-03-07" || m.YnabDate != "2026-03-08" || m.DaysApart != 1 {
		t.Errorf("mismatch = %+v, want newer 03-07 vs 03-08 one day apart", m)
	}
}

// Widening the match window must not turn genuinely uncovered YNAB
// transactions into false "missing from bank" rows.
func TestOutsideRangeNotReportedMissingFromBank(t *testing.T) {
	stubBudget(t, []map[string]any{
		ynabTxn("newer", "2026-03-08", -12340),
		ynabTxn("older", "2026-02-24", -56780),
		ynabTxn("uncovered", "2026-03-09", -99990), // after the CSV, matches nothing
	})

	got, err := runComparison("tok", "bud", "acct", "", 2, []byte(chaseCSV), "chase.csv")
	if err != nil {
		t.Fatalf("runComparison: %v", err)
	}
	for _, e := range got.MissingFromBank {
		if e.ID == "uncovered" {
			t.Errorf("MissingFromBank included %q, which the CSV does not cover", e.ID)
		}
	}
}
