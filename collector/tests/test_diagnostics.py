import json
import tempfile
import unittest
from diagnostics import Diagnostics

class DiagnosticsTests(unittest.TestCase):
    def test_restart_ack_redaction_and_full_queue(self):
        with tempfile.TemporaryDirectory() as directory:
            path=directory+'/diagnostics.db';q=Diagnostics(path)
            q.record('gateway.error','error',code='FAILED',token='never-store',exception_type='ValueError')
            first=q.next();self.assertNotIn('never-store',json.dumps(first));q.db.close()
            q=Diagnostics(path);self.assertEqual(first,q.next());q.ack(first['event_id']);self.assertIsNone(q.next())
            q.max_bytes=1;q.record('buffer.failure','error',code='FULL');self.assertEqual(q.failures,1);q.db.close()
            q=Diagnostics(path);self.assertEqual(q.failures,1);q.db.close()
    def test_repeated_failures_flush_without_next_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            q=Diagnostics(directory+'/diagnostics.db')
            q.record('gateway.error','error',code='FAILED');q.record('gateway.error','error',code='FAILED')
            for window in q.windows.values():window[0]-=61
            q.flush();q.ack(q.next()['event_id']);self.assertEqual(q.next()['context']['repeat_count'],1);q.db.close()
