#!/usr/bin/env python3
"""Refuse ${{ }} expressions inside workflow `run:` scripts.

An expression is substituted into the script text before a shell ever sees it,
so any value an attacker can influence becomes code rather than data. The safe
form is always the same: bind it in `env:` and reference the shell variable.

This checks the shape rather than a list of known-tainted fields, because that
list grows and nobody revisits it. A workflow author who genuinely wants a
trusted expression in a script can still bind it through `env:`, which costs two
lines and reads more clearly anyway.
"""
import pathlib
import re
import sys

RUN = re.compile(r"^\s*-?\s*run:\s*(\|-?|>-?)?\s*(.*)$")


def offending_lines(path: pathlib.Path):
    lines = path.read_text().split("\n")
    inside = False
    indent = 0
    for number, line in enumerate(lines, start=1):
        match = RUN.match(line)
        if match:
            inside = True
            indent = len(line) - len(line.lstrip())
            if "${{" in (match.group(2) or ""):
                yield number, line.strip()
            continue
        if not inside:
            continue
        stripped = line.strip()
        if stripped and (len(line) - len(line.lstrip())) <= indent:
            inside = False
            continue
        if "${{" in line:
            yield number, stripped


def main() -> int:
    failed = False
    for path in sorted(pathlib.Path(".github/workflows").glob("*.y*ml")):
        for number, text in offending_lines(path):
            print(
                f"{path}:{number}: an expression is interpolated into a shell script.\n"
                f"    {text}\n"
                "    Bind it with `env:` and use the shell variable instead; an expansion "
                "here is code, not data.",
                file=sys.stderr,
            )
            failed = True
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
