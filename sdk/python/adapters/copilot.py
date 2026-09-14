"""Copilot SDK isolated-session adapter, using the host's permission handler.

Use CopilotWorkers(client, scope=..., session_config=...) with common.serve.
No session is created until a mention has been claimed. Never attach this
controller to another interactive Copilot session.
"""
import asyncio
from .common import worker_prompt, parse_worker_outcome


class CopilotWorkers:
    def __init__(self, client, *, scope, session_config, timeout=300, record_worker=None):
        if not scope.strip() or not callable(session_config.get("on_permission_request")):
            raise ValueError("scope and the host's on_permission_request handler are required")
        if "session_id" in session_config:
            raise ValueError("workers must create independent sessions")
        self.client, self.scope = client, scope
        self.config, self.timeout = dict(session_config), timeout
        self.record_worker = record_worker

    async def __call__(self, job):
        session = await self.client.create_session(session_id=job["worker_id"], **self.config)
        if self.record_worker:
            try:
                await self.record_worker({"worker_id": job["worker_id"], "event_seq": job["event_seq"],
                                          "session_id": session.session_id, "host": "copilot"})
            except BaseException:
                await session.disconnect()
                raise
        return CopilotWorker(session, worker_prompt(job, self.scope), self.timeout)


class CopilotWorker:
    execution_mode = "isolated_session"

    def __init__(self, session, prompt, timeout):
        self.session, self.prompt, self.timeout = session, prompt, timeout
        self.completed = False
        self.closed = False

    async def wait(self):
        response = await self.session.send_and_wait(self.prompt, timeout=self.timeout)
        content = getattr(getattr(response, "data", None), "content", None)
        if not isinstance(content, str) or not content.strip():
            raise RuntimeError("Copilot returned no final assistant reply; claim retained")
        self.completed = True
        return parse_worker_outcome(content)

    async def cancel(self):
        if self.closed:
            return
        try:
            if not self.completed:
                await asyncio.wait_for(self.session.abort(), 10)
        finally:
            await asyncio.wait_for(self.session.disconnect(), 10)
            self.closed = True
