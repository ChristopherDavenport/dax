package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses/echo"
)

// Forward puts a port on the workspace's loopback, a server the model
// started where the tools act, on this machine's loopback alone, and
// the forward ends with the session (#154).
func TestForwardReachesAPortWhereTheToolsAct(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "dev server") }))
	port := ln.Addr().(*net.TCPAddr).Port

	ctx := context.Background()
	s, err := New(ctx, options(t, &echo.Adapter{}))
	if err != nil {
		t.Fatal(err)
	}
	url, err := s.Forward(ctx, port)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") || url == "http://127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("forwarded at %q, want a loopback port of its own", url)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "dev server" {
		t.Errorf("through the forward: %q", body)
	}
	if again, err := s.Forward(ctx, port); err != nil || again != url {
		t.Errorf("forwarding again = %q, %v; want the same %q", again, err, url)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get(url); err == nil {
		t.Error("the forward outlived the session")
	}
	if _, err := s.Forward(ctx, 0); err == nil {
		t.Error("port 0 was forwarded")
	}
}
