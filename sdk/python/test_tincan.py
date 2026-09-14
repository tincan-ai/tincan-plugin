import asyncio
import unittest

from tincan import Tincan


class FakeClient(Tincan):
    def __init__(self):
        self.events = asyncio.Queue()
        self.worker_events = asyncio.Queue()
        self.claimed = {}
        self.completed = []
        self.outcomes = {}
        self.claim_calls = asyncio.Queue()

    async def call(self, method, **params):
        key = (params["connection"], params["seq"])
        if method == "claim":
            await self.claim_calls.put(key)
            if key in self.claimed or key in self.completed:
                return {"acquired": False}
            self.claimed[key] = "secret-claim"
            return {"acquired": True, "claim": "secret-claim",
                    "event": {"seq": key[1], "payload": {"text": "peer body"}}}
        if method == "outcome":
            assert params["claim"] == self.claimed[key]
            self.outcomes[key] = params["outcome"]
            if params["outcome"]["status"] == "completed":
                self.claimed.pop(key)
                self.completed.append(key)
            elif params["outcome"]["status"] != "needs_recovery":
                self.claimed.pop(key)
            return {}
        raise AssertionError(method)

    async def mention(self, connection="conn", seq=1):
        await self.events.put({"event": "mention", "data": {
            "connection": connection, "event_seq": seq}})


class Worker:
    execution_mode = "subprocess"

    def __init__(self):
        self.result = asyncio.get_running_loop().create_future()
        self.cancelled = False

    async def wait(self):
        return await self.result

    async def cancel(self):
        self.cancelled = True
        if not self.result.done():
            self.result.cancel()


class DispatcherTests(unittest.IsolatedAsyncioTestCase):
    async def stop(self, dispatcher):
        dispatcher.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await dispatcher

    async def test_join_notices_reach_owner_without_worker_or_ack(self):
        client = FakeClient()

        async def unexpected(job):
            self.fail("join review must not start an automatic worker")

        dispatcher = asyncio.create_task(client.dispatch_mentions(unexpected))
        try:
            for kind in ("join_request", "join_status"):
                notice = {"event": kind, "data": {"connection": "owner", "event_seq": 1}}
                await client.events.put(notice)
                forwarded = await asyncio.wait_for(client.worker_events.get(), 1)
                self.assertEqual(forwarded, notice)
            self.assertTrue(client.claim_calls.empty())
            self.assertFalse(client.completed)
        finally:
            await self.stop(dispatcher)

    async def test_workers_run_separately_and_ack_only_after_completion(self):
        client = FakeClient()
        started = asyncio.Queue()

        async def spawn(job):
            self.assertNotIn("claim", job)  # Controller owns final commit.
            self.assertEqual(job["event"]["payload"]["text"], "peer body")
            worker = Worker()
            await started.put(worker)
            return worker

        dispatcher = asyncio.create_task(client.dispatch_mentions(spawn))
        await client.mention("a")
        first = await asyncio.wait_for(started.get(), 1)
        await client.mention("a")  # Duplicate while the same worker is active.
        await client.events.put({"event": "message", "data": {"connection": "b", "event_seq": 1}})
        second = await asyncio.wait_for(started.get(), 1)
        self.assertFalse(client.completed)
        second.result.set_result({"status": "completed", "reply": "done"})
        done = await asyncio.wait_for(client.worker_events.get(), 1)
        self.assertEqual(done["connection"], "b")
        self.assertEqual(client.completed, [("b", 1)])
        self.assertTrue(started.empty())
        first.result.set_result({"status": "completed"})
        await asyncio.wait_for(client.worker_events.get(), 1)
        await self.stop(dispatcher)
        self.assertCountEqual(client.completed, [("a", 1), ("b", 1)])

    async def test_failures_keep_claim_and_pending_request(self):
        client = FakeClient()

        async def spawn(job):
            worker = Worker()
            worker.result.set_result({"status": "needs_recovery"})
            return worker

        dispatcher = asyncio.create_task(client.dispatch_mentions(spawn))
        await client.mention()
        notice = await asyncio.wait_for(client.worker_events.get(), 1)
        self.assertEqual(notice["event"], "needs_attention")
        self.assertFalse(client.completed)
        self.assertIn(("conn", 1), client.claimed)
        await client.claim_calls.get()
        await self.stop(dispatcher)
        # A fresh controller cannot accidentally rerun that uncertain worker.
        async def unexpected(job):
            self.fail("duplicate worker launched after controller restart")
        dispatcher = asyncio.create_task(client.dispatch_mentions(unexpected))
        await client.mention()
        await asyncio.wait_for(client.claim_calls.get(), 1)
        await client.events.put({"event": "closed", "error": "closed"})
        with self.assertRaises(RuntimeError):
            await dispatcher
        self.assertFalse(client.completed)

    async def test_waiting_outcome_does_not_block_next_message(self):
        client = FakeClient()
        async def spawn(job):
            worker = Worker()
            worker.result.set_result({"status":"awaiting_approval","question":"May I inspect data?","context":"review"} if job["event_seq"] == 1 else {"status":"completed","reply":"answer"})
            return worker
        dispatcher = asyncio.create_task(client.dispatch_mentions(spawn, max_workers=1))
        await client.mention(seq=1)
        notice = await asyncio.wait_for(client.worker_events.get(), 1)
        self.assertEqual(notice["event"], "approval_needed")
        self.assertNotIn(("conn",1), client.completed)
        await client.mention(seq=2)
        notice = await asyncio.wait_for(client.worker_events.get(), 1)
        self.assertEqual(notice["event"], "handled")
        self.assertEqual(client.completed, [("conn",2)])
        await self.stop(dispatcher)

    async def test_approval_arriving_during_worker_cleanup_is_not_lost(self):
        client = FakeClient()
        cleanup = asyncio.Event()
        attempts = []
        async def spawn(job):
            worker = Worker()
            attempts.append(job)
            if len(attempts) == 1:
                worker.result.set_result({"status":"awaiting_approval","question":"May I proceed?"})
                worker.cancel = cleanup.wait
            else:
                worker.result.set_result({"status":"completed"})
            return worker
        dispatcher = asyncio.create_task(client.dispatch_mentions(spawn))
        await client.mention()
        await asyncio.wait_for(client.worker_events.get(), 1)
        # Simulate the controller's ready notice after a real user decision,
        # arriving before the previous worker has finished cleanup.
        await client.mention()
        await asyncio.sleep(0)
        cleanup.set()
        notice = await asyncio.wait_for(client.worker_events.get(), 1)
        self.assertEqual(notice["event"], "handled")
        self.assertEqual(len(attempts), 2)
        await self.stop(dispatcher)

    async def test_capacity_and_cancellation(self):
        client = FakeClient()
        started = asyncio.Queue()

        async def spawn(job):
            worker = Worker()
            await started.put(worker)
            return worker

        dispatcher = asyncio.create_task(client.dispatch_mentions(spawn, max_workers=1))
        await client.mention("a")
        first = await asyncio.wait_for(started.get(), 1)
        await client.mention("b")
        # A running worker doesn't block the event reader or caller's UI task.
        await asyncio.sleep(0)
        self.assertTrue(started.empty())
        await self.stop(dispatcher)
        self.assertTrue(first.cancelled)
        self.assertFalse(client.completed)

    async def test_foreground_or_queued_result_is_rejected(self):
        for mode, status in [("foreground", "completed"), ("subagent", "queued")]:
            client = FakeClient()

            async def spawn(job):
                worker = Worker()
                worker.execution_mode = mode
                worker.result.set_result({"status": status})
                return worker

            dispatcher = asyncio.create_task(client.dispatch_mentions(spawn))
            await client.mention()
            notice = await asyncio.wait_for(client.worker_events.get(), 1)
            self.assertEqual(notice["event"], "needs_attention")
            self.assertFalse(client.completed)
            await self.stop(dispatcher)


if __name__ == "__main__":
    unittest.main()
