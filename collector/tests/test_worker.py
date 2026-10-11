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
        self.worker.diagnostics.db.close()
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
        worker.client=gateway
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

    async def test_sdk_control_payloads_do_not_break_capture(self):
        gateway=Gateway(self.worker)
        self.worker.client=gateway
        # Exercise the real SDK dispatch entry, before its own parser.
        for packet in ({'op':0,'t':'SESSIONS_REPLACE','d':[{'status':'online'}]},
                       {'op':9,'d':True}, {'op':1,'d':12}, {'op':11,'d':None}):
            gateway.dispatch('socket_raw_receive',json.dumps(packet))
        self.assertIsNotNone(self.worker.last_heartbeat)
        self.assertEqual(self.worker.box.count(),0)
        gateway.dispatch('socket_raw_receive',json.dumps({'op':0,'t':'MESSAGE_DELETE','d':{'id':'900','channel_id':'200'}}))
        self.assertEqual(self.worker.box.count(),1)
        await gateway.close()

    async def test_malformed_message_stops_new_risk_with_safe_error(self):
        self.worker.capture({'op':0,'t':'MESSAGE_UPDATE','d':{'channel_id':'200','id':[]}})
        self.assertTrue(self.worker.halted)
        self.assertEqual(self.worker.error_detail['code'],'MESSAGE_PAYLOAD_INVALID')

    async def test_fatal_task_is_closed_and_same_token_reapplies_once(self):
        from unittest.mock import AsyncMock
        from types import SimpleNamespace
        worker=self.worker
        client=SimpleNamespace(start=AsyncMock(side_effect=AttributeError('secret-must-not-be-logged')),
                               close=AsyncMock(),is_ready=lambda:False)
        worker.client=client;worker.token='private';worker.last_heartbeat='old'
        worker.client_task=asyncio.create_task(worker.run_gateway(client,'private'))
        await worker.client_task
        client.close.assert_awaited()
        self.assertIsNone(worker.last_heartbeat)
        self.assertEqual(worker.error_detail['exception_type'],'AttributeError')
        self.assertNotIn('secret',json.dumps(worker.error_detail))
        worker.send=AsyncMock()
        new=SimpleNamespace(start=AsyncMock(side_effect=lambda *a,**k: asyncio.sleep(100)),close=AsyncMock(),is_ready=lambda:False)
        # An async fake that stays alive.
        async def start(*a,**k):await asyncio.sleep(100)
        new.start=start
        with patch('worker.Gateway',return_value=new) as factory:
            config={'enabled':True,'token':'private','channels':{},'version':'v2'}
            await asyncio.gather(worker.configure(config),worker.configure(config))
            self.assertEqual(factory.call_count,1)
            worker.client_task.cancel()
            with self.assertRaises(asyncio.CancelledError):await worker.client_task

    async def test_old_callbacks_cannot_change_current_state(self):
        old=Gateway(self.worker)
        self.worker.client=object();self.worker.generation+=1
        self.worker.state='connecting'
        await old.on_ready();await old.on_disconnect();await old.on_resumed()
        self.assertEqual(self.worker.state,'connecting')
        await old.close()

    async def test_auth_failure_and_buffer_failure_do_not_restart(self):
        from unittest.mock import AsyncMock
        worker=self.worker;worker.send=AsyncMock();worker.token='private'
        worker.client_task=asyncio.create_task(asyncio.sleep(0));await worker.client_task
        for state,halted in [('auth_invalid',False),('buffer_failed',True)]:
            worker.state=state;worker.halted=halted
            with patch('worker.Gateway') as factory:
                await worker.configure({'enabled':True,'token':'private','channels':{},'version':'same'})
                factory.assert_not_called()
            self.assertEqual(worker.state,state)

    async def test_resume_with_gap_does_not_claim_ready(self):
        from unittest.mock import AsyncMock
        worker=self.worker;worker.send=AsyncMock();worker.box.begin_recovery('200')
        gateway=Gateway(worker);worker.client=gateway
        await gateway.on_resumed()
        self.assertEqual(worker.state,'recovering')
        worker.send.assert_awaited_once_with({'type':'refresh'})
        await gateway.close()

    async def test_diagnostics_require_capability_and_lost_ack_retries(self):
        from unittest.mock import AsyncMock
        worker=self.worker;worker.writer=object();worker.configured.set();sent=[]
        async def send(frame):
            sent.append(frame)
            future=worker.acks[frame['data']['event_id']]
            if len(sent)==1:future.set_exception(ConnectionError('lost'))
            else:future.set_result(None)
        worker.send=send
        task=asyncio.create_task(worker.diagnostic_loop())
        try:
            await asyncio.sleep(.01);self.assertEqual(sent,[])
            worker.diagnostics_enabled=True
            for _ in range(45):
                if len(sent)>1:break
                await asyncio.sleep(.1)
            self.assertGreaterEqual(len(sent),2)
            self.assertEqual(sent[0]['data']['event_id'],sent[1]['data']['event_id'])
            self.assertEqual(worker.box.count(),0)
        finally:
            task.cancel()
            with self.assertRaises(asyncio.CancelledError):await task

    async def test_notification_realtime_and_resume_boundary(self):
        import time
        from core import baseline_snowflake
        from unittest.mock import AsyncMock
        w=self.worker;w.writer=object();w.configured.set();w.state='connected'
        w.notification_supported=True;w.notification_epoch='backend-1'
        w.notification_since={'200':time.time()-1};w.channel_states={'200':('ready','')}
        mid=baseline_snowflake(time.time())
        w.capture({'op':0,'t':'MESSAGE_CREATE','d':{'id':mid,'channel_id':'200'}})
        event=w.box.next();self.assertTrue(event['notification_realtime'])
        self.assertEqual(event['notification_epoch'],'backend-1');w.box.ack(event['event_id'])
        gateway=Gateway(w);w.client=gateway;w.send=AsyncMock()
        await gateway.on_disconnect()
        w.capture({'op':0,'t':'MESSAGE_CREATE','d':{'id':mid,'channel_id':'200'}})
        event=w.box.next();self.assertFalse(event['notification_realtime']);w.box.ack(event['event_id'])
        await gateway.on_resumed()
        w.capture({'op':0,'t':'MESSAGE_CREATE','d':{'id':mid,'channel_id':'200'}})
        self.assertFalse(w.box.next()['notification_realtime'])
        await gateway.close()

    async def test_notification_only_permission_check_never_fetches_history(self):
        from types import SimpleNamespace
        from unittest.mock import AsyncMock
        w=self.worker;w.channels={'200':{'trade_recovery':False}};w.send=AsyncMock()
        w.client=SimpleNamespace(fetch_channel=AsyncMock(return_value=SimpleNamespace(guild=None)))
        w.fetch_history=AsyncMock(side_effect=AssertionError('no notification history'))
        await w.recover()
        self.assertEqual(w.channel_states['200'][0],'ready')
        w.fetch_history.assert_not_called();self.assertEqual(w.box.count(),0)
        w.capture({'op':0,'t':'MESSAGE_UPDATE','d':{'id':'9','channel_id':'200'}})
        self.assertEqual(w.box.count(),0)

    async def test_adding_notification_leaves_trade_channel_ready(self):
        from types import SimpleNamespace
        from unittest.mock import AsyncMock
        w=self.worker;w.send=AsyncMock();w.token='same';w.state='connected'
        w.channels={'200':{'trade_recovery':True}};w.channel_states={'200':('ready','')}
        w.client=SimpleNamespace(is_ready=lambda:True)
        w.client_task=asyncio.create_task(asyncio.sleep(100))
        w.schedule_recovery=lambda channels=None:setattr(w,'selected_recovery',channels)
        try:
            await w.configure({'token':'same','enabled':True,'version':'new',
                'channels':{'200':{'trade_recovery':True},'300':{'trade_recovery':False}},
                'notification_epoch':'boot','channel_notification_v1':True})
            self.assertEqual(w.selected_recovery,{'300'})
            self.assertEqual(w.channel_states['200'][0],'ready')
            self.assertEqual(w.state,'connected')
        finally:
            w.client_task.cancel()
            with self.assertRaises(asyncio.CancelledError):await w.client_task

    async def test_removed_channel_cannot_be_marked_ready_by_old_check(self):
        from types import SimpleNamespace
        from unittest.mock import AsyncMock
        w=self.worker;w.channels={'200':{'trade_recovery':False}};w.send=AsyncMock()
        entered=asyncio.Event();release=asyncio.Event()
        async def fetch(_):
            entered.set();await release.wait();return SimpleNamespace(guild=None)
        w.client=SimpleNamespace(fetch_channel=fetch)
        recovery=asyncio.create_task(w.recover())
        await entered.wait()
        await w.configure({'enabled':False,'channels':{},'version':'removed'})
        # Re-add the same ID with a new verification generation.
        await w.configure({'enabled':False,'channels':{'200':{'trade_recovery':False}},'version':'readded'})
        release.set();await recovery
        self.assertEqual(w.channel_states['200'][0],'recovering')
        self.assertNotIn('200',w.notification_since)

    async def test_notification_snapshot_and_collector_restart_boundary(self):
        from unittest.mock import AsyncMock
        w=self.worker;w.writer=object();w.configured.set()
        original={'id':'900','channel_id':'200','author':{'id':'7'},'timestamp':'2026-10-09T12:00:00Z','content':'first'}
        w.box.append('MESSAGE_CREATE',original,notification_realtime=True,notification_epoch='same-go',notification_instance=w.notification_instance)
        w.box.cache(dict(original,content='edited',edited_timestamp='2026-10-09T12:01:00Z'))
        sent=[]
        async def send(frame):
            sent.append(frame['data']);w.acks[frame['data']['event_id']].set_result(None)
        w.send=send
        task=asyncio.create_task(w.deliver())
        try:
            for _ in range(20):
                if sent:break
                await asyncio.sleep(.01)
            self.assertTrue(sent[0]['notification_realtime'])
            self.assertEqual(sent[0]['notification_payload']['content'],'first')
            self.assertEqual(sent[0]['payload']['content'],'edited')
            w.box.append('MESSAGE_CREATE',original,notification_realtime=True,notification_epoch='same-go',notification_instance='previous-worker')
            w.wake.set()
            for _ in range(20):
                if len(sent)>1:break
                await asyncio.sleep(.01)
            self.assertFalse(sent[1]['notification_realtime'])
        finally:
            task.cancel()
            with self.assertRaises(asyncio.CancelledError):await task

    async def test_notification_gap_does_not_put_trade_route_in_recovery_on_resume(self):
        from unittest.mock import AsyncMock
        w=self.worker;w.channels={'200':{'trade_recovery':True},'300':{'trade_recovery':False}}
        w.channel_states={'200':('ready',''),'300':('forbidden','no access')}
        w.box.begin_recovery('300');w.send=AsyncMock()
        gateway=Gateway(w);w.client=gateway
        await gateway.on_resumed()
        self.assertEqual(w.state,'connected')
        self.assertEqual(w.channel_states['200'][0],'ready')
        w.send.assert_awaited_once_with({'type':'refresh'})
        await gateway.close()

    async def test_local_ipc_reconnect_reestablishes_notification_boundary_without_gateway_restart(self):
        from types import SimpleNamespace
        from unittest.mock import AsyncMock
        w=self.worker;w.send=AsyncMock();w.token='same';w.state='connected'
        w.channel_states={'200':('ready','')};w.notification_epoch='same-go'
        w.client=SimpleNamespace(is_ready=lambda:True);w.client_task=asyncio.create_task(asyncio.sleep(100))
        w.invalidate_notifications();w.configured.clear()
        try:
            await w.configure({'enabled':True,'token':'same','channels':{'200':{}},'version':'v', 'notification_epoch':'same-go','channel_notification_v1':True})
            self.assertIn('200',w.notification_since)
            self.assertIsNone(w.recovery_task)
            self.assertEqual(w.state,'connected')
        finally:
            w.client_task.cancel()
            with self.assertRaises(asyncio.CancelledError):await w.client_task
