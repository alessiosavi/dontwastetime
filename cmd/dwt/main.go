// Command dwt is the DontWasteTime email-triage daemon and its one-shot tools.
// It is the only place in the module that reads the wall clock.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alessiosavi/go-dontwastetime/internal/config"
	"github.com/alessiosavi/go-dontwastetime/internal/digest"
	"github.com/alessiosavi/go-dontwastetime/internal/explore"
	"github.com/alessiosavi/go-dontwastetime/internal/graph"
	"github.com/alessiosavi/go-dontwastetime/internal/mailbox"
	"github.com/alessiosavi/go-dontwastetime/internal/pipeline"
	"github.com/alessiosavi/go-dontwastetime/internal/service"
	"github.com/alessiosavi/go-dontwastetime/internal/threads"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: dwt <command> [flags]

commands:
  serve        run pollers, classifier, threads, digest and alerts (launchd runs this)
  sync         one sync pass [--mailbox addr] [--initial-days n]
  classify     drain pending classify/thread jobs once [--limit n]
  digest       build and send the digest [--date YYYY-MM-DD|today] [--dry-run] [--since YYYY-MM-DD]
  send-note    mail a text note --to <addr> --subject <s> --file <path> [--dry-run]
  explore      phase-0 clustering report [--days n] [--mailboxes a,b] [--max-messages n]
  eval         run --gold file.jsonl [--model name] [--out dir]
  mailboxes    discover [--write-config] | verify
  correct      <message-id> <label>   relabel a message (ids are printed in the digest)
  handled      <message-id> [--note text] close the current obligation resolved off-mail
  snooze       <message-id> <YYYY-MM-DD> hide the current obligation until 08:00 Europe/Rome
  mute-thread  <message-id|thread-key> stop tracking a thread, including future mail
  reopen       <message-id|thread-key> clear handled, snooze and mute; recompute
  doctor       health checks; exits non-zero on any red item
  mcp          serve agent tools over stdio --operator <name> [--now <RFC3339>]
  version      print the version

common flags: --config (default $DWT_CONFIG or config.json), --state-dir, --env, --taxonomy
mcp flag: --now fixes the clock for replays on a database copy
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func logger() *slog.Logger { return slog.Default() }

// run dispatches one subcommand and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve", "sync", "classify", "digest", "send-note", "explore", "eval", "mailboxes", "correct", "handled", "snooze", "mute-thread", "reopen", "doctor", "mcp", "version", "help", "-h", "--help":
	default:
		fmt.Fprintf(stderr, "dwt: unknown command %q\n%s", cmd, usage)
		return 2
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := dispatch(ctx, cmd, rest, stdout, stderr); err != nil {
		if errors.Is(err, errUsage) {
			return 2
		}
		fmt.Fprintf(stderr, "dwt %s: %v\n", cmd, err)
		if cmd == "send-note" {
			if errors.Is(err, graph.ErrDeliveryUnknown) {
				return 3
			}
			if errors.Is(err, digest.ErrNoteNotSent) {
				return 4
			}
		}
		return 1
	}
	return 0
}

var errUsage = errors.New("usage")

func dispatch(ctx context.Context, cmd string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	defCfg := os.Getenv("DWT_CONFIG")
	if defCfg == "" {
		defCfg = "config.json"
	}
	var p paths
	fs.StringVar(&p.config, "config", defCfg, "config file")
	fs.StringVar(&p.stateDir, "state-dir", "", "state directory (default from config / DWT_STATE_DIR)")
	fs.StringVar(&p.env, "env", "", "env file (default: .env next to the config)")
	fs.StringVar(&p.taxonomy, "taxonomy", "", "taxonomy file (default: taxonomy.json next to the config)")
	dryRun := fs.Bool("dry-run", false, "serve/digest/send-note: send nothing; serve/digest save digests and alerts to the state dir")
	initialDays := fs.Int("initial-days", 0, "serve/sync: days to read on a mailbox's first sync (default from config, 90)")
	mailboxFlag := fs.String("mailbox", "", "sync: only this mailbox")
	limit := fs.Int("limit", 0, "classify: max classify jobs to run (0 = all)")
	fs.Bool("once", true, "classify: one pass (the only mode)")
	date := fs.String("date", "today", "digest: YYYY-MM-DD or today (digest instant is 08:00 Europe/Rome)")
	since := fs.String("since", "", "digest: include mail received since YYYY-MM-DD (00:00 Europe/Rome), without the late-mail cap")
	days := fs.Int("days", 90, "explore: window in days")
	mailboxes := fs.String("mailboxes", "", "explore: comma-separated mailboxes (default: all enabled)")
	maxMessages := fs.Int("max-messages", 5000, "explore: total message cap")
	writeConfig := fs.Bool("write-config", false, "mailboxes discover: merge found mailboxes into the config file")
	gold := fs.String("gold", "", "eval run: gold JSONL file")
	model := fs.String("model", "", "eval run: override the configured model for this run")
	evalOut := fs.String("out", "", "eval run: report directory (default: <state>/eval/runs)")
	operator := fs.String("operator", "", "mcp: configured operator name (required)")
	nowFlag := fs.String("now", "", "mcp: fixed clock (RFC3339) for replays on a database copy")
	note := fs.String("note", "", "handled: optional audit note")
	var noteTo, noteSubject, noteFile string
	if cmd == "send-note" {
		fs.StringVar(&noteTo, "to", "", "send-note: one configured digest recipient")
		fs.StringVar(&noteSubject, "subject", "", "send-note: subject (at most 200 characters, no CR/LF)")
		fs.StringVar(&noteFile, "file", "", "send-note: UTF-8 text file (at most 64 KiB)")
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	var nowSet, noteToSet, noteSubjectSet, noteFileSet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "now":
			nowSet = true
		case "to":
			noteToSet = true
		case "subject":
			noteSubjectSet = true
		case "file":
			noteFileSet = true
		}
	})
	var fixed time.Time
	if nowSet {
		if cmd != "mcp" {
			fmt.Fprintln(stderr, "--now is only supported by mcp; usage: dwt mcp --operator <name> [--now <RFC3339>] [common flags]")
			return errUsage
		}
		fixed, err = time.Parse(time.RFC3339, *nowFlag)
		if err != nil {
			fmt.Fprintln(stderr, "--now must be RFC3339; usage: dwt mcp --operator <name> [--now <RFC3339>] [common flags]")
			return errUsage
		}
	}
	switch cmd {
	case "version":
		fmt.Fprintf(stdout, "dwt %s\n", version)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	if cmd == "eval" && (len(pos) != 1 || pos[0] != "run" || *gold == "") {
		fmt.Fprintln(stderr, "usage: dwt eval run --gold file.jsonl [--model name] [--out dir]")
		return errUsage
	}
	if cmd == "mcp" && (*operator == "" || len(pos) != 0) {
		fmt.Fprintln(stderr, "usage: dwt mcp --operator <name> [--now <RFC3339>] [common flags]")
		return errUsage
	}
	var noteInput digest.Note
	if cmd == "send-note" {
		if !noteToSet || !noteSubjectSet || !noteFileSet || len(pos) != 0 {
			fmt.Fprintln(stderr, "usage: dwt send-note --to <addr> --subject <s> --file <path> [--dry-run] [common flags]")
			return errUsage
		}
		text, err := readNoteFile(noteFile)
		if err != nil {
			return fmt.Errorf("%w: %w", digest.ErrNoteNotSent, err)
		}
		noteInput = digest.Note{To: noteTo, Subject: noteSubject, Text: text}
	}
	var digestAt time.Time
	var digestOpts digest.Options
	if cmd == "digest" {
		digestAt, err = digestInstant(*date, time.Now())
		if err != nil {
			return err
		}
		if *since != "" {
			from, err := time.ParseInLocation(time.DateOnly, *since, config.Rome)
			if err != nil || !from.Before(digestAt) {
				fmt.Fprintln(stderr, "--since must be YYYY-MM-DD before the digest instant")
				return errUsage
			}
			digestOpts.Since = from
		}
	}

	n := need{}
	switch cmd {
	case "serve", "classify", "explore", "eval":
		n = need{graph: true, llm: true}
	case "sync", "mailboxes":
		n = need{graph: true}
	case "digest", "send-note":
		n = need{graph: !*dryRun}
	}
	if cmd == "eval" {
		n.model = *model
	}
	a, err := openApp(p, n)
	if err != nil {
		if cmd == "send-note" {
			return fmt.Errorf("%w: %w", digest.ErrNoteNotSent, err)
		}
		return err
	}
	defer a.close()
	if nowSet {
		a.now = func() time.Time { return fixed }
	}
	if *initialDays > 0 {
		a.cfg.InitialDays = *initialDays
	}
	if cmd == "doctor" && a.cfg.RequireGraph() == nil {
		a.c = graph.New(a.cfg.Graph.TenantID, a.cfg.Graph.ClientID, a.cfg.Graph.ClientSecret, graph.WithClock(time.Now))
	}

	switch cmd {
	case "mcp":
		err := serveMCP(ctx, a, *operator, os.Stdin, stdout)
		if errors.Is(err, service.ErrUnknownOperator) {
			fmt.Fprintln(stderr, "unknown operator; usage: dwt mcp --operator <configured-name> [--now <RFC3339>] [common flags]")
			return errUsage
		}
		return err
	case "serve":
		return a.serve(ctx, *dryRun)
	case "sync":
		return a.syncCmd(ctx, *mailboxFlag, stdout)
	case "classify":
		return a.classifyCmd(ctx, *limit, stdout)
	case "eval":
		return a.evalCmd(ctx, *gold, *model, *evalOut, stdout)
	case "digest":
		if err := a.runDigest(ctx, digestAt, *dryRun, nil, digestOpts); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "digest %s saved under %s (dry-run=%v)\n", config.Day(digestAt), filepath.Join(a.cfg.StateDir, "digests"), *dryRun)
		return nil
	case "send-note":
		return digest.SendNote(ctx, a.s, a.c, a.cfg, noteInput, *dryRun, a.now())
	case "explore":
		o := explore.Options{Days: *days, MaxMessages: *maxMessages, Mailboxes: splitList(*mailboxes)}
		if len(o.Mailboxes) == 0 {
			for _, mb := range a.cfg.Mailboxes {
				if mb.Enabled {
					o.Mailboxes = append(o.Mailboxes, mb.Address)
				}
			}
		}
		paths, err := explore.Run(ctx, a.c, a.s, a.llm, a.tx, o, filepath.Join(a.cfg.StateDir, "reports"), a.now())
		for _, p := range paths {
			fmt.Fprintln(stdout, p)
		}
		return err
	case "mailboxes":
		return a.mailboxesCmd(ctx, pos, p.config, *writeConfig, stdout)
	case "correct":
		if len(pos) != 2 {
			fmt.Fprintln(stderr, "usage: dwt correct <message-id> <label>")
			return errUsage
		}
		return a.correct(ctx, pos[0], pos[1], stdout)
	case "handled":
		if len(pos) != 1 {
			fmt.Fprintln(stderr, "usage: dwt handled <message-id> [--note text]")
			return errUsage
		}
		return a.triageCmd(ctx, cmd, pos[0], *note, stdout)
	case "snooze":
		if len(pos) != 2 {
			fmt.Fprintln(stderr, "usage: dwt snooze <message-id> <YYYY-MM-DD>")
			return errUsage
		}
		return a.triageCmd(ctx, cmd, pos[0], pos[1], stdout)
	case "mute-thread":
		if len(pos) != 1 {
			fmt.Fprintln(stderr, "usage: dwt mute-thread <message-id|thread-key>")
			return errUsage
		}
		return a.muteThread(ctx, pos[0], stdout)
	case "reopen":
		if len(pos) != 1 {
			fmt.Fprintln(stderr, "usage: dwt reopen <message-id|thread-key>")
			return errUsage
		}
		return a.triageCmd(ctx, cmd, pos[0], "", stdout)
	case "doctor":
		if !a.doctor(ctx, stdout) {
			return errors.New("red items above")
		}
	}
	return nil
}

// readNoteFile bounds memory use even for a file larger than the note limit.
func readNoteFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	const maxBytes = 64 * 1024
	raw, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxBytes {
		return "", errors.New("note: file exceeds 64 KiB")
	}
	return string(raw), nil
}

func (a *app) syncCmd(ctx context.Context, only string, out io.Writer) error {
	if err := mailbox.SyncFromConfig(ctx, a.s, a.cfg); err != nil {
		return err
	}
	var pollers []*pipeline.Poller
	for _, mb := range a.cfg.Mailboxes {
		if mb.Enabled && (only == "" || strings.EqualFold(only, mb.Address)) {
			pollers = append(pollers, pipeline.NewPoller(a.c, a.s, mb, a.cfg, nil))
		}
	}
	if len(pollers) == 0 {
		return fmt.Errorf("no enabled mailbox matches %q", only)
	}
	var errs []error
	for _, p := range pollers {
		f, ins, err := p.SyncOnce(ctx, a.now())
		fmt.Fprintf(out, "%s: fetched %d, new %d\n", p.Mailbox(), f, ins)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// classifyCmd drains pending classify and thread_refresh jobs once. One-shot
// commands never send alerts (serve does).
func (a *app) classifyCmd(ctx context.Context, limit int, out io.Writer) error {
	h := map[string]pipeline.Handler{
		pipeline.KindClassify:      pipeline.ClassifyHandler(a.s, a.c, a.llm, a.tx, a.cfg, a.now),
		pipeline.KindThreadRefresh: threads.RefreshHandler(a.s, a.cfg, nil, a.now),
	}
	kinds := []string{pipeline.KindClassify, pipeline.KindThreadRefresh}
	done := 0
	for limit == 0 || done < limit {
		j, err := a.s.Claim(ctx, kinds, a.now())
		if err != nil || j == nil {
			fmt.Fprintf(out, "processed %d jobs\n", done)
			return err
		}
		err = h[j.Kind](ctx, j)
		switch {
		case errors.Is(err, pipeline.ErrBudgetReached):
			_ = a.s.Release(ctx, j.ID, a.now().Add(pipeline.BudgetWindow), a.now())
			fmt.Fprintf(out, "processed %d jobs; stopped: %v\n", done, pipeline.ErrBudgetReached)
			return nil
		case err != nil:
			_ = a.s.Fail(ctx, j.ID, err.Error(), a.now())
			return fmt.Errorf("job %d (%s): %w", j.ID, j.Kind, err)
		}
		if err := a.s.Complete(ctx, j.ID, a.now()); err != nil {
			return err
		}
		done++
	}
	fmt.Fprintf(out, "processed %d jobs (limit)\n", done)
	return nil
}

func (a *app) mailboxesCmd(ctx context.Context, pos []string, cfgPath string, write bool, out io.Writer) error {
	if len(pos) != 1 || (pos[0] != "discover" && pos[0] != "verify") {
		fmt.Fprintln(out, "usage: dwt mailboxes discover [--write-config] | verify")
		return errUsage
	}
	if pos[0] == "verify" {
		if err := mailbox.SyncFromConfig(ctx, a.s, a.cfg); err != nil {
			return err
		}
		bad := 0
		for _, st := range mailbox.VerifyAll(ctx, a.c, a.s, a.now()) {
			if st.OK {
				fmt.Fprintf(out, "ok        %s\n", st.Address)
			} else {
				bad++
				fmt.Fprintf(out, "DEGRADED  %s: %s\n", st.Address, st.Reason)
			}
		}
		if bad > 0 {
			return fmt.Errorf("%d mailbox(es) not readable", bad)
		}
		return nil
	}
	var expected []string
	for _, mb := range a.cfg.Mailboxes {
		expected = append(expected, mb.Address)
	}
	found, missing, err := mailbox.Discover(ctx, a.c, expected)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "found %d readable mailboxes\n", len(found))
	for _, m := range missing {
		fmt.Fprintf(out, "MISSING  %s (in config, not readable: no such mailbox, or outside the app's access)\n", m)
	}
	if !write {
		for _, f := range found {
			fmt.Fprintln(out, "  "+f)
		}
		return nil
	}
	added, err := mergeMailboxes(cfgPath, found)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "added %d mailboxes to %s (enabled, digest:false; review before serving; backup at %s.bak)\n", len(added), cfgPath, cfgPath)
	for _, x := range added {
		fmt.Fprintln(out, "  + "+x)
	}
	return nil
}

// digestInstant is 08:00 Europe/Rome of the given day.
func digestInstant(date string, now time.Time) (time.Time, error) {
	d := now.In(config.Rome)
	if date != "" && date != "today" {
		t, err := time.ParseInLocation(time.DateOnly, date, config.Rome)
		if err != nil {
			return time.Time{}, fmt.Errorf("--date %q: want YYYY-MM-DD or today", date)
		}
		d = t
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 8, 0, 0, 0, config.Rome), nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(strings.ToLower(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseArgs parses flags anywhere on the line (`mailboxes discover
// --write-config` and `mailboxes --write-config discover` both work) and
// returns the positional arguments in order.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}
