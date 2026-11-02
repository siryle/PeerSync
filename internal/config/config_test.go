package config

import (
	"strings"
	"testing"
	"time"
)

func TestScanIntervals(t *testing.T) {
	for _, tc := range []struct {
		name, scan, verify, bad string
		wantScan, wantVerify    time.Duration
	}{
		{"defaults", "", "", "", 20 * time.Second, 5 * time.Minute},
		{"docker", "5m", "1h", "", 5 * time.Minute, time.Hour},
		{"short-verification", "1h", "50ms", "", time.Hour, 50 * time.Millisecond},
		{"invalid-scan", "minutes", "", "PEERSYNC_SCAN_INTERVAL", 0, 0},
		{"zero-scan", "0s", "", "PEERSYNC_SCAN_INTERVAL", 0, 0},
		{"negative-verification", "", "-1h", "PEERSYNC_VERIFY_INTERVAL", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PEERSYNC_SCAN_INTERVAL", tc.scan)
			t.Setenv("PEERSYNC_VERIFY_INTERVAL", tc.verify)
			scan, verify, err := ScanIntervals()
			if tc.bad != "" {
				if err == nil || !strings.Contains(err.Error(), tc.bad) {
					t.Fatalf("expected error naming %s, got %v", tc.bad, err)
				}
				return
			}
			if err != nil || scan != tc.wantScan || verify != tc.wantVerify {
				t.Fatalf("intervals = %v, %v, %v", scan, verify, err)
			}
		})
	}
}
