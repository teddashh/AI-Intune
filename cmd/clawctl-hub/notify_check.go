package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

var errNotifyCheckFailed = errors.New("notify-check failed")

func cmdNotifyCheck(argv []string) {
	if err := runNotifyCheck(argv, os.Stdout, os.Stderr, ""); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		if !errors.Is(err, errNotifyCheckFailed) {
			log.Print(err)
		}
		os.Exit(1)
	}
}

func writeNotifyCheckUsage(errOut io.Writer) {
	fmt.Fprintln(errOut, "usage: clawctl-hub notify-check [--notify-env PATH]")
	fmt.Fprintln(errOut, "  Load the notify env file and check configured channels.")
	fmt.Fprintln(errOut, "  Telegram: getMe and getChat (sends nothing).")
	fmt.Fprintln(errOut, "  Webhook: validate config and print scheme+host (does not POST).")
}

// runNotifyCheck loads the same env file serve() uses. apiBase is empty in
// production and set only by tests to an httptest Telegram stand-in.
func runNotifyCheck(argv []string, out, errOut io.Writer, apiBase string) error {
	fs := flag.NewFlagSet("notify-check", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		writeNotifyCheckUsage(errOut)
		fs.PrintDefaults()
	}
	envPath := fs.String("notify-env", os.Getenv("CLAWCTL_NOTIFY_ENV"), "path to the notify env file (secrets live in the file, never in flags)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		writeNotifyCheckUsage(errOut)
		return fmt.Errorf("notify-check: unexpected arguments %q", strings.Join(fs.Args(), " "))
	}
	path := strings.TrimSpace(*envPath)
	if path == "" {
		writeNotifyCheckUsage(errOut)
		return errors.New("notify-check: no env file; set --notify-env or CLAWCTL_NOTIFY_ENV")
	}
	n := builtinNotifier{path: path, apiBase: apiBase}
	return n.check(context.Background(), out)
}
