import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import release_gate as gate

REPO = "TransmuteLabs/magpie"
SHA = "a" * 40


def run(**changes):
    value = {"id": 30, "run_attempt": 1, "head_sha": SHA, "head_branch": "devzeus",
             "event": "push", "path": ".github/workflows/test.yml",
             "head_repository": {"full_name": REPO}, "status": "completed", "conclusion": "success"}
    value.update(changes)
    return value


class ReleaseGateTest(unittest.TestCase):
    def test_accepts_only_exact_source(self):
        for changes in ({"head_sha": "b" * 40}, {"head_branch": "main"},
                        {"event": "pull_request"}, {"path": ".github/workflows/fork-release.yml"},
                        {"head_repository": {"full_name": "foreign/magpie"}}, {"head_repository": None}):
            with self.subTest(changes=changes):
                self.assertIsNone(gate.latest_test({"workflow_runs": [run(**changes)]}, REPO, SHA))
        for event in ("push", "workflow_dispatch"):
            self.assertEqual(gate.latest_test({"workflow_runs": [run(event=event)]}, REPO, SHA)["id"], 30)

    def test_qualified_path_preserves_workflow_and_ref(self):
        for suffix in ("", "@devzeus", "@refs/heads/devzeus", "@" + SHA):
            with self.subTest(suffix=suffix):
                self.assertIsNotNone(gate.latest_test({"workflow_runs": [run(path=".github/workflows/test.yml" + suffix)]}, REPO, SHA))
        for path in (".github/workflows/test.yml@main", ".github/workflows/test.yml@refs/heads/main",
                     ".github/workflows/test.yml@" + "b" * 40, ".github/workflows/test.yml@",
                     ".github/workflows/test.yml@devzeus@main", ".github/workflows/other.yml@devzeus"):
            with self.subTest(path=path):
                self.assertIsNone(gate.latest_test({"workflow_runs": [run(path=path)]}, REPO, SHA))

    def test_qualified_new_failure_still_supersedes_old_green(self):
        payload = {"workflow_runs": [run(id=29), run(path=".github/workflows/test.yml@devzeus", conclusion="failure")]}
        with self.assertRaisesRegex(ValueError, "failure"):
            gate.wait_for_test(REPO, SHA, 1, fetch=lambda *_: payload)

    def test_newest_failure_cannot_reuse_older_green(self):
        payload = {"workflow_runs": [run(id=29), run(conclusion="failure")]}
        with self.assertRaisesRegex(ValueError, "failure"):
            gate.wait_for_test(REPO, SHA, 1, fetch=lambda *_: payload)

    def test_rerun_attempt_is_not_previous_success(self):
        payload = {"workflow_runs": [run(), run(run_attempt=2, conclusion="cancelled")]}
        with self.assertRaisesRegex(ValueError, "cancelled"):
            gate.wait_for_test(REPO, SHA, 1, fetch=lambda *_: payload)

    def test_all_non_success_terminal_conclusions_refuse(self):
        for conclusion in ("failure", "cancelled", "skipped", "neutral", "timed_out", "action_required", None):
            with self.subTest(conclusion=conclusion), self.assertRaises(ValueError):
                gate.wait_for_test(REPO, SHA, 1, fetch=lambda *_: {"workflow_runs": [run(conclusion=conclusion)]})

    def test_missing_pending_and_newer_pending_timeout(self):
        for runs in ([], [run(status="queued")], [run(id=29), run(status="in_progress")]):
            clock = iter([0, 1])
            with self.subTest(runs=runs), self.assertRaises(TimeoutError):
                gate.wait_for_test(REPO, SHA, 1, fetch=lambda *_: {"workflow_runs": runs}, clock=lambda: next(clock))

    def test_queued_then_completed(self):
        payloads = iter([{"workflow_runs": []}, {"workflow_runs": [run(status="in_progress")]}, {"workflow_runs": [run()]}])
        waits = []
        gate.wait_for_test(REPO, SHA, 60, fetch=lambda *_: next(payloads), clock=lambda: 0, sleep=waits.append)
        self.assertEqual(waits, [30, 30])

    def test_unreadable_is_not_success(self):
        for payload in ({}, [], {"workflow_runs": None}, {"workflow_runs": [None]}, {"workflow_runs": [run(id=None)]},
                        {"workflow_runs": [run(id=True)]}, {"workflow_runs": [run(run_attempt=0)]},
                        {"workflow_runs": [run()], "total_count": 101}):
            with self.subTest(payload=payload), self.assertRaises(ValueError):
                gate.latest_test(payload, REPO, SHA)
        with patch.object(gate.subprocess, "run", side_effect=subprocess.CalledProcessError(1, ["gh"])):
            with self.assertRaises(subprocess.CalledProcessError):
                gate.fetch_runs(REPO, SHA)

    def test_api_selects_workflow_and_commit(self):
        with patch.object(gate.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, stdout=json.dumps({"workflow_runs": []}))) as call:
            gate.fetch_runs(REPO, SHA)
        command = call.call_args.args[0]
        self.assertIn("/actions/workflows/test.yml/runs?", command[-1])
        self.assertIn("head_sha=" + SHA, command[-1])
        self.assertEqual(call.call_args.kwargs["timeout"], 60)

    def test_publishing_requires_gate(self):
        workflow = Path(__file__).resolve().parents[1].joinpath(".github/workflows/fork-release.yml").read_text()
        release = workflow.split("\n  release:\n", 1)[1]
        self.assertIn("needs: [check, app, verified]", release)
        self.assertIn("python3 build/release_gate.py", workflow)
        self.assertIn('"$GITHUB_REPOSITORY" "$GITHUB_SHA"', workflow)
        self.assertIn("actions: read", workflow)

    def test_command_refuses_failed_tests_and_accepts_success(self):
        # Exercise the installed command boundary without a GitHub connection
        # or a developer token. The fake gh is the entire API fixture.
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            executable = root / "gh"
            executable.write_text("#!/usr/bin/env python3\nfrom pathlib import Path\nprint(Path(__file__).with_name('reply.json').read_text())\n")
            executable.chmod(0o700)
            env = {"PATH": str(root) + os.pathsep + os.defpath, "HOME": directory}
            command = [sys.executable, str(Path(gate.__file__).resolve()), REPO, SHA, "--timeout", "1"]
            for suffix in ("", "@devzeus", "@refs/heads/devzeus", "@" + SHA):
                for conclusion in ("failure", "success", "cancelled"):
                    with self.subTest(conclusion=conclusion, suffix=suffix):
                        value = run(conclusion=conclusion, path=".github/workflows/test.yml" + suffix)
                        (root / "reply.json").write_text(json.dumps({"total_count": 1, "workflow_runs": [value]}))
                        result = subprocess.run(command, env=env, capture_output=True, text=True, timeout=10)
                        self.assertEqual(result.returncode == 0, conclusion == "success", result.stderr)


if __name__ == "__main__":
    unittest.main()
