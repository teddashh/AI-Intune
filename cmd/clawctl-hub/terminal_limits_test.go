package main

import (
	"testing"
	"time"
)

func TestParseTerminalLimitsDefaults(t *testing.T) {
	idle, life, err := parseTerminalLimits("", "")
	if err != nil || idle != 30*time.Minute || life != 12*time.Hour {
		t.Fatalf("defaults idle=%v life=%v err=%v", idle, life, err)
	}
	idle, life, err = parseTerminalLimits("1m30s", "90m")
	if err != nil || idle != 90*time.Second || life != 90*time.Minute {
		t.Fatalf("explicit idle=%v life=%v err=%v", idle, life, err)
	}
}

func TestParseTerminalLimits(t *testing.T) {
	tests := []struct {
		idle    string
		life    string
		wantErr bool
	}{
		{"", "", false}, // defaults
		{"10m", "10h", false},
		{"59s", "10h", true},  // < 1m
		{"25h", "100h", true}, // > 24h
		{"1m30s", "10h", false},
		{"1m30.5s", "10h", true}, // non-whole seconds
		{"10m", "59s", true},     // < 1m
		{"10m", "721h", true},    // > 720h
		{"2h", "1h", true},       // idle > life
		{"invalid", "10h", true},
		{"10m", "invalid", true},
		{"0", "10h", true}, // no value disables the idle limit
		{"10m", "0", true}, // no value disables the lifetime
		{"-1m", "10h", true},
		{"10m", "-1h", true},
		{"12h", "12h", false}, // idle == life is allowed
	}
	for _, tc := range tests {
		t.Run(tc.idle+"_"+tc.life, func(t *testing.T) {
			_, _, err := parseTerminalLimits(tc.idle, tc.life)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseTerminalLimits(%q, %q) error = %v, wantErr %v", tc.idle, tc.life, err, tc.wantErr)
			}
		})
	}
}
