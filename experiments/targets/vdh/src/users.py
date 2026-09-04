from db import run_query


def get_user_by_id(conn, user_id):
    """Safe: user_id is passed as a bound parameter."""
    return run_query(conn, "SELECT id, name, is_admin FROM users WHERE id = ?", (user_id,))


def get_user_by_name(conn, username):
    """VULNERABLE: username is spliced directly into the SQL text via an
    f-string, so a value like `' OR '1'='1` changes the query's meaning."""
    sql = f"SELECT id, name, is_admin FROM users WHERE name = '{username}'"
    return run_query(conn, sql)
