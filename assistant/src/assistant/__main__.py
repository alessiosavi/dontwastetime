"""Support python -m assistant with the same CLI as the console script."""

import sys

from assistant.cli import main


if __name__ == "__main__":
    sys.exit(main())
