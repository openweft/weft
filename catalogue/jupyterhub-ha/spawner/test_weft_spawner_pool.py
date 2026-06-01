"""Unit tests for ``WeftSpawner.stop_many`` — the parallel bulk-stop
path used by the Hub admin "Stop all" action.

We don't run a real Hub here — the CI gate is ``py_compile`` + this
file as a plain unittest module. Each test mocks ``subprocess.run``
to count concurrent calls and assert the pool obeys ``max_workers``
and the user-count bound.
"""

from __future__ import annotations

import os
import sys
import threading
import time
import types
import unittest
from unittest import mock

# Import the module under test from this directory regardless of
# where pytest / py_compile happens to invoke us from.
HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

import weft_spawner  # noqa: E402  — path injected above


class _ConcurrencyTracker:
    """Tracks how many ``subprocess.run`` calls are in flight at once.

    Each fake invocation increments a counter, sleeps long enough for
    the scheduler to actually overlap workers, then decrements. We
    record the high-water mark so assertions can pin the bound.
    """

    def __init__(self, sleep_s: float = 0.05) -> None:
        self._lock = threading.Lock()
        self.in_flight = 0
        self.peak = 0
        self.calls = 0
        self.sleep_s = sleep_s
        # Capture every command for assertions about flag construction.
        self.commands: list[list[str]] = []

    def __call__(self, cmd, capture_output=True, text=True, check=False):
        with self._lock:
            self.in_flight += 1
            self.peak = max(self.peak, self.in_flight)
            self.calls += 1
            self.commands.append(list(cmd))
        try:
            # The sleep is what gives the pool a chance to overlap —
            # without it, each call returns before the next worker
            # starts and ``peak`` would always be 1.
            time.sleep(self.sleep_s)
        finally:
            with self._lock:
                self.in_flight -= 1
        return types.SimpleNamespace(returncode=0, stdout="", stderr="")


class StopManyTests(unittest.TestCase):
    def test_respects_max_workers(self) -> None:
        """With 50 users and max_workers=8, peak concurrency stays ≤ 8."""
        users = [f"u{i}" for i in range(50)]
        tracker = _ConcurrencyTracker()
        with mock.patch.object(weft_spawner.subprocess, "run", tracker):
            results = weft_spawner.WeftSpawner.stop_many(
                users,
                project_uuid="proj-uuid",
                max_workers=8,
            )
        self.assertEqual(tracker.calls, 50)
        self.assertLessEqual(
            tracker.peak,
            8,
            f"peak concurrency {tracker.peak} exceeded max_workers=8",
        )
        # Each user should have an outcome ; all OK since the fake
        # subprocess returned returncode=0.
        self.assertEqual(len(results), 50)
        for name in users:
            self.assertIn(name, results)
            self.assertTrue(results[name].ok)

    def test_clamped_to_user_count(self) -> None:
        """4 users with max_workers=16 should peak at ≤ 4 (not waste threads)."""
        users = ["a", "b", "c", "d"]
        tracker = _ConcurrencyTracker(sleep_s=0.1)
        with mock.patch.object(weft_spawner.subprocess, "run", tracker):
            weft_spawner.WeftSpawner.stop_many(
                users,
                project_uuid="proj-uuid",
                max_workers=16,
            )
        self.assertEqual(tracker.calls, 4)
        self.assertLessEqual(
            tracker.peak,
            4,
            f"peak {tracker.peak} > user count 4",
        )

    def test_hard_cap_at_sixteen(self) -> None:
        """Even if a caller passes max_workers=100, the pool is clamped to 16."""
        users = [f"u{i}" for i in range(40)]
        tracker = _ConcurrencyTracker()
        with mock.patch.object(weft_spawner.subprocess, "run", tracker):
            weft_spawner.WeftSpawner.stop_many(
                users,
                project_uuid="proj-uuid",
                max_workers=100,
            )
        self.assertLessEqual(
            tracker.peak,
            16,
            f"peak {tracker.peak} > hard cap 16",
        )

    def test_empty_user_list_no_pool_spawn(self) -> None:
        """Empty input is a no-op — don't even open a pool."""
        tracker = _ConcurrencyTracker()
        with mock.patch.object(weft_spawner.subprocess, "run", tracker):
            results = weft_spawner.WeftSpawner.stop_many(
                [],
                project_uuid="proj-uuid",
                max_workers=16,
            )
        self.assertEqual(results, {})
        self.assertEqual(tracker.calls, 0)

    def test_failure_does_not_abort_batch(self) -> None:
        """One worker raising shouldn't poison the whole batch — the
        admin UI needs to see partial success."""
        users = ["alice", "bob", "carol"]

        def flaky(cmd, capture_output=True, text=True, check=False):
            # Make bob's call raise ; alice + carol succeed.
            if "vm-jh-bob" in cmd:
                raise OSError("agent socket gone")
            return types.SimpleNamespace(returncode=0, stdout="", stderr="")

        with mock.patch.object(weft_spawner.subprocess, "run", flaky):
            results = weft_spawner.WeftSpawner.stop_many(
                users,
                project_uuid="proj-uuid",
                max_workers=4,
            )
        self.assertTrue(results["alice"].ok)
        self.assertTrue(results["carol"].ok)
        self.assertFalse(results["bob"].ok)
        self.assertIn("agent socket gone", results["bob"].stderr)

    def test_command_shape(self) -> None:
        """Each worker shells the right ``weft instance stop`` command."""
        tracker = _ConcurrencyTracker(sleep_s=0.001)
        with mock.patch.object(weft_spawner.subprocess, "run", tracker):
            weft_spawner.WeftSpawner.stop_many(
                ["Alice Smith"],  # mixed case + space → sanitisation check
                project_uuid="proj-uuid",
                weft_binary="/run/weft/bin/weft",
                weft_socket="/run/weft/weft.sock",
                max_workers=2,
                now=True,
            )
        self.assertEqual(len(tracker.commands), 1)
        cmd = tracker.commands[0]
        self.assertEqual(cmd[0], "/run/weft/bin/weft")
        self.assertIn("--socket", cmd)
        self.assertIn("/run/weft/weft.sock", cmd)
        self.assertIn("instance", cmd)
        self.assertIn("stop", cmd)
        # _safe_username collapses "Alice Smith" → "alice-smith".
        self.assertIn("vm-jh-alice-smith", cmd)
        self.assertIn("--project", cmd)
        self.assertIn("proj-uuid", cmd)
        self.assertIn("--force", cmd)  # now=True ⇒ --force


if __name__ == "__main__":
    unittest.main()
