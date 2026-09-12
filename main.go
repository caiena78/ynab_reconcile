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

type resultData struct {
	AccountName     string
	BankFile        string
	MappingName     string
	RangeStart      string
	RangeEnd        string
	MissingFromYnab []BankEntry
	MissingFromBank []YnabEntry
}

func main() {
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/accounts", handleAccounts)
	http.HandleFunc("/compare", handleCompare)
	http.HandleFunc("/mappings/edit", handleMappingEdit)
	http.HandleFunc("/mappings/save", handleMappingSave)
	http.HandleFunc("/mappings/delete", handleMappingDelete)

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
		MappingName:     mappingLabel,
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
