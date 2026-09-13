package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// baseURL is a var rather than a const so tests can point the client at a
// stub server instead of the live API.
var baseURL = "https://api.ynab.com/v1"

type Account struct {
	ID     string
	Name   string
	Closed bool
}

type YnabEntry struct {
	ID       string
	Date     time.Time
	Amount   float64
	Payee    string
	Memo     string
	Category string
	Cleared  string
	Approved bool
	// ImportDate is the date the bank reported when YNAB imported this
	// transaction, recovered from its import_id. Empty for transactions
	// entered by hand. It is the strongest evidence that a YNAB row and a
	// bank row are the same charge despite disagreeing dates.
	ImportDate string
	// IsSplit marks a transaction divided across categories. YNAB's API
	// ignores a date sent for one of these, so the results page reports the
	// mismatch without offering to fix it.
	IsSplit bool
}

// apiError carries YNAB's status code alongside its message, so callers can
// react to a specific failure — notably 409, which is how YNAB reports that an
// import_id is already taken.
type apiError struct {
	Status int
	Detail string
}

func (e *apiError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("YNAB API error (%d): %s", e.Status, e.Detail)
	}
	return fmt.Sprintf("YNAB API error: status %d", e.Status)
}

// statusIs reports whether err is a YNAB API error with the given status.
func statusIs(err error, status int) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

type ynabClient struct {
	token    string
	budgetID string
	http     *http.Client
}

func newYnabClient(token, budgetID string) *ynabClient {
	return &ynabClient{token: token, budgetID: budgetID, http: &http.Client{Timeout: 30 * time.Second}}
}

// do issues a request and decodes the response. A nil body sends none.
func (c *ynabClient) do(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var apiErr struct {
			Error struct {
				Detail string `json:"detail"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		return &apiError{Status: resp.StatusCode, Detail: apiErr.Error.Detail}
	}

	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *ynabClient) get(path string, out any) error {
	return c.do(http.MethodGet, path, nil, out)
}

func (c *ynabClient) listAccounts() ([]Account, error) {
	var parsed struct {
		Data struct {
			Accounts []struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Closed bool   `json:"closed"`
			} `json:"accounts"`
		} `json:"data"`
	}
	if err := c.get(fmt.Sprintf("/budgets/%s/accounts", c.budgetID), &parsed); err != nil {
		return nil, err
	}
	accounts := make([]Account, 0, len(parsed.Data.Accounts))
	for _, a := range parsed.Data.Accounts {
		accounts = append(accounts, Account{ID: a.ID, Name: a.Name, Closed: a.Closed})
	}
	return accounts, nil
}

func (c *ynabClient) listTransactions(accountID string) ([]YnabEntry, error) {
	var parsed struct {
		Data struct {
			Transactions []struct {
				ID              string `json:"id"`
				Date            string `json:"date"`
				Amount          int64  `json:"amount"`
				PayeeName       string `json:"payee_name"`
				Memo            string `json:"memo"`
				CategoryName    string `json:"category_name"`
				Cleared         string `json:"cleared"`
				Approved        bool   `json:"approved"`
				ImportID        string `json:"import_id"`
				Deleted         bool   `json:"deleted"`
				Subtransactions []struct {
					ID string `json:"id"`
				} `json:"subtransactions"`
			} `json:"transactions"`
		} `json:"data"`
	}
	if err := c.get(fmt.Sprintf("/budgets/%s/accounts/%s/transactions", c.budgetID, accountID), &parsed); err != nil {
		return nil, err
	}

	result := make([]YnabEntry, 0, len(parsed.Data.Transactions))
	for _, t := range parsed.Data.Transactions {
		if t.Deleted {
			continue
		}
		d, err := parseFlexibleDate(t.Date, "2006-01-02")
		if err != nil {
			return nil, fmt.Errorf("parsing YNAB transaction date %q: %w", t.Date, err)
		}
		result = append(result, YnabEntry{
			ID:         t.ID,
			Date:       d,
			Amount:     round2(float64(t.Amount) / 1000.0),
			Payee:      t.PayeeName,
			Memo:       t.Memo,
			Category:   t.CategoryName,
			Cleared:    t.Cleared,
			Approved:   t.Approved,
			ImportDate: importDate(t.ImportID),
			IsSplit:    len(t.Subtransactions) > 0,
		})
	}
	return result, nil
}

// importDate pulls the bank's own date out of an import_id. YNAB assigns
// imported transactions an id shaped "YNAB:[milliunits]:[iso date]:[n]", so
// the third segment is the date the bank reported at import time — which may
// differ from the date the transaction now carries.
func importDate(importID string) string {
	if !strings.HasPrefix(importID, "YNAB:") {
		return ""
	}
	parts := strings.Split(importID, ":")
	if len(parts) < 4 {
		return ""
	}
	if _, err := time.Parse("2006-01-02", parts[2]); err != nil {
		return ""
	}
	return parts[2]
}

// transactionDetail holds the fields that are writable on an existing
// transaction, so an update can send the current values back unchanged
// alongside the one field being edited.
type transactionDetail struct {
	ID              string  `json:"id"`
	Date            string  `json:"date"`
	Amount          int64   `json:"amount"`
	AccountID       string  `json:"account_id"`
	PayeeID         *string `json:"payee_id"`
	CategoryID      *string `json:"category_id"`
	Memo            *string `json:"memo"`
	Cleared         string  `json:"cleared"`
	Approved        bool    `json:"approved"`
	FlagColor       *string `json:"flag_color"`
	Subtransactions []struct {
		ID string `json:"id"`
	} `json:"subtransactions"`
}

func (c *ynabClient) getTransaction(id string) (*transactionDetail, error) {
	var parsed struct {
		Data struct {
			Transaction transactionDetail `json:"transaction"`
		} `json:"data"`
	}
	if err := c.get(fmt.Sprintf("/budgets/%s/transactions/%s", c.budgetID, id), &parsed); err != nil {
		return nil, err
	}
	return &parsed.Data.Transaction, nil
}

// updateTransactionDate moves an existing YNAB transaction to newDate. This is
// the only write this app performs against YNAB.
//
// The current values of every other writable field are read first and sent
// back unchanged, so nothing else on the transaction can be lost. Splits are
// refused up front: YNAB silently ignores a date change on a split
// transaction, which would otherwise look like a successful update.
func (c *ynabClient) updateTransactionDate(id string, newDate time.Time) (string, error) {
	current, err := c.getTransaction(id)
	if err != nil {
		return "", fmt.Errorf("looking up transaction: %w", err)
	}
	if len(current.Subtransactions) > 0 {
		return current.Date, fmt.Errorf("this is a split transaction, and YNAB does not allow changing a split's date")
	}

	want := newDate.Format("2006-01-02")
	if current.Date == want {
		return current.Date, nil
	}

	body := map[string]any{
		"transaction": map[string]any{
			"account_id":  current.AccountID,
			"date":        want,
			"amount":      current.Amount,
			"payee_id":    current.PayeeID,
			"category_id": current.CategoryID,
			"memo":        current.Memo,
			"cleared":     current.Cleared,
			"approved":    current.Approved,
			"flag_color":  current.FlagColor,
		},
	}

	var parsed struct {
		Data struct {
			Transaction transactionDetail `json:"transaction"`
		} `json:"data"`
	}
	path := fmt.Sprintf("/budgets/%s/transactions/%s", c.budgetID, id)
	if err := c.do(http.MethodPut, path, body, &parsed); err != nil {
		return "", err
	}

	// YNAB accepts the request but ignores the date in some cases, so confirm
	// against what it echoed back rather than trusting the 200.
	if got := parsed.Data.Transaction.Date; got != want {
		return got, fmt.Errorf("YNAB accepted the update but left the date at %s", got)
	}
	return want, nil
}

// Category is one selectable YNAB category. Categories are offered as a fixed
// list rather than free text because YNAB only accepts an existing category's
// id — a typed name would have nowhere to go.
type Category struct {
	ID   string
	Name string
}

// CategoryGroup keeps the grouping YNAB shows in its own UI, so the dropdown
// reads the way the budget does.
type CategoryGroup struct {
	Name       string
	Categories []Category
}

// listCategories returns the assignable categories, grouped.
//
// Three kinds of entry are left out. Hidden and deleted categories are gone
// from the budget's working set. The "Credit Card Payments" group is managed
// by YNAB itself — those categories move money via transfers, and a
// transaction cannot be assigned to one. And YNAB's own "Uncategorized"
// placeholder is dropped because leaving the dropdown unset already means
// exactly that.
func (c *ynabClient) listCategories() ([]CategoryGroup, error) {
	var parsed struct {
		Data struct {
			CategoryGroups []struct {
				Name       string `json:"name"`
				Hidden     bool   `json:"hidden"`
				Deleted    bool   `json:"deleted"`
				Categories []struct {
					ID      string `json:"id"`
					Name    string `json:"name"`
					Hidden  bool   `json:"hidden"`
					Deleted bool   `json:"deleted"`
				} `json:"categories"`
			} `json:"category_groups"`
		} `json:"data"`
	}
	if err := c.get(fmt.Sprintf("/budgets/%s/categories", c.budgetID), &parsed); err != nil {
		return nil, err
	}

	var groups []CategoryGroup
	for _, g := range parsed.Data.CategoryGroups {
		if g.Hidden || g.Deleted || g.Name == "Credit Card Payments" {
			continue
		}
		// "Internal Master Category" is YNAB's plumbing name for the group
		// holding Ready to Assign; it would read as noise in a dropdown.
		name := g.Name
		if name == "Internal Master Category" {
			name = "Inflow"
		}

		group := CategoryGroup{Name: strings.TrimSpace(name)}
		for _, cat := range g.Categories {
			if cat.Hidden || cat.Deleted || cat.Name == "Uncategorized" {
				continue
			}
			group.Categories = append(group.Categories, Category{ID: cat.ID, Name: strings.TrimSpace(cat.Name)})
		}
		if len(group.Categories) > 0 {
			groups = append(groups, group)
		}
	}
	return groups, nil
}

// newTransaction is one transaction to create, in the app's own terms. The
// client translates it into YNAB's wire shape.
type newTransaction struct {
	AccountID  string
	Date       time.Time
	Amount     float64 // dollars, negative for an outflow
	PayeeName  string
	CategoryID string // empty leaves the transaction uncategorized
	Memo       string
}

// YNAB rejects payees and memos longer than these.
const (
	maxPayeeLen = 200
	maxMemoLen  = 500
)

// truncate shortens s to at most n characters, counting runes so a multi-byte
// name cannot be cut mid-character.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// createTransaction adds one transaction to the budget and returns its new id.
//
// Each create carries an import_id in the same "YNAB:[milliunits]:[date]:[n]"
// shape YNAB assigns to bank-imported transactions. That buys two things: YNAB
// refuses a second transaction with an id it has already seen, so a double
// click or a re-run cannot post the charge twice; and if the bank's own import
// later delivers the same charge, YNAB treats it as already present instead of
// creating a duplicate. It also means the next comparison can read the bank's
// date back off the transaction, the same way it does for real imports.
//
// The occurrence counter is what makes two genuinely separate charges for the
// same amount on the same day both postable: when YNAB reports the id is
// taken, the next occurrence is tried.
func (c *ynabClient) createTransaction(t newTransaction) (string, error) {
	if t.AccountID == "" {
		return "", fmt.Errorf("missing account")
	}
	date := t.Date.Format("2006-01-02")
	milli := int64(math.Round(t.Amount * 1000))

	var lastErr error
	for occurrence := 1; occurrence <= 20; occurrence++ {
		body := map[string]any{
			"transaction": map[string]any{
				"account_id": t.AccountID,
				"date":       date,
				"amount":     milli,
				"payee_name": nilIfEmpty(truncate(t.PayeeName, maxPayeeLen)),
				"memo":       nilIfEmpty(truncate(t.Memo, maxMemoLen)),
				// Added by hand from a statement the user is reading, so it
				// is as confirmed as a transaction gets.
				"cleared":     "cleared",
				"approved":    true,
				"category_id": nilIfEmpty(t.CategoryID),
				"import_id":   fmt.Sprintf("YNAB:%d:%s:%d", milli, date, occurrence),
			},
		}

		var parsed struct {
			Data struct {
				Transaction struct {
					ID string `json:"id"`
				} `json:"transaction"`
			} `json:"data"`
		}
		err := c.do(http.MethodPost, fmt.Sprintf("/budgets/%s/transactions", c.budgetID), body, &parsed)
		if err == nil {
			return parsed.Data.Transaction.ID, nil
		}
		if !statusIs(err, http.StatusConflict) {
			return "", err
		}
		lastErr = err
	}
	return "", fmt.Errorf("this charge already appears in YNAB: %w", lastErr)
}

// nilIfEmpty sends JSON null rather than an empty string, which YNAB reads as
// "leave unset" instead of "set to blank".
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
