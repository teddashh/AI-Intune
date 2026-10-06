package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/processenv"
)

// notifier is the delivery seam. nil means no command is configured. A later
// Telegram or webhook sender can implement the same method without changing
// deliver's name or parameters.
type notifier interface {
	Notify(ctx context.Context, kind, body string) (channel string, err error)
}

type commandNotifier struct {
	cmd string
}

func (n commandNotifier) Notify(ctx context.Context, kind, body string) (string, error) {
	cmd := processenv.CommandContext(ctx, "sh", "-c", n.cmd)
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return "cmd", fmt.Errorf("%w: %s", err, msg)
		}
		return "cmd", err
	}
	return "cmd", nil
}

func (h *hub) notifier() notifier {
	if h == nil || strings.TrimSpace(h.notifyCmd) == "" {
		return nil
	}
	return commandNotifier{cmd: h.notifyCmd}
}

const (
	notifyNotConfigured = "notify not configured"
	notifyBackoffCap    = 60 * time.Minute
)

// notifyBackoff is the wait after failuresSinceDue failed attempts:
// min(1m * 2^(failures-1), 60m). The shift stops at 6 so a large count
// cannot overflow time.Duration.
func notifyBackoff(failures int) time.Duration {
	if failures <= 1 {
		return time.Minute
	}
	shift := failures - 1
	if shift >= 6 {
		return notifyBackoffCap
	}
	d := time.Minute << shift
	if d <= 0 || d > notifyBackoffCap {
		return notifyBackoffCap
	}
	return d
}

// logNotifyConfigured records that a command is set. It does not run it.
func (h *hub) logNotifyConfigured() {
	if h.notifier() != nil {
		log.Printf("notify command is configured")
	}
}

func (h *hub) reportDue(now time.Time) (time.Time, bool) {
	hh, mm, ok := parseHM(h.reportAt)
	if !ok {
		return time.Time{}, false
	}
	return time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location()), true
}

func (h *hub) alreadyNotifiedToday(kind string, now time.Time) bool {
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	rows, _, _, err := h.store.NotificationAttemptsSince(kind, start)
	if err != nil {
		log.Printf("notify attempt lookup failed kind=%s: %v", kind, err)
		return false
	}
	return rows > 0
}

// deliver records every real attempt and returns true only when the notifier
// accepted the body. Backoff for the daily report stays in maybeSendReport.
func (h *hub) deliver(kind, body string, now time.Time) bool {
	n := h.notifier()
	if n == nil {
		// One undelivered row per kind per day. A later caller (including a
		// future alert sweep) must not append another row or another /fail ping.
		if h.alreadyNotifiedToday(kind, now) {
			return false
		}
		if err := h.store.RecordNotification(kind, "stdout", body, false, notifyNotConfigured, now); err != nil {
			log.Printf("notify record failed kind=%s: %v", kind, err)
			return false
		}
		log.Printf("WARN notify not configured kind=%s; recorded once and will not retry today", kind)
		log.Printf("[%s]\n%s", kind, body)
		h.noteNotifyFailure(kind, now)
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	channel, err := n.Notify(ctx, kind, body)
	if channel == "" {
		channel = "cmd"
	}
	if err != nil {
		if recErr := h.store.RecordNotification(kind, channel, body, false, err.Error(), now); recErr != nil {
			log.Printf("notify record failed kind=%s: %v", kind, recErr)
		}
		h.logNotifyAttempt(kind, len(body), now, false)
		h.noteNotifyFailure(kind, now)
		return false
	}
	if recErr := h.store.RecordNotification(kind, channel, body, true, "", now); recErr != nil {
		log.Printf("notify record failed kind=%s: %v", kind, recErr)
	}
	h.logNotifyAttempt(kind, len(body), now, true)
	h.touchReportStamp(kind, now)
	h.pingReportWatchdog(kind, true)
	return true
}

func (h *hub) logNotifyAttempt(kind string, nbytes int, now time.Time, delivered bool) {
	attempt := 1
	next := ""
	if due, ok := h.reportDue(now); ok && kind == "daily" {
		rows, failures, lastFailed, err := h.store.NotificationAttemptsSince(kind, due)
		if err == nil && rows > 0 {
			attempt = rows
			if !delivered && failures > 0 && !lastFailed.IsZero() {
				next = lastFailed.Add(notifyBackoff(failures)).Format(time.RFC3339)
			}
		}
	}
	if delivered {
		log.Printf("notify attempt kind=%s bytes=%d attempt=%d outcome=delivered", kind, nbytes, attempt)
		return
	}
	if next != "" {
		log.Printf("notify attempt kind=%s bytes=%d attempt=%d outcome=failed next=%s", kind, nbytes, attempt, next)
		return
	}
	log.Printf("notify attempt kind=%s bytes=%d attempt=%d outcome=failed", kind, nbytes, attempt)
}

// noteNotifyFailure pings the daily watchdog on the first failure of the day
// and again when the backoff step reaches the 60 minute cap. A single direct
// deliver with no report schedule still pings once.
func (h *hub) noteNotifyFailure(kind string, now time.Time) {
	if kind != "daily" {
		return
	}
	due, ok := h.reportDue(now)
	if !ok {
		h.pingReportWatchdog(kind, false)
		return
	}
	_, failures, _, err := h.store.NotificationAttemptsSince(kind, due)
	if err != nil {
		log.Printf("notify failure count failed kind=%s: %v", kind, err)
		return
	}
	if failures <= 1 || notifyBackoff(failures) == notifyBackoffCap {
		h.pingReportWatchdog(kind, false)
	}
}
