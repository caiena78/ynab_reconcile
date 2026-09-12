package main

import (
	"strings"
	"testing"
)

// All fixtures below are synthetic. They mimic the shape of real exports
// (column names, sign conventions, trailing columns) without using anyone's
// actual transactions.

// Citi-style: two columns, credit already negative in the file. The trailing
// column exercises mappings that ignore columns they don't name.
const citiCSV = `Status,Date,Description,Debit,Credit,Card Holder
Cleared,08/29/2026,"EXAMPLE STORE",100.00,,CARD HOLDER
Cleared,08/24/2026,"EXAMPLE REFUND",,-50.00,CARD HOLDER
`

// Two columns, but both written as positive numbers.
const bothPositiveCSV = `Date,Description,Withdrawal,Deposit
09/01/2026,"EXAMPLE GROCER",25.00,
09/02/2026,"EXAMPLE EMPLOYER",,1500.00
`

// Single signed column, purchases already negative.
const singleSignedCSV = `Transaction Date,Posting Date,Reference Number,Amount,Description
5/1/2026,5/1/2026,REF1,-40.00,"EXAMPLE STORE"
5/25/2026,5/25/2026,REF2,500.00,"EXAMPLE PAYMENT"
`

// Single column with purchases as positive numbers, plus messy formatting.
const singleInvertedCSV = `Date,Amount,Description
2026-03-04,"$1,234.56","EXAMPLE PURCHASE"
2026-03-05,(99.00),"EXAMPLE REFUND"
`

func amounts(t *testing.T, csv string, m *Mapping) []float64 {
	t.Helper()
	entries, err := parseBankCSV(strings.NewReader(csv), m)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := make([]float64, len(entries))
	for i, e := range entries {
		out[i] = e.Amount
	}
	return out
}

func want(t *testing.T, got []float64, expected ...float64) {
	t.Helper()
	if len(got) != len(expected) {
		t.Fatalf("got %v, want %v", got, expected)
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Fatalf("got %v, want %v", got, expected)
		}
	}
}

func TestCitiAutoDetect(t *testing.T) {
	want(t, amounts(t, citiCSV, nil), -100.00, 50.00)
}

func TestSingleSignedAutoDetect(t *testing.T) {
	want(t, amounts(t, singleSignedCSV, nil), -40.00, 500.00)
}

func TestTwoColumnsBothPositive(t *testing.T) {
	m := &Mapping{
		Name:              "test",
		DateColumn:        "Date",
		DescriptionColumn: "Description",
		AmountMode:        AmountModeDebitCredit,
		DebitColumn:       "Withdrawal",
		CreditColumn:      "Deposit",
		DebitSign:         -1,
		CreditSign:        1,
	}
	want(t, amounts(t, bothPositiveCSV, m), -25.00, 1500.00)
}

func TestSingleInverted(t *testing.T) {
	m := &Mapping{
		Name:         "test",
		DateColumn:   "Date",
		AmountMode:   AmountModeSingle,
		AmountColumn: "Amount",
		InvertAmount: true,
	}
	want(t, amounts(t, singleInvertedCSV, m), -1234.56, 99.00)
}

func TestCaseInsensitiveColumns(t *testing.T) {
	m := &Mapping{
		Name:         "test",
		DateColumn:   "  dATE  ",
		AmountMode:   AmountModeSingle,
		AmountColumn: "amount",
	}
	want(t, amounts(t, singleInvertedCSV, m), 1234.56, -99.00)
}

func TestMissingColumnIsExplained(t *testing.T) {
	m := &Mapping{
		Name:         "test",
		DateColumn:   "Nope",
		AmountMode:   AmountModeSingle,
		AmountColumn: "Amount",
	}
	_, err := parseBankCSV(strings.NewReader(singleInvertedCSV), m)
	if err == nil || !strings.Contains(err.Error(), "Nope") {
		t.Fatalf("expected a helpful error naming the column, got %v", err)
	}
}

func TestMappingSlugCannotEscapeDir(t *testing.T) {
	for _, name := range []string{"../evil", "..\\evil", "a/b/c", "....//x"} {
		slug := mappingSlug(name)
		if strings.ContainsAny(slug, `/\.`) {
			t.Fatalf("slug %q from %q still contains a path character", slug, name)
		}
	}
}
