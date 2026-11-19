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
        default=argparse.SUPPRESS if suppress_defaults else None,
    )
    parser.add_argument(
        "--state-root", type=Path, help="directory di stato dwt che contiene assistant/<profilo>",
        default=argparse.SUPPRESS if suppress_defaults else None,
    )


def _positive_jobs(value: str) -> int:
    try:
        jobs = int(value)
    except ValueError:
        raise argparse.ArgumentTypeError("jobs deve essere un intero positivo") from None
    if jobs < 1:
        raise argparse.ArgumentTypeError("jobs deve essere un intero positivo")
    return jobs


def _bench_seed(args) -> int:
    from assistant.bench.gate import EvaluationIncomplete, evaluate, seed_bar
    from assistant.bench.report import write_report
    from assistant.bench.seed import build_seed_turns, ensure_seed_snapshot, load_seed_turns
    from assistant.snapshot import _snapshots, take_snapshot
    from assistant.spend import spend_between

    p = load_profile(args.profile, args.state_root)
    started = datetime.now(timezone.utc)
    run_dir = p.bench_dir / "seed-runs" / started.strftime("%Y%m%dT%H%M%S%fZ")
    run_dir.mkdir(mode=0o700, parents=True)
    snapshot = ensure_seed_snapshot(p)
    if not _snapshots(p):
        # Refresh next-turn evidence after retention without replacing seed.db.
        take_snapshot(p, datetime.now(timezone.utc))
    templates = [seed for seed in load_seed_turns() if args.split == "all" or seed.split == args.split]
    turns = [turn for turn in build_seed_turns(p, snapshot)
             if args.split == "all" or turn.seed.split == args.split]
    resolved = {turn.turn_id for turn in turns}
    skipped = [seed.id for seed in templates if seed.id not in resolved]
    scores, incomplete = [], False
    try:
        scores = evaluate(p, p.root / "harness-live", turns, run_dir,
                          seeds={turn.turn_id: turn.seed for turn in turns},
                          seed_snapshot=snapshot, jobs=args.jobs)
    except EvaluationIncomplete:
        incomplete = True
    passed = not incomplete and seed_bar(scores)
    cost = spend_between(p, started, datetime.now(timezone.utc))
    report = write_report(run_dir, scores=scores, passed=passed, incomplete=incomplete,
                          splits={seed.id: seed.split for seed in templates}, skipped=skipped, cost=cost)
    for score in scores:
        print(f"{score.turn_id}: {score.mean:.2f}; fallimenti: {', '.join(score.deterministic) or 'nessuno'}")
    print(f"{'PASS' if passed else 'FAIL'} {report}")
    return 0 if passed else 1


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(prog="assistant", description="L'assistente privato dell'operatore.")
    _common(parser)
    commands = parser.add_subparsers(dest="command", required=True)
    installing = commands.add_parser("install", help="installa un profilo")
    installing.add_argument("--repo", type=Path, required=True)
    installing.add_argument("--dev-recipient")
    installing.add_argument("--recap-recipient")
    installing.add_argument("--provider-json", type=Path)
    installing.add_argument("--desktop-launcher", action="store_true")
    installing.add_argument("--no-launchd", action="store_true")
    removing = commands.add_parser("uninstall", help="rimuovi il job e il launcher")
    removing.add_argument("--purge", action="store_true", help="elimina anche lo stato del profilo")
    launching = commands.add_parser("launch", help="apri l'assistente nel browser")
    launching.add_argument("--tui", action="store_true", help="usa l'interfaccia nel terminale")
    notes = commands.add_parser("notes-mcp", help="avvia il server MCP delle note")
    notes.add_argument("--memory-dir", type=Path, required=True)
    ticking = commands.add_parser("tick", help="esegui i job periodici dovuti")
    ticking.add_argument("--now", type=datetime.fromisoformat,
                         help="orario ISO con fuso, solo per test o esecuzioni manuali")
    exporting = commands.add_parser("export", help="esporta le conversazioni nuove o aggiornate")
    snapshotting = commands.add_parser("snapshot", help="salva una copia dello stato dwt")
    recapping = commands.add_parser("recap", help="prepara e invia il riepilogo dell'assistente")
    recapping.add_argument("--dry-run", action="store_true", help="scrivi i file senza inviare")
    tone_card = commands.add_parser("tone-card", help="prepara una bozza della scheda di tono (solo dev)")
    bench = commands.add_parser("bench", help="valuta l'assistente sulla copia della posta")
    bench_commands = bench.add_subparsers(dest="bench_command")
    seeding = bench_commands.add_parser("seed", help="valuta il live sul set iniziale")
    _common(seeding, suppress_defaults=True)
    seeding.add_argument("--split", choices=("all", "train", "holdout"), default="all")
    seeding.add_argument("--jobs", type=_positive_jobs, default=4)
    improving = bench_commands.add_parser("improve-worker", help="avvia il processo privato di miglioramento")
    _common(improving, suppress_defaults=True)
    manual_improve = bench_commands.add_parser("improve", help="esegui il miglioramento in primo piano")
    _common(manual_improve, suppress_defaults=True)
    promoting = bench_commands.add_parser("promote", help="copia un candidato approvato nel repository")
    _common(promoting, suppress_defaults=True)
    promoting.add_argument("--run", required=True)
    promoting.add_argument("--repo", type=Path, required=True)
    for subcommand in (
        installing, removing, launching, notes, ticking, exporting, snapshotting, recapping, tone_card, bench,
    ):
        _common(subcommand, suppress_defaults=True)
    args = parser.parse_args(argv)
    if args.profile is None:
        args.profile = "dev" if args.command == "bench" and args.bench_command == "seed" else "operator"
    try:
        if args.command == "bench":
            if args.bench_command == "seed":
                return _bench_seed(args)
            if args.bench_command == "improve-worker":
                from assistant.bench.worker import improve_worker_main

                return improve_worker_main(args.profile, args.state_root)
            if args.bench_command == "improve":
                command = [sys.executable, "-m", "assistant", "bench", "improve-worker",
                           "--profile", args.profile]
                if args.state_root is not None:
                    command.extend(["--state-root", str(args.state_root.expanduser().resolve())])
                return subprocess.run(command, stdin=subprocess.DEVNULL, check=False).returncode
            if args.bench_command == "promote":
                from assistant.bench.improve import promote

                for path in promote(load_profile(args.profile, args.state_root), args.run, args.repo):
                    print(path)
                return 0
            bench.print_help()
            return 0
        if args.command == "install":
            install(
                args.profile, repo=args.repo, state_root=args.state_root,
                dev_recipient=args.dev_recipient, desktop_launcher=args.desktop_launcher,
                provider_json=args.provider_json, no_launchd=args.no_launchd,
                recap_recipient=args.recap_recipient,
            )
        elif args.command == "uninstall":
            uninstall(args.profile, state_root=args.state_root, purge=args.purge)
        elif args.command == "launch":
            return launch(args.profile, state_root=args.state_root, tui=args.tui)
        elif args.command == "notes-mcp":
            from assistant.notes_mcp import create_server

            create_server(args.memory_dir).run(transport="stdio")
        elif args.command == "tick":
            from assistant.tick import tick

            tick(load_profile(args.profile, args.state_root), args.now or datetime.now(timezone.utc))
        elif args.command == "export":
            from assistant.sessions import export_new_sessions

            paths = export_new_sessions(load_profile(args.profile, args.state_root))
            print(f"Conversazioni esportate: {len(paths)}.")
        elif args.command == "snapshot":
            from assistant.snapshot import take_snapshot

            path = take_snapshot(load_profile(args.profile, args.state_root), datetime.now(timezone.utc))
            print(f"Copia dello stato: {path}")
        elif args.command == "tone-card":
            from assistant.tone import generate_tone_card

            if args.profile != "dev":
                raise ValueError("La scheda di tono si genera solo con il profilo dev, sulla copia del database.")
            path = generate_tone_card(load_profile(args.profile, args.state_root))
            print(f"Bozza della scheda di tono da rivedere: {path}")
        elif args.command == "recap":
            from assistant.locks import tick_lock
            from assistant.markers import load_markers
            from assistant.sessions import export_new_sessions
            from assistant.recap import (
                DwtClient, ROME, build_recap, deliver_recap, recap_period_start, reconcile_claim,
                recap_recipient, summarize_with_agent, write_recap,
            )

            p = load_profile(args.profile, args.state_root)
            with tick_lock(p) as acquired:
                if not acquired:
                    print("Un altro processo dell'assistente è in corso; riprova tra poco.", file=sys.stderr)
                    return 1
                now = datetime.now(timezone.utc)
                reconcile_claim(p, now)
                if load_markers(p).recap_claim == f"recap:{now.astimezone(ROME).date().isoformat()}":
                    print("Recap già preso in carico oggi.")
                    return 0
                recap_recipient(p)
                export_new_sessions(p)
                with DwtClient(p, now=now) as client:
                    recap = build_recap(p, recap_period_start(p, now), now, client=client,
                                        summarize=summarize_with_agent(p))
                if args.dry_run:
                    path = write_recap(p, recap, now)
                    print(f"Recap salvato: {path}")
                else:
                    outcome = deliver_recap(p, recap, now)
                    print({"sent": "Recap inviato.", "skipped": "Recap già preso in carico oggi.",
                           "failed": "Recap non inviato; riprova più tardi.",
                           "unknown": "Consegna del recap incerta: controlla la posta."}[outcome])
                    return 0 if outcome in ("sent", "skipped") else 1
    except (OSError, ValueError, RuntimeError, sqlite3.Error) as error:
        print(f"Assistente: {error}", file=sys.stderr)
        return 1
    except subprocess.SubprocessError:
        print("Assistente: un comando esterno non è riuscito.", file=sys.stderr)
        return 1
    return 0
