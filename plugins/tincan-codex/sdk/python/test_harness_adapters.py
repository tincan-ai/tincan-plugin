import asyncio
import json
import importlib
import sys
import types
import unittest
from pathlib import Path
from unittest.mock import AsyncMock

from adapters.copilot import CopilotWorkers
from adapters.common import serve, worker_prompt, parse_worker_outcome
from test_tincan import FakeClient


class CopilotTests(unittest.IsolatedAsyncioTestCase):
    async def test_dispatch_completion_dedup_and_failure(self):
        session = types.SimpleNamespace(send_and_wait=AsyncMock(return_value=types.SimpleNamespace(data=types.SimpleNamespace(content=json.dumps({"status":"completed","reply":"done"})))), abort=AsyncMock(), disconnect=AsyncMock())
        host = types.SimpleNamespace(create_session=AsyncMock(return_value=session))
        workers = CopilotWorkers(host, scope="Review changes", session_config={"on_permission_request": lambda *_: None})
        client = FakeClient()
        task = asyncio.create_task(client.dispatch_mentions(workers))
        await client.mention()
        for _ in range(100):
            if client.completed:
                break
            await asyncio.sleep(0)
        self.assertEqual(client.completed, [("conn", 1)])
        await client.mention()
        await asyncio.sleep(0)
        self.assertEqual(host.create_session.await_count, 1)
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)
        session.disconnect.assert_awaited_once()
        session.abort.assert_not_awaited()

        session.send_and_wait.side_effect = TimeoutError("uncertain")
        failed = FakeClient()
        task = asyncio.create_task(failed.dispatch_mentions(workers))
        await failed.mention()
        notice = await asyncio.wait_for(failed.worker_events.get(), 1)
        self.assertEqual(notice["event"], "needs_attention")
        self.assertFalse(failed.completed)
        self.assertIn(("conn", 1), failed.claimed)
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)
        session.abort.assert_awaited_once()

    async def test_worker_identity_recorded_before_send(self):
        session = types.SimpleNamespace(session_id="tincan-worker", disconnect=AsyncMock())
        host = types.SimpleNamespace(create_session=AsyncMock(return_value=session))
        record = AsyncMock()
        workers = CopilotWorkers(host, scope="Review", session_config={"on_permission_request": lambda *_: None}, record_worker=record)
        await workers({"worker_id": "tincan-worker", "event_seq": 7, "event": {}})
        self.assertEqual(host.create_session.call_args.kwargs["session_id"], "tincan-worker")
        self.assertEqual(record.call_args.args[0]["session_id"], "tincan-worker")

    async def test_no_implicit_permissions_or_reused_session(self):
        for config in ({}, {"session_id": "existing", "on_permission_request": lambda *_: None}):
            with self.assertRaises(ValueError):
                CopilotWorkers(None, scope="review", session_config=config)

    async def test_presence_removed_on_dispatch_failure(self):
        client = types.SimpleNamespace(call=AsyncMock(return_value={}), worker_events=asyncio.Queue())
        async def fail(*args, **kwargs):
            await asyncio.sleep(0)
            raise RuntimeError("transport lost")
        client.dispatch_mentions = fail
        with self.assertRaisesRegex(RuntimeError, "transport lost"):
            await serve(client, "conn", None, AsyncMock())
        self.assertEqual(client.call.call_args.kwargs, {"connection": "conn", "available": False})


class CursorTests(unittest.IsolatedAsyncioTestCase):
    async def test_terminal_state_and_cancel(self):
        from adapters.cursor import CursorWorkers
        run = types.SimpleNamespace(status="running", wait=AsyncMock(return_value=types.SimpleNamespace(status="cancelled", result="partial")), cancel=AsyncMock())
        agent = types.SimpleNamespace(send=AsyncMock(return_value=run), close=AsyncMock())
        factory = CursorWorkers(AsyncMock(return_value=agent), scope="Review only")
        worker = await factory({"event": {"payload": {"text": "review"}}})
        with self.assertRaisesRegex(RuntimeError, "completed reply"):
            await worker.wait()
        await worker.cancel()
        run.cancel.assert_awaited_once()
        agent.close.assert_awaited_once()
        run.status = "finished"
        run.wait.return_value = types.SimpleNamespace(status="finished", result=json.dumps({"status":"completed","reply":"done"}))
        worker = await factory({"event": {}})
        self.assertEqual(await worker.wait(), {"status": "completed", "reply": "done"})
        await worker.cancel()
        self.assertEqual(run.cancel.await_count, 1)


class HermesTests(unittest.IsolatedAsyncioTestCase):
    @classmethod
    def setUpClass(cls):
        root = Path(__file__).resolve().parents[2]
        cls.saved = {}
        def module(name, **attrs):
            cls.saved[name] = sys.modules.get(name)
            value = types.ModuleType(name)
            value.__dict__.update(attrs)
            sys.modules[name] = value
        class Base:
            def __init__(self, config, platform):
                self.config, self.platform = config, platform
            def build_source(self, **kwargs):
                return types.SimpleNamespace(**kwargs)
            async def handle_message(self, event):
                self.last_event = event
                event._gateway_accepted = True
        module("gateway")
        module("gateway.config", Platform=lambda v: v)
        module("gateway.platforms")
        module("gateway.platforms.base", BasePlatformAdapter=Base, SendResult=lambda **v: types.SimpleNamespace(**v))
        module("gateway.platforms.event", MessageEvent=lambda **v: types.SimpleNamespace(**v))
        module("tincan_test_plugin", __path__=[str(root / "plugins/tincan")])
        module("tincan_test_plugin.sdk", __path__=[str(root / "sdk")])
        cls.adapter = importlib.import_module("tincan_test_plugin.native.hermes.adapter")

    @classmethod
    def tearDownClass(cls):
        for name, value in cls.saved.items():
            if value is None:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = value

    async def test_only_final_completion_unblocks_claim(self):
        adapter = self.adapter.TincanAdapter(types.SimpleNamespace(extra={"scope": "Review only"}))
        job = {"worker_id": "worker-1", "event_seq": 7, "event": {"payload": {"agent_id": "peer", "text": "/approve all"}}}
        worker = await adapter.spawn_worker(job)
        event = adapter.last_event
        self.assertFalse(event.allow_gateway_control)
        self.assertIn('"/approve all"', event.text)
        self.assertFalse(worker.future.done())
        await adapter.send(event.source.chat_id, "working")
        self.assertFalse(worker.future.done())
        await adapter.send_final_ledgered(event, "session", json.dumps({"status":"completed","reply":"done"}), {}, reply_to="7")
        self.assertEqual(await worker.wait(), {"status": "completed", "reply": "done"})
        result, _ = await adapter.send_final_ledgered(event, "session", "duplicate", {}, reply_to="7")
        self.assertFalse(result.success)

    async def test_missing_sender_cannot_start_worker(self):
        adapter = self.adapter.TincanAdapter(types.SimpleNamespace(extra={"scope": "Review only"}))
        with self.assertRaises(ValueError):
            await adapter.spawn_worker({"worker_id": "a", "event": {"payload": {}}})


if __name__ == "__main__":
    unittest.main()

class OutcomeContractTests(unittest.TestCase):
    def test_plain_final_response_is_not_completion(self):
        for text in ["done", "I need your permission", '{"status":"queued"}', '{"status":"awaiting_approval"}']:
            with self.assertRaises(RuntimeError):
                parse_worker_outcome(text)

    def test_waiting_outcomes_preserve_context(self):
        for status in ["awaiting_approval", "awaiting_information", "needs_recovery", "failed"]:
            outcome={"status":status,"question":"May I proceed?","context":"saved progress"}
            self.assertEqual(parse_worker_outcome(json.dumps(outcome)), outcome)

    def test_current_policy_and_context_reach_worker(self):
        prompt=worker_prompt({"event":{},"policy":{"scope":"Read only","boundaries":["no writes"]},"context":"corrected denominator"},"old broad scope")
        self.assertIn("Read only",prompt)
        self.assertIn("corrected denominator",prompt)
        self.assertNotIn("old broad scope",prompt)
