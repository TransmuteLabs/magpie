#!/usr/bin/env python3
"""Require the existing Test workflow for the exact release commit."""

import argparse
import json
import re
import subprocess
import time
from urllib.parse import urlencode


def latest_test(payload, repo, sha):
    if not isinstance(payload, dict):
        raise ValueError("GitHub did not return a workflow response")
    runs = payload.get("workflow_runs")
    if not isinstance(runs, list):
        raise ValueError("GitHub did not return workflow runs")
    if payload.get("total_count", len(runs)) != len(runs):
        raise ValueError("GitHub returned an incomplete workflow run listing")
    # GitHub returns either the bare path or a path qualified by its
    # workflow ref. The source SHA/repository are still checked below.
    allowed_paths = {
        ".github/workflows/test.yml",
        ".github/workflows/test.yml@devzeus",
        ".github/workflows/test.yml@refs/heads/devzeus",
        f".github/workflows/test.yml@{sha}",
    }
    matching = []
    for run in runs:
        if not isinstance(run, dict):
            raise ValueError("GitHub returned a malformed workflow run")
        origin = run.get("head_repository")
        path = run.get("path")
        if (
            run.get("head_sha") == sha
            and run.get("head_branch") == "devzeus"
            and run.get("event") in ("push", "workflow_dispatch")
            and isinstance(path, str) and path in allowed_paths
            and isinstance(origin, dict)
            and str(origin.get("full_name", "")).lower() == repo.lower()
        ):
            if any(type(run.get(key)) is not int or run[key] < 1 for key in ("id", "run_attempt")):
                raise ValueError("GitHub returned a run without its identity/attempt")
            matching.append(run)
    return max(matching, key=lambda run: (run["id"], run["run_attempt"]), default=None)


def fetch_runs(repo, sha):
    query = urlencode({"head_sha": sha, "branch": "devzeus", "per_page": 100})
    result = subprocess.run(
        ["gh", "api", f"repos/{repo}/actions/workflows/test.yml/runs?{query}"],
        capture_output=True, text=True, timeout=60, check=True,
    )
    return json.loads(result.stdout)


def wait_for_test(repo, sha, timeout, fetch=fetch_runs, clock=time.monotonic, sleep=time.sleep):
    deadline = clock() + timeout
    while True:
        run = latest_test(fetch(repo, sha), repo, sha)
        if run is not None and run.get("status") == "completed":
            if run.get("conclusion") != "success":
                raise ValueError(f"Test run {run['id']} attempt {run['run_attempt']} ended with {run.get('conclusion')!r}")
            print(f"Test passed for {sha}: run {run['id']}, attempt {run['run_attempt']}", flush=True)
            return
        remaining = deadline - clock()
        if remaining <= 0:
            raise TimeoutError(f"No successful Test workflow for {sha} before the release deadline")
        print(f"Waiting for Test on {sha}", flush=True)
        sleep(min(30, remaining))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("repo")
    parser.add_argument("sha")
    parser.add_argument("--timeout", type=int, default=7200)
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", args.repo):
        parser.error("expected owner/repository")
    if not re.fullmatch(r"[0-9a-f]{40}", args.sha) or args.timeout < 1:
        parser.error("expected an exact commit SHA and positive timeout")
    try:
        wait_for_test(args.repo, args.sha, args.timeout)
    except (ValueError, TimeoutError, subprocess.SubprocessError) as error:
        # Do not echo gh stderr or environment: the token belongs to the job.
        raise SystemExit(f"Release verification refused: {type(error).__name__}: {error}") from None


if __name__ == "__main__":
    main()
