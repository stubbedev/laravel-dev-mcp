package app

import (
	"bufio"
	"context"
	"errors"
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

// redisTarget is where and as whom to connect to one Redis database.
type redisTarget struct {
	addr     string
	username string // Redis 6+ ACL user; empty authenticates as default
	password string
	db       int
}

const (
	// redisIOTimeout bounds a connection's reads and writes when the context
	// carries no deadline of its own.
	redisIOTimeout = 30 * time.Second
	// redisDialTimeout bounds connecting, so a firewalled port fails fast.
	redisDialTimeout = 5 * time.Second
	// respCRLFLen is the length of the CRLF that terminates every RESP line.
	respCRLFLen = 2
	// respMinLineLen is the shortest valid RESP line: a type byte plus CRLF.
	respMinLineLen = 1 + respCRLFLen
)

var (
	errRESPShortReply  = errors.New("short RESP reply")
	errRESPUnknownType = errors.New("unknown RESP type")
	errRedisReply      = errors.New("redis")
)

// dialRedis connects, authenticates and selects the target database. Every
// failure names the address, since that is what a user needs to fix.
func dialRedis(ctx context.Context, target redisTarget) (*redisConn, error) {
	var dialer net.Dialer

	dialer.Timeout = redisDialTimeout

	netConn, err := dialer.DialContext(ctx, "tcp", target.addr)
	if err != nil {
		return nil, fmt.Errorf("redis connect (%s): %w", target.addr, err)
	}
	// The dial honors ctx, but reads on the open socket don't; a deadline
	// keeps an unresponsive server from hanging the tool call.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(redisIOTimeout)
	}

	err = netConn.SetDeadline(deadline)
	if err != nil {
		_ = netConn.Close()

		return nil, fmt.Errorf("redis connect (%s): %w", target.addr, err)
	}

	client := &redisConn{conn: netConn, r: bufio.NewReader(netConn)}

	err = client.handshake(target)
	if err != nil {
		_ = client.Close()

		return nil, fmt.Errorf("redis connect (%s): %w", target.addr, err)
	}

	return client, nil
}

func (rc *redisConn) Close() error {
	err := rc.conn.Close()
	if err != nil {
		return fmt.Errorf("close redis connection: %w", err)
	}

	return nil
}

// handshake authenticates and selects the target database, as needed.
func (rc *redisConn) handshake(target redisTarget) error {
	if target.password != "" {
		auth := []string{"AUTH", target.password}
		if target.username != "" {
			auth = []string{"AUTH", target.username, target.password}
		}

		_, err := rc.cmd(auth...)
		if err != nil {
			return err
		}
	}

	if target.db != 0 {
		_, err := rc.cmd("SELECT", strconv.Itoa(target.db))
		if err != nil {
			return err
		}
	}

	return nil
}

// cmd sends one command and returns the decoded reply (string, int64, nil, or
// []any for arrays).
func (rc *redisConn) cmd(args ...string) (any, error) {
	var wire strings.Builder
	fmt.Fprintf(&wire, "*%d\r\n", len(args))

	for _, a := range args {
		fmt.Fprintf(&wire, "$%d\r\n%s\r\n", len(a), a)
	}

	_, err := io.WriteString(rc.conn, wire.String())
	if err != nil {
		return nil, fmt.Errorf("send redis command: %w", err)
	}

	return readRESP(rc.r)
}

// readRESP decodes one RESP2 reply. Split out from redisConn so the decoder can
// be tested against canned bytes without a server.
func readRESP(reader *bufio.Reader) (any, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read RESP reply: %w", err)
	}

	if len(line) < respMinLineLen {
		return nil, fmt.Errorf("%w %q", errRESPShortReply, line)
	}

	typ, body := line[0], strings.TrimRight(line[1:], "\r\n")
	switch typ {
	case '+':
		return body, nil
	case '-':
		return nil, fmt.Errorf("%w: %s", errRedisReply, body)
	case ':':
		num, err := strconv.ParseInt(body, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse RESP integer: %w", err)
		}

		return num, nil
	case '$':
		return readRESPBulk(reader, body)
	case '*':
		return readRESPArray(reader, body)
	default:
		return nil, fmt.Errorf("%w %q", errRESPUnknownType, typ)
	}
}

// readRESPBulk reads a bulk string's payload, given its length header.
func readRESPBulk(reader *bufio.Reader, header string) (any, error) {
	size, err := strconv.Atoi(header)
	if err != nil {
		return nil, fmt.Errorf("parse RESP bulk length: %w", err)
	}

	if size < 0 {
		return nil, nil //nolint:nilnil // a null bulk string is a valid reply (missing key), not an error
	}

	buf := make([]byte, size+respCRLFLen) // value + trailing CRLF

	_, err = io.ReadFull(reader, buf)
	if err != nil {
		return nil, fmt.Errorf("read RESP bulk string: %w", err)
	}

	return string(buf[:size]), nil
}

// readRESPArray reads an array's elements, given its length header.
func readRESPArray(reader *bufio.Reader, header string) (any, error) {
	size, err := strconv.Atoi(header)
	if err != nil {
		return nil, fmt.Errorf("parse RESP array length: %w", err)
	}

	if size < 0 {
		return nil, nil //nolint:nilnil // a null array is a valid reply, not an error
	}

	arr := make([]any, size)
	for i := range arr {
		arr[i], err = readRESP(reader)
		if err != nil {
			return nil, err
		}
	}

	return arr, nil
}

// ── typed helpers ────────────────────────────────────────────────────────────

func (rc *redisConn) getString(key string) (string, bool, error) {
	reply, err := rc.cmd("GET", key)
	if err != nil {
		return "", false, err
	}

	if reply == nil {
		return "", false, nil
	}

	return fmt.Sprint(reply), true, nil
}

func (rc *redisConn) intCmd(args ...string) (int64, error) {
	v, err := rc.cmd(args...)
	if err != nil {
		return 0, err
	}

	num, ok := v.(int64)
	if !ok {
		return 0, nil
	}

	return num, nil
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

	arr, ok := reply.([]any)
	if !ok {
		return []string{}, nil
	}

	out := make([]string, 0, len(arr))
	for _, v := range arr {
		out = append(out, fmt.Sprint(v))
	}

	return out, nil
}
