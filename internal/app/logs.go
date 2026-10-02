package app

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// logEntryStart matches the start of a Laravel/Monolog line:
// "[2024-01-02 15:04:05] local.ERROR: message".
var logEntryStart = regexp.MustCompile(`^\[\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}`)

// maxLogTail caps how many bytes we read from the end of a log file.
const maxLogTail = 2 << 20 // 2 MiB

type logEntry struct {
	Timestamp string `json:"timestamp,omitempty"`
	Level     string `json:"level,omitempty"`
	Channel   string `json:"channel,omitempty"`
	Message   string `json:"message"`
}

// tailBytes returns up to the last maxBytes bytes of a file.
func tailBytes(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // G304: tailing the project's own log files is the point
	if err != nil {
		return nil, fmt.Errorf("opening log: %w", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("reading log size: %w", err)
	}

	start := int64(0)
	if info.Size() > maxBytes {
		start = info.Size() - maxBytes
	}

	_, err = file.Seek(start, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("seeking log tail: %w", err)
	}

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("reading log tail: %w", err)
	}

	return data, nil
}

var headerRe = regexp.MustCompile(`^\[([^\]]+)\]\s+([^.]+)\.(\w+):\s?(.*)$`)

// parseLogHeader splits an entry's first line into its fields and the start of
// its message; a line that doesn't match the header shape is all message.
func parseLogHeader(line string) (logEntry, string) {
	match := headerRe.FindStringSubmatch(line)
	if match == nil {
		return logEntry{Timestamp: "", Level: "", Channel: "", Message: ""}, line
	}

	return logEntry{Timestamp: match[1], Level: strings.ToUpper(match[3]), Channel: match[2], Message: ""}, match[4]
}

// parseLogEntries splits raw Laravel log text into entries (header line + any
// following stack-trace lines), most recent last.
func parseLogEntries(raw string) []logEntry {
	var (
		entries []logEntry
		cur     *logEntry
		msg     strings.Builder // cur's message; traces run to thousands of lines
	)

	flush := func() {
		if cur != nil {
			cur.Message = strings.TrimRight(msg.String(), "\n")
			entries = append(entries, *cur)
			cur = nil
		}
	}

	for line := range strings.Lines(raw) {
		line = strings.TrimSuffix(line, "\n")
		if logEntryStart.MatchString(line) {
			flush()

			entry, first := parseLogHeader(line)

			msg.Reset()
			msg.WriteString(first)

			cur = &entry

			continue
		}

		if cur != nil {
			msg.WriteByte('\n')
			msg.WriteString(line)
		}
	}

	flush()

	return entries
}

func lastN(entries []logEntry, n int) []logEntry {
	if n > 0 && len(entries) > n {
		return entries[len(entries)-n:]
	}

	return entries
}
