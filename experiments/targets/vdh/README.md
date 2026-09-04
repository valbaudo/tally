# vdh (toy)

A small toy repo with the same bug **class** seeded four times in four
different files/functions, plus four safe siblings doing the analogous
operation correctly -- since VDH hunts by class (not by single known
location), the target needs more than one instance to make "hunting" mean
anything, and needs decoys so flagging everything doesn't trivially win.

**There is deliberately no gate here.** `GROUND_TRUTH.md` is for a human to
score a hunter's report against, per the ticket -- not something the agent
or a script consults.

## What it is

```
src/db.py        run_query() -- the shared sink, correct itself
src/users.py     get_user_by_id (safe)   / get_user_by_name (VULNERABLE)
src/orders.py    get_orders_for_customer (safe) / search_orders_by_status (VULNERABLE)
src/search.py    list_categories (safe)  / search_products (VULNERABLE)
src/reports.py   yearly_summary (safe)   / monthly_summary (VULNERABLE)
```

Bug class: SQL built by splicing untrusted input into the query text
(f-string / `%` / `.format()` / `+`) instead of passing it as a bound `?`
parameter to `sqlite3`. Four instances, four different splicing mechanisms,
so a hunter matching on syntax (e.g. only grepping for f-strings) misses
some.

## Ground truth

See `GROUND_TRUTH.md`: exact file:line for all 4 true positives and the 4
safe decoys, for computing a hunter's recall and precision by hand.

## Proof the bugs are real (not a gate)

```bash
cd experiments/targets/vdh
python3 demo_exploits.py
```

This is diagnostic output for a human, not a pass/fail check -- it exists so
the seeded bugs are demonstrably exploitable rather than merely asserted.
Runs in well under a second, no network, no external DB (in-memory sqlite3).

### Real output (captured 2026-09-04)

```
--- src/users.py: get_user_by_name ---
  safe get_user_by_id(3):           [(3, 'root', 1)]
  vulnerable get_user_by_name("nobody' OR '1'='1"):
    -> [(1, 'alice', 0), (2, 'bob', 0), (3, 'root', 1)]  (dumps every user, not just 'nobody')

--- src/orders.py: search_orders_by_status ---
  safe get_orders_for_customer(1):  [(1, 'shipped'), (2, 'pending')]
  vulnerable search_orders_by_status("x' OR '1'='1"):
    -> [(1, 1), (2, 1), (3, 2)]  (dumps all orders, not just status='x')

--- src/search.py: search_products ---
  safe list_categories():           [('widget',), ('gadget',)]
  vulnerable search_products("%' UNION SELECT id, name FROM users --"):
    -> [(1, 'alice'), (1, 'widget'), (2, 'bob'), (2, 'gadget'), (3, 'root')]  (leaks the users table through a products query)

--- src/reports.py: monthly_summary ---
  safe yearly_summary('2026'):      [('2026-01', 100), ('2026-02', 200)]
  vulnerable monthly_summary("2026-01' OR '1'='1"):
    -> [(100,), (200,)]  (dumps every month's total, not just one)

All four vulnerable calls returned rows outside their intended scope; all four safe siblings did not.
```

Each vulnerable call returns rows outside the scope its argument asked for;
each safe sibling, given the equivalent request, does not.

## Pinned

Python 3.14.7, stdlib `sqlite3` only. No network, no external services.
