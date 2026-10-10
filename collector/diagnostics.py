"""Bounded diagnostic queue; independent from the business receipt outbox."""
import json
import sqlite3
import sys
import time
import uuid
from core import utcnow


class Diagnostics:
    def __init__(self, path, max_bytes=4 * 1024 * 1024):
        self.db = sqlite3.connect(path)
        self.db.execute('PRAGMA journal_mode=WAL')
        self.db.execute('PRAGMA synchronous=FULL')
        self.db.executescript('''CREATE TABLE IF NOT EXISTS diagnostics (
          seq INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT UNIQUE, body TEXT);
          CREATE TABLE IF NOT EXISTS diagnostic_meta (id INTEGER PRIMARY KEY, failures INTEGER);
          INSERT OR IGNORE INTO diagnostic_meta VALUES(1,0);''')
        self.max_bytes = max_bytes
        self.failures = self.db.execute('SELECT failures FROM diagnostic_meta WHERE id=1').fetchone()[0]
        self.windows = {}

    def record(self, event, level='info', **context):
        now = time.monotonic()
        key = (event, context.get('channel_id'), context.get('code'))
        if level in ('warn', 'error'):
            old = self.windows.get(key)
            if old and now-old[0] < 60:
                old[1] += 1
                return
            self.flush()
            if len(self.windows)<1000:
                self.windows[key] = [now, 0, event, level, context]
        self.append(event, level, context)

    def flush(self):
        now = time.monotonic()
        for key, (start, count, event, level, context) in list(self.windows.items()):
            if now-start >= 60:
                if count:
                    self.append(event+'.repeated', level, dict(context, repeat_count=count))
                del self.windows[key]

    def append(self, event, level, context):
        # Callers supply codes/locations only, never exception text or payloads.
        allowed = {'code','stage','exception_type','close_code','file','function','line',
                   'last_heartbeat','channel_id','state','previous_state','config_version','repeat_count','generation'}
        context = {k:v for k,v in context.items() if k in allowed and isinstance(v,(str,int,bool,float))}
        context = {k:(v[:256] if isinstance(v,str) else v) for k,v in context.items()}
        row = {'event_id':str(uuid.uuid4()),'component':'collector','event':event,
               'level':level,'message':event,'context':context,'occurred_at':utcnow()}
        body=json.dumps(row,separators=(',',':'))
        try:
            size=self.db.execute('SELECT coalesce(sum(length(body)),0) FROM diagnostics').fetchone()[0]
            if size+len(body.encode()) > self.max_bytes:raise RuntimeError('diagnostic capacity')
            with self.db:self.db.execute('INSERT INTO diagnostics(event_id,body) VALUES(?,?)',(row['event_id'],body))
        except Exception:
            self.failures+=1
            try:
                with self.db:self.db.execute('UPDATE diagnostic_meta SET failures=? WHERE id=1',(self.failures,))
            except Exception:pass
            print(json.dumps({'event':'diagnostics.incomplete','failures':self.failures,'code':event}),file=sys.stderr)

    def next(self):
        row=self.db.execute('SELECT body FROM diagnostics ORDER BY seq LIMIT 1').fetchone()
        return json.loads(row[0]) if row else None

    def ack(self, event_id):
        with self.db:self.db.execute('DELETE FROM diagnostics WHERE event_id=?',(event_id,))
