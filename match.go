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

// CSVField is one cell of the original bank row, kept in file order so the
// whole row can be shown back to the user when they verify a match.
type CSVField struct {
	Name  string
	Value string
}

type BankEntry struct {
	Date        time.Time
	Amount      float64
	Description string
	// Fields is the source row, non-empty cells only.
	Fields []CSVField
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

// parseFlexibleDate tries every known layout. A non-empty preferred layout is
// tried first, which lets a mapping disambiguate formats the guesser would
// otherwise read the wrong way round (e.g. day-first exports).
func parseFlexibleDate(s string, preferred string) (time.Time, error) {
	s = strings.TrimSpace(s)
	layouts := dateLayouts
	if preferred != "" {
		layouts = append([]string{preferred}, dateLayouts...)
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized date format: %q", s)
}

// readHeader returns a lookup of lower-cased, trimmed column name -> index,
// so mappings can name columns without matching case exactly, plus the column
// names in their original spelling and file order for display.
func readHeader(reader *csv.Reader) (map[string]int, []string, error) {
	header, err := reader.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("reading CSV header: %w", err)
	}
	const utf8BOM = string(rune(0xFEFF))
	colIdx := map[string]int{}
	names := make([]string, 0, len(header))
	for i, h := range header {
		clean := strings.TrimSpace(strings.TrimPrefix(h, utf8BOM))
		colIdx[strings.ToLower(clean)] = i
		names = append(names, clean)
	}
	return colIdx, names, nil
}

func lookupCol(colIdx map[string]int, name string) (int, bool) {
	i, ok := colIdx[strings.ToLower(strings.TrimSpace(name))]
	return i, ok
}

// headerNames lists the columns actually present, for error messages.
func headerNames(colIdx map[string]int) string {
	names := make([]string, len(colIdx))
	for name, i := range colIdx {
		if i < len(names) {
			names[i] = name
		}
	}
	return strings.Join(names, ", ")
}

// parseAmountField reads a currency cell, tolerating blanks, thousands
// separators, currency symbols and accounting-style negatives like "(12.34)".
func parseAmountField(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	negative := false
	if strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		negative = true
		s = strings.TrimSuffix(strings.TrimPrefix(s, "("), ")")
	}

	s = strings.NewReplacer("$", "", ",", "", " ", "", string(rune(0x00A0)), "").Replace(s)
	if s == "" || s == "-" {
		return 0, nil
	}

	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if negative {
		v = -v
	}
	return v, nil
}

// parseBankCSV parses an export using the given mapping. A nil mapping falls
// back to sniffing the header row against the built-in mappings.
func parseBankCSV(r io.Reader, m *Mapping) ([]BankEntry, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1

	colIdx, names, err := readHeader(reader)
	if err != nil {
		return nil, err
	}

	if m == nil {
		m, err = detectMapping(colIdx)
		if err != nil {
			return nil, err
		}
	}
	return readMappedCSV(reader, colIdx, names, m)
}

// detectMapping picks the first built-in mapping whose required columns are
// all present, then falls back to a generic date+amount guess.
func detectMapping(colIdx map[string]int) (*Mapping, error) {
	for i := range builtinMappings {
		if mappingFits(colIdx, &builtinMappings[i]) {
			return &builtinMappings[i], nil
		}
	}

	// Generic single-amount fallback: any recognizable date column paired
	// with an "amount" column.
	for _, dateName := range []string{"date", "transaction date", "posting date", "post date"} {
		if _, ok := colIdx[dateName]; !ok {
			continue
		}
		if _, ok := colIdx["amount"]; !ok {
			continue
		}
		descName := ""
		for _, d := range []string{"description", "payee", "name", "memo"} {
			if _, ok := colIdx[d]; ok {
				descName = d
				break
			}
		}
		return &Mapping{
			Name:              "auto-detected",
			DateColumn:        dateName,
			DescriptionColumn: descName,
			AmountMode:        AmountModeSingle,
			AmountColumn:      "amount",
		}, nil
	}

	return nil, fmt.Errorf("could not auto-detect this CSV layout (columns: %s) — pick or create a mapping for it", headerNames(colIdx))
}

// mappingFits reports whether every column a mapping requires is present.
func mappingFits(colIdx map[string]int, m *Mapping) bool {
	if _, ok := lookupCol(colIdx, m.DateColumn); !ok {
		return false
	}
	switch m.AmountMode {
	case AmountModeSingle:
		_, ok := lookupCol(colIdx, m.AmountColumn)
		return ok
	case AmountModeDebitCredit:
		_, debitOK := lookupCol(colIdx, m.DebitColumn)
		_, creditOK := lookupCol(colIdx, m.CreditColumn)
		return debitOK && creditOK
	}
	return false
}

// readMappedCSV reads the remaining rows using the column positions the
// mapping names. Rows too short to hold every mapped column are skipped,
// which drops the trailing blank lines some exports end with.
func readMappedCSV(reader *csv.Reader, colIdx map[string]int, names []string, m *Mapping) ([]BankEntry, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}

	dateCol, ok := lookupCol(colIdx, m.DateColumn)
	if !ok {
		return nil, fmt.Errorf("mapping %q expects a %q column, but this file has: %s", m.Name, m.DateColumn, headerNames(colIdx))
	}
	maxCol := dateCol

	descCol, hasDesc := -1, false
	if m.DescriptionColumn != "" {
		descCol, hasDesc = lookupCol(colIdx, m.DescriptionColumn)
		if hasDesc && descCol > maxCol {
			maxCol = descCol
		}
	}

	var amountCol, debitCol, creditCol int
	switch m.AmountMode {
	case AmountModeSingle:
		amountCol, ok = lookupCol(colIdx, m.AmountColumn)
		if !ok {
			return nil, fmt.Errorf("mapping %q expects an %q column, but this file has: %s", m.Name, m.AmountColumn, headerNames(colIdx))
		}
		if amountCol > maxCol {
			maxCol = amountCol
		}
	case AmountModeDebitCredit:
		debitCol, ok = lookupCol(colIdx, m.DebitColumn)
		if !ok {
			return nil, fmt.Errorf("mapping %q expects a %q column, but this file has: %s", m.Name, m.DebitColumn, headerNames(colIdx))
		}
		creditCol, ok = lookupCol(colIdx, m.CreditColumn)
		if !ok {
			return nil, fmt.Errorf("mapping %q expects a %q column, but this file has: %s", m.Name, m.CreditColumn, headerNames(colIdx))
		}
		if debitCol > maxCol {
			maxCol = debitCol
		}
		if creditCol > maxCol {
			maxCol = creditCol
		}
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

		if maxCol >= len(row) {
			continue
		}
		if strings.TrimSpace(row[dateCol]) == "" {
			continue
		}

		d, err := parseFlexibleDate(row[dateCol], m.DateLayout)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", rowNum, err)
		}

		var amount float64
		switch m.AmountMode {
		case AmountModeSingle:
			v, err := parseAmountField(row[amountCol])
			if err != nil {
				return nil, fmt.Errorf("row %d: invalid amount %q", rowNum, row[amountCol])
			}
			amount = v * m.amountMultiplier()
		case AmountModeDebitCredit:
			debit, err := parseAmountField(row[debitCol])
			if err != nil {
				return nil, fmt.Errorf("row %d: invalid debit %q", rowNum, row[debitCol])
			}
			credit, err := parseAmountField(row[creditCol])
			if err != nil {
				return nil, fmt.Errorf("row %d: invalid credit %q", rowNum, row[creditCol])
			}
			// Take the magnitude of each column so exports that already
			// sign their credits and those that don't both land on YNAB's
			// convention: negative for money out, positive for money in.
			amount = math.Abs(debit)*m.debitMultiplier() + math.Abs(credit)*m.creditMultiplier()
		}

		desc := ""
		if hasDesc {
			desc = row[descCol]
		}

		// Keep the source row so the user can inspect every column when
		// verifying a match. Blank cells are dropped as noise.
		fields := make([]CSVField, 0, len(names))
		for k, name := range names {
			if k >= len(row) {
				break
			}
			if v := strings.TrimSpace(row[k]); v != "" {
				fields = append(fields, CSVField{Name: name, Value: v})
			}
		}

		result = append(result, BankEntry{
			Date:        d,
			Amount:      round2(amount),
			Description: desc,
			Fields:      fields,
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
//
// pairs[i] is the index of the YNAB entry that bank entry i matched, or -1 if
// it matched nothing. Callers need the pairing itself, not just whether a
// match happened, to report pairs whose dates disagree.
func matchEntries(bankEntries []BankEntry, ynabEntries []YnabEntry, toleranceDays int) (pairs []int, ynabMatched []bool) {
	pairs = make([]int, len(bankEntries))
	for i := range pairs {
		pairs[i] = -1
	}
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
			pairs[i] = best
			ynabMatched[best] = true
		}
	}
	return pairs, ynabMatched
}
