"""Cursor SDK adapter. The host supplies an async agent factory and its policy."""
import asyncio
from .common import worker_prompt, parse_worker_outcome


class CursorWorkers:
    def __init__(self, create_agent, *, scope, timeout=300, record_worker=None):
        if not callable(create_agent) or not scope.strip():
            raise ValueError("a host-owned agent factory and authorized scope are required")
        self.create_agent, self.scope, self.timeout = create_agent, scope, timeout
        self.record_worker = record_worker

    async def __call__(self, job):
        agent = await self.create_agent()
        if self.record_worker:
            try:
                await self.record_worker({"worker_id": job["worker_id"], "event_seq": job["event_seq"],
                                          "agent_id": agent.agent_id, "host": "cursor"})
            except BaseException:
                await agent.close()
                raise
        return CursorWorker(agent, worker_prompt(job, self.scope), self.timeout)


class CursorWorker:
    execution_mode = "isolated_session"

    def __init__(self, agent, prompt, timeout):
        self.agent, self.prompt, self.timeout = agent, prompt, timeout
        self.run = None
        self.closed = False

    async def wait(self):
        self.run = await self.agent.send(self.prompt)
        result = await asyncio.wait_for(self.run.wait(), self.timeout)
        if result.status != "finished" or not isinstance(result.result, str) or not result.result.strip():
            raise RuntimeError("Cursor did not confirm a completed reply; claim retained")
        return parse_worker_outcome(result.result)

    async def cancel(self):
        if self.closed:
            return
        try:
            if self.run is not None and self.run.status == "running":
                await asyncio.wait_for(self.run.cancel(), 10)
        finally:
            await asyncio.wait_for(self.agent.close(), 10)
            self.closed = True
