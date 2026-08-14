package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestConfiguredLogLevel(t *testing.T) {
	t.Parallel()

	tests := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for value, want := range tests {
		if got := configuredLogLevel(value); got != want {
			t.Errorf("configuredLogLevel(%q) = %s, want %s", value, got, want)
		}
	}
}

func TestLoggerRespectsConfiguredLevel(t *testing.T) {
	var level slog.LevelVar
	var output bytes.Buffer
	logger := newLogger(&output, &level)

	level.Set(configuredLogLevel("info"))
	if logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug log is enabled at info level")
	}
	logger.Debug("debug marker")
	if output.Len() != 0 {
		t.Fatalf("debug output at info level = %q, want empty", output.String())
	}

	level.Set(configuredLogLevel("debug"))
	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug log is disabled at debug level")
	}
	logger.Debug("debug marker")
	if got := output.String(); !strings.Contains(got, "level=DEBUG") || !strings.Contains(got, `msg="debug marker"`) {
		t.Fatalf("debug output = %q, want DEBUG marker", got)
	}
}
