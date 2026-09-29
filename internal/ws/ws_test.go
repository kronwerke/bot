package ws

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// RFC 6455 section 1.3 example.
func TestAcceptKeyMatchesRFCExample(t *testing.T) {
	if got := AcceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("AcceptKey = %s", got)
	}
}

func serve(t *testing.T, fn func(*ServerConn)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc, err := Upgrade(w, r)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		fn(sc)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/?v=10"
}

func TestEchoLargeAndFragmentedMessages(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 70000) // 64 bit length
	mid := bytes.Repeat([]byte("y"), 300)   // 16 bit length
	url := serve(t, func(sc *ServerConn) {
		for i := 0; i < 3; i++ {
			_, msg, err := sc.Read()
			if err != nil {
				return
			}
			if i == 2 {
				sc.WriteFragmented(msg)
			} else {
				sc.Write(msg)
			}
		}
	})
	c, err := Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range [][]byte{[]byte("hi"), big, mid} {
		if err := c.WriteText(m); err != nil {
			t.Fatal(err)
		}
		op, got, err := c.Read()
		if err != nil {
			t.Fatal(err)
		}
		if op != OpText || !bytes.Equal(got, m) {
			t.Fatalf("echo of %d bytes came back as %d bytes", len(m), len(got))
		}
	}
}

func TestPingIsAnsweredAndReadContinues(t *testing.T) {
	url := serve(t, func(sc *ServerConn) {
		sc.Ping([]byte("p"))
		sc.Write([]byte("after ping"))
		time.Sleep(100 * time.Millisecond)
	})
	c, err := Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.Read()
	if err != nil || string(msg) != "after ping" {
		t.Fatalf("got %q, %v", msg, err)
	}
}

func TestCloseCodeIsReported(t *testing.T) {
	url := serve(t, func(sc *ServerConn) { sc.CloseWith(4004) })
	c, err := Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Read()
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != 4004 {
		t.Fatalf("want close 4004, got %v", err)
	}
}

func TestHandshakeRefusedWithoutUpgrade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusBadRequest)
	}))
	defer srv.Close()
	_, err := Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err == nil {
		t.Fatal("dial succeeded against a plain HTTP server")
	}
}
