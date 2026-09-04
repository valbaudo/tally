"""GET /notes/<id> -- the vulnerable endpoint.

The bug: it checks that the caller has *a* valid session (authentication)
but never checks that the session's own user id matches the note id being
requested (authorization) -- a textbook broken object-level authorization /
IDOR. Any logged-in user can read any other user's note.

`fixed=True` applies the one-line correction: reject the request unless the
session's user id equals the requested note id.
"""
from users import USERS_BY_ID


def handle_notes(note_id, token, sessions, fixed):
    """Returns (status_code, response_dict)."""
    session_user_id = sessions.get(token)
    if session_user_id is None:
        return 401, {"error": "not authenticated"}

    entry = USERS_BY_ID.get(note_id)
    if entry is None:
        return 404, {"error": "no such note"}

    if fixed and session_user_id != note_id:
        return 403, {"error": "forbidden: not your note"}

    # BUG (when not fixed): returns the note regardless of who owns it.
    _, user = entry
    return 200, {"note": user["note"]}
