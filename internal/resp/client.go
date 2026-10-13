// Package resp is a small, dependency-free Redis client that speaks the
// RESP2 wire protocol over a pool of TCP connections.
package resp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

// ErrNil is returned when Redis replies with a null bulk string (key missing).
var ErrNil = errors.New("resp: nil reply")

// RedisError is an error reply sent by the server (e.g. "WRONGTYPE ...").
type RedisError string

func (e RedisError) Error() string { return "redis: " + string(e) }

type conn struct {
	nc net.Conn
	r  *bufio.Reader
	w  *bufio.Writer
}

// Client is safe for concurrent use. Connections are reused through a
// bounded idle pool.
type Client struct {
	addr    string
	timeout time.Duration
	mu      sync.Mutex
	idle    []*conn
	maxIdle int
}

// New creates a client for addr (host:port).
func New(addr string) *Client {
	return &Client{addr: addr, timeout: 3 * time.Second, maxIdle: 16}
}

// Ping checks connectivity.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Do(ctx, "PING")
	return err
}

// Do sends one command and returns the decoded reply: string, int64, []any or nil.
func (c *Client) Do(ctx context.Context, args ...string) (any, error) {
	cn, err := c.get(ctx)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = cn.nc.SetDeadline(deadline)

	if err := writeCommand(cn.w, args); err != nil {
		cn.nc.Close()
		return nil, err
	}
	reply, err := readReply(cn.r)
	var redisErr RedisError
	if err != nil && !errors.As(err, &redisErr) && !errors.Is(err, ErrNil) {
		cn.nc.Close() // broken connection: don't return it to the pool
		return nil, err
	}
	c.put(cn)
	return reply, err
}

// Close closes all idle connections.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cn := range c.idle {
		cn.nc.Close()
	}
	c.idle = nil
	return nil
}

func (c *Client) get(ctx context.Context) (*conn, error) {
	c.mu.Lock()
	if n := len(c.idle); n > 0 {
		cn := c.idle[n-1]
		c.idle = c.idle[:n-1]
		c.mu.Unlock()
		return cn, nil
	}
	c.mu.Unlock()
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, fmt.Errorf("resp: dial %s: %w", c.addr, err)
	}
	return &conn{nc: nc, r: bufio.NewReader(nc), w: bufio.NewWriter(nc)}, nil
}

func (c *Client) put(cn *conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.idle) >= c.maxIdle {
		cn.nc.Close()
		return
	}
	c.idle = append(c.idle, cn)
}

// writeCommand encodes args as a RESP array of bulk strings.
func writeCommand(w *bufio.Writer, args []string) error {
	fmt.Fprintf(w, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(w, "$%d\r\n%s\r\n", len(a), a)
	}
	return w.Flush()
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return "", fmt.Errorf("resp: malformed line %q", line)
	}
	return line[:len(line)-2], nil
}

func readReply(r *bufio.Reader) (any, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	if line == "" {
		return nil, errors.New("resp: empty reply")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return nil, RedisError(line[1:])
	case ':':
		return strconv.ParseInt(line[1:], 10, 64)
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, ErrNil
		}
		buf := make([]byte, n+2)
		if _, err := ioReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, ErrNil
		}
		out := make([]any, n)
		for i := range out {
			v, err := readReply(r)
			if err != nil && !errors.Is(err, ErrNil) {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	default:
		return nil, fmt.Errorf("resp: unknown reply type %q", line[0])
	}
}

func ioReadFull(r *bufio.Reader, buf []byte) (int, error) {
	read := 0
	for read < len(buf) {
		n, err := r.Read(buf[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}
