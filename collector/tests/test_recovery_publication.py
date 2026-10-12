"""Exercise recovery and live delivery together over the real frame writer."""
import asyncio
import json
import os
import tempfile
import unittest
from contextlib import suppress
from datetime import datetime, timezone
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

from core import baseline_snowflake
from worker import Gateway, Worker


class FrameWriter:
    def __init__(self, worker):
        self.worker = worker
        self.frames = []

    def write(self, data):
        frame = json.loads(data)
        self.frames.append(frame)
        if frame['type'] == 'event':
            self.worker.acks[frame['data']['event_id']].set_result(None)

    async def drain(self):
        await asyncio.sleep(0)


class RecoveryPublicationTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.env = patch.dict(os.environ, {'DISCORD_BUFFER_DIR': self.directory.name})
        self.env.start()
        clock_patch = patch('worker.time.time', return_value=1800000000.0)
        self.clock = clock_patch.start()
        self.addCleanup(clock_patch.stop)
        self.worker = Worker()
        w = self.worker
        w.channels = {'200': {'trade_recovery': True}, '300': {'trade_recovery': False}}
        w.writer = FrameWriter(w)
        w.configured.set()
        w.notification_supported = True
        w.notification_epoch = 'offline-backend'
        self.gateway = Gateway(w)
        w.client = self.gateway
        self.gateway.get_channel = lambda _: SimpleNamespace(guild=None)
        self.gateway.fetch_channel = AsyncMock(return_value=SimpleNamespace(guild=None))
        w.fetch_history = AsyncMock(return_value=[])
        self.deliver_task = None

    async def asyncTearDown(self):
        if self.deliver_task:
            self.deliver_task.cancel()
            with suppress(asyncio.CancelledError):
                await self.deliver_task
        await self.gateway.close()
        w = self.worker
        w.diagnostics.db.close()
        w.box.db.close()
        w.lock_file.close()
        self.env.stop()
        self.directory.cleanup()

    async def ready(self, session):
        self.gateway.dispatch('socket_raw_receive', json.dumps({
            'op': 0, 't': 'READY', 'd': {'session_id': session}}))
        await self.gateway.on_ready()
        await self.worker.recover()

    def capture(self, channel='300'):
        self.clock.return_value += 1
        now = self.clock.return_value
        self.gateway.dispatch('socket_raw_receive', json.dumps({
            'op': 0, 't': 'MESSAGE_CREATE', 'd': {
                'id': baseline_snowflake(now), 'channel_id': channel,
                'author': {'id': '7'}, 'content': 'offline sample',
                'timestamp': datetime.fromtimestamp(now, timezone.utc).isoformat()}}))

    async def drained(self):
        async def wait():
            while self.worker.box.count():
                await asyncio.sleep(.001)
        await asyncio.wait_for(wait(), 2)

    async def test_empty_recovery_keeps_consecutive_notifications_and_trade_channel_ready(self):
        w = self.worker
        await self.ready('first')
        self.assertEqual(w.box.count(), 0)
        self.deliver_task = asyncio.create_task(w.deliver())
        for channel in ('300', '300', '200'):
            self.capture(channel)
            await self.drained()
        events = [f['data'] for f in w.writer.frames if f['type'] == 'event']
        self.assertEqual([e['notification_realtime'] for e in events], [True, True, True])
        self.assertEqual(w.channel_states, {'200': ('ready', ''), '300': ('ready', '')})
        self.assertFalse(w.recovery_barrier)
        self.assertIsNone(w.box.recovery('200'))
        self.assertIsNone(w.box.recovery('300'))
        # A later new session must behave the same way, without replaying mail.
        await self.ready('second')
        self.capture()
        await self.drained()
        self.assertTrue(w.writer.frames[-1]['data']['notification_realtime'])
        self.assertEqual(w.channel_states['200'][0], 'ready')

    async def test_delayed_status_cannot_publish_old_ready_after_new_session(self):
        w = self.worker
        await self.ready('first')
        w.writer.frames.clear()
        await w.write_lock.acquire()
        pending = asyncio.create_task(w.channel_status('200', 'ready'))
        await asyncio.sleep(0)
        self.gateway.dispatch('socket_raw_receive', json.dumps({
            'op': 0, 't': 'READY', 'd': {'session_id': 'second'}}))
        w.write_lock.release()
        await pending
        statuses = [f['data']['state'] for f in w.writer.frames
                    if f['type'] == 'channel' and f['data']['channel_id'] == '200']
        self.assertTrue(statuses)
        self.assertTrue(all(state == 'recovering' for state in statuses), statuses)

    async def test_message_during_recovery_is_gated_before_delivery(self):
        w = self.worker
        self.gateway.dispatch('socket_raw_receive', json.dumps({
            'op': 0, 't': 'READY', 'd': {'session_id': 'first'}}))
        self.capture('200')
        self.deliver_task = asyncio.create_task(w.deliver())
        await self.drained()
        frames = w.writer.frames
        event_index = next(i for i, f in enumerate(frames) if f['type'] == 'event')
        prior = [f['data'] for f in frames[:event_index] if f['type'] == 'channel']
        self.assertEqual({f['channel_id'] for f in prior}, {'200', '300'})
        self.assertTrue(all(f['state'] == 'recovering' for f in prior))
        self.assertFalse(frames[event_index]['data']['notification_realtime'])
        self.assertIsNotNone(w.box.recovery('200'))

    async def test_new_ready_while_publishing_does_not_lose_new_barrier(self):
        w = self.worker
        self.gateway.dispatch('socket_raw_receive', json.dumps({
            'op': 0, 't': 'READY', 'd': {'session_id': 'first'}}))
        self.capture()
        original_drain = w.writer.drain
        replaced = False

        async def drain():
            nonlocal replaced
            if not replaced:
                replaced = True
                self.gateway.dispatch('socket_raw_receive', json.dumps({
                    'op': 0, 't': 'READY', 'd': {'session_id': 'second'}}))
            await original_drain()

        w.writer.drain = drain
        self.deliver_task = asyncio.create_task(w.deliver())
        await self.drained()
        # The second READY also needs its channel gate before the event.
        frames = w.writer.frames
        event_index = next(i for i, f in enumerate(frames) if f['type'] == 'event')
        statuses = [f for f in frames[:event_index] if f['type'] == 'channel']
        self.assertGreaterEqual(len(statuses), 4)

    async def test_empty_queue_publishes_recovery_without_waiting_for_a_message(self):
        w = self.worker
        self.gateway.dispatch('socket_raw_receive', json.dumps({
            'op': 0, 't': 'READY', 'd': {'session_id': 'first'}}))
        self.deliver_task = asyncio.create_task(w.deliver())
        async def wait():
            while not w.writer.frames:
                await asyncio.sleep(.001)
        await asyncio.wait_for(wait(), 2)
        statuses = [f['data'] for f in w.writer.frames if f['type'] == 'channel']
        self.assertEqual({f['channel_id'] for f in statuses}, {'200', '300'})
        self.assertTrue(all(f['state'] == 'recovering' for f in statuses))
        self.assertEqual(w.box.count(), 0)

    async def test_permission_failure_is_not_replaced_by_deferred_recovery_status(self):
        w = self.worker
        self.gateway.fetch_channel.side_effect = PermissionError('offline access denied')
        await self.ready('first')
        self.assertEqual(w.channel_states['300'][0], 'forbidden')
        self.deliver_task = asyncio.create_task(w.deliver())
        self.capture('200')
        await self.drained()
        self.assertEqual(w.channel_states['300'][0], 'forbidden')
        self.assertEqual(w.channel_states['200'][0], 'ready')
        self.assertIsNotNone(w.box.recovery('300'))

    async def test_config_reporting_cannot_undo_concurrent_channel_recovery(self):
        w = self.worker
        w.state = 'connected'
        w.channel_states = {'200': ('ready', ''), '300': ('recovering', '')}
        original_drain = w.writer.drain
        recovery = None

        async def drain():
            nonlocal recovery
            if recovery is None:
                recovery = asyncio.create_task(w.recover({'300'}))
                # recover updates the local state, then waits for write_lock.
                await asyncio.sleep(0)
            await original_drain()

        w.writer.drain = drain
        await w.configure({'enabled': False, 'version': 'same', 'channels': w.channels})
        await recovery
        self.assertEqual(w.channel_states['300'][0], 'ready')
        statuses = [f['data']['state'] for f in w.writer.frames
                    if f['type'] == 'channel' and f['data']['channel_id'] == '300']
        self.assertEqual(statuses[-1], 'ready')

    async def test_new_session_while_event_waits_for_socket_cannot_send_old_email(self):
        w = self.worker
        await self.ready('first')
        self.capture()
        await w.write_lock.acquire()
        self.deliver_task = asyncio.create_task(w.deliver())
        async def wait():
            while not w.acks:
                await asyncio.sleep(.001)
        await asyncio.wait_for(wait(), 2)
        self.gateway.dispatch('socket_raw_receive', json.dumps({
            'op': 0, 't': 'READY', 'd': {'session_id': 'second'}}))
        w.write_lock.release()
        await self.drained()
        event = next(f['data'] for f in w.writer.frames if f['type'] == 'event')
        self.assertFalse(event['notification_realtime'])
        self.assertNotIn('notification_payload', event)

    async def test_failed_status_write_retries_without_claiming_publication_complete(self):
        w = self.worker
        self.gateway.dispatch('socket_raw_receive', json.dumps({
            'op': 0, 't': 'READY', 'd': {'session_id': 'first'}}))
        original_drain = w.writer.drain
        w.writer.drain = AsyncMock(side_effect=ConnectionError('offline IPC failure'))
        with self.assertRaises(ConnectionError):
            await w.send(None)
        self.assertTrue(w.recovery_barrier)
        self.assertEqual(w.channel_states['200'][0], 'recovering')
        w.writer.drain = original_drain
        await w.send(None)
        self.assertFalse(w.recovery_barrier)
        self.assertIsNotNone(w.box.recovery('200'))
