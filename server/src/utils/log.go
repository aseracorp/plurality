package utils

import (
	"fmt"
	"log"
	"os"
	"strings"
)

var (
	Reset    = "\033[0m"
	Bold     = "\033[1m"
	nRed     = "\033[31m"
	nGreen   = "\033[32m"
	nYellow  = "\033[33m"
	nBlue    = "\033[34m"
	nPurple  = "\033[35m"
	nCyan    = "\033[36m"
	nGray    = "\033[37m"
	nWhite   = "\033[97m"
	nMagenta = "\033[95m"
	nBlack   = "\033[30m"
	bRed     = "\033[41m"
	bGreen   = "\033[42m"
	bYellow  = "\033[43m"
	bBlue    = "\033[44m"
	bMagenta = "\033[45m"
	bCyan    = "\033[46m"
	bGray    = "\033[47m"
	bWhite   = "\033[107m"
	bPurple  = "\033[45m"
)

type LogLevel int
type LoggingLevel string

const (
	DEBUG LogLevel = iota
	INFO
	WARNING
	ERROR
	FATAL
)

var LoggingLevelLabels = map[LoggingLevel]LogLevel{
	"DEBUG":   DEBUG,
	"INFO":    INFO,
	"WARNING": WARNING,
	"ERROR":   ERROR,
}

// defaultLogLevel is the level used when LOG_LEVEL is unset or set to an
// unrecognized value. Previously RawLogMessage did a raw map lookup on the
// env var: when LOG_LEVEL was unset (the common case) the lookup returned 0
// (DEBUG) so everything logged, but when it was set to anything not in the
// map — e.g. "FATAL", or any case mismatch like "info" — the lookup also
// yielded 0 and, worse, could silently suppress or over-suppress messages.
// Normalize the env value to a stable default so logs are predictable.
const defaultLogLevel = INFO

// currentLogLevel resolves the effective log level from the LOG_LEVEL env
// var, normalizing to a stable default when it is absent or unrecognized.
func currentLogLevel() LogLevel {
	raw := strings.TrimSpace(os.Getenv("LOG_LEVEL"))
	if raw == "" {
		return defaultLogLevel
	}
	// Case-insensitive so "DEBUG"/"debug"/"Debug" all behave identically.
	upper := strings.ToUpper(raw)
	if lvl, ok := LoggingLevelLabels[LoggingLevel(upper)]; ok {
		return lvl
	}
	// Unknown value: fall back to the default rather than a silent/odd level.
	return defaultLogLevel
}

var (
	logger      *log.Logger
	errorLogger *log.Logger

	loggerPlain      *log.Logger
	errorLoggerPlain *log.Logger
)

func RawLogMessage(level LogLevel, prefix, prefixColor, color, message string) {
	if currentLogLevel() <= level {
		logString := prefixColor + Bold + prefix + Reset + " " + color + message + Reset

		log.Println(logString)
	}
}

func Debug(message string, args ...interface{}) {
	RawLogMessage(DEBUG, "[DEBUG]", bPurple, nPurple, fmt.Sprintf(message, args...))
}

func Log(message string, args ...interface{}) {
	RawLogMessage(INFO, "[INFO] ", bBlue, nBlue, fmt.Sprintf(message, args...))
}

func LogReq(message string) {
	RawLogMessage(INFO, "[REQ]  ", bGreen, nGreen, message)
}

func Warn(message string, args ...interface{}) {
	RawLogMessage(WARNING, "[WARN] ", bYellow, nYellow, fmt.Sprintf(message, args...))
}

func Error(message string, err error, args ...interface{}) {
	errStr := ""
	if err != nil {
		errStr = err.Error()
		RawLogMessage(ERROR, "[ERROR]", bRed, nRed, fmt.Sprintf(message+" : "+errStr, args...))
	} else {
		RawLogMessage(ERROR, "[ERROR]", bRed, nRed, fmt.Sprintf(message, args...))
	}
}

func MajorError(message string, err error, args ...interface{}) {
	Error(message, err)
}

func Fatal(message string, err error, args ...interface{}) {
	errStr := ""
	if err != nil {
		errStr = err.Error()
		RawLogMessage(FATAL, "[FATAL]", bRed, nRed, fmt.Sprintf(message+" : "+errStr, args...))
	} else {
		RawLogMessage(FATAL, "[FATAL]", bRed, nRed, fmt.Sprintf(message, args...))
	}

	os.Exit(1)
}