import asyncio
import tempfile
import unittest
from pathlib import Path
from core import Outbox, OutboxFull, merge_message, bounded_history

class DurabilityTests(unittest.TestCase):
    def test_restart_ack_and_capacity(self):
        with tempfile.TemporaryDirectory() as directory:
            path = str(Path(directory)/'events.db')
            box = Outbox(path, max_bytes=1024)
            event_id = box.append('MESSAGE_CREATE', {'id':'100','channel_id':'2','content':'a'})
            original = box.next()
            box.db.close()
            box = Outbox(path, max_bytes=1024)
            self.assertEqual(box.next(), original)
            with self.assertRaises(OutboxFull):
                box.append('MESSAGE_CREATE', {'id':'101','channel_id':'2','content':'x'*2048})
            self.assertEqual(box.count(),1)
            box.ack(event_id)
            self.assertEqual(box.count(),0)
            box.db.close()

    def test_gap_checkpoint_cannot_advance_past_missing_history(self):
        with tempfile.TemporaryDirectory() as directory:
            box = Outbox(str(Path(directory)/'events.db'))
            box.advance('2','100',1)
            self.assertEqual(box.begin_recovery('2')[0],'100')
            box.advance('2','200',1)
            box.end_recovery('2',False)
            self.assertEqual(box.begin_recovery('2')[0],'100')
            box.end_recovery('2',True)
            self.assertEqual(box.begin_recovery('2')[0],'200')
            box.db.close()

    def test_edit_missing_empty_and_order(self):
        previous={'content':'a','attachments':[1],'edited_timestamp':'2026-10-09T12:00:00Z'}
        self.assertEqual(merge_message(previous,{'content':'b'})['attachments'],[1])
        self.assertEqual(merge_message(previous,{'attachments':[]})['attachments'],[])
        self.assertEqual(merge_message(previous,{'content':'old','edited_timestamp':'2026-10-09T11:00:00Z'}),previous)

class HistoryTests(unittest.IsolatedAsyncioTestCase):
    async def test_bounded_gap_and_order(self):
        calls=[]
        async def fetch(channel,limit,before=None):
            calls.append(before)
            top=before-1 if before else 3000
            return [{'id':str(i)} for i in range(top,top-100,-1)]
        rows,complete=await bounded_history(fetch,'2','1')
        self.assertFalse(complete)
        self.assertEqual(len(calls),10)
        self.assertEqual(len(rows),1000)
        self.assertEqual(rows,sorted(rows,key=lambda x:int(x['id'])))
        rows,complete=await bounded_history(fetch,'2','2950')
        self.assertTrue(complete)
        self.assertEqual(len(rows),50)

    async def test_first_baseline_repairs_more_than_one_page(self):
        calls=[]
        async def fetch(channel,limit,before=None):
            calls.append(before)
            top=before-1 if before else 1400
            return [{'id':str(i)} for i in range(top,top-100,-1)]
        rows,complete=await bounded_history(fetch,'2','1100',include_baseline=True)
        self.assertTrue(complete)
        self.assertEqual(len(calls),4)
        self.assertEqual(len([m for m in rows if int(m['id'])>1100]),300)
        self.assertTrue(any(int(m['id'])<1100 for m in rows))
