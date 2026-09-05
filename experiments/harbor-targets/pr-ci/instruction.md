A git repository is checked out at `/app/repo`. Its CI check is red:

```
cd /app/repo && ./ci.sh
```

fails, because `average()` in `calc.py` is wrong.

Fix `calc.py` so that `./ci.sh` exits 0, then hand over your fix **as a
unified diff**, written to the absolute path `/app/outputs/fix.patch`:

```
cd /app/repo && git diff > /app/outputs/fix.patch
```

Rules:

- `/app/outputs/fix.patch` is the only thing that is handed over. Nothing else you
  do in this container is looked at.
- The patch is applied to a pristine copy of the repo, and only its changes
  to `calc.py` are kept. `test_calc.py` and `ci.sh` are always the original
  ones — editing them changes nothing.
- Do not commit, push, or open a PR. Producing the diff is the whole job.
