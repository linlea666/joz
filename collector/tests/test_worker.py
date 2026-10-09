import asyncio
import json
import os
import tempfile
import unittest
from unittest.mock import patch
from worker import Worker, Gateway

class WorkerTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.directory=tempfile.TemporaryDirectory()
        self.env=patch.dict(os.environ, {'DISCORD_BUFFER_DIR':self.directory.name})
        self.env.start()
        self.worker=Worker()
        self.worker.channels={'200':{}}
    async def asyncTearDown(self):
        if self.worker.recovery_task:
            self.worker.recovery_task.cancel()
        self.worker.box.db.close()
        self.worker.lock_file.close()
        self.env.stop()
        self.directory.cleanup()
    async def test_sdk_options_and_raw_capture(self):
        gateway=Gateway(self.worker)
        self.worker.capture(json.dumps({'op':0,'t':'MESSAGE_CREATE','s':19,'d':{'id':'900','channel_id':'200','content':'new'}}))
        self.worker.capture(json.dumps({'op':0,'t':'MESSAGE_UPDATE','s':20,'d':{'id':'900','channel_id':'200','content':'edit'}}))
        self.worker.capture(json.dumps({'op':0,'t':'MESSAGE_DELETE_BULK','s':21,'d':{'ids':['900','901'],'channel_id':'200'}}))
        self.assertEqual(self.worker.box.count(),4)
        self.assertEqual(self.worker.box.next()['sequence'],19)
        await gateway.close()
    async def test_no_extra_fetch_when_edit_cache_present(self):
        self.worker.box.cache({'id':'900','channel_id':'200','content':'old','author':{'id':'7'},'timestamp':'2026-10-09T12:00:00Z','attachments':[1]})
        event={'kind':'MESSAGE_UPDATE','channel_id':'200','message_id':'900','payload':{'id':'900','channel_id':'200','content':'new'}}
        result=await self.worker.hydrate(event)
        self.assertEqual(result['attachments'],[1])
        self.assertEqual(result['content'],'new')
    async def test_buffer_full_halts_connection(self):
        self.worker.box.max_bytes=1
        self.worker.capture({'op':0,'t':'MESSAGE_CREATE','d':{'id':'900','channel_id':'200'}})
        self.assertTrue(self.worker.halted)
        self.assertEqual(self.worker.state,'buffer_failed')
    async def test_lost_ack_replays_same_identity(self):
        worker=self.worker
        worker.box.append('MESSAGE_CREATE',{'id':'900','channel_id':'200','author':{'id':'7'},'timestamp':'2026-10-09T12:00:00Z','content':'new'})
        worker.writer=object();worker.configured.set();sent=[]
        async def send(frame):
            sent.append(frame)
            future=worker.acks[frame['data']['event_id']]
            if len(sent)==1:
                future.set_exception(ConnectionError('lost ack'))
            else:
                future.set_result(None)
        worker.send=send
        task=asyncio.create_task(worker.deliver())
        try:
            for _ in range(35):
                if len(sent)>1:break
                await asyncio.sleep(.1)
            self.assertEqual(len(sent),2)
            self.assertEqual(sent[0]['data']['event_id'],sent[1]['data']['event_id'])
            self.assertEqual(worker.box.count(),0)
        finally:
            task.cancel()
            try:await task
            except asyncio.CancelledError:pass

    async def test_ready_refreshes_inventory_but_resume_never_fetches(self):
        from unittest.mock import AsyncMock
        worker=self.worker
        worker.send=AsyncMock()
        worker.fetch_history=AsyncMock(side_effect=AssertionError('Resume must not fetch'))
        gateway=Gateway(worker)
        await gateway.on_resumed()
        self.assertEqual(worker.state,'connected')
        worker.fetch_history.assert_not_called()
        await gateway.on_ready()
        self.assertEqual(worker.state,'recovering')
        worker.send.assert_awaited_once_with({'type':'refresh'})
        self.assertIsNotNone(worker.box.recovery('200'))
        await gateway.close()

    async def test_backend_checkpoint_precedes_recovery_anchor(self):
        from unittest.mock import AsyncMock
        worker=self.worker
        worker.send=AsyncMock()
        await worker.configure({'enabled':False,'channels':{'200':{}},'version':'v1',
                                'checkpoints':[{'channel_id':'200','last_message_id':'1234'}]})
        self.assertEqual(worker.box.recovery('200')[0],'1234')

    async def test_incomplete_history_stays_gap_after_new_event_checkpoint(self):
        from types import SimpleNamespace
        from unittest.mock import AsyncMock
        worker=self.worker
        worker.send=AsyncMock()
        worker.client=SimpleNamespace(get_channel=lambda _:SimpleNamespace(guild=None))
        worker.box.advance('200','100',1)
        async def fetch(channel,limit,before=None):
            top=before-1 if before else 3000
            return [{'id':str(i)} for i in range(top,top-100,-1)]
        worker.fetch_history=fetch
        # Model successful backend receipts while preserving the oldest gap anchor.
        worker.box.drained=lambda _: True
        await worker.recover()
        self.assertEqual(worker.channel_states['200'][0],'gap')
        worker.box.advance('200','4000')
        self.assertEqual(worker.box.recovery('200')[0],'100')
        self.assertEqual(worker.box.count(),1000)

    async def test_recovery_verifies_active_cards(self):
        from types import SimpleNamespace
        from unittest.mock import AsyncMock
        worker=self.worker
        worker.send=AsyncMock()
        worker.active_cards=[{'channel_id':'200','message_id':'80'}]
        worker.client=SimpleNamespace(get_channel=lambda _:SimpleNamespace(guild=None),
                                     http=SimpleNamespace(get_message=AsyncMock(return_value={'id':'80','content':'edited'})))
        worker.fetch_history=AsyncMock(return_value=[])
        worker.box.drained=lambda _: True
        await worker.recover()
        worker.client.http.get_message.assert_awaited_once_with(200,80)
        self.assertEqual(worker.box.next()['kind'],'MESSAGE_UPDATE')
        self.assertEqual(worker.channel_states['200'][0],'ready')

    async def test_ready_frame_freezes_gap_before_sdk_ready_callback(self):
        worker=self.worker
        worker.box.advance('200','100',1)
        worker.capture({'op':0,'t':'READY','s':1,'d':{'session_id':'new'}})
        worker.box.advance('200','300')
        self.assertEqual(worker.box.recovery('200')[0],'100')
        self.assertTrue(worker.recovery_barrier)
        self.assertEqual(worker.channel_states['200'][0],'recovering')
