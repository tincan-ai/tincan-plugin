#!/usr/bin/env python3
"""Run a dedicated Cursor or Copilot agent with isolated mention workers.
Requires the selected host SDK and its normal authentication.
"""
import argparse
import asyncio
import json
import os
from pathlib import Path

from tincan import Tincan
from adapters.common import serve
from adapters.copilot import CopilotWorkers


async def run(args):
    # Check optional dependencies before creating or joining a Tincan room.
    if args.host == "copilot":
        from copilot import CopilotClient
        from copilot.session import PermissionDecisionUserNotAvailable
    else:
        from cursor_sdk import AsyncClient, LocalAgentOptions

    root = Path(args.state_dir).expanduser().resolve()
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    state = root / "connection.json"
    saved = json.loads(state.read_text()) if state.exists() else None
    if saved and saved.get("host") != args.host:
        raise ValueError("This state directory belongs to another runtime; choose a dedicated directory for this harness")
    client = await Tincan.start(args.executable, host=args.host + "-sdk", server=args.server,
                                state_dir=root / "connections")
    async def record_worker(value):
        directory = root / "workers"
        directory.mkdir(mode=0o700, exist_ok=True)
        path = directory / (value["worker_id"] + ".json")
        fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(fd, "w") as output:
            json.dump(value, output)
            output.flush()
            os.fsync(output.fileno())

    try:
        view = await client.call("connect", **({"connection": saved["connection"]} if saved else {
            "name": args.host.capitalize(), "url": args.invite or "",
        }))
        if view.get("setup_error") or not view.get("connection"):
            raise RuntimeError(view.get("setup_error", "Tincan connection unavailable"))
        fd = os.open(state.with_suffix(".tmp"), os.O_CREAT | os.O_WRONLY | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as output:
            json.dump({"connection": view["connection"], "host": args.host}, output)
        os.replace(state.with_suffix(".tmp"), state)
        if view.get("share_url"):
            print("Tincan invite:", view["share_url"], flush=True)
        if view.get("status") == "pending":
            print("Join awaits owner approval. Run this command again after approval.", flush=True)
            return
        async def report(notice):
            if notice.get("event") != "handled":
                approval = notice.get("approval", {})
                print("Tincan needs owner review:", approval.get("question") or notice.get("error") or notice.get("event"), flush=True)
                if approval:
                    print("Decision:", approval["id"], "request:", notice["event_seq"], flush=True)
                    return {"presented": True}
        if args.host == "copilot":
            async with CopilotClient() as copilot:
                config = {"working_directory": str(Path(args.project).resolve()),
                          "on_permission_request": lambda request, context: PermissionDecisionUserNotAvailable()}
                if args.model:
                    config["model"] = args.model
                workers = CopilotWorkers(copilot, scope=args.scope, session_config=config, record_worker=record_worker)
                await serve(client, view["connection"], workers, report)
        else:
            from adapters.cursor import CursorWorkers
            async with await AsyncClient.launch_bridge(workspace=str(Path(args.project).resolve())) as cursor:
                async def create_agent():
                    options = {"local": LocalAgentOptions(cwd=str(Path(args.project).resolve()),
                               setting_sources=["project", "user", "team", "mdm"], auto_review=True)}
                    if args.model:
                        options["model"] = args.model
                    return await cursor.agents.create(**options)
                workers = CursorWorkers(create_agent, scope=args.scope, record_worker=record_worker)
                await serve(client, view["connection"], workers, report)
    finally:
        await client.close()


def main(host=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", choices=["copilot", "cursor"], default=host, required=host is None)
    parser.add_argument("--executable", required=True, help="Absolute path to bundled bin/tincan")
    parser.add_argument("--state-dir", required=True, help="Private persistent directory for this dedicated agent")
    parser.add_argument("--project", required=True)
    parser.add_argument("--scope", required=True, help="Authorized scope for incoming work")
    parser.add_argument("--invite")
    parser.add_argument("--server")
    parser.add_argument("--model", help="Optional explicit model; otherwise retain host default")
    args = parser.parse_args()
    if not args.scope.strip():
        parser.error("scope must be nonempty")
    asyncio.run(run(args))


if __name__ == "__main__":
    main()
