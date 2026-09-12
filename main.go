package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
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
	Error          string
	Notice         string
	Token          string
	BudgetID       string
	Accounts       []accountOption
	BankFiles      []string
	ToleranceDays  int
	Mappings       []Mapping
	SelectedM      string
	MappingDir     string
	Editing        *Mapping
	EditingIsNew   bool
	ShowMappingBox bool
}

// dateMismatch is a pair the matcher is confident about — same amount, dates
// within tolerance — whose dates nonetheless disagree. These are the rows the
// results page offers to realign, by moving the YNAB date to the bank's.
type dateMismatch struct {
	YnabID      string
	BankDate    string
	YnabDate    string
	Amount      float64
	Description string
	Payee       string
	DaysApart   int

	// Everything below is shown only when the row is expanded, so the pair
	// can be eyeballed before anything is written to YNAB.
	BankFields     []CSVField
	YnabMemo       string
	YnabCategory   string
	YnabCleared    string
	YnabApproved   bool
	YnabImportDate string
	// ImportDateAgrees reports whether the date the bank gave YNAB at import
	// is the same date this CSV carries. When true, the pair is the same
	// charge and the YNAB date was changed after import.
	ImportDateAgrees bool
}

// missingEntry is a bank row with no YNAB counterpart — a candidate to add.
// It carries the row's own key so the results page can address one row, and
// the pre-filled payee the user is free to change before adding.
type missingEntry struct {
	Key         string
	Date        string
	Amount      float64
	Description string
	// Payee is what the payee box starts out holding: the bank's description,
	// which is the only name the CSV actually gives us.
	Payee  string
	Fields []CSVField
}

type resultData struct {
	AccountName     string
	AccountID       string
	BankFile        string
	MappingName     string
	RangeStart      string
	RangeEnd        string
	Token           string
	BudgetID        string
	MissingFromYnab []missingEntry
	MissingFromBank []YnabEntry
	DateMismatches  []dateMismatch
	Categories      []CategoryGroup
	// CategoryError explains an empty dropdown when the category list could
	// not be fetched. The comparison itself still stands, so this is a note
	// on the page rather than a failed run.
	CategoryError string
}

func main() {
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/accounts", handleAccounts)
	http.HandleFunc("/compare", handleCompare)
	http.HandleFunc("/mappings/edit", handleMappingEdit)
	http.HandleFunc("/mappings/save", handleMappingSave)
	http.HandleFunc("/mappings/delete", handleMappingDelete)
	http.HandleFunc("/update-date", handleUpdateDate)
	http.HandleFunc("/add-transaction", handleAddTransaction)

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
	log.Printf("Bank CSV folder: %s", bankCSVDir())
	log.Printf("Mapping folder:  %s", mappingDir())

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
// account list, and renders step 2: pick an account, a bank CSV, and the
// mapping that describes that CSV's columns.
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

	data := selectData{
		Token:         token,
		BudgetID:      budgetID,
		Accounts:      options,
		BankFiles:     bankFiles,
		ToleranceDays: defaultToleranceDays,
		MappingDir:    mappingDir(),
	}
	if mappings, err := listMappings(); err != nil {
		data.Error = "Loading mappings: " + err.Error()
	} else {
		data.Mappings = mappings
	}

	renderSelect(w, data)
}

func renderSelect(w http.ResponseWriter, data selectData) {
	if data.MappingDir == "" {
		data.MappingDir = mappingDir()
	}
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
	mappingName := r.FormValue("mapping")

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

	result, err := runComparison(token, budgetID, accountID, mappingName, toleranceDays, csvBytes, bankFileLabel)
	if err != nil {
		backToSelect(w, r, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "result.html", result); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleMappingEdit loads a saved mapping into the editor on step 2, or
// opens a blank editor when no mapping is named. It stays a POST so the
// token travels in the request body rather than the URL.
func handleMappingEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		failIndex(w, r, "Could not parse form: "+err.Error())
		return
	}

	data := buildSelectData(r)
	data.ShowMappingBox = true

	name := r.FormValue("mapping")
	copyFrom := r.FormValue("copyFrom")
	if copyFrom != "" {
		name = copyFrom
	}

	if name == "" {
		data.Editing = &Mapping{AmountMode: AmountModeSingle, DebitSign: -1, CreditSign: 1}
		data.EditingIsNew = true
		renderSelect(w, data)
		return
	}

	m, err := findMapping(name)
	if err != nil || m == nil {
		data.Error = fmt.Sprintf("Could not open mapping %q: it is no longer available.", name)
		data.Editing = &Mapping{AmountMode: AmountModeSingle, DebitSign: -1, CreditSign: 1}
		data.EditingIsNew = true
		renderSelect(w, data)
		return
	}

	editing := *m
	if copyFrom != "" || editing.Builtin {
		// Built-ins can't be overwritten, so opening one starts a copy.
		editing.Name = editing.Name + " copy"
		editing.Builtin = false
		data.EditingIsNew = true
		data.Notice = fmt.Sprintf("Editing a copy of %q — built-in mappings can't be overwritten.", m.Name)
	}
	data.Editing = &editing
	renderSelect(w, data)
}

// handleMappingSave writes the editor's contents to a JSON file in
// mappingDir() and returns to step 2 with the saved mapping selected.
func handleMappingSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		failIndex(w, r, "Could not parse form: "+err.Error())
		return
	}

	m := mappingFromForm(r)

	// Renaming a saved mapping leaves the old file behind, so remove it.
	original := strings.TrimSpace(r.FormValue("originalName"))

	if err := saveMapping(m); err != nil {
		data := buildSelectData(r)
		data.Error = "Could not save mapping: " + err.Error()
		data.Editing = m
		data.ShowMappingBox = true
		data.EditingIsNew = original == ""
		renderSelect(w, data)
		return
	}

	if original != "" && mappingSlug(original) != mappingSlug(m.Name) && !isBuiltinName(original) {
		if err := deleteMapping(original); err != nil {
			log.Printf("Renamed mapping %q to %q but could not remove the old file: %v", original, m.Name, err)
		}
	}

	path, _ := mappingPath(m.Name)
	data := buildSelectData(r)
	data.SelectedM = m.Name
	data.Notice = fmt.Sprintf("Saved mapping %q to %s", m.Name, path)
	renderSelect(w, data)
}

func handleMappingDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		failIndex(w, r, "Could not parse form: "+err.Error())
		return
	}

	name := r.FormValue("mapping")
	err := deleteMapping(name)

	data := buildSelectData(r)
	if err != nil {
		data.Error = "Could not delete mapping: " + err.Error()
	} else {
		data.Notice = fmt.Sprintf("Deleted mapping %q", name)
	}
	renderSelect(w, data)
}

// handleUpdateDate moves one YNAB transaction's date to the bank's date. It
// speaks JSON so the results page can update a single row in place rather
// than re-running the whole comparison, which would need the CSV again.
//
// This is the only endpoint that writes to YNAB.
func handleUpdateDate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}

	var req struct {
		Token         string `json:"token"`
		BudgetID      string `json:"budgetID"`
		TransactionID string `json:"transactionID"`
		Date          string `json:"date"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Could not read request: "+err.Error())
		return
	}

	if req.Token == "" {
		req.Token = os.Getenv("YNAB_TOKEN")
	}
	if req.Token == "" || req.TransactionID == "" || req.Date == "" {
		writeJSONError(w, http.StatusBadRequest, "Missing token, transaction or date")
		return
	}
	if req.BudgetID == "" {
		req.BudgetID = envOr("YNAB_BUDGET_ID", "last-used")
	}

	newDate, err := parseFlexibleDate(req.Date, "2006-01-02")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	client := newYnabClient(req.Token, req.BudgetID)
	finalDate, err := client.updateTransactionDate(req.TransactionID, newDate)
	if err != nil {
		log.Printf("Update of transaction %s failed: %v", req.TransactionID, err)
		writeJSONError(w, http.StatusOK, err.Error())
		return
	}

	log.Printf("Moved transaction %s to %s", req.TransactionID, finalDate)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "date": finalDate})
}

// handleAddTransaction creates one YNAB transaction from a bank CSV row the
// comparison found no match for. Like /update-date it speaks JSON, so a row
// can be added without re-running the comparison — which would need the CSV
// uploaded again.
//
// The payee arrives as free text because the CSV's description is the only
// name we have and it usually wants tidying. The category arrives as an id,
// never a name: YNAB matches categories by id alone, so a typed name would be
// silently dropped. An empty id means uncategorized, which is a real state in
// YNAB and the default here.
func handleAddTransaction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}

	var req struct {
		Token      string  `json:"token"`
		BudgetID   string  `json:"budgetID"`
		AccountID  string  `json:"accountID"`
		Date       string  `json:"date"`
		Amount     float64 `json:"amount"`
		PayeeName  string  `json:"payeeName"`
		CategoryID string  `json:"categoryID"`
		Memo       string  `json:"memo"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "Could not read request: "+err.Error())
		return
	}

	if req.Token == "" {
		req.Token = os.Getenv("YNAB_TOKEN")
	}
	if req.Token == "" || req.AccountID == "" || req.Date == "" {
		writeJSONError(w, http.StatusBadRequest, "Missing token, account or date")
		return
	}
	if req.Amount == 0 {
		writeJSONError(w, http.StatusBadRequest, "A transaction needs a non-zero amount")
		return
	}
	if req.BudgetID == "" {
		req.BudgetID = envOr("YNAB_BUDGET_ID", "last-used")
	}

	date, err := parseFlexibleDate(req.Date, "2006-01-02")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	client := newYnabClient(req.Token, req.BudgetID)
	id, err := client.createTransaction(newTransaction{
		AccountID:  req.AccountID,
		Date:       date,
		Amount:     round2(req.Amount),
		PayeeName:  req.PayeeName,
		CategoryID: req.CategoryID,
		Memo:       req.Memo,
	})
	if err != nil {
		log.Printf("Adding transaction (%s, %.2f) failed: %v", req.Date, req.Amount, err)
		writeJSONError(w, http.StatusOK, err.Error())
		return
	}

	log.Printf("Added transaction %s: %s %.2f %q", id, req.Date, req.Amount, req.PayeeName)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id})
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

// mappingFromForm builds a Mapping out of the editor's fields.
func mappingFromForm(r *http.Request) *Mapping {
	m := &Mapping{
		Name:              strings.TrimSpace(r.FormValue("name")),
		Description:       strings.TrimSpace(r.FormValue("description")),
		DateColumn:        strings.TrimSpace(r.FormValue("dateColumn")),
		DescriptionColumn: strings.TrimSpace(r.FormValue("descriptionColumn")),
		AmountMode:        r.FormValue("amountMode"),
		AmountColumn:      strings.TrimSpace(r.FormValue("amountColumn")),
		InvertAmount:      r.FormValue("invertAmount") != "",
		DebitColumn:       strings.TrimSpace(r.FormValue("debitColumn")),
		CreditColumn:      strings.TrimSpace(r.FormValue("creditColumn")),
		DateLayout:        strings.TrimSpace(r.FormValue("dateLayout")),
		DebitSign:         -1,
		CreditSign:        1,
	}
	if m.AmountMode != AmountModeSingle && m.AmountMode != AmountModeDebitCredit {
		m.AmountMode = AmountModeSingle
	}
	if r.FormValue("debitSign") == "1" {
		m.DebitSign = 1
	}
	if r.FormValue("creditSign") == "-1" {
		m.CreditSign = -1
	}
	return m
}

// buildSelectData reconstructs step 2 from whatever the form carried,
// re-fetching the account, file and mapping lists so the user never has to
// re-enter their token.
func buildSelectData(r *http.Request) selectData {
	token := r.FormValue("token")
	budgetID := r.FormValue("budgetID")

	data := selectData{
		Token:         token,
		BudgetID:      budgetID,
		ToleranceDays: defaultToleranceDays,
		SelectedM:     r.FormValue("mapping"),
		MappingDir:    mappingDir(),
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
	if mappings, err := listMappings(); err == nil {
		data.Mappings = mappings
	} else {
		data.Error = "Loading mappings: " + err.Error()
	}

	return data
}

// backToSelect re-renders step 2 with an error message.
func backToSelect(w http.ResponseWriter, r *http.Request, msg string) {
	data := buildSelectData(r)
	data.Error = msg
	renderSelect(w, data)
}

func failIndex(w http.ResponseWriter, r *http.Request, msg string) {
	renderIndex(w, indexData{
		Error:       msg,
		BudgetID:    r.FormValue("budgetID"),
		HasEnvToken: os.Getenv("YNAB_TOKEN") != "",
	})
}

func runComparison(token, budgetID, accountID, mappingName string, toleranceDays int, csvData []byte, bankFileLabel string) (*resultData, error) {
	if token == "" {
		return nil, fmt.Errorf("missing YNAB token")
	}
	if accountID == "" {
		return nil, fmt.Errorf("choose an account to compare")
	}

	mapping, err := findMapping(mappingName)
	if err != nil {
		return nil, err
	}

	bankEntries, err := parseBankCSV(bytes.NewReader(csvData), mapping)
	if err != nil {
		return nil, fmt.Errorf("reading bank CSV: %w", err)
	}
	if len(bankEntries) == 0 {
		return nil, fmt.Errorf("no entries found in %q", bankFileLabel)
	}

	mappingLabel := "auto-detected"
	if mapping != nil {
		mappingLabel = mapping.Name
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
	// Match against YNAB transactions in the CSV's own date range widened by
	// the tolerance: a date mismatch means the YNAB date sits outside that
	// range by up to toleranceDays, so clipping to the range exactly would
	// discard the very candidates we exist to report.
	matchStart := oldest.AddDate(0, 0, -toleranceDays)
	matchEnd := newest.AddDate(0, 0, toleranceDays)

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
		if !e.Date.Before(matchStart) && !e.Date.After(matchEnd) {
			filtered = append(filtered, e)
		}
	}
	ynabEntries = filtered

	pairs, ynabMatched := matchEntries(bankEntries, ynabEntries, toleranceDays)

	var missingFromYnab []missingEntry
	var mismatches []dateMismatch
	for i, j := range pairs {
		if j == -1 {
			b := bankEntries[i]
			missingFromYnab = append(missingFromYnab, missingEntry{
				Key:         fmt.Sprintf("m%d", i),
				Date:        b.Date.Format("2006-01-02"),
				Amount:      b.Amount,
				Description: b.Description,
				Payee:       strings.Join(strings.Fields(b.Description), " "),
				Fields:      b.Fields,
			})
			continue
		}
		b, y := bankEntries[i], ynabEntries[j]
		if b.Date.Equal(y.Date) {
			continue
		}
		days := int(b.Date.Sub(y.Date).Hours() / 24)
		if days < 0 {
			days = -days
		}
		bankDate := b.Date.Format("2006-01-02")
		mismatches = append(mismatches, dateMismatch{
			YnabID:           y.ID,
			BankDate:         bankDate,
			YnabDate:         y.Date.Format("2006-01-02"),
			Amount:           b.Amount,
			Description:      b.Description,
			Payee:            y.Payee,
			DaysApart:        days,
			BankFields:       b.Fields,
			YnabMemo:         y.Memo,
			YnabCategory:     y.Category,
			YnabCleared:      y.Cleared,
			YnabApproved:     y.Approved,
			YnabImportDate:   y.ImportDate,
			ImportDateAgrees: y.ImportDate != "" && y.ImportDate == bankDate,
		})
	}

	var missingFromBank []YnabEntry
	for i, matched := range ynabMatched {
		if matched {
			continue
		}
		// Report only entries the CSV actually covers. One pulled in by the
		// widened window alone isn't missing from the bank — the export just
		// doesn't reach its date.
		if ynabEntries[i].Date.Before(oldest) || ynabEntries[i].Date.After(newest) {
			continue
		}
		missingFromBank = append(missingFromBank, ynabEntries[i])
	}

	// A failed category fetch must not sink the comparison: the dropdown goes
	// empty and the page says why, but the results are still worth showing.
	categories, err := client.listCategories()
	categoryError := ""
	if err != nil {
		log.Printf("Could not load categories: %v", err)
		categoryError = "Categories could not be loaded (" + err.Error() + "), so transactions can only be added uncategorized."
	}

	return &resultData{
		AccountName:     accountName,
		AccountID:       accountID,
		Categories:      categories,
		CategoryError:   categoryError,
		BankFile:        bankFileLabel,
		MappingName:     mappingLabel,
		RangeStart:      oldest.Format("2006-01-02"),
		RangeEnd:        newest.Format("2006-01-02"),
		Token:           token,
		BudgetID:        budgetID,
		MissingFromYnab: missingFromYnab,
		MissingFromBank: missingFromBank,
		DateMismatches:  mismatches,
	}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
