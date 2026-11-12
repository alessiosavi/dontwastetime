"""Import Harness² without letting its config load the caller's .env."""

import os
from pathlib import Path
import tempfile
from threading import Lock
from types import SimpleNamespace


_lock = Lock()
_loaded: SimpleNamespace | None = None


def harness2() -> SimpleNamespace:
    """Return the path-independent APIs, initialized once from an empty cwd.

    chdir is process-wide: this helper serializes its own callers, and is intended
    for the assistant's single-threaded startup, before starting other workers.
    The improvement worker has its own process and import policy.
    """
    global _loaded
    with _lock:
        if _loaded is None:
            original = Path.cwd()
            with tempfile.TemporaryDirectory(prefix="assistant-h2-empty-") as empty:
                try:
                    os.chdir(empty)
                    from harness2.core import tree
                    from harness2.substrate.opencode import OPENCODE_SURFACE, OpencodeSubstrate
                    from harness2.benchmarks.jobbench import render_opencode_tree

                    _loaded = SimpleNamespace(
                        OPENCODE_SURFACE=OPENCODE_SURFACE,
                        OpencodeSubstrate=OpencodeSubstrate,
                        render_opencode_tree=render_opencode_tree,
                        tree=tree,
                    )
                finally:
                    os.chdir(original)
        return _loaded
