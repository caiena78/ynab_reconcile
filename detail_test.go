package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestBankEntryKeepsSourceRow(t *testing.T) {
	const csv = `Transaction Date,Post Date,Description,Category,Type,Amount,Memo
02/08/2026,02/09/2026,EXAMPLE STORE*A1B2C3,Shopping,Sale,-12.34,
`
	entries, err := parseBankCSV(strings.NewReader(csv), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}

	got := map[string]string{}
	for _, f := range entries[0].Fields {
		got[f.Name] = f.Value
	}

	// Every populated column must survive, including ones no mapping names,
	// since they are exactly what makes a match verifiable by eye.
	for name, want := range map[string]string{
		"Transaction Date": "02/08/2026",
		"Post Date":        "02/09/2026",
		"Description":      "EXAMPLE STORE*A1B2C3",
		"Category":         "Shopping",
		"Type":             "Sale",
		"Amount":           "-12.34",
	} {
		if got[name] != want {
			t.Errorf("field %q = %q, want %q", name, got[name], want)
		}
	}
	if _, ok := got["Memo"]; ok {
		t.Error("blank cells should be dropped, Memo was kept")
	}

	// Order must follow the file, not map iteration.
	if entries[0].Fields[0].Name != "Transaction Date" {
		t.Errorf("first field is %q, want the first CSV column", entries[0].Fields[0].Name)
	}
}

func TestImportDate(t *testing.T) {
	cases := map[string]string{
		"YNAB:-33440:2026-09-09:1": "2026-09-09",
		"YNAB:-12340:2026-02-08:2": "2026-02-08",
		"":                         "",
		"something-else":           "",
		"YNAB:-100:notadate:1":     "",
		"YNAB:-100:2026-09-09":     "",
	}
	for in, want := range cases {
		if got := importDate(in); got != want {
			t.Errorf("importDate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetailRowRenders(t *testing.T) {
	result := resultData{
		AccountName: "Visa", BankFile: "a.csv", MappingName: "auto-detected",
		RangeStart: "2026-01-01", RangeEnd: "2026-09-11",
		Token: "tok", BudgetID: "last-used",
		DateMismatches: []dateMismatch{
			{
				YnabID: "id-1", BankDate: "2026-09-08", YnabDate: "2026-09-06",
				Amount: -33.44, Description: "EXAMPLE MKT*D4E5F6", Payee: "Example Store", DaysApart: 2,
				BankFields: []CSVField{
					{Name: "Transaction Date", Value: "09/08/2026"},
					{Name: "Type", Value: "Sale"},
				},
				YnabMemo: "gift", YnabCategory: "Shopping", YnabCleared: "cleared",
				YnabApproved: true, YnabImportDate: "2026-09-08", ImportDateAgrees: true,
			},
			{
				YnabID: "id-2", BankDate: "2026-02-08", YnabDate: "2026-02-01",
				Amount: -12.34, Description: "EXAMPLE STORE*A1B", Payee: "Example Store", DaysApart: 7,
				BankFields: []CSVField{{Name: "Transaction Date", Value: "02/08/2026"}},
			},
		},
	}

	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, "result.html", result); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`id="detail-id-1"`,
		`id="toggle-id-1"`,
		"Transaction Date",
		"09/08/2026",
		"Shopping",
		"Bank date at import",
		"Same charge.",    // import date agrees
		"entered by hand", // id-2 has no import date
		`data-target="id-2"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expanded detail is missing %q", want)
		}
	}

	// The detail rows must start collapsed.
	if strings.Count(out, `class="detail"`) != 2 {
		t.Errorf("expected 2 detail rows, found %d", strings.Count(out, `class="detail"`))
	}
	if strings.Count(out, "hidden>") != 2 {
		t.Errorf("both detail rows should render hidden, found %d", strings.Count(out, "hidden>"))
	}
}
