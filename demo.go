package main

import (
	"context"
	"fmt"
	"time"
)

func demoTransaction(id, name, category string, amount float64, day int, pending bool) Transaction {
	t := Transaction{ID: id, AccountID: "demo-checking", Date: fmt.Sprintf("%s-%02d", time.Now().Format("2006-01"), day), Name: name, Amount: amount, Currency: "USD", Pending: pending}
	t.Category.Primary = category
	return t
}
func (a *App) seedDemo() error {
	var count int
	a.db.QueryRow("SELECT COUNT(*) FROM items WHERE id='demo'").Scan(&count)
	if count > 0 {
		return nil
	}
	if _, err := a.db.Exec("INSERT INTO items(id,token) VALUES('demo','')"); err != nil {
		return err
	}
	ac := Account{ID: "demo-checking", Name: "Demo checking • 4821", Type: "depository"}
	ac.Balances.Currency = "USD"
	data := []Transaction{demoTransaction("rent", "Apartment rent", "RENT_AND_UTILITIES", 1450, 1, false), demoTransaction("pay", "Monthly paycheck", "INCOME", -3800, 1, false), demoTransaction("groceries", "Whole Foods Market", "FOOD_AND_DRINK", 86.42, 3, false), demoTransaction("train", "Metro transit", "TRANSPORTATION", 34, 4, false), demoTransaction("coffee", "Blue Bottle Coffee", "FOOD_AND_DRINK", 6.5, 5, false), demoTransaction("shop", "Bookshop", "GENERAL_MERCHANDISE", 42.8, 6, false), demoTransaction("dinner", "Neighborhood dinner", "FOOD_AND_DRINK", 54.2, 7, false), demoTransaction("internet", "Internet service", "RENT_AND_UTILITIES", 65, 8, false), demoTransaction("transfer", "Savings transfer", "TRANSFER_OUT", 300, 9, false), demoTransaction("music", "Music subscription", "ENTERTAINMENT", 10.99, 10, false), demoTransaction("pending", "Corner market", "FOOD_AND_DRINK", 23.6, 11, true), demoTransaction("unknown", "Local merchant", "", 19, 11, false)}
	if err := a.syncItem(context.Background(), "demo", func(context.Context, string) (Page, error) {
		return Page{Added: data, Accounts: []Account{ac}, Next: "demo-1"}, nil
	}); err != nil {
		return err
	}
	month := time.Now().Format("2006-01")
	for cat, amount := range map[string]int{"FOOD_AND_DRINK": 30000, "RENT_AND_UTILITIES": 160000, "TRANSPORTATION": 10000, "ENTERTAINMENT": 5000, "GENERAL_MERCHANDISE": 15000} {
		if _, err := a.db.Exec("INSERT INTO budgets VALUES(?,?,?,?)", month, cat, "USD", amount); err != nil {
			return err
		}
	}
	return nil
}

// The first demo sync changes a merchant amount and replaces a pending transaction.
// Later calls are empty updates, demonstrating stable totals on replay.
func (a *App) demoSync(ctx context.Context) error {
	return a.syncItem(ctx, "demo", func(ctx context.Context, c string) (Page, error) {
		if c != "demo-1" {
			return Page{Next: c}, nil
		}
		p := Page{Next: "demo-2", Modified: []Transaction{demoTransaction("groceries", "Whole Foods Market", "FOOD_AND_DRINK", 91.42, 3, false)}, Added: []Transaction{demoTransaction("posted", "Corner market", "FOOD_AND_DRINK", 23.6, 11, false)}}
		p.Removed = append(p.Removed, struct {
			ID string `json:"transaction_id"`
		}{"pending"})
		return p, nil
	})
}
