package main

import (
	"database/sql"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	db, err := sql.Open("sqlite", os.Args[1])
	if err != nil {
		panic(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE, name TEXT, status TEXT CHECK (status IN ('active','suspended','deleted')), created_at TEXT NOT NULL)`,
		`CREATE TABLE sessions (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id), logged_in_at TEXT NOT NULL, ip TEXT, device TEXT)`,
		`CREATE TABLE products (id INTEGER PRIMARY KEY, sku TEXT UNIQUE, title TEXT, price_cents INTEGER)`,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id), total_cents INTEGER NOT NULL, state TEXT CHECK (state IN ('pending','paid','shipped','cancelled')), placed_at TEXT)`,
		`CREATE TABLE order_items (id INTEGER PRIMARY KEY, order_id INTEGER NOT NULL REFERENCES orders(id), product_id INTEGER NOT NULL REFERENCES products(id), qty INTEGER, unit_price_cents INTEGER)`,
		`CREATE INDEX idx_sessions_user ON sessions(user_id)`,
		`CREATE INDEX idx_sessions_time ON sessions(logged_in_at)`,
		`CREATE VIEW v_active_users AS SELECT * FROM users WHERE status='active'`,
		`INSERT INTO users VALUES (1,'a@x.com','Ana','active','2026-01-01'),(2,'b@x.com','Beto','suspended','2026-02-01')`,
		`INSERT INTO sessions VALUES (1,1,'2026-04-10','1.1.1.1','ios'),(2,1,'2026-07-01','1.1.1.1','web'),(3,2,'2026-04-12','2.2.2.2','web')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			panic(s + ": " + err.Error())
		}
	}
}
