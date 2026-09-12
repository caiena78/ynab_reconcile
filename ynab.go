package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const baseURL = "https://api.ynab.com/v1"

type Account struct {
	ID     string
	Name   string
	Closed bool
}

type YnabEntry struct {
	Date   time.Time
	Amount float64
	Payee  string
	Memo   string
}

type ynabClient struct {
	token    string
	budgetID string
	http     *http.Client
}

func newYnabClient(token, budgetID string) *ynabClient {
	return &ynabClient{token: token, budgetID: budgetID, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *ynabClient) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error struct {
				Detail string `json:"detail"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if apiErr.Error.Detail != "" {
			return fmt.Errorf("YNAB API error (%d): %s", resp.StatusCode, apiErr.Error.Detail)
		}
		return fmt.Errorf("YNAB API error: status %d", resp.StatusCode)
	}

	return json.NewDecoder(resp.Body).Decode(out)
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
				Date      string `json:"date"`
				Amount    int64  `json:"amount"`
				PayeeName string `json:"payee_name"`
				Memo      string `json:"memo"`
				Deleted   bool   `json:"deleted"`
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
			Date:   d,
			Amount: round2(float64(t.Amount) / 1000.0),
			Payee:  t.PayeeName,
			Memo:   t.Memo,
		})
	}
	return result, nil
}
