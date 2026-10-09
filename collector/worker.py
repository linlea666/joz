"""Single personal-account Gateway collector. No trade or exchange code here."""
import asyncio
import json
import logging
import os
import signal
import time
from contextlib import suppress

import discord
from healthcheck import write_status
from core import Outbox, OutboxFull, baseline_snowflake, bounded_history, merge_message, snowflake_time, utcnow

logging.basicConfig(level=logging.WARNING)
# SDK debug logging includes credentials in outbound frames. Only incoming
# debug events are consumed; SDK payload logging must remain disabled.
logging.getLogger('discord').setLevel(logging.CRITICAL)


class Gateway(discord.Client):
    def __init__(self, owner):
        super().__init__(enable_debug_events=True, guild_subscriptions=False,
                         chunk_guilds_at_startup=False, max_messages=100,
                         sync_presence=False)
        self.owner = owner

    def dispatch(self, event, /, *args, **kwargs):
        if event == 'socket_raw_receive':
            # Synchronous capture preserves wire order before asynchronous SDK
            # callbacks and before any model/network consumer can run.
            self.owner.capture(args[0])
        return super().dispatch(event, *args, **kwargs)

    async def on_ready(self):
        if self.owner.halted:
            return
        self.owner.state = 'recovering'
        for channel in self.owner.channels:
            self.owner.box.begin_recovery(channel)
        # Get the CURRENT active-card inventory on every new session. Resume
        # has its own callback and does not trigger this bounded REST repair.
        with suppress(ConnectionError, OSError):
            await self.owner.send({'type':'refresh'})

    async def on_disconnect(self):
        if not self.owner.halted:
            self.owner.state = 'reconnecting'

    async def on_resumed(self):
        self.owner.state = 'connected'
        # A successful SDK Resume replays events; no REST history polling.


class Worker:
    def __init__(self):
        directory = os.getenv('DISCORD_BUFFER_DIR', '/var/lib/discord-collector')
        os.makedirs(directory, mode=0o700, exist_ok=True)
        os.chmod(directory, 0o700)
        # Holding this advisory lock prevents two SDK sessions sharing one buffer.
        import fcntl
        self.lock_file = open(os.path.join(directory, 'collector.lock'), 'a')
        fcntl.flock(self.lock_file, fcntl.LOCK_EX | fcntl.LOCK_NB)
        self.box = Outbox(os.path.join(directory, 'outbox.db'))
        self.writer = None
        self.write_lock = asyncio.Lock()
        self.config_lock = asyncio.Lock()
        self.client = None
        self.client_task = None
        self.recovery_task = None
        self.token = ''
        self.channels = {}
        self.active_cards = []
        self.version = ''
        self.state = 'disabled'
        self.last_heartbeat = None
        self.last_error = ''
        self.session_id = None
        self.channel_states = {}
        self.configured = asyncio.Event()
        self.acks = {}
        self.wake = asyncio.Event()
        self.halted = False
        self.recovery_barrier = False

    def capture(self, raw):
        packet = json.loads(raw) if isinstance(raw, (str, bytes)) else raw
        if packet.get('op') == 11:
            self.last_heartbeat = utcnow()
        if packet.get('t') == 'READY':
            self.session_id = packet['d'].get('session_id')
            # Freeze BEFORE subsequent frames can advance receipts. SDK on_ready
            # can run later, after guild hydration and other message dispatches.
            self.state = 'recovering'
            try:
                for channel in self.channels:
                    self.box.begin_recovery(channel)
                    self.channel_states[channel] = ('recovering', '')
                self.recovery_barrier = True
            except Exception:
                self.fail_buffer()
                return
        kind = packet.get('t')
        data = packet.get('d') or {}
        channel = str(data.get('channel_id', ''))
        if self.halted or channel not in self.channels or kind not in {
                'MESSAGE_CREATE', 'MESSAGE_UPDATE', 'MESSAGE_DELETE', 'MESSAGE_DELETE_BULK'}:
            return
        try:
            ids = data.get('ids', []) if kind == 'MESSAGE_DELETE_BULK' else [data['id']]
            for message_id in ids:
                payload = dict(data, id=str(message_id), channel_id=channel)
                self.box.append(kind, payload, session=self.session_id, sequence=packet.get('s'), config_version=self.version)
            self.wake.set()
        except Exception:
            # Never continue with a silently dropped frame after a disk error.
            self.fail_buffer()

    def fail_buffer(self):
        self.halted = True
        self.state, self.last_error = 'buffer_failed', 'durable event buffer failed; manual recovery required'
        if self.client:
            asyncio.create_task(self.client.close())

    async def send(self, frame):
        async with self.write_lock:
            if self.writer is None:
                raise ConnectionError('backend unavailable')
            self.writer.write((json.dumps(frame, separators=(',', ':')) + '\n').encode())
            await self.writer.drain()

    async def channel_status(self, channel, state, error=''):
        checkpoint = self.box.checkpoint(channel)
        recovery = self.box.recovery(channel)
        if recovery:
            checkpoint = recovery
        self.channel_states[channel] = (state, error)
        await self.send({'type': 'channel', 'data': {
            'channel_id': channel, 'state': state, 'last_error': error,
            'last_message_id': checkpoint[0] if checkpoint else '', 'updated_at': utcnow()}})

    async def fetch_history(self, channel, limit, before=None):
        return await self.client.http.logs_from(int(channel), limit, before=before)

    async def hydrate(self, event):
        payload = event['payload']
        if event['kind'] not in ('MESSAGE_CREATE', 'MESSAGE_UPDATE'):
            return payload
        cached = self.box.cached(event['channel_id'], event['message_id'])
        if event['kind'] == 'MESSAGE_CREATE' and cached and cached.get('edited_timestamp'):
            return cached
        if not cached and ('author' not in payload or 'timestamp' not in payload):
            try:
                cached = await self.client.http.get_message(int(event['channel_id']), int(event['message_id']))
            except discord.NotFound:
                event['kind'] = 'MESSAGE_DELETE'
                return {'id':event['message_id'], 'channel_id':event['channel_id']}
        merged = merge_message(cached, payload)
        self.box.cache(merged)
        return merged

    async def deliver(self):
        while True:
            try:
                event = self.box.next()
                if not event or not self.writer or not self.configured.is_set():
                    self.wake.clear()
                    try:
                        await asyncio.wait_for(self.wake.wait(), 2)
                    except asyncio.TimeoutError:
                        pass
                    continue
                if self.recovery_barrier:
                    # This frame precedes events on the same local stream, so
                    # Go cannot dispatch a new arrival ahead of gap repair.
                    for channel in self.channels:
                        await self.channel_status(channel, 'recovering')
                    self.recovery_barrier = False
                event['payload'] = await self.hydrate(event)
                future = asyncio.get_running_loop().create_future()
                self.acks[event['event_id']] = future
                try:
                    await self.send({'type': 'event', 'data': event})
                    await asyncio.wait_for(future, 30)
                    # Receipt precedes checkpoint, so a crash here only replays.
                    if event['kind'] == 'MESSAGE_CREATE':
                        self.box.advance(event['channel_id'], event['message_id'], (self.box.recovery(event['channel_id']) or (None, None))[1])
                    self.box.ack(event['event_id'])
                finally:
                    self.acks.pop(event['event_id'], None)
            except asyncio.CancelledError:
                raise
            except Exception:
                self.last_error = 'event delivery pending; retrying without dropping receipt'
                await asyncio.sleep(2)

    def schedule_recovery(self):
        # Freeze the oldest unverified checkpoint BEFORE processing new frames.
        for channel in self.channels:
            self.box.begin_recovery(channel)
            self.channel_states[channel] = ('recovering', '')
        if self.recovery_task and not self.recovery_task.done():
            self.recovery_task.cancel()
        self.recovery_task = asyncio.create_task(self.recover())

    async def recover(self):
        for channel in list(self.channels):
            if channel not in self.channels:
                continue
            try:
                await self.channel_status(channel, 'recovering')
                obj = self.client.get_channel(int(channel)) or await self.client.fetch_channel(int(channel))
                guild = getattr(obj, 'guild', None)
                if guild:
                    await guild.subscribe(typing=True, activities=False, threads=False, member_updates=False)
                start, baseline_at, prior_state = self.box.begin_recovery(channel)
                if start != '0':
                    history, complete = await bounded_history(self.fetch_history, channel, start)
                else:
                    history, complete = await bounded_history(self.fetch_history, channel, baseline_snowflake(baseline_at), include_baseline=True)
                for msg in history:
                    msg['channel_id'] = channel
                    self.box.append('MESSAGE_CREATE', msg, baseline=snowflake_time(msg['id']) < baseline_at, config_version=self.version)
                for card in self.active_cards:
                    if card['channel_id'] == channel:
                        try:
                            msg = await self.client.http.get_message(int(channel), int(card['message_id']))
                            self.box.append('MESSAGE_UPDATE', dict(msg, channel_id=channel), config_version=self.version)
                        except discord.NotFound:
                            self.box.append('MESSAGE_DELETE', {'channel_id':channel, 'id':card['message_id']}, config_version=self.version)
                watermark = self.box.watermark()
                self.wake.set()
                while not self.box.drained(watermark):
                    await asyncio.sleep(0.1)
                if not self.box.checkpoint(channel):
                    self.box.advance(channel, '0', baseline_at)
                self.box.end_recovery(channel, complete)
                await self.channel_status(channel, 'ready' if complete else 'gap',
                                          '' if complete else 'history exceeds 1000 messages; source review required')
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                self.box.end_recovery(channel, False)
                status = getattr(exc, 'status', None)
                await self.channel_status(channel, 'forbidden' if status in (403, 404) else 'gap',
                                          'source recovery incomplete')

        if not self.halted and self.state not in {'reconnecting', 'connection_failed', 'auth_invalid', 'disabled'}:
            self.state = 'connected'

    async def run_gateway(self, client, token):
        try:
            await client.start(token, reconnect=True)
        except discord.LoginFailure:
            self.state, self.last_error = 'auth_invalid', 'Gateway authentication failed'
        except discord.ConnectionClosed as exc:
            if not self.halted:
                self.state = 'auth_invalid' if exc.code == 4004 else 'connection_failed'
                self.last_error = 'Gateway authentication failed' if exc.code == 4004 else 'Gateway stopped; inspect collector status'
        except Exception:
            if not self.halted:
                self.state, self.last_error = 'connection_failed', 'Gateway stopped; inspect collector status'

    async def configure(self, data):
        async with self.config_lock:
            token = data.get('token', '') if data.get('enabled') else ''
            channels = data.get('channels', {})
            changed = set(channels) != set(self.channels)
            if token != self.token:
                if self.recovery_task:
                    self.recovery_task.cancel()
                    with suppress(asyncio.CancelledError):
                        await self.recovery_task
                if self.client:
                    await self.client.close()
                if self.client_task:
                    with suppress(asyncio.CancelledError):
                        await self.client_task
            token_changed = token != self.token
            self.channels, self.active_cards = channels, data.get('active_cards', [])
            # Backend progress is only a lower bound; local outbox remains the
            # authority for unacknowledged receipts across process restarts.
            for item in data.get('checkpoints', []):
                ch, mid = item['channel_id'], item.get('last_message_id')
                if mid and not self.box.checkpoint(ch):
                    self.box.advance(ch, mid)
            for ch in channels:
                if ch not in self.channel_states:
                    self.box.begin_recovery(ch)
                    self.channel_states[ch] = ('recovering', '')
            self.version = data['version']
            if token_changed:
                self.token = token
                self.last_heartbeat = None
                self.state = 'connecting' if token else 'disabled'
                self.client = Gateway(self) if token else None
                self.client_task = asyncio.create_task(self.run_gateway(self.client, token)) if token else None
            elif (changed or self.state == "recovering") and self.client and self.client.is_ready():
                self.schedule_recovery()
            self.configured.set()
            for ch, (state, error) in list(self.channel_states.items()):
                if ch in channels:
                    await self.channel_status(ch, state, error)
            return {'applied_version': self.version}

    async def request(self, frame):
        try:
            op, data = frame['op'], frame.get('data', {})
            if op == 'configure':
                result = await self.configure(data)
            elif op == 'test':
                token = data.get('token') or self.token
                if not token:
                    raise ValueError('no credential')
                if self.client and token == self.token and self.client.user and self.state == 'connected':
                    result = {'id': str(self.client.user.id), 'username': self.client.user.name}
                else:
                    # Explicit validation uses the same SDK, and never opens a
                    # second Gateway connection or a periodic HTTP client.
                    async with discord.Client() as probe:
                        await probe.login(token)
                        result = {'id': str(probe.user.id), 'username': probe.user.name}
            elif op in ('message', 'preview'):
                if not self.client or not self.client.is_ready():
                    raise ValueError('collector not ready')
                channel = str(data['channel_id'])
                if not channel.isdecimal():
                    raise ValueError('invalid channel')
                if op == 'message':
                    result = await self.client.http.get_message(int(channel), int(data['message_id']))
                else:
                    result = await self.fetch_history(channel, min(100, max(1, int(data.get('limit', 5)))))
            else:
                raise ValueError('unknown operation')
            await self.send({'type': 'response', 'id': frame['id'], 'data': result})
        except Exception as exc:
            # Exception text may contain a credential or private payload.
            await self.send({'type': 'response', 'id': frame['id'],
                             'status_code': getattr(exc, 'status', 0),
                             'error': f"collector request failed ({type(exc).__name__}, HTTP {getattr(exc, 'status', 'n/a')})"})

    async def status_loop(self):
        while True:
            with suppress(ConnectionError, OSError):
                data = {'state': self.state, 'last_error': self.last_error,
                        'applied_version': self.version, 'backlog': self.box.count()}
                if self.last_heartbeat:
                    data['last_heartbeat'] = self.last_heartbeat
                await self.send({'type': 'status', 'data': data})
                write_status(bool(self.writer and self.configured.is_set()), self.state, bool(self.token))
            await asyncio.sleep(5)

    async def connect_backend(self):
        while True:
            try:
                self.configured.clear()
                reader, self.writer = await asyncio.open_unix_connection(
                    os.getenv('DISCORD_SOCKET_PATH', '/run/nofx-discord/collector.sock'), limit=16 << 20)
                await self.send({'type': 'hello', 'data': 1})
                self.wake.set()
                while line := await reader.readline():
                    frame = json.loads(line)
                    if frame['type'] == 'ack':
                        future = self.acks.get(frame.get('id'))
                        if future and not future.done():
                            if frame.get('error'):
                                future.set_exception(RuntimeError('backend rejected event'))
                            else:
                                future.set_result(None)
                    elif frame['type'] == 'request':
                        asyncio.create_task(self.request(frame))
            except (OSError, ConnectionError, ValueError):
                pass
            finally:
                if self.writer:
                    self.writer.close()
                self.writer = None
            await asyncio.sleep(2)

    async def run(self):
        task = asyncio.current_task()
        loop = asyncio.get_running_loop()
        for sig in (signal.SIGTERM, signal.SIGINT):
            loop.add_signal_handler(sig, task.cancel)
        try:
            await asyncio.gather(self.connect_backend(), self.deliver(), self.status_loop())
        finally:
            if self.client:
                await self.client.close()
            self.box.db.close()
            self.lock_file.close()


if __name__ == '__main__':
    os.umask(0o077)
    asyncio.run(Worker().run())
