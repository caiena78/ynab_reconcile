package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

type BankEntry struct {
	Date        time.Time
	Amount      float64
	Description string
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

var dateLayouts = []string{
	"2006-01-02",
	time.RFC3339,
	"01/02/2006",
	"1/2/2006",
	"01-02-2006",
	"1-2-2006",
	"2006/01/02",
	"January 2, 2006",
	"Jan 2, 2006",
	"Jan 2 2006",
	"02 Jan 2006",
}

func parseFlexibleDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date format: %q", s)
}

func readHeader(reader *csv.Reader) (map[string]int, error) {
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("reading CSV header: %w", err)
	}
	const utf8BOM = "\uFEFF"
	colIdx := map[string]int{}
	for i, h := range header {
		colIdx[strings.TrimSpace(strings.TrimPrefix(h, utf8BOM))] = i
	}
	return colIdx, nil
}

func parseAmountField(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}

// parseBankCSV auto-detects the export format and parses accordingly.
// Supported formats:
//   - Sam's Club style: "Posting Date", "Amount" (signed), "Description"
//   - Citi style: "Date", "Description", "Debit", "Credit"
func parseBankCSV(r io.Reader) ([]BankEntry, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1

	colIdx, err := readHeader(reader)
	if err != nil {
		return nil, err
	}

	if _, ok := colIdx["Posting Date"]; ok {
		if _, ok := colIdx["Amount"]; ok {
			return readSamsStyleCSV(reader, colIdx)
		}
	}
	if _, ok := colIdx["Debit"]; ok {
		if _, ok := colIdx["Credit"]; ok {
			return readCitiStyleCSV(reader, colIdx)
		}
	}
	return nil, fmt.Errorf("unrecognized CSV format: expected either a \"Posting Date\"/\"Amount\" column pair or \"Debit\"/\"Credit\" columns")
}

// readSamsStyleCSV expects "Posting Date", "Amount" (signed), and "Description".
func readSamsStyleCSV(reader *csv.Reader, colIdx map[string]int) ([]BankEntry, error) {
	dateCol := colIdx["Posting Date"]
	amountCol := colIdx["Amount"]
	descCol, ok := colIdx["Description"]
	if !ok {
		return nil, fmt.Errorf(`CSV is missing required column "Description"`)
	}

	var result []BankEntry
	rowNum := 1
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading CSV row %d: %w", rowNum, err)
		}
		rowNum++

		maxCol := dateCol
		if amountCol > maxCol {
			maxCol = amountCol
		}
		if descCol > maxCol {
			maxCol = descCol
		}
		if maxCol >= len(row) {
			continue
		}

		d, err := parseFlexibleDate(row[dateCol])
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", rowNum, err)
		}
		amount, err := parseAmountField(row[amountCol])
		if err != nil {
			return nil, fmt.Errorf("row %d: invalid amount %q", rowNum, row[amountCol])
		}

		result = append(result, BankEntry{
			Date:        d,
			Amount:      round2(amount),
			Description: row[descCol],
		})
	}
	return result, nil
}

// readCitiStyleCSV expects "Date", "Description", "Debit", "Credit". Debit is
// an unsigned purchase amount; Credit is already negative-signed (refunds and
// payments). The combined signed amount is -(debit + credit), which yields a
// negative amount for purchases and a positive amount for refunds/payments \u2014
// matching YNAB's sign convention for a credit card account.
func readCitiStyleCSV(reader *csv.Reader, colIdx map[string]int) ([]BankEntry, error) {
	dateCol, ok := colIdx["Date"]
	if !ok {
		return nil, fmt.Errorf(`CSV is missing required column "Date"`)
	}
	descCol, ok := colIdx["Description"]
	if !ok {
		return nil, fmt.Errorf(`CSV is missing required column "Description"`)
	}
	debitCol := colIdx["Debit"]
	creditCol := colIdx["Credit"]

	var result []BankEntry
	rowNum := 1
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading CSV row %d: %w", rowNum, err)
		}
		rowNum++

		maxCol := dateCol
		for _, c := range []int{descCol, debitCol, creditCol} {
			if c > maxCol {
				maxCol = c
			}
		}
		if maxCol >= len(row) {
			continue
		}

		d, err := parseFlexibleDate(row[dateCol])
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", rowNum, err)
		}
		debit, err := parseAmountField(row[debitCol])
		if err != nil {
			return nil, fmt.Errorf("row %d: invalid debit %q", rowNum, row[debitCol])
		}
		credit, err := parseAmountField(row[creditCol])
		if err != nil {
			return nil, fmt.Errorf("row %d: invalid credit %q", rowNum, row[creditCol])
		}

		result = append(result, BankEntry{
			Date:        d,
			Amount:      round2(-(debit + credit)),
			Description: row[descCol],
		})
	}
	return result, nil
}

func datesClose(d1, d2 time.Time, toleranceDays int) bool {
	diff := d1.Sub(d2)
	if diff < 0 {
		diff = -diff
	}
	return diff <= time.Duration(toleranceDays)*24*time.Hour
}

// amountsEqual compares rounded dollar amounts with a small epsilon so that
// floating-point rounding from two different code paths (milliunits/1000 vs.
// debit-credit arithmetic) can never cause a spurious mismatch.
func amountsEqual(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < 0.005
}

// matchEntries pairs each bank entry with the unmatched YNAB entry that has
// the same amount and the closest date within tolerance. Picking the closest
// date (rather than just the first candidate encountered) avoids
// order-dependent mismatches when the same amount appears more than once on
// either side within the comparison window.
func matchEntries(bankEntries []BankEntry, ynabEntries []YnabEntry, toleranceDays int) (bankMatched, ynabMatched []bool) {
	bankMatched = make([]bool, len(bankEntries))
	ynabMatched = make([]bool, len(ynabEntries))

	for i, b := range bankEntries {
		best := -1
		var bestDiff time.Duration
		for j, y := range ynabEntries {
			if ynabMatched[j] {
				continue
			}
			if !amountsEqual(b.Amount, y.Amount) || !datesClose(b.Date, y.Date, toleranceDays) {
				continue
			}
			diff := b.Date.Sub(y.Date)
			if diff < 0 {
				diff = -diff
			}
			if best == -1 || diff < bestDiff {
				best = j
				bestDiff = diff
			}
		}
		if best != -1 {
			bankMatched[i] = true
			ynabMatched[best] = true
		}
	}
	return bankMatched, ynabMatched
}
