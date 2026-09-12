package main

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
)

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.ParseFS(templateFS, "templates/*.html"))

const defaultToleranceDays = 2

type indexData struct {
	Error       string
	BudgetID    string
	HasEnvToken bool
}

type accountOption struct {
	ID   string
	Name string
}

type selectData struct {
	Error         string
	Token         string
	BudgetID      string
	Accounts      []accountOption
	BankFiles     []string
	ToleranceDays int
}

type resultData struct {
	AccountName     string
	BankFile        string
	RangeStart      string
	RangeEnd        string
	MissingFromYnab []BankEntry
	MissingFromBank []YnabEntry
}

func main() {
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/accounts", handleAccounts)
	http.HandleFunc("/compare", handleCompare)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatal(err)
	}

	url := fmt.Sprintf("http://localhost:%s/", port)
	log.Printf("Listening on %s", url)

	if os.Getenv("NO_BROWSER") == "" {
		if err := openBrowser(url); err != nil {
			log.Printf("Could not open browser automatically: %v", err)
		}
	}

	log.Fatal(http.Serve(ln, nil))
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	renderIndex(w, indexData{
		BudgetID:    envOr("YNAB_BUDGET_ID", "last-used"),
		HasEnvToken: os.Getenv("YNAB_TOKEN") != "",
	})
}

func renderIndex(w http.ResponseWriter, data indexData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "index.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleAccounts takes the YNAB token + budget ID from step 1, fetches the
// account list, and renders step 2: pick an account and a bank CSV to
// compare against.
func handleAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		failIndex(w, r, "Could not parse form: "+err.Error())
		return
	}

	token := r.FormValue("token")
	if token == "" {
		token = os.Getenv("YNAB_TOKEN")
	}
	if token == "" {
		failIndex(w, r, "A YNAB token is required (either in the form or the YNAB_TOKEN server environment variable).")
		return
	}

	budgetID := r.FormValue("budgetID")
	if budgetID == "" {
		budgetID = envOr("YNAB_BUDGET_ID", "last-used")
	}

	client := newYnabClient(token, budgetID)
	accounts, err := client.listAccounts()
	if err != nil {
		failIndex(w, r, "Fetching accounts: "+err.Error())
		return
	}

	var options []accountOption
	for _, a := range accounts {
		if a.Closed {
			continue
		}
		options = append(options, accountOption{ID: a.ID, Name: a.Name})
	}
	sort.Slice(options, func(i, j int) bool { return options[i].Name < options[j].Name })
	if len(options) == 0 {
		failIndex(w, r, "No open accounts found in this budget.")
		return
	}

	bankFiles, err := listBankCSVFiles()
	if err != nil {
		failIndex(w, r, "Listing bank CSV files: "+err.Error())
		return
	}

	renderSelect(w, selectData{
		Token:         token,
		BudgetID:      budgetID,
		Accounts:      options,
		BankFiles:     bankFiles,
		ToleranceDays: defaultToleranceDays,
	})
}

func renderSelect(w http.ResponseWriter, data selectData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "select.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func handleCompare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "Could not parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	token := r.FormValue("token")
	budgetID := r.FormValue("budgetID")
	accountID := r.FormValue("accountID")

	toleranceDays := defaultToleranceDays
	if v := r.FormValue("toleranceDays"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed >= 0 {
			toleranceDays = parsed
		}
	}

	// Bank data comes either from a file picked out of bankCSVDir(), or from
	// an uploaded file, which takes precedence if both are provided.
	var csvBytes []byte
	var bankFileLabel string
	if file, header, err := r.FormFile("bankcsvUpload"); err == nil {
		defer file.Close()
		csvBytes, err = io.ReadAll(file)
		if err != nil {
			backToSelect(w, r, "Could not read uploaded file: "+err.Error())
			return
		}
		bankFileLabel = header.Filename
	} else {
		selected := r.FormValue("bankFile")
		if selected == "" {
			backToSelect(w, r, "Choose a bank CSV file or upload one.")
			return
		}
		csvBytes, err = readBankCSVFile(selected)
		if err != nil {
			backToSelect(w, r, fmt.Sprintf("Could not read %q: %s", selected, err.Error()))
			return
		}
		bankFileLabel = selected
	}

	result, err := runComparison(token, budgetID, accountID, toleranceDays, csvBytes, bankFileLabel)
	if err != nil {
		backToSelect(w, r, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "result.html", result); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// backToSelect re-renders step 2 with an error, re-fetching the account and
// bank-file lists so the user doesn't have to re-enter their token.
func backToSelect(w http.ResponseWriter, r *http.Request, msg string) {
	token := r.FormValue("token")
	budgetID := r.FormValue("budgetID")

	data := selectData{
		Error:         msg,
		Token:         token,
		BudgetID:      budgetID,
		ToleranceDays: defaultToleranceDays,
	}
	if v := r.FormValue("toleranceDays"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed >= 0 {
			data.ToleranceDays = parsed
		}
	}

	if token != "" {
		client := newYnabClient(token, budgetID)
		if accounts, err := client.listAccounts(); err == nil {
			for _, a := range accounts {
				if !a.Closed {
					data.Accounts = append(data.Accounts, accountOption{ID: a.ID, Name: a.Name})
				}
			}
			sort.Slice(data.Accounts, func(i, j int) bool { return data.Accounts[i].Name < data.Accounts[j].Name })
		}
	}
	if files, err := listBankCSVFiles(); err == nil {
		data.BankFiles = files
	}

	renderSelect(w, data)
}

func failIndex(w http.ResponseWriter, r *http.Request, msg string) {
	renderIndex(w, indexData{
		Error:       msg,
		BudgetID:    r.FormValue("budgetID"),
		HasEnvToken: os.Getenv("YNAB_TOKEN") != "",
	})
}

func runComparison(token, budgetID, accountID string, toleranceDays int, csvData []byte, bankFileLabel string) (*resultData, error) {
	if token == "" {
		return nil, fmt.Errorf("missing YNAB token")
	}
	if accountID == "" {
		return nil, fmt.Errorf("choose an account to compare")
	}

	bankEntries, err := parseBankCSV(bytes.NewReader(csvData))
	if err != nil {
		return nil, fmt.Errorf("reading bank CSV: %w", err)
	}
	if len(bankEntries) == 0 {
		return nil, fmt.Errorf("no entries found in %q", bankFileLabel)
	}

	oldest, newest := bankEntries[0].Date, bankEntries[0].Date
	for _, e := range bankEntries {
		if e.Date.Before(oldest) {
			oldest = e.Date
		}
		if e.Date.After(newest) {
			newest = e.Date
		}
	}
	// Only compare against YNAB transactions that fall within the CSV's own
	// date range — the tolerance is used solely to decide whether a bank
	// entry and a YNAB entry inside that range are "close enough" to match.
	rangeStart := oldest
	rangeEnd := newest

	client := newYnabClient(token, budgetID)

	accounts, err := client.listAccounts()
	if err != nil {
		return nil, fmt.Errorf("fetching accounts: %w", err)
	}
	var accountName string
	for _, a := range accounts {
		if a.ID == accountID {
			accountName = a.Name
			break
		}
	}
	if accountName == "" {
		return nil, fmt.Errorf("selected account not found in this budget")
	}

	ynabEntries, err := client.listTransactions(accountID)
	if err != nil {
		return nil, fmt.Errorf("fetching transactions: %w", err)
	}

	filtered := ynabEntries[:0:0]
	for _, e := range ynabEntries {
		if !e.Date.Before(rangeStart) && !e.Date.After(rangeEnd) {
			filtered = append(filtered, e)
		}
	}
	ynabEntries = filtered

	bankMatched, ynabMatched := matchEntries(bankEntries, ynabEntries, toleranceDays)

	var missingFromYnab []BankEntry
	for i, matched := range bankMatched {
		if !matched {
			missingFromYnab = append(missingFromYnab, bankEntries[i])
		}
	}

	var missingFromBank []YnabEntry
	for i, matched := range ynabMatched {
		if !matched {
			missingFromBank = append(missingFromBank, ynabEntries[i])
		}
	}

	return &resultData{
		AccountName:     accountName,
		BankFile:        bankFileLabel,
		RangeStart:      oldest.Format("2006-01-02"),
		RangeEnd:        newest.Format("2006-01-02"),
		MissingFromYnab: missingFromYnab,
		MissingFromBank: missingFromBank,
	}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
