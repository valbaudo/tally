from db import run_query


def list_categories(conn):
    """Safe: no user input in the query at all."""
    return run_query(conn, "SELECT DISTINCT name FROM products")


def search_products(conn, keyword):
    """VULNERABLE: keyword is glued into the LIKE pattern with plain string
    concatenation, same class as users.get_user_by_name and
    orders.search_orders_by_status -- unsanitized input reaching the SQL
    text instead of a bound parameter."""
    sql = "SELECT id, name FROM products WHERE name LIKE '%" + keyword + "%'"
    return run_query(conn, sql)
