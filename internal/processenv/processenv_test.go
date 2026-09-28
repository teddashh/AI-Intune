package processenv

import (
	"reflect"
	"testing"
)

func TestWithoutSystemdNotificationsRemovesOnlyServiceControlVariables(t *testing.T) {
	input := []string{
		"HOME=/home/operator",
		"NOTIFY_SOCKET=/run/user/1000/notify",
		"WATCHDOG_PID=42",
		"WATCHDOG_USEC=30000000",
		"NOTIFY_SOCKET_BACKUP=keep",
		"PATH=/usr/bin",
		"malformed",
	}
	want := []string{
		"HOME=/home/operator",
		"NOTIFY_SOCKET_BACKUP=keep",
		"PATH=/usr/bin",
		"malformed",
	}
	if got := WithoutSystemdNotifications(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("子行程環境 = %q；要 %q", got, want)
	}
	if !reflect.DeepEqual(input, []string{
		"HOME=/home/operator",
		"NOTIFY_SOCKET=/run/user/1000/notify",
		"WATCHDOG_PID=42",
		"WATCHDOG_USEC=30000000",
		"NOTIFY_SOCKET_BACKUP=keep",
		"PATH=/usr/bin",
		"malformed",
	}) {
		t.Fatal("原始環境被改動")
	}
}
