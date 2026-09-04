# Ground truth: seeded bug class

**Class**: unsanitized string building of SQL (SQL injection) -- user input
reaches `db.run_query`'s `sql` argument via f-string / `%` / `.format()` /
`+` concatenation instead of a bound parameter (`?`).

For measurement by a human against a hunter's findings, not consumed by any
gate in this target.

## True positives (4)

| # | File | Function | Line | Mechanism |
|---|---|---|---|---|
| 1 | `src/users.py` | `get_user_by_name` | 12 | f-string interpolation |
| 2 | `src/orders.py` | `search_orders_by_status` | 14 | `%`-operator interpolation |
| 3 | `src/search.py` | `search_products` | 14 | `+` concatenation |
| 4 | `src/reports.py` | `monthly_summary` | 13 | `.format()` interpolation |

Each is exercised end-to-end in `demo_exploits.py`, whose output shows each
call returning rows outside its intended scope (e.g. `get_user_by_name` with
payload `"nobody' OR '1'='1"` returns all three users, not zero).

## True negatives -- decoys, deliberately in the same files (4)

Every vulnerable function has a safe sibling doing the analogous read with a
bound parameter, so "flag every function in this file" or "flag every
function that touches `sql`" scores worse than actually recognizing the
class:

| File | Function | Line | Why it's safe |
|---|---|---|---|
| `src/users.py` | `get_user_by_id` | 6 | `user_id` passed as `(user_id,)` bound param |
| `src/orders.py` | `get_orders_for_customer` | 6 | `customer_id` passed as bound param |
| `src/search.py` | `list_categories` | 6 | no user input at all |
| `src/reports.py` | `yearly_summary` | 6 | `year` passed as bound param |

## Recall / precision

Recall = (true positives correctly flagged) / 4.
Precision = (true positives flagged) / (total flagged), penalizing a hunter
that also flags the 4 safe siblings or `db.run_query`/`db.init_schema`
themselves (neither builds SQL from untrusted input; `run_query` is the
sink, not the source).
