"""Command line entry point for profile installation and interactive use."""

import argparse
from datetime import datetime, timezone
from pathlib import Path
import sqlite3
import subprocess
import sys

from assistant.install import install, uninstall
from assistant.launch import launch
from assistant.profile import load_profile


def _common(parser: argparse.ArgumentParser, *, suppress_defaults: bool = False) -> None:
    parser.add_argument(
        "--profile", choices=("operator", "dev"),
        default=argparse.SUPPRESS if suppress_defaults else "operator",
    )
    parser.add_argument(
        "--state-root", type=Path, help="directory di stato dwt che contiene assistant/<profilo>",
        default=argparse.SUPPRESS if suppress_defaults else None,
    )


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(prog="assistant", description="L'assistente privato di Operator.")
    _common(parser)
    commands = parser.add_subparsers(dest="command", required=True)
    installing = commands.add_parser("install", help="installa un profilo")
    installing.add_argument("--repo", type=Path, required=True)
    installing.add_argument("--dev-recipient")
    installing.add_argument("--provider-json", type=Path)
    installing.add_argument("--desktop-launcher", action="store_true")
    installing.add_argument("--no-launchd", action="store_true")
    removing = commands.add_parser("uninstall", help="rimuovi il job e il launcher")
    removing.add_argument("--purge", action="store_true", help="elimina anche lo stato del profilo")
    launching = commands.add_parser("launch", help="apri l'assistente nel browser")
    launching.add_argument("--tui", action="store_true", help="usa l'interfaccia nel terminale")
    notes = commands.add_parser("notes-mcp", help="avvia il server MCP delle note")
    notes.add_argument("--memory-dir", type=Path, required=True)
    ticking = commands.add_parser("tick", help="job periodico (non ancora disponibile)")
    exporting = commands.add_parser("export", help="esporta le conversazioni nuove o aggiornate")
    snapshotting = commands.add_parser("snapshot", help="salva una copia dello stato dwt")
    for subcommand in (installing, removing, launching, notes, ticking, exporting, snapshotting):
        _common(subcommand, suppress_defaults=True)
    args = parser.parse_args(argv)
    try:
        if args.command == "install":
            install(
                args.profile, repo=args.repo, state_root=args.state_root,
                dev_recipient=args.dev_recipient, desktop_launcher=args.desktop_launcher,
                provider_json=args.provider_json, no_launchd=args.no_launchd,
            )
        elif args.command == "uninstall":
            uninstall(args.profile, state_root=args.state_root, purge=args.purge)
        elif args.command == "launch":
            return launch(args.profile, state_root=args.state_root, tui=args.tui)
        elif args.command == "notes-mcp":
            from assistant.notes_mcp import create_server

            create_server(args.memory_dir).run(transport="stdio")
        elif args.command == "tick":
            print("tick non ancora disponibile", file=sys.stderr)
            return 1
        elif args.command == "export":
            from assistant.sessions import export_new_sessions

            paths = export_new_sessions(load_profile(args.profile, args.state_root))
            print(f"Conversazioni esportate: {len(paths)}.")
        elif args.command == "snapshot":
            from assistant.snapshot import take_snapshot

            path = take_snapshot(load_profile(args.profile, args.state_root), datetime.now(timezone.utc))
            print(f"Copia dello stato: {path}")
    except (OSError, ValueError, RuntimeError, sqlite3.Error) as error:
        print(f"Assistente: {error}", file=sys.stderr)
        return 1
    except subprocess.SubprocessError:
        print("Assistente: un comando esterno non è riuscito.", file=sys.stderr)
        return 1
    return 0
