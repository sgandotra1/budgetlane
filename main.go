package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed web/*
var assets embed.FS

type Transaction struct {
	ID         string  `json:"transaction_id"`
	AccountID  string  `json:"account_id"`
	Date       string  `json:"date"`
	Name       string  `json:"name"`
	Merchant   string  `json:"merchant_name"`
	Amount     float64 `json:"amount"`
	Currency   string  `json:"iso_currency_code"`
	Unofficial string  `json:"unofficial_currency_code"`
	Pending    bool    `json:"pending"`
	Category   struct {
		Primary  string `json:"primary"`
		Detailed string `json:"detailed"`
	} `json:"personal_finance_category"`
}
type Account struct {
	ID       string `json:"account_id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Balances struct {
		Currency string `json:"iso_currency_code"`
	} `json:"balances"`
}
type Page struct {
	Added    []Transaction `json:"added"`
	Modified []Transaction `json:"modified"`
	Removed  []struct {
		ID string `json:"transaction_id"`
	} `json:"removed"`
	Accounts []Account `json:"accounts"`
	Next     string    `json:"next_cursor"`
	More     bool      `json:"has_more"`
}
type APIError struct {
	Code string `json:"error_code"`
}

func (e APIError) Error() string { return "Plaid request failed: " + e.Code }

type Plaid struct {
	ClientID, Secret string
	Client           *http.Client
}

func (p *Plaid) call(ctx context.Context, path string, body map[string]any, out any) error {
	body["client_id"] = p.ClientID
	body["secret"] = p.Secret
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://sandbox.plaid.com"+path, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Plaid-Version", "2020-09-14")
	resp, err := p.Client.Do(req)
	if err != nil {
		return errors.New("Plaid is unreachable; please retry")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e APIError
		json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
		if e.Code == "" {
			e.Code = "UPSTREAM_ERROR"
		}
		return e
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(out)
}
func (p *Plaid) page(ctx context.Context, token, cursor string) (Page, error) {
	var out Page
	err := p.call(ctx, "/transactions/sync", map[string]any{"access_token": token, "cursor": cursor, "count": 100, "options": map[string]any{"personal_finance_category_version": "v2"}}, &out)
	return out, err
}

type App struct {
	db    *sql.DB
	mode  string
	plaid *Plaid
	aead  cipher.AEAD
	mu    sync.Mutex
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS items(id TEXT PRIMARY KEY, token TEXT NOT NULL, cursor TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS accounts(id TEXT PRIMARY KEY,item_id TEXT NOT NULL REFERENCES items(id),name TEXT NOT NULL,type TEXT NOT NULL,currency TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS transactions(id TEXT PRIMARY KEY,item_id TEXT NOT NULL REFERENCES items(id),account_id TEXT NOT NULL,date TEXT NOT NULL,name TEXT NOT NULL,amount INTEGER NOT NULL,currency TEXT NOT NULL,pending INTEGER NOT NULL,category TEXT NOT NULL,detailed TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS budgets(month TEXT NOT NULL,category TEXT NOT NULL,currency TEXT NOT NULL,amount INTEGER NOT NULL,PRIMARY KEY(month,category,currency));
 CREATE TABLE IF NOT EXISTS sync_runs(id INTEGER PRIMARY KEY,created_at TEXT NOT NULL,item_id TEXT NOT NULL,status TEXT NOT NULL,added INTEGER NOT NULL,modified INTEGER NOT NULL,removed INTEGER NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
func (a *App) encrypt(token string) string {
	nonce := make([]byte, a.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(a.aead.Seal(nonce, nonce, []byte(token), nil))
}
func (a *App) decrypt(value string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	n := a.aead.NonceSize()
	if len(b) < n {
		return "", errors.New("invalid encrypted token")
	}
	plain, err := a.aead.Open(nil, b[:n], b[n:], nil)
	return string(plain), err
}

// Stage the entire update before committing. A pagination mutation restarts from the original cursor.
func (a *App) syncItem(ctx context.Context, id string, fetch func(context.Context, string) (Page, error)) error {
	var original string
	if err := a.db.QueryRow("SELECT cursor FROM items WHERE id=?", id).Scan(&original); err != nil {
		return err
	}
	for attempt := 0; attempt < 3; attempt++ {
		cursor := original
		pages := []Page{}
		restart := false
		for n := 0; n < 1000; n++ {
			p, err := fetch(ctx, cursor)
			if err != nil {
				var pe APIError
				if errors.As(err, &pe) && pe.Code == "TRANSACTIONS_SYNC_MUTATION_DURING_PAGINATION" {
					restart = true
					break
				}
				a.recordFailure(id)
				return err
			}
			pages = append(pages, p)
			if !p.More {
				break
			}
			if p.Next == cursor || n == 999 {
				a.recordFailure(id)
				return errors.New("invalid pagination response")
			}
			cursor = p.Next
		}
		if restart {
			continue
		}
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		added, modified, removed := 0, 0, 0
		for _, p := range pages {
			for _, ac := range p.Accounts {
				_, err = tx.Exec("INSERT INTO accounts VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,type=excluded.type,currency=excluded.currency", ac.ID, id, ac.Name, ac.Type, ac.Balances.Currency)
				if err != nil {
					break
				}
			}
			if err != nil {
				break
			}
			for _, t := range append(append([]Transaction{}, p.Added...), p.Modified...) {
				category := t.Category.Primary
				if category == "" {
					category = "UNCATEGORIZED"
				}
				currency := t.Currency
				if currency == "" {
					currency = t.Unofficial
				}
				if currency == "" {
					currency = "UNKNOWN"
				}
				name := t.Merchant
				if name == "" {
					name = t.Name
				}
				_, err = tx.Exec(`INSERT INTO transactions VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET account_id=excluded.account_id,date=excluded.date,name=excluded.name,amount=excluded.amount,currency=excluded.currency,pending=excluded.pending,category=excluded.category,detailed=excluded.detailed`, t.ID, id, t.AccountID, t.Date, name, int64(math.Round(t.Amount*100)), currency, t.Pending, category, t.Category.Detailed)
				if err != nil {
					break
				}
			}
			if err != nil {
				break
			}
			for _, r := range p.Removed {
				_, err = tx.Exec("DELETE FROM transactions WHERE id=? AND item_id=?", r.ID, id)
				if err != nil {
					break
				}
			}
			if err != nil {
				break
			}
			added += len(p.Added)
			modified += len(p.Modified)
			removed += len(p.Removed)
		}
		if err == nil {
			_, err = tx.Exec("UPDATE items SET cursor=? WHERE id=?", pages[len(pages)-1].Next, id)
		}
		if err == nil {
			_, err = tx.Exec("INSERT INTO sync_runs(created_at,item_id,status,added,modified,removed) VALUES(?,?,?,?,?,?)", time.Now().UTC().Format(time.RFC3339), id, "success", added, modified, removed)
		}
		if err != nil {
			tx.Rollback()
			a.recordFailure(id)
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return nil
	}
	a.recordFailure(id)
	return errors.New("transactions changed during pagination; please retry")
}
func (a *App) recordFailure(id string) {
	a.db.Exec("INSERT INTO sync_runs(created_at,item_id,status,added,modified,removed) VALUES(?,?,?,0,0,0)", time.Now().UTC().Format(time.RFC3339), id, "failed")
}
func excluded(category, detailed string) bool {
	return strings.HasPrefix(category, "TRANSFER") || category == "LOAN_PAYMENTS" || strings.Contains(detailed, "CREDIT_CARD_PAYMENT")
}
func spend(amount int64, pending bool, category, detailed string) int64 {
	if amount <= 0 || pending || excluded(category, detailed) {
		return 0
	}
	return amount
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil {
		fail(w, 400, "Invalid request")
		return false
	}
	if dec.Decode(new(any)) != io.EOF {
		fail(w, 400, "Invalid request")
		return false
	}
	return true
}
func validMonth(s string) bool { _, err := time.Parse("2006-01", s); return err == nil && len(s) == 7 }
func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	month := r.URL.Query().Get("month")
	if month == "" {
		month = time.Now().Format("2006-01")
	}
	if !validMonth(month) {
		fail(w, 400, "Invalid month")
		return
	}
	rows, err := a.db.Query("SELECT id,date,name,amount,currency,pending,category,detailed,account_id FROM transactions WHERE substr(date,1,7)=? ORDER BY date DESC,id", month)
	if err != nil {
		fail(w, 500, "Database unavailable")
		return
	}
	transactions := []map[string]any{}
	totals := map[string]int64{}
	categories := map[string]map[string]int64{}
	pendingTotals := map[string]int64{}
	for rows.Next() {
		var id, date, name, currency, category, detailed, account string
		var amount int64
		var pending bool
		if err = rows.Scan(&id, &date, &name, &amount, &currency, &pending, &category, &detailed, &account); err != nil {
			break
		}
		s := spend(amount, pending, category, detailed)
		totals[currency] += s
		if categories[currency] == nil {
			categories[currency] = map[string]int64{}
		}
		if s > 0 {
			categories[currency][category] += s
		}
		if pending && amount > 0 && !excluded(category, detailed) {
			pendingTotals[currency] += amount
		}
		transactions = append(transactions, map[string]any{"id": id, "date": date, "name": name, "amount": amount, "currency": currency, "pending": pending, "category": category, "excluded": excluded(category, detailed), "account_id": account})
	}
	rows.Close()
	if err != nil {
		fail(w, 500, "Could not read transactions")
		return
	}
	budgets := []map[string]any{}
	rows, err = a.db.Query("SELECT category,currency,amount FROM budgets WHERE month=?", month)
	if err != nil {
		fail(w, 500, "Could not read budgets")
		return
	}
	for rows.Next() {
		var cat, cur string
		var limit int64
		rows.Scan(&cat, &cur, &limit)
		budgets = append(budgets, map[string]any{"category": cat, "currency": cur, "limit": limit, "spent": categories[cur][cat]})
	}
	rows.Close()
	accounts := []map[string]string{}
	rows, err = a.db.Query("SELECT name,type,currency FROM accounts ORDER BY name")
	if err != nil {
		fail(w, 500, "Could not read accounts")
		return
	}
	for rows.Next() {
		var name, kind, cur string
		rows.Scan(&name, &kind, &cur)
		accounts = append(accounts, map[string]string{"name": name, "type": kind, "currency": cur})
	}
	rows.Close()
	months := []string{time.Now().Format("2006-01")}
	rows, err = a.db.Query("SELECT DISTINCT substr(date,1,7) FROM transactions ORDER BY 1 DESC")
	if err != nil {
		fail(w, 500, "Could not read months")
		return
	}
	for rows.Next() {
		var m string
		rows.Scan(&m)
		if m != months[0] {
			months = append(months, m)
		}
	}
	rows.Close()
	runs := []map[string]any{}
	rows, err = a.db.Query("SELECT created_at,status,added,modified,removed FROM sync_runs ORDER BY id DESC LIMIT 6")
	if err != nil {
		fail(w, 500, "Could not read sync history")
		return
	}
	for rows.Next() {
		var at, status string
		var ad, mo, re int
		rows.Scan(&at, &status, &ad, &mo, &re)
		runs = append(runs, map[string]any{"at": at, "status": status, "added": ad, "modified": mo, "removed": re})
	}
	rows.Close()
	writeJSON(w, 200, map[string]any{"mode": a.mode, "month": month, "months": months, "totals": totals, "pending_totals": pendingTotals, "categories": categories, "transactions": transactions, "budgets": budgets, "accounts": accounts, "sync_runs": runs})
}
func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/dashboard", a.dashboard)
	mux.HandleFunc("POST /api/budgets", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Month    string `json:"month"`
			Category string `json:"category"`
			Currency string `json:"currency"`
			Amount   int64  `json:"amount"`
		}
		if !readJSON(w, r, &b) {
			return
		}
		if !validMonth(b.Month) || len(b.Category) < 1 || len(b.Category) > 100 || len(b.Currency) > 10 || b.Currency == "" || b.Amount < 0 || b.Amount > 100000000 {
			fail(w, 400, "Enter a valid monthly budget")
			return
		}
		_, err := a.db.Exec("INSERT INTO budgets VALUES(?,?,?,?) ON CONFLICT(month,category,currency) DO UPDATE SET amount=excluded.amount", b.Month, b.Category, b.Currency, b.Amount)
		if err != nil {
			fail(w, 500, "Budget could not be saved")
			return
		}
		writeJSON(w, 200, map[string]bool{"saved": true})
	})
	mux.HandleFunc("POST /api/link-token", func(w http.ResponseWriter, r *http.Request) {
		if a.mode != "sandbox" {
			fail(w, 400, "Start in Sandbox mode to connect Plaid")
			return
		}
		var out map[string]any
		err := a.plaid.call(r.Context(), "/link/token/create", map[string]any{"client_name": "Budgetlane", "user": map[string]string{"client_user_id": "local-budgetlane-user"}, "products": []string{"transactions"}, "country_codes": []string{"US"}, "language": "en"}, &out)
		if err != nil {
			fail(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"link_token": out["link_token"]})
	})
	mux.HandleFunc("POST /api/exchange", func(w http.ResponseWriter, r *http.Request) {
		if a.mode != "sandbox" {
			fail(w, 400, "Plaid is available in Sandbox mode")
			return
		}
		var body struct {
			Token string `json:"public_token"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if body.Token == "" {
			fail(w, 400, "Missing token")
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		var out struct {
			Token string `json:"access_token"`
			ID    string `json:"item_id"`
		}
		if err := a.plaid.call(r.Context(), "/item/public_token/exchange", map[string]any{"public_token": body.Token}, &out); err != nil {
			fail(w, 502, err.Error())
			return
		}
		if out.ID == "" || out.Token == "" {
			fail(w, 502, "Invalid exchange response")
			return
		}
		_, err := a.db.Exec("INSERT INTO items(id,token) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET token=excluded.token", out.ID, a.encrypt(out.Token))
		if err != nil {
			fail(w, 500, "Connection could not be saved")
			return
		}
		writeJSON(w, 200, map[string]bool{"connected": true})
	})
	mux.HandleFunc("POST /api/sync", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.mode == "demo" {
			if err := a.demoSync(r.Context()); err != nil {
				fail(w, 500, "Demo sync failed")
				return
			}
			writeJSON(w, 200, map[string]bool{"synced": true})
			return
		}
		rows, err := a.db.Query("SELECT id,token FROM items WHERE id!='demo'")
		if err != nil {
			fail(w, 500, "Could not read connections")
			return
		}
		type item struct{ id, token string }
		items := []item{}
		for rows.Next() {
			var i item
			rows.Scan(&i.id, &i.token)
			items = append(items, i)
		}
		rows.Close()
		if len(items) == 0 {
			fail(w, 400, "Connect a Sandbox bank first")
			return
		}
		for _, i := range items {
			token, e := a.decrypt(i.token)
			if e != nil {
				fail(w, 500, "Cannot decrypt token; check encryption key")
				return
			}
			if err = a.syncItem(r.Context(), i.id, func(ctx context.Context, c string) (Page, error) { return a.plaid.page(ctx, token, c) }); err != nil {
				fail(w, 502, err.Error())
				return
			}
		}
		writeJSON(w, 200, map[string]bool{"synced": true})
	})
	sub, _ := fs.Sub(assets, "web")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.Split(r.Host, ":")[0]
		if host != "localhost" && host != "127.0.0.1" {
			fail(w, 403, "Use the localhost address")
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == "POST" {
			if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
				fail(w, 403, "Cross-origin requests are not allowed")
				return
			}
			if r.Header.Get("Content-Type") != "application/json" {
				fail(w, 415, "Use application/json")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	mode := env("APP_MODE", "demo")
	if mode != "demo" && mode != "sandbox" {
		log.Fatal("APP_MODE must be demo or sandbox")
	}
	path := env("DB_PATH", mode+".db")
	db, err := openDB(path)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err = os.Chmod(path, 0600); err != nil {
		log.Fatal(err)
	}
	a := &App{db: db, mode: mode}
	if mode == "sandbox" {
		key, err := base64.StdEncoding.DecodeString(os.Getenv("TOKEN_ENCRYPTION_KEY"))
		if err != nil || len(key) != 32 {
			log.Fatal("TOKEN_ENCRYPTION_KEY must be a base64-encoded 32-byte key")
		}
		block, _ := aes.NewCipher(key)
		a.aead, _ = cipher.NewGCM(block)
		a.plaid = &Plaid{os.Getenv("PLAID_CLIENT_ID"), os.Getenv("PLAID_SECRET"), &http.Client{Timeout: 20 * time.Second}}
		if a.plaid.ClientID == "" || a.plaid.Secret == "" {
			log.Fatal("Set PLAID_CLIENT_ID and PLAID_SECRET")
		}
	} else {
		if err = a.seedDemo(); err != nil {
			log.Fatal(err)
		}
	}
	port := env("PORT", "8080")
	server := &http.Server{Addr: "127.0.0.1:" + port, Handler: a.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 120 * time.Second}
	fmt.Printf("Budgetlane (%s): http://localhost:%s\n", mode, port)
	log.Fatal(server.ListenAndServe())
}
