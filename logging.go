package main

import (
	"fmt"
	"log"
	"os"
	"time"
)

// ANSI escape sequences used to color the level tag on terminals.
const (
	colorReset  = "\033[0m"
	colorGray   = "\033[90m"
	colorGreen  = "\033[32m"
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
)

// logTimeLayout is the timestamp prefix of every log line.
const logTimeLayout = "2006/01/02 15:04:05"

// logColors reports whether stderr (the log destination) is a terminal; ANSI
// colors are only emitted then, so files and log collectors get plain text.
var logColors = stderrIsTerminal()

func init() {
	log.SetFlags(0)
}

func stderrIsTerminal() bool {
	info, err := os.Stderr.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// logWithLevel writes one line: timestamp, level tag and the formatted
// message with every URL redacted (see sensitiveURLPattern). Every call
// emits a line; there is no level filtering.
func logWithLevel(level string, color string, format string, args ...any) {
	levelText := "[" + level + "]"
	if logColors {
		levelText = color + levelText + colorReset
	}
	message := redactURLs(fmt.Sprintf(format, args...))
	line := time.Now().Format(logTimeLayout) + " " + levelText + " " + message
	log.Print(line)
}

// logDebugf logs at debug level (always emitted).
func logDebugf(format string, args ...any) {
	logWithLevel("debug", colorGray, format, args...)
}

// logInfof logs at info level.
func logInfof(format string, args ...any) {
	logWithLevel("info", colorGreen, format, args...)
}

// logWarnf logs at warn level.
func logWarnf(format string, args ...any) {
	logWithLevel("warn", colorYellow, format, args...)
}

// logErrorf logs at error level.
func logErrorf(format string, args ...any) {
	logWithLevel("error", colorRed, format, args...)
}
