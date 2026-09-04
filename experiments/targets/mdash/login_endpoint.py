"""POST /login -- the one endpoint that is NOT buggy. Checks username +
password, mints an opaque session token on success."""
import secrets

from users import USERS


def handle_login(body, sessions):
    """body: dict with 'username' and 'password'.
    Returns (status_code, response_dict)."""
    username = body.get("username", "")
    password = body.get("password", "")
    user = USERS.get(username)
    if user is None or user["password"] != password:
        return 401, {"error": "invalid credentials"}
    token = secrets.token_hex(16)
    sessions[token] = user["id"]
    return 200, {"token": token}
