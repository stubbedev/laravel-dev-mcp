package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// redisConn is a tiny read-only RESP2 client — just enough to inspect cache and
// queue state without pulling in a redis dependency (which would also grow the
// binary we keep deliberately small). Speaks the handful of read commands the
// state tool needs.
type redisConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialRedis(ctx context.Context, addr, password string, db int) (*redisConn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	rc := &redisConn{conn: c, r: bufio.NewReader(c)}
	if password != "" {
		if _, err := rc.cmd("AUTH", password); err != nil {
			_ = rc.Close()
			return nil, err
		}
	}
	if db != 0 {
		if _, err := rc.cmd("SELECT", strconv.Itoa(db)); err != nil {
			_ = rc.Close()
			return nil, err
		}
	}
	return rc, nil
}

func (rc *redisConn) Close() error { return rc.conn.Close() }

// cmd sends one command and returns the decoded reply (string, int64, nil, or
// []any for arrays).
func (rc *redisConn) cmd(args ...string) (any, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := rc.conn.Write([]byte(b.String())); err != nil {
		return nil, err
	}
	return readRESP(rc.r)
}

// readRESP decodes one RESP2 reply. Split out from redisConn so the decoder can
// be tested against canned bytes without a server.
func readRESP(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(line) < 3 {
		return nil, fmt.Errorf("short RESP reply %q", line)
	}
	typ, body := line[0], strings.TrimRight(line[1:], "\r\n")
	switch typ {
	case '+':
		return body, nil
	case '-':
		return nil, fmt.Errorf("redis: %s", body)
	case ':':
		n, err := strconv.ParseInt(body, 10, 64)
		return n, err
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil // null bulk string
		}
		buf := make([]byte, n+2) // value + trailing CRLF
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil
		}
		arr := make([]any, n)
		for i := range arr {
			if arr[i], err = readRESP(r); err != nil {
				return nil, err
			}
		}
		return arr, nil
	}
	return nil, fmt.Errorf("unknown RESP type %q", typ)
}

// ── typed helpers ────────────────────────────────────────────────────────────

func (rc *redisConn) getString(key string) (string, bool, error) {
	v, err := rc.cmd("GET", key)
	if err != nil {
		return "", false, err
	}
	if v == nil {
		return "", false, nil
	}
	return fmt.Sprint(v), true, nil
}

func (rc *redisConn) intCmd(args ...string) (int64, error) {
	v, err := rc.cmd(args...)
	if err != nil {
		return 0, err
	}
	n, _ := v.(int64)
	return n, nil
}

// scan returns up to limit keys matching pattern via a bounded SCAN walk.
func (rc *redisConn) scan(pattern string, limit int) ([]string, error) {
	cursor := "0"
	var keys []string
	for {
		reply, err := rc.cmd("SCAN", cursor, "MATCH", pattern, "COUNT", "200")
		if err != nil {
			return nil, err
		}
		arr, ok := reply.([]any)
		if !ok || len(arr) != 2 {
			return keys, nil
		}
		cursor = fmt.Sprint(arr[0])
		if batch, ok := arr[1].([]any); ok {
			for _, k := range batch {
				keys = append(keys, fmt.Sprint(k))
				if len(keys) >= limit {
					return keys, nil
				}
			}
		}
		if cursor == "0" {
			return keys, nil
		}
	}
}

// lrange returns up to limit elements from the head of a list.
func (rc *redisConn) lrange(key string, limit int) ([]string, error) {
	reply, err := rc.cmd("LRANGE", key, "0", strconv.Itoa(limit-1))
	if err != nil {
		return nil, err
	}
	arr, _ := reply.([]any)
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		out = append(out, fmt.Sprint(v))
	}
	return out, nil
}
