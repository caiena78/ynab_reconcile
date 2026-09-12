package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubYNAB serves one transaction and records the update it receives, so the
// write path can be exercised without touching a real budget.
type stubYNAB struct {
	txn        map[string]any
	gotPut     map[string]any
	putCalled  bool
	forceDate  string // if set, the date the server pretends to have saved
	failStatus int
}

func (s *stubYNAB) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("missing or wrong auth header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"transaction": s.txn},
			})
		case http.MethodPut:
			s.putCalled = true
			if s.failStatus != 0 {
				w.WriteHeader(s.failStatus)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"detail": "nope"},
				})
				return
			}
			body, _ := io.ReadAll(r.Body)
			var wrapper struct {
				Transaction map[string]any `json:"transaction"`
			}
			if err := json.Unmarshal(body, &wrapper); err != nil {
				t.Fatalf("PUT body is not JSON: %v", err)
			}
			s.gotPut = wrapper.Transaction

			saved := map[string]any{}
			for k, v := range s.txn {
				saved[k] = v
			}
			if s.forceDate != "" {
				saved["date"] = s.forceDate
			} else {
				saved["date"] = wrapper.Transaction["date"]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"transaction": saved},
			})
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	old := baseURL
	baseURL = srv.URL
	t.Cleanup(func() { baseURL = old; srv.Close() })
	return srv
}

func basicTxn() map[string]any {
	return map[string]any{
		"id":          "abc",
		"date":        "2026-02-01",
		"amount":      -12340,
		"account_id":  "acct-1",
		"payee_id":    "payee-1",
		"category_id": "cat-1",
		"memo":        "Christy paid half",
		"cleared":     "cleared",
		"approved":    true,
		"flag_color":  nil,
	}
}

func TestUpdateDateSendsEveryFieldBack(t *testing.T) {
	s := &stubYNAB{txn: basicTxn()}
	s.server(t)

	c := newYnabClient("tok", "last-used")
	got, err := c.updateTransactionDate("abc", time.Date(2026, 2, 8, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got != "2026-02-08" {
		t.Fatalf("returned date %q, want 2026-02-08", got)
	}

	// The date must change and nothing else may be dropped, or YNAB would
	// clear whatever was omitted.
	if s.gotPut["date"] != "2026-02-08" {
		t.Errorf("date sent = %v, want 2026-02-08", s.gotPut["date"])
	}
	for _, field := range []string{"account_id", "amount", "payee_id", "category_id", "memo", "cleared", "approved"} {
		if _, ok := s.gotPut[field]; !ok {
			t.Errorf("field %q was not sent back, YNAB could clear it", field)
		}
	}
	if s.gotPut["memo"] != "Christy paid half" {
		t.Errorf("memo altered: %v", s.gotPut["memo"])
	}
	if s.gotPut["amount"] != float64(-12340) {
		t.Errorf("amount altered: %v", s.gotPut["amount"])
	}
}

func TestUpdateDateRefusesSplits(t *testing.T) {
	txn := basicTxn()
	txn["subtransactions"] = []map[string]any{{"id": "sub-1"}, {"id": "sub-2"}}
	s := &stubYNAB{txn: txn}
	s.server(t)

	c := newYnabClient("tok", "last-used")
	_, err := c.updateTransactionDate("abc", time.Date(2026, 2, 8, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected a split transaction to be refused")
	}
	if !strings.Contains(err.Error(), "split") {
		t.Errorf("error should explain splits, got: %v", err)
	}
	if s.putCalled {
		t.Error("no write should be attempted for a split")
	}
}

func TestUpdateDateDetectsSilentlyIgnoredDate(t *testing.T) {
	s := &stubYNAB{txn: basicTxn(), forceDate: "2026-02-01"}
	s.server(t)

	c := newYnabClient("tok", "last-used")
	_, err := c.updateTransactionDate("abc", time.Date(2026, 2, 8, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected an error when YNAB keeps the old date")
	}
	if !strings.Contains(err.Error(), "2026-02-01") {
		t.Errorf("error should report the date YNAB kept, got: %v", err)
	}
}

func TestUpdateDateNoOpWhenAlreadyCorrect(t *testing.T) {
	s := &stubYNAB{txn: basicTxn()}
	s.server(t)

	c := newYnabClient("tok", "last-used")
	got, err := c.updateTransactionDate("abc", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got != "2026-02-01" {
		t.Fatalf("got %q", got)
	}
	if s.putCalled {
		t.Error("no write should happen when the date already matches")
	}
}

func TestUpdateDateSurfacesAPIError(t *testing.T) {
	s := &stubYNAB{txn: basicTxn(), failStatus: http.StatusBadRequest}
	s.server(t)

	c := newYnabClient("tok", "last-used")
	if _, err := c.updateTransactionDate("abc", time.Date(2026, 2, 8, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("expected the API error to surface")
	}
}

// The endpoint must never return a raw 500 to the page, and must refuse
// incomplete requests.
func TestUpdateDateEndpoint(t *testing.T) {
	s := &stubYNAB{txn: basicTxn()}
	s.server(t)

	post := func(body string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/update-date", strings.NewReader(body))
		w := httptest.NewRecorder()
		handleUpdateDate(w, req)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("response is not JSON: %q", w.Body.String())
		}
		return out
	}

	ok := post(`{"token":"tok","budgetID":"last-used","transactionID":"abc","date":"2026-02-08"}`)
	if ok["ok"] != true {
		t.Errorf("expected success, got %v", ok)
	}

	bad := post(`{"token":"tok","transactionID":"","date":""}`)
	if bad["ok"] != false || bad["error"] == "" {
		t.Errorf("expected a described failure, got %v", bad)
	}
}

func TestMatchEntriesReportsPairs(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 2, d, 0, 0, 0, 0, time.UTC) }

	bank := []BankEntry{
		{Date: day(8), Amount: -12.34, Description: "a"},
		{Date: day(9), Amount: -50.00, Description: "b"},
	}
	ynab := []YnabEntry{
		{ID: "y1", Date: day(9), Amount: -50.00},
		{ID: "y2", Date: day(7), Amount: -12.34},
	}

	pairs, ynabMatched := matchEntries(bank, ynab, 2)
	if pairs[0] != 1 {
		t.Errorf("bank[0] should pair with ynab[1], got %d", pairs[0])
	}
	if pairs[1] != 0 {
		t.Errorf("bank[1] should pair with ynab[0], got %d", pairs[1])
	}
	for i, m := range ynabMatched {
		if !m {
			t.Errorf("ynab[%d] should be matched", i)
		}
	}
}
