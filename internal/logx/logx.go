// Package logx provides the privacy-aware leveled logger used across the proxy.
//
// Port of proxy.mjs log(): one line per entry, optional data object rendered as
// JSON. Privacy rules from the reference: API key fragments, upstream error
// bodies (beyond the summarized snippet), and stack traces never reach logs.
package logx

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func parseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "info"
	}
}

type Logger struct {
	mu     sync.Mutex
	min    Level
	file   io.Writer // optional secondary sink (logFile)
	stdout io.Writer
}

var std = &Logger{min: LevelInfo, stdout: os.Stdout}

// Init configures the process-wide logger. logFile may be empty (stdout only).
func Init(level, logFile string) {
	std.mu.Lock()
	defer std.mu.Unlock()
	std.min = parseLevel(level)
	std.stdout = os.Stdout
	std.file = nil
	if logFile != "" {
		if f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			std.file = f
		} else {
			fmt.Fprintf(os.Stderr, "[config] Failed to open logFile %s: %v\n", logFile, err)
		}
	}
}

// MinLevel returns the configured minimum level.
func MinLevel() Level { std.mu.Lock(); defer std.mu.Unlock(); return std.min }

func (lg *Logger) write(level Level, msg string, data any) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	if level < lg.min {
		return
	}
	var line string
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			b = []byte(fmt.Sprintf("%v", data))
		}
		line = fmt.Sprintf("[%s] [%s] %s %s\n", time.Now().UTC().Format(time.RFC3339Nano), level, msg, b)
	} else {
		line = fmt.Sprintf("[%s] [%s] %s\n", time.Now().UTC().Format(time.RFC3339Nano), level, msg)
	}
	fmt.Fprint(lg.stdout, line)
	if lg.file != nil {
		fmt.Fprint(lg.file, line)
	}
}

func Debug(msg string, data ...any) { std.write(LevelDebug, msg, orNil(data)) }
func Info(msg string, data ...any)  { std.write(LevelInfo, msg, orNil(data)) }
func Warn(msg string, data ...any)  { std.write(LevelWarn, msg, orNil(data)) }
func Error(msg string, data ...any) { std.write(LevelError, msg, orNil(data)) }

func orNil(data []any) any {
	if len(data) == 0 {
		return nil
	}
	return data[0]
}

// SummarizeUpstreamError flattens an upstream error body into one line for
// logs, truncated to limit chars (port of summarizeUpstreamError).
func SummarizeUpstreamError(text string, limit int) string {
	if text == "" {
		return ""
	}
	if limit <= 0 {
		limit = 500
	}
	flat := strings.Join(strings.Fields(text), " ")
	if len(flat) > limit {
		return flat[:limit] + fmt.Sprintf("…(%d more)", len(flat)-limit)
	}
	return flat
}
