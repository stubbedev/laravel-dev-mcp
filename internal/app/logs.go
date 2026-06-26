package app

import (
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

// tailBytes returns up to the last n bytes of a file.
func tailBytes(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if info.Size() > n {
		start = info.Size() - n
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

var headerRe = regexp.MustCompile(`^\[([^\]]+)\]\s+([^.]+)\.(\w+):\s?(.*)$`)

// parseLogEntries splits raw Laravel log text into entries (header line + any
// following stack-trace lines), most recent last.
func parseLogEntries(raw string) []logEntry {
	lines := strings.Split(raw, "\n")
	var entries []logEntry
	var cur *logEntry
	flush := func() {
		if cur != nil {
			cur.Message = strings.TrimRight(cur.Message, "\n")
			entries = append(entries, *cur)
			cur = nil
		}
	}
	for _, line := range lines {
		if logEntryStart.MatchString(line) {
			flush()
			e := logEntry{}
			if m := headerRe.FindStringSubmatch(line); m != nil {
				e.Timestamp = m[1]
				e.Channel = m[2]
				e.Level = strings.ToUpper(m[3])
				e.Message = m[4]
			} else {
				e.Message = line
			}
			cur = &e
			continue
		}
		if cur != nil {
			cur.Message += "\n" + line
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
