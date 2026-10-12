package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func testApp(t *testing.T) *App {
	t.Helper()
	db, err := openDB(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec("INSERT INTO items(id,token) VALUES('test','')"); err != nil {
		t.Fatal(err)
	}
	return &App{db: db, mode: "demo"}
}
func transaction(id string, amount float64) Transaction {
	return demoTransaction(id, "Merchant", "FOOD_AND_DRINK", amount, 3, false)
}
func cursor(t *testing.T, a *App) string {
	t.Helper()
	var c string
	if err := a.db.QueryRow("SELECT cursor FROM items WHERE id='test'").Scan(&c); err != nil {
		t.Fatal(err)
	}
	return c
}
func count(t *testing.T, a *App) int {
	t.Helper()
	var n int
	a.db.QueryRow("SELECT COUNT(*) FROM transactions").Scan(&n)
	return n
}
func TestSyncReplayModifyRemove(t *testing.T) {
	a := testApp(t)
	ctx := context.Background()
	p := Page{Added: []Transaction{transaction("a", 10), transaction("b", 20)}, Next: "one"}
	fetch := func(context.Context, string) (Page, error) { return p, nil }
	for i := 0; i < 2; i++ {
		if err := a.syncItem(ctx, "test", fetch); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, a); n != 2 {
		t.Fatalf("replay duplicated: %d", n)
	}
	p = Page{Modified: []Transaction{transaction("a", 15)}, Next: "two"}
	p.Removed = append(p.Removed, struct {
		ID string `json:"transaction_id"`
	}{"b"})
	if err := a.syncItem(ctx, "test", fetch); err != nil {
		t.Fatal(err)
	}
	var sum int
	a.db.QueryRow("SELECT SUM(amount) FROM transactions").Scan(&sum)
	if sum != 1500 || count(t, a) != 1 || cursor(t, a) != "two" {
		t.Fatal("incorrect update")
	}
}
func TestPagination(t *testing.T) {
	a := testApp(t)
	calls := 0
	err := a.syncItem(context.Background(), "test", func(ctx context.Context, c string) (Page, error) {
		calls++
		if c == "" {
			return Page{Added: []Transaction{transaction("a", 10)}, Next: "page1", More: true}, nil
		}
		if c != "page1" {
			t.Fatalf("wrong cursor %s", c)
		}
		return Page{Added: []Transaction{transaction("b", 20)}, Next: "done"}, nil
	})
	if err != nil || calls != 2 || count(t, a) != 2 || cursor(t, a) != "done" {
		t.Fatalf("pagination: %v", err)
	}
}
func TestFailureLeavesDatabaseAndCursor(t *testing.T) {
	a := testApp(t)
	err := a.syncItem(context.Background(), "test", func(ctx context.Context, c string) (Page, error) {
		if c == "" {
			return Page{Added: []Transaction{transaction("a", 10)}, Next: "page1", More: true}, nil
		}
		return Page{}, errors.New("network failed")
	})
	if err == nil || count(t, a) != 0 || cursor(t, a) != "" {
		t.Fatal("failed sync committed partial update")
	}
	var status string
	a.db.QueryRow("SELECT status FROM sync_runs").Scan(&status)
	if status != "failed" {
		t.Fatal("failure not recorded")
	}
}
func TestMutationRestartsEntireLoop(t *testing.T) {
	a := testApp(t)
	calls := 0
	err := a.syncItem(context.Background(), "test", func(ctx context.Context, c string) (Page, error) {
		calls++
		switch calls {
		case 1:
			return Page{Added: []Transaction{transaction("stale", 10)}, Next: "page1", More: true}, nil
		case 2:
			return Page{}, APIError{Code: "TRANSACTIONS_SYNC_MUTATION_DURING_PAGINATION"}
		case 3:
			if c != "" {
				t.Fatal("did not restart original cursor")
			}
			return Page{Added: []Transaction{transaction("fresh", 20)}, Next: "done"}, nil
		}
		t.Fatal("unexpected call")
		return Page{}, nil
	})
	if err != nil || count(t, a) != 1 {
		t.Fatal(err)
	}
	var id string
	a.db.QueryRow("SELECT id FROM transactions").Scan(&id)
	if id != "fresh" {
		t.Fatal("stale page committed")
	}
}
func TestDatabaseRollback(t *testing.T) {
	a := testApp(t)
	a.db.Exec("CREATE TRIGGER reject_cursor BEFORE UPDATE ON items BEGIN SELECT RAISE(ABORT, 'reject'); END;")
	err := a.syncItem(context.Background(), "test", func(context.Context, string) (Page, error) {
		return Page{Added: []Transaction{transaction("a", 10)}, Next: "done"}, nil
	})
	if err == nil || count(t, a) != 0 || cursor(t, a) != "" {
		t.Fatal("database transaction was not atomic")
	}
}
func TestSpendingRules(t *testing.T) {
	for _, tc := range []struct {
		name               string
		amount             int64
		pending            bool
		category, detailed string
		want               int64
	}{{"purchase", 1050, false, "FOOD_AND_DRINK", "", 1050}, {"pending", 1050, true, "FOOD_AND_DRINK", "", 0}, {"income", -1000, false, "INCOME", "", 0}, {"refund", -1000, false, "FOOD_AND_DRINK", "", 0}, {"transfer", 1000, false, "TRANSFER_OUT", "", 0}, {"loan", 1000, false, "LOAN_PAYMENTS", "", 0}, {"card", 1000, false, "OTHER", "CREDIT_CARD_PAYMENT", 0}, {"unknown", 1000, false, "UNCATEGORIZED", "", 1000}} {
		t.Run(tc.name, func(t *testing.T) {
			if n := spend(tc.amount, tc.pending, tc.category, tc.detailed); n != tc.want {
				t.Fatalf("got %d want %d", n, tc.want)
			}
		})
	}
}
func TestBudgetValidationAndOrigin(t *testing.T) {
	a := testApp(t)
	for _, tc := range []struct {
		body, origin string
		status       int
	}{{`{"month":"bad","category":"FOOD","currency":"USD","amount":100}`, "", 400}, {`{"month":"2026-10","category":"FOOD","currency":"USD","amount":-1}`, "", 400}, {`{"month":"2026-10","category":"FOOD","currency":"USD","amount":100}`, "https://evil.example", 403}, {`{"month":"2026-10","category":"FOOD","currency":"USD","amount":100}`, "", 200}} {
		req := httptest.NewRequest("POST", "http://localhost/api/budgets", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("got %d want %d: %s", w.Code, tc.status, w.Body.String())
		}
	}
}
