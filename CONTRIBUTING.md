# Contributing

## Getting set up

The only prerequisite is Go. `controller-gen` and `setup-envtest` are Go tool
dependencies, so a fresh checkout needs no global installs.

```console
git clone https://github.com/waveoffai/waveoff && cd waveoff
make            # generate, fmt, vet, checks, unit and API-server tests, build
make help       # every target, with what it does
```

Docker is needed only for `make test-e2e`. Python and Node are needed only for
`make test-integration`.

To see the whole thing working end to end on a throwaway cluster:

```console
make quickstart
```

## Before you open a pull request

```console
make            # unit, property and API-server tests
make lint-go    # golangci-lint
```

Then decide from [`.claude/rules/tests.md`](.claude/rules/tests.md) whether your
change needs `make test-integration` or `make test-e2e`. If it touches the
recorder, replay, the webhooks, injection or either traffic router, it does.

The pull request template asks which suites you ran. Answer it honestly,
including "none of these could have caught this" when that is the case — that
answer starts a more useful conversation than a tick in the wrong box.

## What review looks for

In order:

1. **Does it fail open?** An error path returning `nil`, a default that
   promotes, an unclassified tool passed through, a missing verdict read as a
   pass. This project decides whether to ship software; failing open ships the
   exact change nobody could check.
2. **Does it change what a digest means?** Moving a field in or out of
   `behaviorDigest` changes the identity of every manifest already issued. That
   is breaking even when the schema is untouched. See
   [`.claude/rules/digests.md`](.claude/rules/digests.md).
3. **Does it weaken a stated guarantee**, and if so, is the weakening written
   down where a user will find it rather than only in the PR?
4. **Could the test have caught the bug?** A test that exercises the fixed path
   and would also pass on the broken one is not a regression test.

## Conventions

**Prose is British English. Identifiers are not.** The CRD field is
`behaviorDigest`, the interface method is `Analyze`, the MCP notification is
`notifications/initialized`. Those name things outside this repository and are
not ours to respell. `project-words.txt` teaches the spell checker the rest;
adding a word to it is meant to be an unremarkable commit.

**A failed gate waves off a candidate.** It does not "fail" or "abort" it. The
wave-off is the decision and the rollback is the implementation — you can wave
off a candidate that was never rolled out, and roll back for reasons that have
nothing to do with a gate.

**Comments say why, not what.** The code says what it does. A comment earns its
place by recording the reason a decision was made, or the bug that made it
necessary. Several comments in this repository name the specific failure they
prevent; that is the standard.

**Error messages are read at 2am.** A rejection should name the command that
fixes it. "invalid" has wasted the one chance you had to help.

**Assert limitations.** When something cannot be fixed now, the convention is a
test that pins the current behaviour with a comment explaining what would
improve it — so that whoever does improve it has to come and say so. See
`TestPodLevelPolicyCannotHideTheToolPlaneFromTheAgent`.

## Things that will fail CI

- Telemetry of any kind. No phone-home, no version check, no usage statistics.
  Not behind a flag. `make lint-boundary` greps for it.
- Metering or gating recording, replay or corpus size.
- "Enterprise", "upgrade to unlock", or the GitHub organisation handle from the
  module path, in any user-facing string. The handle is legal in an import path
  and nowhere else.
- A GitHub Action pinned to a tag rather than a commit SHA
  (`make check-pins`; `make update-pins` fixes it). A mutable action tag is the
  same hazard this project refuses in a container image, and holding that line
  in the README while not holding it in CI would be indefensible.
- Generated files that are out of date (`make generate`).
- Sample manifests whose digests are stale (`waveoff verify --write`).

## Reporting a security issue

Do not open a public issue. See [SECURITY.md](SECURITY.md).
