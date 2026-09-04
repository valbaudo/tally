"""Shared query-execution helper. This module is the sink, not the source
of the bug -- it's a thin, correct wrapper. The bug class lives in *how the
callers build the SQL string* before it reaches here."""
import sqlite3


def run_query(conn: sqlite3.Connection, sql: str, params=()):
    cur = conn.execute(sql, params)
    return cur.fetchall()


def init_schema(conn: sqlite3.Connection):
    conn.executescript(
        """
        CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT, is_admin INTEGER);
        CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_id INTEGER, status TEXT);
        CREATE TABLE products (id INTEGER PRIMARY KEY, name TEXT);
        CREATE TABLE sales (id INTEGER PRIMARY KEY, month TEXT, total INTEGER);

        INSERT INTO users VALUES (1, 'alice', 0);
        INSERT INTO users VALUES (2, 'bob', 0);
        INSERT INTO users VALUES (3, 'root', 1);

        INSERT INTO orders VALUES (1, 1, 'shipped');
        INSERT INTO orders VALUES (2, 1, 'pending');
        INSERT INTO orders VALUES (3, 2, 'shipped');

        INSERT INTO products VALUES (1, 'widget');
        INSERT INTO products VALUES (2, 'gadget');

        INSERT INTO sales VALUES (1, '2026-01', 100);
        INSERT INTO sales VALUES (2, '2026-02', 200);
        """
    )
    conn.commit()
