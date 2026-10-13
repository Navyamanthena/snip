package resp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestWriteCommand(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	if err := writeCommand(w, []string{"SET", "k", "hello"}); err != nil {
		t.Fatal(err)
	}
	want := "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$5\r\nhello\r\n"
	if buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
}

func TestReadReply(t *testing.T) {
	cases := []struct {
		in   string
		want any
		err  error
	}{
		{"+OK\r\n", "OK", nil},
		{":42\r\n", int64(42), nil},
		{"$5\r\nhello\r\n", "hello", nil},
		{"$-1\r\n", nil, ErrNil},
		{"*2\r\n$1\r\na\r\n:7\r\n", []any{"a", int64(7)}, nil},
	}
	for _, c := range cases {
		got, err := readReply(bufio.NewReader(strings.NewReader(c.in)))
		if !errors.Is(err, c.err) {
			t.Fatalf("%q: err %v want %v", c.in, err, c.err)
		}
		if gs, ok := got.([]any); ok {
			ws := c.want.([]any)
			if len(gs) != len(ws) || gs[0] != ws[0] || gs[1] != ws[1] {
				t.Fatalf("%q: got %v want %v", c.in, got, c.want)
			}
			continue
		}
		if got != c.want {
			t.Fatalf("%q: got %v want %v", c.in, got, c.want)
		}
	}
}

func TestErrorReply(t *testing.T) {
	_, err := readReply(bufio.NewReader(strings.NewReader("-WRONGTYPE bad\r\n")))
	var re RedisError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "WRONGTYPE") {
		t.Fatalf("got %v", err)
	}
}

// Runs against a real Redis when REDIS_ADDR is set (CI starts one).
func TestAgainstRealRedis(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set")
	}
	c := New(addr)
	defer c.Close()
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(ctx, "SET", "resp:test", "v1"); err != nil {
		t.Fatal(err)
	}
	v, err := c.Do(ctx, "GET", "resp:test")
	if err != nil || v != "v1" {
		t.Fatalf("got %v %v", v, err)
	}
	if _, err := c.Do(ctx, "GET", "resp:missing"); !errors.Is(err, ErrNil) {
		t.Fatalf("expected ErrNil, got %v", err)
	}
}
