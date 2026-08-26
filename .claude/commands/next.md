# What to work on next

Give an honest status, then a short recommendation. Do not pad it.

## Gather

1. `git status --short` and `git log --oneline -10` — what is in flight.
2. If the repository has a remote and `gh` is authenticated:
   `gh pr list --state open --json number,title,author,isDraft,reviewRequests,url`
   and `gh issue list --state open --json number,title,labels,url`.
   If it does not, say so and skip this rather than inventing work items.
3. `README.md` "Status" table — the layer states and what is marked unvalidated.
4. `CHANGELOG.md` `Unreleased` — what has landed since the last release.

## Report

```
## Done since last release
## In flight
## Pending, in order
## Blocked, and on what
```

## Rules

- **Distinguish "not started" from "not validated".** A layer that ships but has
  never seen production traffic is not the same as one that does not exist, and
  the README says which is which.
- **Name the biggest risk first**, even when it is not the most tractable item.
- Do not recommend work the user has already declined. Check the history.
- If a limitation is known and undocumented, that is a pending item.
