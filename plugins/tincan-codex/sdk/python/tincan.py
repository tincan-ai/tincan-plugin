"""Async stdlib client for `tincan sidecar` (Python 3.10+)."""
import asyncio
import json
import uuid


class Tincan:
    def __init__(self, process):
        self.process = process
        self.events = asyncio.Queue()
        self.worker_events = asyncio.Queue()
        self._requests = {}
        self._sequence = 0
        self._reader = asyncio.create_task(self._read())

    @classmethod
    async def start(cls, executable="tincan", server=None, *, state_dir=None, host=None):
        args = [executable, "sidecar"]
        for flag, value in (("--server", server), ("--state-dir", state_dir), ("--host", host)):
            if value is not None:
                args.extend((flag, str(value)))
        process = await asyncio.create_subprocess_exec(
            *args,
            stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
        )
        return cls(process)

    async def _read(self):
        failure = RuntimeError("Tincan sidecar closed")
        try:
            async for line in self.process.stdout:
                message = json.loads(line)
                if "event" in message:
                    await self.events.put(message)
                    continue
                future = self._requests.get(message.get("id"))
                if future is not None and not future.done():
                    if "error" in message:
                        future.set_exception(RuntimeError(message["error"]))
                    else:
                        future.set_result(message.get("result"))
        except Exception as error:
            failure = error
        finally:
            for future in self._requests.values():
                if not future.done():
                    future.set_exception(failure)
            await self.events.put({"event": "closed", "error": str(failure)})

    async def call(self, method, **params):
        if self._reader.done():
            raise RuntimeError("Tincan sidecar closed")
        self._sequence += 1
        request_id = self._sequence
        future = asyncio.get_running_loop().create_future()
        self._requests[request_id] = future
        try:
            self.process.stdin.write((json.dumps({
                "id": request_id, "method": method, "params": params,
            }) + "\n").encode())
            await self.process.stdin.drain()
            return await future
        finally:
            self._requests.pop(request_id, None)

    async def close(self):
        self.process.stdin.close()
        try:
            await asyncio.wait_for(self.process.wait(), 5)
        except asyncio.TimeoutError:
            self.process.kill()
            await self.process.wait()
        await self._reader

    async def dispatch_mentions(self, spawn_worker, *, max_workers=4):
        """Dispatch notices to host-owned, isolated workers while the UI stays free.

        spawn_worker(job) must start a background subagent/subprocess/session and
        return a handle with execution_mode, async wait(), and async cancel().
        wait() returns {"status": "completed", "reply": optional_text} only once
        work is finished. A queue receipt is not completion. The controller owns
        the claim token and commits the reply/ack after successful completion.
        Run this coroutine as a background task; it makes no idle model calls.
        Join approval/status notices are forwarded to worker_events for the
        host UI to show to the owner; no worker is started or claim taken.
        """
        if max_workers < 1:
            raise ValueError("max_workers must be positive")
        capacity = asyncio.Semaphore(max_workers)
        active = set()
        dirty = set()
        tasks = set()

        async def dispatch(key):
            worker = None
            claimed = None
            committed = False
            try:
                async with capacity:
                    worker_id = "tincan-" + uuid.uuid4().hex
                    claimed = await self.call("claim", connection=key[0],
                                              seq=key[1], worker_id=worker_id)
                    if not claimed.get("acquired"):
                        return
                    job = {"connection": key[0], "event_seq": key[1],
                           "worker_id": worker_id, "event": claimed["event"],
                           "execution": "background_delegate", "context": claimed.get("context", ""),
                           "policy": claimed.get("policy"), "related_commitments":claimed.get("related_commitments",[])}
                    # The host, not asyncio itself, provides model isolation.
                    worker = await spawn_worker(job)
                    if worker.execution_mode not in {"subagent", "subprocess", "isolated_session"}:
                        raise RuntimeError("host must launch an isolated background worker")
                    result = await worker.wait()
                    if not isinstance(result, dict) or result.get("status") not in {
                        "completed", "awaiting_approval", "awaiting_information", "failed", "needs_recovery"
                    }:
                        raise RuntimeError("worker did not return a structured outcome")
                    await self.call("outcome", connection=key[0], seq=key[1],
                                    claim=claimed["claim"], outcome=result)
                    committed = True
                    await self.worker_events.put({"event": "handled" if result["status"] == "completed" else "approval_needed" if result["status"].startswith("awaiting_") else "needs_attention",
                                                  "connection": key[0], "event_seq": key[1]})
            except asyncio.CancelledError:
                raise
            except Exception as error:
                if claimed and claimed.get("acquired") and not committed:
                    try:
                        await self.call("outcome", connection=key[0], seq=key[1], claim=claimed["claim"],
                                        outcome={"status":"needs_recovery", "context":str(error)})
                    except Exception:
                        pass  # The original durable claim still prevents unsafe retry.
                # Keep the durable claim: an uncertain worker must not be retried
                # while it could still be applying side effects.
                await self.worker_events.put({"event": "needs_attention", "connection": key[0],
                                              "event_seq": key[1], "error": str(error)})
            finally:
                if worker is not None:
                    # A completed handle should treat cancel as a no-op. On
                    # shutdown/failure this requests host-owned worker cleanup.
                    try:
                        await worker.cancel()
                    except Exception as error:
                        await self.worker_events.put({"event": "needs_attention", "connection": key[0],
                                                      "event_seq": key[1], "error": str(error)})
                active.discard(key)
                if key in dirty:
                    dirty.discard(key)
                    await self.events.put({"event":"mention","data":{"connection":key[0],"event_seq":key[1]}})

        try:
            while True:
                event = await self.events.get()
                if event.get("event") == "closed":
                    raise RuntimeError(event.get("error", "Tincan sidecar closed"))
                if event.get("event") in {"join_request", "join_status", "approval_needed", "needs_attention"}:
                    # Account admission needs the owner, never an automatic worker.
                    await self.worker_events.put(event)
                    continue
                if event.get("event") != "mention":
                    continue
                data = event["data"]
                key = (data["connection"], data["event_seq"])
                if key in active:
                    dirty.add(key)
                    continue
                active.add(key)
                task = asyncio.create_task(dispatch(key))
                tasks.add(task)
                task.add_done_callback(tasks.discard)
        finally:
            remaining = list(tasks)
            for task in remaining:
                task.cancel()
            await asyncio.gather(*remaining, return_exceptions=True)
