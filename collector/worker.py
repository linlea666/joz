"""Single personal-account Gateway collector. No trade or exchange code here."""
import asyncio
import json
import logging
import os
import signal
import time
import traceback
import uuid
from contextlib import suppress

import discord
from diagnostics import Diagnostics
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
        self.generation = owner.generation

    def current(self):
        return self.owner.client is self and self.generation == self.owner.generation

    def dispatch(self, event, /, *args, **kwargs):
        if event == 'socket_raw_receive' and self.current():
            # Synchronous capture preserves wire order before asynchronous SDK
            # callbacks and before any model/network consumer can run.
            self.owner.capture(args[0])
        return super().dispatch(event, *args, **kwargs)

    async def on_ready(self):
        if not self.current() or self.owner.halted:
            return
        self.owner.invalidate_notifications()
        self.owner.state = 'recovering'
        for channel in self.owner.channels:
            self.owner.channel_generations[channel] = self.owner.channel_generations.get(channel, 0) + 1
            self.owner.box.begin_recovery(channel)
            self.owner.channel_states[channel] = ('recovering', '')
        # Get the CURRENT active-card inventory on every new session. Resume
        # has its own callback and does not trigger this bounded REST repair.
        with suppress(ConnectionError, OSError):
            await self.owner.send({'type':'refresh'})

    async def on_disconnect(self):
        if self.current() and not self.owner.halted:
            self.owner.last_heartbeat = None
            self.owner.invalidate_notifications()
            self.owner.state = 'reconnecting'

    async def on_resumed(self):
        if not self.current() or self.owner.halted:
            return
        incomplete = {ch for ch in self.owner.channels if self.owner.box.recovery(ch)}
        self.owner.state = 'recovering' if any(self.owner.channels[ch].get('trade_recovery', True) for ch in incomplete) else 'connected'
        if incomplete:
            with suppress(ConnectionError, OSError):
                await self.owner.send({'type':'refresh'})
        self.owner.notification_since = {ch:time.time() for ch in self.owner.channels if self.owner.channel_states.get(ch, ('',))[0]=='ready'}
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
        self.diagnostics = Diagnostics(os.path.join(directory, 'diagnostics.db'))
        self.diagnostics_enabled = False
        self.generation = 0
        self.error_detail = {}
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
        self.recovery_pending = set()
        self.notification_since = {}
        self.notification_epoch = ''
        self.notification_supported = False
        self.notification_instance = str(uuid.uuid4())
        self.channel_generations = {}

    def invalidate_notifications(self):
        self.notification_since.clear()
        self.notification_instance = str(uuid.uuid4())

    def capture(self, raw):
        try:
            packet = json.loads(raw) if isinstance(raw, (str, bytes)) else raw
            if not isinstance(packet, dict):
                raise ValueError('invalid envelope')
            if packet.get('op') == 11:
                self.last_heartbeat = utcnow()
                return
            if packet.get('op') != 0 or self.halted:
                return
            kind = packet.get('t')
            if kind == 'READY':
                data = packet.get('d')
                if not isinstance(data, dict):
                    raise ValueError('invalid ready')
                self.session_id = data.get('session_id')
                self.invalidate_notifications()
                self.state = 'recovering'
                for channel in self.channels:
                    self.channel_generations[channel] = self.channel_generations.get(channel, 0) + 1
                    self.box.begin_recovery(channel)
                    self.channel_states[channel] = ('recovering', '')
                # Identity distinguishes a new READY arriving while an older
                # status publication is waiting for the local socket to drain.
                self.recovery_barrier = object()
                self.wake.set()
                return
            if kind not in {'MESSAGE_CREATE','MESSAGE_UPDATE','MESSAGE_DELETE','MESSAGE_DELETE_BULK'}:
                return
            data = packet.get('d')
            if not isinstance(data, dict) or not str(data.get('channel_id','')).isdecimal():
                raise ValueError('invalid message envelope')
            channel = str(data['channel_id'])
            if channel not in self.channels:
                return
            if not self.channels[channel].get('trade_recovery', True) and kind != 'MESSAGE_CREATE':
                return
            if any(key in data and not isinstance(data[key], typ) for key,typ in (('content',str),('attachments',list),('embeds',list),('author',dict))):
                raise ValueError('invalid message fields')
            ids = data.get('ids') if kind == 'MESSAGE_DELETE_BULK' else [data.get('id')]
            if not isinstance(ids,list) or not ids or any(not isinstance(i,(str,int)) or isinstance(i,bool) or not str(i).isdecimal() for i in ids):
                raise ValueError('invalid message identity')
            # Validate all IDs before appending any bulk event.
            for message_id in ids:
                self.box.append(kind, dict(data,id=str(message_id),channel_id=channel),
                                session=self.session_id, sequence=packet.get('s'), config_version=self.version, origin='gateway',
                                notification_epoch=self.notification_epoch, notification_instance=self.notification_instance,
                                notification_realtime=bool(self.notification_supported and self.writer and self.configured.is_set() and self.state=='connected' and kind=='MESSAGE_CREATE' and self.channel_states.get(channel, ('',))[0]=='ready' and channel in self.notification_since and snowflake_time(message_id)>=self.notification_since[channel]))
            self.wake.set()
        except (ValueError, TypeError, KeyError) as exc:
            self.fail_buffer(exc, 'MESSAGE_PAYLOAD_INVALID')
        except Exception as exc:
            self.fail_buffer(exc)

    def record_error(self, code, stage, exc=None):
        detail = {'code':code,'stage':stage,'occurred_at':utcnow()}
        if self.last_heartbeat:detail['last_heartbeat']=self.last_heartbeat
        if exc:
            detail['exception_type'] = type(exc).__name__
            if isinstance(getattr(exc,'code',None),int):detail['close_code'] = exc.code
            frames=traceback.extract_tb(exc.__traceback__)
            if frames:
                frame=frames[-1]
                detail.update(file=os.path.basename(frame.filename),function=frame.name,line=frame.lineno)
        self.error_detail = detail
        self.last_error = code
        self.diagnostics.record('gateway.error','error',**detail)

    def fail_buffer(self, exc=None, code='EVENT_BUFFER_FAILED'):
        self.halted = True
        self.state = 'buffer_failed'
        self.record_error(code,'capture',exc)
        if self.client:
            asyncio.create_task(self.client.close())

    async def send(self, frame):
        async with self.write_lock:
            if self.writer is None:
                raise ConnectionError('backend unavailable')
            barrier = self.recovery_barrier
            if barrier and (frame is None or frame['type'] != 'hello'):
                # Publish CURRENT states, without changing them. A recovery
                # may already have completed with an empty outbox. Keep the
                # snapshot and the following event ordered on the same stream.
                for channel in self.channels:
                    self.write_frame(self.channel_frame(channel))
            else:
                barrier = None
            if frame is not None:
                if frame['type'] == 'channel':
                    # A READY or configuration change can occur while waiting
                    # for write_lock. Never publish a captured, obsolete state.
                    frame = self.channel_frame(frame['data']['channel_id'])
                elif frame['type'] == 'event':
                    event = frame['data']
                    if event.get('notification_instance') != self.notification_instance:
                        event['notification_realtime'] = False
                        event.pop('notification_payload', None)
                if frame is not None:
                    self.write_frame(frame)
            await self.writer.drain()
            if barrier and self.recovery_barrier is barrier:
                self.recovery_barrier = False

    def write_frame(self, frame):
        # Only called while write_lock is held; no await before writer.write.
        self.writer.write((json.dumps(frame, separators=(',', ':')) + '\n').encode())

    def channel_frame(self, channel):
        if channel not in self.channels:
            return None
        state, error = self.channel_states.get(channel, ('recovering', ''))
        checkpoint = self.box.recovery(channel) or self.box.checkpoint(channel)
        return {'type': 'channel', 'data': {
            'channel_id': channel, 'state': state, 'last_error': error,
            'last_message_id': checkpoint[0] if checkpoint else '', 'updated_at': utcnow()}}

    async def channel_status(self, channel, state, error=''):
        if self.channel_states.get(channel) != (state,error):
            self.diagnostics.record('channel.state', 'warn' if error else 'info',channel_id=channel,state=state)
        self.channel_states[channel] = (state, error)
        await self.send(self.channel_frame(channel))

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
                if self.recovery_barrier and self.writer and self.configured.is_set():
                    # An empty outbox must not postpone publication until the
                    # first live message. send() also checks again after awaits.
                    await self.send(None)
                event = self.box.next()
                if not event or not self.writer or not self.configured.is_set():
                    self.wake.clear()
                    try:
                        await asyncio.wait_for(self.wake.wait(), 2)
                    except asyncio.TimeoutError:
                        pass
                    continue
                # Never turn buffered receipts from an earlier process/connection
                # into live email, even when the Go backend did not restart.
                event['notification_realtime'] = bool(event.get('notification_realtime') and event.get('notification_instance') == self.notification_instance)
                if event['notification_realtime']:
                    event['notification_payload'] = event['payload']
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

    def schedule_recovery(self, channels=None):
        # Incremental subscriptions cannot put healthy trading channels behind
        # a new notification channel's permissions/history barrier.
        selected = set(self.channels if channels is None else channels)
        for channel in selected:
            self.box.begin_recovery(channel)
            self.channel_states[channel] = ('recovering', '')
            self.notification_since.pop(channel, None)
            self.channel_generations[channel] = self.channel_generations.get(channel, 0) + 1
        self.recovery_pending.update(selected)
        if self.recovery_task is None or self.recovery_task.done():
            self.recovery_task = asyncio.create_task(self.drain_recovery())

    async def drain_recovery(self):
        while self.recovery_pending:
            selected = self.recovery_pending.copy()
            self.recovery_pending.difference_update(selected)
            await self.recover(selected)

    async def recover(self, channels=None):
        for channel in list(self.channels if channels is None else channels):
            if channel not in self.channels:
                continue
            generation = self.channel_generations.get(channel, 0)
            client = self.client
            def current():
                return channel in self.channels and self.channel_generations.get(channel, 0) == generation and self.client is client
            trade_recovery = self.channels[channel].get('trade_recovery', True)
            try:
                await self.channel_status(channel, 'recovering')
                obj = (client.get_channel(int(channel)) if trade_recovery else None) or await client.fetch_channel(int(channel))
                if not current():
                    continue
                guild = getattr(obj, 'guild', None)
                if guild:
                    await guild.subscribe(typing=True, activities=False, threads=False, member_updates=False)
                if not current():
                    continue
                if not trade_recovery:
                    # Permission/subscription validation only; never read history
                    # for a channel whose sole consumer is realtime email.
                    me = getattr(guild, 'me', None) if guild else None
                    if me is not None and hasattr(obj, 'permissions_for') and not obj.permissions_for(me).view_channel:
                        raise PermissionError('channel not visible')
                    self.box.end_recovery(channel, True)
                    self.notification_since[channel] = time.time()
                    await self.channel_status(channel, 'ready')
                    continue
                start, baseline_at, prior_state = self.box.begin_recovery(channel)
                if start != '0':
                    history, complete = await bounded_history(self.fetch_history, channel, start)
                else:
                    history, complete = await bounded_history(self.fetch_history, channel, baseline_snowflake(baseline_at), include_baseline=True)
                if not current():
                    continue
                for msg in history:
                    msg['channel_id'] = channel
                    self.box.append('MESSAGE_CREATE', msg, baseline=snowflake_time(msg['id']) < baseline_at, config_version=self.version, origin='recovery')
                for card in self.active_cards:
                    if card['channel_id'] == channel:
                        try:
                            msg = await self.client.http.get_message(int(channel), int(card['message_id']))
                            if not current():
                                break
                            self.box.append('MESSAGE_UPDATE', dict(msg, channel_id=channel), config_version=self.version)
                        except discord.NotFound:
                            if not current():
                                break
                            self.box.append('MESSAGE_DELETE', {'channel_id':channel, 'id':card['message_id']}, config_version=self.version)
                if not current():
                    continue
                watermark = self.box.watermark()
                self.wake.set()
                while current() and not self.box.drained(watermark):
                    await asyncio.sleep(0.1)
                if not current():
                    continue
                if not self.box.checkpoint(channel):
                    self.box.advance(channel, '0', baseline_at)
                self.box.end_recovery(channel, complete)
                if complete:
                    self.notification_since[channel] = time.time()
                await self.channel_status(channel, 'ready' if complete else 'gap',
                                          '' if complete else 'history exceeds 1000 messages; source review required')
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                if not current():
                    continue
                self.box.end_recovery(channel, False)
                status = getattr(exc, 'status', None)
                await self.channel_status(channel, 'forbidden' if status in (403, 404) or isinstance(exc, PermissionError) else 'gap',
                                          'source recovery incomplete')

        if not self.halted and self.state not in {'reconnecting', 'connection_failed', 'auth_invalid', 'disabled'}:
            self.state = 'connected'

    async def run_gateway(self, client, token):
        generation = self.generation
        try:
            await client.start(token, reconnect=True)
            if self.client is client and not self.halted:
                self.state = 'connection_failed'
                self.record_error('GATEWAY_TASK_ENDED','receive')
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            if self.client is client and generation == self.generation and not self.halted:
                invalid = isinstance(exc,discord.LoginFailure) or getattr(exc,'code',None) == 4004
                self.state = 'auth_invalid' if invalid else 'connection_failed'
                self.record_error('GATEWAY_AUTH_INVALID' if invalid else 'GATEWAY_TASK_FAILED','receive',exc)
        finally:
            if self.client is client and generation == self.generation:
                self.last_heartbeat = None
                if self.recovery_task:
                    self.recovery_task.cancel()
                    await asyncio.gather(self.recovery_task,return_exceptions=True)
            # Invalidate SDK callbacks before close, including delayed on_ready.
            if generation == self.generation:self.generation += 1
            try:
                await client.close()
            except Exception as exc:
                if self.client is client:
                    self.state = 'connection_failed'
                    self.record_error('GATEWAY_CLOSE_FAILED','close',exc)

    async def configure(self, data):
        async with self.config_lock:
            token = data.get('token', '') if data.get('enabled') else ''
            channels = data.get('channels', {})
            affected = {ch for ch in channels if ch not in self.channels or channels[ch].get('trade_recovery', True) != self.channels[ch].get('trade_recovery', True)}
            changed = set(channels) != set(self.channels) or bool(affected)
            token_changed = token != self.token
            restart = token_changed or bool(token and (self.client_task is None or self.client_task.done()) and self.state != 'auth_invalid' and not self.halted)
            if restart:
                self.generation += 1
                self.recovery_pending.clear()
                self.invalidate_notifications()
                if self.recovery_task:
                    self.recovery_task.cancel()
                    await asyncio.gather(self.recovery_task,return_exceptions=True)
                if self.client:
                    await self.client.close()
                if self.client_task:
                    self.client_task.cancel()
                    await asyncio.gather(self.client_task,return_exceptions=True)
            self.diagnostics_enabled = data.get('diagnostics_v1') is True
            for ch in set(self.channels)-set(channels):
                self.notification_since.pop(ch, None)
                self.channel_generations[ch] = self.channel_generations.get(ch, 0) + 1
                self.channel_states.pop(ch, None)
                self.recovery_pending.discard(ch)
            self.channels, self.active_cards = channels, data.get('active_cards', [])
            epoch = data.get('notification_epoch', '')
            if epoch != self.notification_epoch or restart or not self.configured.is_set():
                self.notification_since = {ch:time.time() for ch in channels if self.channel_states.get(ch, ('',))[0]=='ready'}
            self.notification_epoch = epoch
            self.notification_supported = data.get('channel_notification_v1') is True
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
            if restart and not self.halted:
                self.token = token
                self.error_detail = {}
                self.last_error = ''
                self.last_heartbeat = None
                self.state = 'connecting' if token else 'disabled'
                self.client = Gateway(self) if token else None
                self.client_task = asyncio.create_task(self.run_gateway(self.client, token)) if token else None
            elif (changed or self.state == "recovering" or any(self.channel_states.get(ch, ("",))[0] != "ready" for ch in channels)) and not self.halted and self.client and self.client_task and not self.client_task.done() and self.client.is_ready():
                self.schedule_recovery(affected | {ch for ch in channels if self.channel_states.get(ch, ('',))[0] != 'ready'})
            self.configured.set()
            for ch in list(self.channel_states):
                if ch in channels:
                    # Report the live state; do not write a snapshot back over
                    # recovery progress made while a previous frame was sent.
                    await self.send(self.channel_frame(ch))
            return {'applied_version': self.version, 'channel_notification_v1': True}

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

    async def diagnostic_loop(self):
        previous = None
        while True:
            try:
                self.diagnostics.flush()
                if previous != self.state:
                    self.diagnostics.record('gateway.state',state=self.state,previous_state=previous or '',generation=self.generation)
                    previous = self.state
                event = self.diagnostics.next()
                if event and self.writer and self.configured.is_set() and self.diagnostics_enabled:
                    future = asyncio.get_running_loop().create_future()
                    self.acks[event['event_id']] = future
                    try:
                        await self.send({'type':'diagnostic','data':event})
                        await asyncio.wait_for(future,10)
                        self.diagnostics.ack(event['event_id'])
                    finally:
                        self.acks.pop(event['event_id'],None)
                else:
                    await asyncio.sleep(1)
            except asyncio.CancelledError:raise
            except Exception:
                logging.warning("diagnostic delivery pending; retained for retry")
                await asyncio.sleep(2)

    async def status_loop(self):
        while True:
            with suppress(ConnectionError, OSError):
                data = {'state': self.state, 'last_error': self.last_error,
                        'applied_version': self.version, 'backlog': self.box.count(),
                        'error_detail':self.error_detail, 'diagnostic_failures':self.diagnostics.failures}
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
                    if frame['type'] in ('ack','diagnostic_ack'):
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
                self.invalidate_notifications()
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
            await asyncio.gather(self.connect_backend(), self.deliver(), self.status_loop(), self.diagnostic_loop())
        finally:
            if self.client:
                await self.client.close()
            self.diagnostics.db.close()
            self.box.db.close()
            self.lock_file.close()


if __name__ == '__main__':
    os.umask(0o077)
    asyncio.run(Worker().run())
