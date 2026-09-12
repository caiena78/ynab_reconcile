package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// bankCSVDir is where downloaded bank/credit-card CSV exports live. Override
// with the BANK_CSV_DIR environment variable.
func bankCSVDir() string {
	if dir := os.Getenv("BANK_CSV_DIR"); dir != "" {
		return dir
	}
	return "."
}

// listBankCSVFiles returns the base names of *.csv files (case-insensitive)
// found in bankCSVDir(), sorted alphabetically.
func listBankCSVFiles() ([]string, error) {
	entries, err := os.ReadDir(bankCSVDir())
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".csv") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	return files, nil
}

// readBankCSVFile reads and validates a bank CSV by base name from
// bankCSVDir(), rejecting any attempt to escape that directory.
func readBankCSVFile(name string) ([]byte, error) {
	clean := filepath.Base(name)
	if clean != name || clean == "." || clean == string(filepath.Separator) {
		return nil, os.ErrInvalid
	}
	return os.ReadFile(filepath.Join(bankCSVDir(), clean))
}
