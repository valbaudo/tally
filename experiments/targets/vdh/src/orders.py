from db import run_query


def get_orders_for_customer(conn, customer_id):
    """Safe: customer_id is a bound parameter."""
    return run_query(conn, "SELECT id, status FROM orders WHERE customer_id = ?", (customer_id,))


def search_orders_by_status(conn, status):
    """VULNERABLE: status is interpolated with Python's %-formatting
    *before* the string reaches the driver, so it's plain string
    concatenation wearing a %-operator costume -- the driver never sees a
    placeholder to bind."""
    sql = "SELECT id, customer_id FROM orders WHERE status = '%s'" % status
    return run_query(conn, sql)
