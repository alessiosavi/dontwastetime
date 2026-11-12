"""A stable lock inode shared by manual jobs and the periodic tick."""

from collections.abc import Iterator
from contextlib import contextmanager
import fcntl

from assistant.profile import Profile


@contextmanager
def tick_lock(p: Profile) -> Iterator[bool]:
    """Yield False immediately if another process owns the profile's tick lock."""
    p.root.mkdir(mode=0o700, parents=True, exist_ok=True)
    with (p.root / "tick.lock").open("a", encoding="utf-8") as lock:
        try:
            fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            yield False
            return
        try:
            yield True
        finally:
            fcntl.flock(lock.fileno(), fcntl.LOCK_UN)
