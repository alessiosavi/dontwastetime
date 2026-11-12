"""Temporary notes-only dispatch; Task 8 will supply the full CLI."""

import sys

from assistant import notes_mcp


if __name__ == "__main__":
    if sys.argv[1:2] != ["notes-mcp"]:
        sys.exit("Uso: python -m assistant notes-mcp --memory-dir <directory>")
    notes_mcp.main(sys.argv[2:])
