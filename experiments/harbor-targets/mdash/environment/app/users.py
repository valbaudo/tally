"""In-memory user store shared by both endpoints. Not a real database --
this is a toy target, kept to stdlib on purpose."""

USERS = {
    "alice": {"id": 1, "password": "alice-pw", "note": "alice's secret: the launch code is 4815"},
    "bob":   {"id": 2, "password": "bob-pw",   "note": "bob's secret: the safe combo is 16-23-42"},
}

USERS_BY_ID = {u["id"]: (name, u) for name, u in USERS.items()}
