"""Recovery behaviour for the shared cloudflared supervisor.

Covers the sleep/wake and half-dead-child failure modes: metrics probing,
clock-jump wake detection, terminate/kill escalation, capped backoff and the
immediate invalidation of a stale published entry.
"""
import itertools
import json
import subprocess
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest.mock import MagicMock, patch

from bridge.named_tunnel import NamedTunnel
from bridge.tunnel import CloudflareConnector, QuickTunnel

ROOT = Path(__file__).resolve().parents[1]


class BlockingStream:
    """Popen-like stdout: yields fixed lines, then blocks until released."""

    def __init__(self, lines=()):
        self.lines = list(lines)
        self.gate = threading.Event()

    def __iter__(self):
        yield from self.lines
        self.gate.wait(10)
        yield ''


def read_status(data, name):
    path = Path(data)/name
    return json.loads(path.read_text(encoding='utf-8')) if path.exists() else {}


class RecoveryTests(unittest.TestCase):
    def make_quick(self, data):
        executable = Path(data)/'cloudflared'
        executable.touch()
        return QuickTunnel(executable, 8787, Path(data), MagicMock())

    def test_probe_failures_replace_child_and_invalidate_entry(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as directory:
            tunnel = self.make_quick(directory)
            tunnel.CHECK_INTERVAL = 0.02
            tunnel.FAIL_STREAK = 3
            tunnel.RESTART_DELAY = 0
            tunnel.START_POLL = 0.02
            first = MagicMock(stdout=BlockingStream(['https://first.trycloudflare.com\n',
                                                      'Registered tunnel connection\n']), poll=lambda: None)
            second = MagicMock(stdout=BlockingStream(), poll=lambda: None)
            calls = []

            def popen(arguments, **kwargs):
                calls.append(list(arguments))
                return first if len(calls) == 1 else second

            seen = set()
            with patch('bridge.tunnel.subprocess.Popen', side_effect=popen), \
                 patch.object(tunnel, '_probe', return_value=False):
                starter = threading.Thread(target=tunnel.start)
                starter.start()
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline and (len(calls) < 2 or 'reconnecting' not in seen):
                    seen.add(read_status(directory, 'cloudflare-status.json').get('state'))
                    time.sleep(0.005)
                self.assertGreaterEqual(len(calls), 2, 'unhealthy child was not replaced')
                self.assertIn('reconnecting', seen)
                self.assertFalse(tunnel.ready.is_set())
                self.assertFalse((Path(directory)/'外网地址.txt').exists())
                metrics = [arguments[arguments.index('--metrics') + 1] for arguments in calls]
                self.assertTrue(all(int(port.rsplit(':', 1)[1]) > 0 for port in metrics))
                self.assertNotEqual(metrics[0], metrics[1])
                tunnel.close()
                starter.join(3)
                self.assertFalse(starter.is_alive() or tunnel.supervisor.is_alive())
            first.terminate.assert_called()

    def test_clock_jump_after_sleep_replaces_child_despite_healthy_probe(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as directory:
            tunnel = self.make_quick(directory)
            tunnel.CHECK_INTERVAL = 0.02
            tunnel.RESTART_DELAY = 0
            tunnel.START_POLL = 0.02
            first = MagicMock(stdout=BlockingStream(['https://first.trycloudflare.com\n',
                                                      'Registered tunnel connection\n']), poll=lambda: None)
            second = MagicMock(stdout=BlockingStream(), poll=lambda: None)
            calls = []

            def popen(arguments, **kwargs):
                calls.append(list(arguments))
                return first if len(calls) == 1 else second

            reads = itertools.count()
            real_monotonic = time.monotonic

            def fake_monotonic():
                return 100.0 + 0.001 * next(reads)

            def fake_wall():
                counter = next(reads)
                # After a few reads the wall clock has crossed a suspend that
                # time.monotonic() (paused during sleep) never saw.
                return 1000.0 + (7200.0 if counter > 4 else 0.001 * counter)

            seen, messages = set(), set()
            with patch('bridge.tunnel.subprocess.Popen', side_effect=popen), \
                 patch('bridge.tunnel.time.monotonic', side_effect=fake_monotonic), \
                 patch('bridge.tunnel.time.time', side_effect=fake_wall), \
                 patch.object(tunnel, '_probe', return_value=True):
                starter = threading.Thread(target=tunnel.start)
                starter.start()
                deadline = real_monotonic() + 5
                while real_monotonic() < deadline and not (len(calls) >= 2 and seen & {'reconnecting'} and
                                                           any('休眠' in message for message in messages)):
                    record = read_status(directory, 'cloudflare-status.json')
                    seen.add(record.get('state'))
                    if record.get('message'):
                        messages.add(record['message'])
                    time.sleep(0.005)
                self.assertGreaterEqual(len(calls), 2, 'wake was not detected')
                first.terminate.assert_called()
                tunnel.close()
                starter.join(3)
                self.assertIn('reconnecting', seen)
            self.assertTrue(any('休眠' in message for message in messages),
                            'wake reason was not reported in the status message')

    def test_wake_gap_predicate(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as directory:
            tunnel = self.make_quick(directory)
            self.assertFalse(tunnel._wake_jump(5, 5))
            self.assertFalse(tunnel._wake_jump(29, 29))
            self.assertTrue(tunnel._wake_jump(31, 31))       # Windows: monotonic ran through sleep.
            self.assertTrue(tunnel._wake_jump(0.02, 7200))   # macOS/Linux: only the wall clock jumped.
            self.assertFalse(tunnel._wake_jump(0.02, 0.02))

    def test_backoff_grows_is_capped_and_resets_on_ready(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as directory:
            tunnel = self.make_quick(directory)
            self.assertEqual(tunnel._next_delay(0, False), CloudflareConnector.RESTART_DELAY)
            self.assertEqual(tunnel._next_delay(2, False), 4)
            self.assertEqual(tunnel._next_delay(30, False), 60)
            self.assertEqual(tunnel._next_delay(60, False), 60)
            self.assertEqual(tunnel._next_delay(60, True), 2)
            tunnel.RESTART_DELAY = 0
            self.assertEqual(tunnel._next_delay(0, False), 0)

    def test_retire_escalates_to_kill_when_terminate_is_ignored(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as directory:
            tunnel = self.make_quick(directory)
            process = MagicMock(poll=lambda: None)
            process.wait.side_effect = [subprocess.TimeoutExpired('cloudflared', 5), None]
            tunnel.process = process
            tunnel._retire()
            process.terminate.assert_called_once()
            process.kill.assert_called_once()

    def test_named_tunnel_reports_retrying_and_keeps_token_out_of_argv(self):
        with tempfile.TemporaryDirectory(dir=ROOT/'.tmp') as directory:
            tunnel = NamedTunnel('/fixture/cloudflared', {'id': 'fixed'}, directory, 'secret-token')
            tunnel.metrics_port = 45678
            arguments = tunnel._arguments()
            self.assertEqual(arguments[arguments.index('--metrics') + 1], '127.0.0.1:45678')
            self.assertNotIn('secret-token', ' '.join(arguments))
            tunnel.failure = 'unhealthy'
            tunnel._lost()
            self.assertEqual(read_status(directory, 'ssh-status-fixed.json')['state'], 'retrying')
            self.assertIn('无响应', read_status(directory, 'ssh-status-fixed.json')['message'])
            tunnel._stopped()
            self.assertEqual(read_status(directory, 'ssh-status-fixed.json')['state'], 'stopped')


if __name__ == '__main__':
    unittest.main()
