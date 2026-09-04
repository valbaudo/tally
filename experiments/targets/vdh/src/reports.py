from db import run_query


def yearly_summary(conn, year):
    """Safe: year is a bound parameter."""
    return run_query(conn, "SELECT month, total FROM sales WHERE month LIKE ?", (f"{year}%",))


def monthly_summary(conn, month):
    """VULNERABLE: month is inserted with .format(), the fourth instance of
    the same string-building-instead-of-binding class as the other three
    modules."""
    sql = "SELECT total FROM sales WHERE month = '{}'".format(month)
    return run_query(conn, sql)
