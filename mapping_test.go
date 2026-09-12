package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestTemplatesRender(t *testing.T) {
	mappings, err := listMappings()
	if err != nil {
		t.Fatalf("listMappings: %v", err)
	}

	cases := map[string]selectData{
		"no editor": {
			Token: "tok", BudgetID: "last-used", ToleranceDays: 2,
			Accounts:  []accountOption{{ID: "1", Name: "Visa"}},
			BankFiles: []string{"a.csv"},
			Mappings:  mappings, SelectedM: "Citi", MappingDir: "mappings",
		},
		"editing single": {
			Token: "tok", ToleranceDays: 2, Mappings: mappings, MappingDir: "mappings",
			Editing: &Mapping{Name: "X", AmountMode: AmountModeSingle, DateColumn: "Date",
				AmountColumn: "Amount", InvertAmount: true},
		},
		"editing debit/credit": {
			Token: "tok", ToleranceDays: 2, Mappings: mappings, MappingDir: "mappings",
			EditingIsNew: true,
			Editing: &Mapping{Name: "Y", AmountMode: AmountModeDebitCredit, DateColumn: "Date",
				DebitColumn: "Withdrawal", CreditColumn: "Deposit", DebitSign: -1, CreditSign: 1},
		},
		"editing flipped signs": {
			Token: "tok", ToleranceDays: 2, Mappings: mappings, MappingDir: "mappings",
			Editing: &Mapping{Name: "Z", AmountMode: AmountModeDebitCredit, DateColumn: "Date",
				DebitColumn: "D", CreditColumn: "C", DebitSign: 1, CreditSign: -1},
		},
	}

	for name, data := range cases {
		if err := templates.ExecuteTemplate(io.Discard, "select.html", data); err != nil {
			t.Errorf("select.html [%s]: %v", name, err)
		}
	}

	if err := templates.ExecuteTemplate(io.Discard, "index.html", indexData{BudgetID: "last-used"}); err != nil {
		t.Errorf("index.html: %v", err)
	}

	result := resultData{
		AccountName: "Visa", BankFile: "a.csv", MappingName: "Citi",
		RangeStart: "2026-01-01", RangeEnd: "2026-02-01",
		Token: "tok", BudgetID: "last-used",
		AccountID:       "acct-1",
		MissingFromYnab: []missingEntry{{Key: "m0", Date: "2026-01-15", Amount: -1.23, Description: "x", Payee: "x"}},
		MissingFromBank: []YnabEntry{{Amount: 4.56, Payee: "y"}},
		Categories: []CategoryGroup{
			{Name: "Needs", Categories: []Category{{ID: "cat-1", Name: "Groceries"}}},
		},
	}
	if err := templates.ExecuteTemplate(io.Discard, "result.html", result); err != nil {
		t.Errorf("result.html: %v", err)
	}

	// With mismatches present, and with a token that would break out of the
	// JS string if it were not escaped.
	result.Token = `a"b</script><script>alert(1)</script>`
	result.DateMismatches = []dateMismatch{
		{YnabID: "id-1", BankDate: "2026-02-08", YnabDate: "2026-02-01",
			Amount: -12.34, Description: "EXAMPLE STORE*A1B", Payee: "Example Store", DaysApart: 7},
	}
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, "result.html", result); err != nil {
		t.Fatalf("result.html with mismatches: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Set to 2026-02-08", `data-id="id-1"`, "Update all 1 dates"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page is missing %q", want)
		}
	}
	if strings.Contains(out, "<script>alert(1)</script>") {
		t.Error("token was interpolated into the page unescaped")
	}
}

func TestSaveLoadDeleteRoundTrip(t *testing.T) {
	t.Setenv("MAPPING_DIR", t.TempDir())

	m := &Mapping{
		Name: "Test Bank!", DateColumn: "Post Date", DescriptionColumn: "Memo",
		AmountMode: AmountModeDebitCredit, DebitColumn: "Withdrawal",
		CreditColumn: "Deposit", DebitSign: -1, CreditSign: 1,
	}
	if err := saveMapping(m); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := findMapping("Test Bank!")
	if err != nil || got == nil {
		t.Fatalf("find after save: %v", err)
	}
	if got.DebitColumn != "Withdrawal" || got.CreditColumn != "Deposit" {
		t.Fatalf("round-trip lost fields: %+v", got)
	}

	all, err := listMappings()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != len(builtinMappings)+1 {
		t.Fatalf("expected builtins + 1 saved, got %d", len(all))
	}

	if err := saveMapping(&Mapping{Name: "Citi", DateColumn: "d", AmountMode: AmountModeSingle, AmountColumn: "a"}); err == nil {
		t.Fatal("expected overwriting a built-in to be refused")
	}
	if err := deleteMapping("Citi"); err == nil {
		t.Fatal("expected deleting a built-in to be refused")
	}

	if err := deleteMapping("Test Bank!"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := findMapping("Test Bank!"); err == nil {
		t.Fatal("expected mapping to be gone after delete")
	}
}
