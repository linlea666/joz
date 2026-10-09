"""Durable transport primitives. This module has no Discord dependency."""
import json
import sqlite3
import time
import uuid
from datetime import datetime, timezone


def utcnow():
    return datetime.now(timezone.utc).isoformat().replace('+00:00', 'Z')


def snowflake_time(value):
    return ((int(value) >> 22) + 1420070400000) / 1000


def baseline_snowflake(timestamp):
    # Include every message in the activation millisecond; the baseline flag
    # still uses the exact activation time before Go creates any delivery.
    return str(max(0, (int(timestamp * 1000) - 1420070400000) << 22))


class OutboxFull(RuntimeError):
    pass


class Outbox:
    def __init__(self, path, max_bytes=128 * 1024 * 1024):
        self.db = sqlite3.connect(path)
        self.db.execute('PRAGMA journal_mode=WAL')
        self.db.execute('PRAGMA synchronous=FULL')
        self.db.executescript('''
          CREATE TABLE IF NOT EXISTS outbox (seq INTEGER PRIMARY KEY AUTOINCREMENT,
            event_id TEXT UNIQUE NOT NULL, channel_id TEXT NOT NULL, body TEXT NOT NULL);
          CREATE TABLE IF NOT EXISTS checkpoints (channel_id TEXT PRIMARY KEY,
            message_id TEXT NOT NULL, baseline_at REAL NOT NULL);
          CREATE TABLE IF NOT EXISTS recovery (channel_id TEXT PRIMARY KEY, message_id TEXT NOT NULL, baseline_at REAL NOT NULL, state TEXT NOT NULL);
          CREATE TABLE IF NOT EXISTS messages (channel_id TEXT, message_id TEXT,
            body TEXT NOT NULL, touched REAL NOT NULL, PRIMARY KEY(channel_id,message_id));
        ''')
        self.max_bytes = max_bytes

    def append(self, kind, payload, baseline=False, received_at=None, session=None, sequence=None, config_version=""):
        event = {'event_id': str(uuid.uuid4()), 'kind': kind,
                 'channel_id': str(payload['channel_id']), 'message_id': str(payload['id']),
                 'received_at': received_at or utcnow(), 'config_version':config_version, 'session': session or '', 'sequence': sequence or 0, 'baseline': baseline, 'payload': payload}
        body = json.dumps(event, separators=(',', ':'))
        size = self.db.execute('SELECT coalesce(sum(length(body)),0) FROM outbox').fetchone()[0]
        if size + len(body.encode()) > self.max_bytes:
            raise OutboxFull('durable event buffer is full')
        with self.db:
            self.db.execute('INSERT INTO outbox(event_id,channel_id,body) VALUES(?,?,?)',
                            (event['event_id'], event['channel_id'], body))
        return event['event_id']

    def next(self):
        row = self.db.execute('SELECT body FROM outbox ORDER BY seq LIMIT 1').fetchone()
        return json.loads(row[0]) if row else None

    def ack(self, event_id):
        with self.db:
            self.db.execute('DELETE FROM outbox WHERE event_id=?', (event_id,))

    def count(self):
        return self.db.execute('SELECT count(*) FROM outbox').fetchone()[0]

    def watermark(self):
        return self.db.execute('SELECT coalesce(max(seq),0) FROM outbox').fetchone()[0]

    def drained(self, watermark):
        return self.db.execute('SELECT count(*) FROM outbox WHERE seq<=?', (watermark,)).fetchone()[0] == 0

    def checkpoint(self, channel):
        return self.db.execute('SELECT message_id,baseline_at FROM checkpoints WHERE channel_id=?', (channel,)).fetchone()

    def advance(self, channel, message, baseline_at=None):
        old = self.checkpoint(channel)
        latest = max(int(old[0]) if old else 0, int(message or 0))
        with self.db:
            self.db.execute('INSERT OR REPLACE INTO checkpoints VALUES(?,?,?)',
                            (channel, str(latest), old[1] if old else (baseline_at or time.time())))

    def recovery(self, channel):
        return self.db.execute('SELECT message_id,baseline_at,state FROM recovery WHERE channel_id=?', (channel,)).fetchone()

    def begin_recovery(self, channel):
        old = self.recovery(channel)
        if old:
            return old
        checkpoint = self.checkpoint(channel)
        row = (checkpoint[0] if checkpoint else '0', checkpoint[1] if checkpoint else time.time(), 'recovering')
        with self.db:
            self.db.execute('INSERT INTO recovery VALUES(?,?,?,?)', (channel, *row))
        return row

    def end_recovery(self, channel, complete):
        with self.db:
            if complete:
                self.db.execute('DELETE FROM recovery WHERE channel_id=?', (channel,))
            else:
                self.db.execute("UPDATE recovery SET state='gap' WHERE channel_id=?", (channel,))

    def cached(self, channel, message):
        row = self.db.execute('SELECT body FROM messages WHERE channel_id=? AND message_id=?', (channel, message)).fetchone()
        return json.loads(row[0]) if row else None

    def cache(self, payload):
        with self.db:
            self.db.execute('INSERT OR REPLACE INTO messages VALUES(?,?,?,?)',
                            (str(payload['channel_id']), str(payload['id']), json.dumps(payload), time.time()))
            self.db.execute('DELETE FROM messages WHERE touched < ?', (time.time() - 30 * 86400,))


def merge_message(previous, patch):
    """Discord edits are patches: missing != explicitly empty/null."""
    if previous and previous.get('edited_timestamp'):
        incoming = patch.get('edited_timestamp')
        if incoming and datetime.fromisoformat(incoming.replace('Z', '+00:00')) < datetime.fromisoformat(previous['edited_timestamp'].replace('Z', '+00:00')):
            return previous.copy()
    merged = dict(previous or {})
    merged.update(patch)
    if previous and previous.get('edited_timestamp') and not patch.get('edited_timestamp'):
        merged['edited_timestamp'] = previous['edited_timestamp']
    return merged


async def bounded_history(fetch, channel, last_id, include_baseline=False):
    """Read at most ten pages. No timer invokes this function."""
    rows, before = [], None
    for _ in range(10):
        batch = await fetch(channel, 100, before=before)
        rows.extend(m for m in batch if include_baseline or int(m['id']) > int(last_id))
        if not batch or len(batch) < 100 or min(int(m['id']) for m in batch) <= int(last_id):
            return sorted(rows, key=lambda m: int(m['id'])), True
        before = min(int(m['id']) for m in batch)
    return sorted(rows, key=lambda m: int(m['id'])), False
