package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Amount modes describe how a bank export encodes the transaction amount.
const (
	// AmountModeSingle: one signed column, e.g. "-40.00" for a purchase.
	AmountModeSingle = "single"
	// AmountModeDebitCredit: two columns, one for money out and one for
	// money in. Either may be blank on any given row.
	AmountModeDebitCredit = "debit_credit"
)

// Mapping describes how one bank's CSV export lines up with the fields this
// app compares against YNAB. Column names are matched case-insensitively and
// ignore surrounding whitespace.
//
// YNAB field   <- mapping
//
//	Date       <- DateColumn
//	Amount     <- AmountColumn, or DebitColumn/CreditColumn combined
//	Payee/Memo <- DescriptionColumn
type Mapping struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// DateColumn feeds the YNAB transaction date. Required.
	DateColumn string `json:"dateColumn"`
	// DescriptionColumn feeds the description shown in the results table,
	// which is compared by eye against the YNAB payee. Optional.
	DescriptionColumn string `json:"descriptionColumn,omitempty"`

	// AmountMode is AmountModeSingle or AmountModeDebitCredit.
	AmountMode string `json:"amountMode"`

	// Single-column mode.
	AmountColumn string `json:"amountColumn,omitempty"`
	// InvertAmount flips the sign of AmountColumn. Set it when the export
	// records purchases as positive numbers; YNAB expects them negative.
	InvertAmount bool `json:"invertAmount,omitempty"`

	// Two-column mode. Each column's absolute value is multiplied by its
	// sign, so a bank that already writes negative credits and one that
	// writes positive credits both work with the same settings.
	DebitColumn  string `json:"debitColumn,omitempty"`
	CreditColumn string `json:"creditColumn,omitempty"`
	DebitSign    int    `json:"debitSign,omitempty"`  // default -1 (money out)
	CreditSign   int    `json:"creditSign,omitempty"` // default +1 (money in)

	// DateLayout pins a Go time layout (e.g. "01/02/2006") instead of
	// letting the flexible parser guess. Optional.
	DateLayout string `json:"dateLayout,omitempty"`

	// Builtin marks the mappings compiled into the app. They can be used
	// and copied, but not overwritten or deleted.
	Builtin bool `json:"-"`
}

// builtinMappings ship with the app so the common cases work with no setup.
var builtinMappings = []Mapping{
	{
		Name:              "Sam's Club",
		Description:       "Single signed Amount column",
		DateColumn:        "Posting Date",
		DescriptionColumn: "Description",
		AmountMode:        AmountModeSingle,
		AmountColumn:      "Amount",
		Builtin:           true,
	},
	{
		Name:              "Citi",
		Description:       "Separate Debit and Credit columns",
		DateColumn:        "Date",
		DescriptionColumn: "Description",
		AmountMode:        AmountModeDebitCredit,
		DebitColumn:       "Debit",
		CreditColumn:      "Credit",
		DebitSign:         -1,
		CreditSign:        1,
		Builtin:           true,
	},
}

// debitMultiplier and creditMultiplier fall back to sensible defaults when a
// saved file leaves the sign at zero.
func (m *Mapping) debitMultiplier() float64 {
	if m.DebitSign > 0 {
		return 1
	}
	return -1
}

func (m *Mapping) creditMultiplier() float64 {
	if m.CreditSign < 0 {
		return -1
	}
	return 1
}

func (m *Mapping) amountMultiplier() float64 {
	if m.InvertAmount {
		return -1
	}
	return 1
}

// Validate reports whether the mapping is complete enough to parse a file.
func (m *Mapping) Validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("mapping needs a name")
	}
	if mappingSlug(m.Name) == "" {
		return fmt.Errorf("mapping name %q has no letters or digits to build a filename from", m.Name)
	}
	if strings.TrimSpace(m.DateColumn) == "" {
		return fmt.Errorf("mapping needs a date column")
	}
	switch m.AmountMode {
	case AmountModeSingle:
		if strings.TrimSpace(m.AmountColumn) == "" {
			return fmt.Errorf("single-amount mappings need an amount column")
		}
	case AmountModeDebitCredit:
		if strings.TrimSpace(m.DebitColumn) == "" || strings.TrimSpace(m.CreditColumn) == "" {
			return fmt.Errorf("debit/credit mappings need both a debit column and a credit column")
		}
	default:
		return fmt.Errorf("unknown amount mode %q", m.AmountMode)
	}
	return nil
}

// mappingDir is where saved mapping files live. Override with the
// MAPPING_DIR environment variable to keep them outside the project.
func mappingDir() string {
	if dir := os.Getenv("MAPPING_DIR"); dir != "" {
		return dir
	}
	return "mappings"
}

// mappingSlug turns a display name into a safe filename stem. Anything that
// isn't a letter, digit, dash or underscore becomes a dash, which keeps a
// user-supplied name from escaping mappingDir().
func mappingSlug(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func mappingPath(name string) (string, error) {
	slug := mappingSlug(name)
	if slug == "" {
		return "", fmt.Errorf("invalid mapping name %q", name)
	}
	return filepath.Join(mappingDir(), slug+".json"), nil
}

// isBuiltinName reports whether name collides with a shipped mapping.
func isBuiltinName(name string) bool {
	for _, b := range builtinMappings {
		if mappingSlug(b.Name) == mappingSlug(name) {
			return true
		}
	}
	return false
}

// listMappings returns the built-in mappings followed by every saved one,
// sorted by name. A missing mapping directory is not an error.
func listMappings() ([]Mapping, error) {
	result := append([]Mapping(nil), builtinMappings...)

	entries, err := os.ReadDir(mappingDir())
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}

	var saved []Mapping
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".json") {
			continue
		}
		m, err := readMappingFile(filepath.Join(mappingDir(), e.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", e.Name(), err)
		}
		saved = append(saved, *m)
	}
	sort.Slice(saved, func(i, j int) bool { return saved[i].Name < saved[j].Name })

	return append(result, saved...), nil
}

func readMappingFile(path string) (*Mapping, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Mapping
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Name == "" {
		m.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return &m, nil
}

// findMapping looks a mapping up by name, built-ins first. An empty name
// returns (nil, nil), meaning "auto-detect from the header row".
func findMapping(name string) (*Mapping, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil
	}
	all, err := listMappings()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if mappingSlug(all[i].Name) == mappingSlug(name) {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("no mapping named %q", name)
}

// saveMapping writes the mapping to mappingDir(), creating the directory if
// needed. Built-in names are rejected so the shipped defaults stay intact —
// save a copy under a different name instead.
func saveMapping(m *Mapping) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if isBuiltinName(m.Name) {
		return fmt.Errorf("%q is a built-in mapping — save your copy under a different name", m.Name)
	}

	path, err := mappingPath(m.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(mappingDir(), 0o755); err != nil {
		return fmt.Errorf("creating mapping directory %s: %w", mappingDir(), err)
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func deleteMapping(name string) error {
	if isBuiltinName(name) {
		return fmt.Errorf("%q is a built-in mapping and cannot be deleted", name)
	}
	path, err := mappingPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no saved mapping named %q", name)
		}
		return err
	}
	return nil
}
