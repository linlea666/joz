import asyncio
import json
import os
import tempfile
import time
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

import discord
import healthcheck
from worker import Worker


def http_error(cls, status):
    return cls(SimpleNamespace(status=status, reason='x'), {'code': 0, 'message': 'x'})


class Transient(Exception):
    status = 503


def diagnostic_events(worker):
    return [json.loads(row[0]) for row in worker.diagnostics.db.execute('SELECT body FROM diagnostics ORDER BY seq')]


class ReliabilityTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.env = patch.dict(os.environ, {'DISCORD_BUFFER_DIR': self.directory.name})
        self.env.start()
        self.worker = Worker()
        self.worker.channels = {'200': {}, '300': {}}
        self.tasks = []

    async def asyncTearDown(self):
        for task in self.tasks:
            task.cancel()
        for _, task in list(self.worker.recovery_retry.values()):
            task.cancel()
        if self.worker.recovery_task:
            self.worker.recovery_task.cancel()
        await asyncio.gather(*self.tasks, return_exceptions=True)
        self.worker.diagnostics.db.close()
        self.worker.box.db.close()
        self.worker.lock_file.close()
        self.env.stop()
        self.directory.cleanup()

    async def wait_for(self, predicate, timeout=3.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                return True
            await asyncio.sleep(0.01)
        return predicate()

    # ---- history recovery retry -------------------------------------------------

    async def test_transient_recovery_failure_retries_with_backoff_then_ready(self):
        w = self.worker
        w.channels = {'200': {}}
        w.RECOVERY_RETRY_BASE = 0.05
        w.RECOVERY_RETRY_MAX = 0.08
        w.send = AsyncMock()
        w.client = SimpleNamespace(get_channel=lambda _: SimpleNamespace(guild=None))
        w.box.advance('200', '100', 1)
        w.box.drained = lambda _: True
        calls = []

        async def fetch(channel, limit, before=None):
            calls.append(time.monotonic())
            if len(calls) <= 3:
                raise Transient('HTTP 503')
            return []
        w.fetch_history = fetch

        await w.recover({'200'})
        self.assertEqual(w.channel_states['200'][0], 'gap')
        self.assertEqual(w.box.recovery('200')[2], 'gap')
        self.assertEqual(w.recovery_retry['200'][0], 0.05)

        self.assertTrue(await self.wait_for(lambda: w.channel_states['200'][0] == 'ready'))
        self.assertEqual(len(calls), 4)
        # Backoff doubled and is capped.
        self.assertGreaterEqual(calls[2] - calls[1], 0.08 - 0.01)
        self.assertNotIn('200', w.recovery_retry)
        self.assertIsNone(w.box.recovery('200'))
        codes = [e['context'].get('code') for e in diagnostic_events(w)]
        self.assertIn('RECOVERY_RETRY_SCHEDULED', codes)

    async def test_forbidden_recovery_is_not_retried(self):
        w = self.worker
        w.channels = {'200': {}}
        w.RECOVERY_RETRY_BASE = 0.01
        w.send = AsyncMock()
        w.client = SimpleNamespace(get_channel=lambda _: SimpleNamespace(guild=None))
        for exc in (http_error(discord.Forbidden, 403), http_error(discord.NotFound, 404), PermissionError('hidden')):
            w.fetch_history = AsyncMock(side_effect=exc)
            await w.recover({'200'})
            self.assertEqual(w.channel_states['200'][0], 'forbidden')
            self.assertNotIn('200', w.recovery_retry)
        await asyncio.sleep(0.05)
        self.assertEqual(w.channel_states['200'][0], 'forbidden')

    async def test_stale_recovery_retry_is_noop(self):
        w = self.worker
        w.channels = {'200': {}}
        w.RECOVERY_RETRY_BASE = 0.03
        w.send = AsyncMock()
        w.client = SimpleNamespace(get_channel=lambda _: SimpleNamespace(guild=None))
        w.fetch_history = AsyncMock(side_effect=Transient('HTTP 502'))
        await w.recover({'200'})
        # A reconfiguration removes the channel before the retry fires.
        w.channels = {}
        called = []
        w.schedule_recovery = lambda channels=None: called.append(channels)
        retry = w.recovery_retry['200'][1]
        await asyncio.wait_for(retry, 1)
        self.assertEqual(called, [])

    # ---- hydrate failure classification -----------------------------------------

    def start_delivery(self, get_message):
        w = self.worker
        w.writer = object()
        w.configured.set()
        w.client = SimpleNamespace(http=SimpleNamespace(get_message=get_message))
        sent = []

        async def send(frame):
            if frame is None or frame['type'] != 'event':
                return
            sent.append(frame['data'])
            w.acks[frame['data']['event_id']].set_result(None)
        w.send = send
        self.tasks.append(asyncio.create_task(w.deliver()))
        return sent

    def append_events(self):
        w = self.worker
        w.box.advance('200', '100', 1)
        # Partial edit without author/timestamp and no cache -> needs get_message.
        w.box.append('MESSAGE_UPDATE', {'id': '150', 'channel_id': '200', 'content': 'edit'})
        w.box.append('MESSAGE_CREATE', {'id': '400', 'channel_id': '300', 'author': {'id': '7'},
                                        'timestamp': '2026-10-09T12:00:00Z', 'content': 'other channel'})

    async def test_hydrate_forbidden_skips_event_marks_gap_and_unblocks_queue(self):
        w = self.worker
        w.channel_states = {'200': ('ready', ''), '300': ('ready', '')}
        self.append_events()
        get_message = AsyncMock(side_effect=http_error(discord.Forbidden, 403))
        sent = self.start_delivery(get_message)

        self.assertTrue(await self.wait_for(lambda: len(sent) == 1 and w.box.count() == 0))
        self.assertEqual(sent[0]['channel_id'], '300')
        get_message.assert_awaited_once_with(200, 150)
        # The skip is never silent: durable gap at the oldest unverified boundary.
        self.assertEqual(w.channel_states['200'][0], 'gap')
        self.assertEqual(w.box.recovery('200'), ('100', 1.0, 'gap'))
        self.assertEqual(w.channel_frame('200')['data']['state'], 'gap')
        self.assertEqual(w.channel_frame('200')['data']['last_message_id'], '100')
        self.assertEqual(w.channel_states['300'][0], 'ready')
        skipped = [e for e in diagnostic_events(w) if e['event'] == 'delivery.skipped']
        self.assertEqual(skipped[0]['context']['code'], 'HYDRATE_FORBIDDEN')
        self.assertEqual(skipped[0]['context']['channel_id'], '200')

    async def test_hydrate_skip_invalidates_in_flight_recovery(self):
        w = self.worker
        w.channel_states = {'200': ('recovering', ''), '300': ('ready', '')}
        w.box.begin_recovery('200')
        generation = w.channel_generations.get('200', 0)
        self.append_events()
        self.start_delivery(AsyncMock(side_effect=http_error(discord.Forbidden, 403)))
        self.assertTrue(await self.wait_for(lambda: w.box.count() == 0))
        self.assertGreater(w.channel_generations['200'], generation)
        self.assertEqual(w.box.recovery('200')[2], 'gap')

    async def test_hydrate_transient_retries_then_delivers(self):
        w = self.worker
        w.HYDRATE_RETRY_BASE = 0.01
        w.HYDRATE_RETRY_MAX = 0.02
        w.channel_states = {'200': ('ready', ''), '300': ('ready', '')}
        self.append_events()
        full = {'id': '150', 'channel_id': '200', 'author': {'id': '7'}, 'timestamp': '2026-10-09T12:00:00Z', 'content': 'old'}
        get_message = AsyncMock(side_effect=[Transient('503'), asyncio.TimeoutError(), full])
        sent = self.start_delivery(get_message)
        self.assertTrue(await self.wait_for(lambda: len(sent) == 2))
        self.assertEqual([e['channel_id'] for e in sent], ['200', '300'])
        self.assertEqual(sent[0]['payload']['content'], 'edit')
        self.assertEqual(get_message.await_count, 3)
        self.assertEqual(w.channel_states['200'][0], 'ready')
        self.assertIsNone(w.box.recovery('200'))
        self.assertIsNone(w.hydrate_failure)

    async def test_hydrate_transient_beyond_threshold_becomes_gap(self):
        w = self.worker
        w.HYDRATE_RETRY_BASE = 0.01
        w.HYDRATE_RETRY_MAX = 0.02
        w.HYDRATE_GIVE_UP = 0.1
        w.channel_states = {'200': ('ready', ''), '300': ('ready', '')}
        self.append_events()
        get_message = AsyncMock(side_effect=Transient('HTTP 503'))
        sent = self.start_delivery(get_message)
        self.assertTrue(await self.wait_for(lambda: len(sent) == 1 and w.box.count() == 0))
        self.assertGreater(get_message.await_count, 1)
        self.assertEqual(sent[0]['channel_id'], '300')
        self.assertEqual(w.channel_states['200'][0], 'gap')
        self.assertEqual(w.box.recovery('200')[0], '100')
        codes = [e['context'].get('code') for e in diagnostic_events(w) if e['event'] == 'delivery.skipped']
        self.assertEqual(codes, ['HYDRATE_RETRY_EXHAUSTED'])

    # ---- queue-head health metric ----------------------------------------------

    async def test_head_stuck_metric_in_status_and_diagnostics(self):
        w = self.worker
        data = w.status_payload()
        self.assertFalse(data['delivery_stuck'])
        self.assertEqual(data['head_event_age_seconds'], 0)
        w.box.append('MESSAGE_CREATE', {'id': '400', 'channel_id': '300'})
        w.track_head(w.box.next())
        self.assertFalse(w.status_payload()['delivery_stuck'])
        # Same head observed again keeps its original age.
        event_id, channel, _ = w.head_event
        w.head_event = (event_id, channel, time.monotonic() - 120)
        w.track_head(w.box.next())
        data = w.status_payload()
        self.assertTrue(data['delivery_stuck'])
        self.assertGreaterEqual(data['head_event_age_seconds'], 120)
        self.assertEqual(data['head_event_channel_id'], '300')
        self.assertIn('stuck', data['last_error'])
        w.status_payload()
        stuck = [e for e in diagnostic_events(w) if e['event'] == 'delivery.stuck']
        self.assertEqual(len(stuck), 1)
        self.assertEqual(stuck[0]['context']['code'], 'HEAD_EVENT_STUCK')
        self.assertGreaterEqual(stuck[0]['context']['duration_ms'], 120000)
        # Queue moves on -> metric resets.
        w.box.ack(event_id)
        w.track_head(w.box.next())
        self.assertFalse(w.status_payload()['delivery_stuck'])

    def test_healthcheck_reports_stuck_head(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(healthcheck, 'PATH', directory + '/h.json'):
            healthcheck.write_status(True, 'connected', True, 95.0, True)
            with open(directory + '/h.json') as f:
                status = json.load(f)
        self.assertTrue(status['delivery_stuck'])
        self.assertTrue(healthcheck.healthy(status, status['checked_at']))
        self.assertIn('95s', healthcheck.describe(status))
        self.assertNotIn('WARNING', healthcheck.describe(dict(status, delivery_stuck=False)))


if __name__ == '__main__':
    unittest.main()
