package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A split is detectable from the account transactions list alone, because
// YNAB returns TransactionDetail there and that always carries
// subtransactions. Nothing should need a second request per row to find out.
func TestListTransactionsMarksSplits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"transactions": []map[string]any{
				{"id": "plain", "date": "2026-03-01", "amount": -12340},
				{"id": "split", "date": "2026-03-02", "amount": -56780,
					"subtransactions": []map[string]any{{"id": "s1"}, {"id": "s2"}}},
				{"id": "emptied", "date": "2026-03-03", "amount": -100,
					"subtransactions": []map[string]any{}},
			}},
		})
	}))
	old := baseURL
	baseURL = srv.URL
	t.Cleanup(func() { baseURL = old; srv.Close() })

	got, err := newYnabClient("tok", "bud").listTransactions("acct")
	if err != nil {
		t.Fatalf("listTransactions: %v", err)
	}
	want := map[string]bool{"plain": false, "split": true, "emptied": false}
	for _, e := range got {
		if want[e.ID] != e.IsSplit {
			t.Errorf("%s: IsSplit = %v, want %v", e.ID, e.IsSplit, want[e.ID])
		}
	}
}

// Splits count toward the mismatch total the user is shown, but not toward
// the bulk button, which must only promise writes that can actually happen.
func TestFixableCountExcludesSplits(t *testing.T) {
	r := resultData{DateMismatches: []dateMismatch{
		{YnabID: "a"},
		{YnabID: "b", YnabIsSplit: true},
		{YnabID: "c"},
	}}
	if got := r.FixableCount(); got != 2 {
		t.Errorf("FixableCount() = %d, want 2", got)
	}
	if len(r.DateMismatches) != 3 {
		t.Error("splits must still be listed, not filtered out")
	}
}

// The split row is reported like any other mismatch, but offers an
// explanation instead of a button that YNAB would silently ignore.
func TestSplitRowRendersWithoutButton(t *testing.T) {
	result := resultData{
		AccountName: "Card", BankFile: "a.csv", MappingName: "auto-detected",
		RangeStart: "2026-03-01", RangeEnd: "2026-03-31",
		Token: "tok", BudgetID: "last-used",
		DateMismatches: []dateMismatch{
			{YnabID: "sp", BankDate: "2026-03-07", YnabDate: "2026-03-08",
				Amount: -12.34, Description: "EXAMPLE MKT*G7H8I9", Payee: "Example Store",
				DaysApart: 1, YnabIsSplit: true},
		},
	}

	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, "result.html", result); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	// Still reported: the user needs to know the dates disagree.
	for _, want := range []string{"2026-03-07", "2026-03-08", "Split — fix in YNAB", "This is a split transaction."} {
		if !strings.Contains(out, want) {
			t.Errorf("split row is missing %q", want)
		}
	}
	// But not actionable, and not swept up by Update all.
	if strings.Contains(out, `id="btn-sp"`) {
		t.Error("a split must not get an update button")
	}
	if strings.Contains(out, `data-id="sp"`) {
		t.Error("a split must not be selectable by updateAll()")
	}
	if strings.Contains(out, "Update all") {
		t.Error("with nothing fixable, the bulk button should not render at all")
	}
}
