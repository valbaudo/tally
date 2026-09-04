# mdash (toy)

A miniature of MDASH's shape: an app with a real logic flaw, and a proving
plugin that only accepts a submission if the flaw was *actually exercised*
(MDASH's oracle is expensive/manual in the real protocol; here it's a
hand-written script standing in for that judgment).

## What it is

Two-endpoint HTTP app, Python stdlib only (`http.server`), two users:

- `POST /login` (`login_endpoint.py`) -- username+password in, opaque
  session token out. Not buggy.
- `GET /notes/<id>` (`notes_endpoint.py`) -- returns a user's private note
  given a valid session token. **Bug**: it checks that the token is valid
  (authenticated) but never checks that the token's owner matches `<id>`
  (authorization) -- a broken object-level access control / IDOR. Any
  logged-in user can read any other user's note by ID.

`app.py` wires the two endpoints into one server and toggles the fix via
`--fixed` / `MDASH_FIXED=1`, which adds the one missing ownership check.

## Ground truth

The flaw is `notes_endpoint.py`, the `if fixed and session_user_id != note_id`
line -- present only when `fixed=True`. Exploit: log in as `alice` (id 1),
request `/notes/2` (bob's note) with alice's own token.

## How to check it

```bash
cd experiments/targets/mdash
python3 exploit_check.py            # against the vulnerable app -> PASS, exit 0
python3 exploit_check.py --fixed    # against the patched app    -> FAIL, exit 1
```

`exploit_check.py` starts `app.py` itself (as a subprocess, on 127.0.0.1:8901), so
there's nothing else to run first.

## Pass condition (the proving plugin)

`exploit_check.py` declares PASS only if **both** hold:

1. **Negative control**: requesting `/notes/2` with no token at all returns
   401 -- proves the endpoint has real authentication, so a later 200 isn't
   just "auth is off".
2. **The exploit**: alice's own valid token against `/notes/2` returns
   HTTP 200 with a body whose `note` field equals bob's actual secret
   string, verbatim -- not merely "some 200", not a crafted decoy. A
   checker that only looked at the status code would be fooled by any
   handler that returns 200 for `/notes/2`, including a buggy one that
   returns empty data; comparing against the real secret closes that hole.

Anything else (control broken, or alice can't read bob's note) is FAIL.

## Real output (captured 2026-09-04)

```
$ python3 exploit_check.py
negative control (no token -> 401): ok (status=401)
alice's token against /notes/2 (bob's note): status=200 body={'note': "bob's secret: the safe combo is 16-23-42"}
PASS: authorization bypass exercised -- alice read bob's real note: "bob's secret: the safe combo is 16-23-42"
exit=0

$ python3 exploit_check.py --fixed
negative control (no token -> 401): ok (status=401)
alice's token against /notes/2 (bob's note): status=403 body={'error': 'forbidden: not your note'}
FAIL: alice could not read bob's note -- ownership check holds
exit=1
```

Both runs complete in ~0.1s.

## Pinned

Python 3.14.7, stdlib only (`http.server`, `urllib`, `json`, `secrets`) -- no
Flask, no third-party packages, no network beyond `127.0.0.1`.
