"""Synchronous dwt tools over the SDK's stdio transport, owned by one portal."""

from contextlib import ExitStack, asynccontextmanager
from datetime import datetime, timedelta
from pathlib import Path

import anyio
from anyio.from_thread import start_blocking_portal
from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

from assistant.profile import Profile
from assistant.render import dwt_command


class DwtError(RuntimeError):
    """dwt rejected a tool call or returned invalid structured content."""


class DwtClient:
    def __init__(self, p: Profile, *, state_dir: Path | None = None, now: datetime | None = None):
        self._command = dwt_command(p, state_dir, now)
        self._stack = None

    @asynccontextmanager
    async def _connect(self):
        parameters = StdioServerParameters(command=self._command[0], args=self._command[1:])
        async with stdio_client(parameters) as (reader, writer):
            async with ClientSession(reader, writer, read_timeout_seconds=timedelta(seconds=60)) as session:
                with anyio.fail_after(60):
                    await session.initialize()
                yield session

    def __enter__(self):
        if self._stack is not None:
            raise RuntimeError("DwtClient è già aperto.")
        with ExitStack() as stack:
            self._portal = stack.enter_context(start_blocking_portal())
            self._session = stack.enter_context(self._portal.wrap_async_context_manager(self._connect()))
            self._stack = stack.pop_all()
        return self

    async def _call(self, tool: str, args: dict | None) -> dict:
        with anyio.fail_after(60):
            result = await self._session.call_tool(tool, args)
        if result.isError:
            text = "\n".join(part.text for part in result.content if part.type == "text")
            raise DwtError(text or f"Errore dwt: {tool}")
        if not isinstance(result.structuredContent, dict):
            raise DwtError(f"Risposta dwt non strutturata: {tool}")
        return result.structuredContent

    def call(self, tool: str, args: dict | None = None) -> dict:
        if self._stack is None:
            raise RuntimeError("Usa DwtClient come context manager.")
        return self._portal.call(self._call, tool, args)

    def __exit__(self, *exc):
        stack, self._stack = self._stack, None
        # The caller's exception must not enter the transport's task group.
        stack.close()
        return False
