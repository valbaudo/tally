"""The outside edge: request handlers a caller reaches from off the machine.

This module builds no SQL of its own — it dispatches. It exists so that
"reachable from outside" is a question with an answer: every vulnerable
function below sits at the end of a real call path that starts here.

Handlers take the parsed request as a mapping, not as a bare string, which is
also why probing a handler directly with a payload does nothing: the bug is
one hop further in, and that is where a finding belongs.
"""
from orders import get_orders_for_customer, search_orders_by_status
from reports import monthly_summary, yearly_summary
from search import list_categories, search_products
from users import get_user_by_id, get_user_by_name


def handle_user_lookup(conn, request):
    """GET /user?name=... — public directory search."""
    return get_user_by_name(conn, request["name"])


def handle_user_profile(conn, request):
    """GET /user/<id> — profile by primary key."""
    return get_user_by_id(conn, request["id"])


def handle_order_search(conn, request):
    """GET /orders?status=... — order list filtered by status."""
    return search_orders_by_status(conn, request["status"])


def handle_order_history(conn, request):
    """GET /orders/mine — the caller's own orders."""
    return get_orders_for_customer(conn, request["customer_id"])


def handle_product_search(conn, request):
    """GET /products?q=... — storefront search."""
    return search_products(conn, request["q"])


def handle_catalog(conn, request):
    """GET /categories — the full category list."""
    return list_categories(conn)


def handle_month_report(conn, request):
    """GET /reports/month?month=... — one month's revenue."""
    return monthly_summary(conn, request["month"])


def handle_year_report(conn, request):
    """GET /reports/year?year=... — one year's revenue."""
    return yearly_summary(conn, request["year"])
