package box

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRulesAllows(t *testing.T) {
	allow := Rules{Internet: InternetAllow, Allow: []string{"proxy.golang.org", "*.githubusercontent.com", "GitHub.com"}}
	for _, tc := range []struct {
		rules Rules
		host  string
		want  bool
	}{
		{Rules{Internet: InternetOff}, "proxy.golang.org", false},
		{Rules{Internet: InternetOn}, "anything.example", true},
		{allow, "proxy.golang.org", true},
		{allow, "sum.golang.org", false},
		{allow, "evil-proxy.golang.org", false},
		{allow, "proxy.golang.org.evil.test", false},
		{allow, "raw.githubusercontent.com", true},
		{allow, "a.b.githubusercontent.com", true},
		{allow, "githubusercontent.com", false},
		{allow, "evilgithubusercontent.com", false},
		{allow, "github.com", true},
		{Rules{Internet: "bogus", Allow: []string{"x.test"}}, "x.test", false},
	} {
		if got := tc.rules.allows(tc.host); got != tc.want {
			t.Errorf("%v allows %q = %v, want %v", tc.rules, tc.host, got, tc.want)
		}
	}
}

func TestBlocked(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.8.9.10", true},
		{"::1", true},
		{"10.1.2.3", true},
		{"172.17.0.1", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true},
		{"fe80::1", true},
		{"fd00::1", true},
		{"100.64.0.1", true},
		{"100.127.255.254", true},
		{"0.0.0.0", true},
		{"::", true},
		{"224.0.0.1", true},
		{"255.255.255.255", true},
		{"::ffff:127.0.0.1", true},
		{"::ffff:10.0.0.1", true},
		{"8.8.8.8", false},
		{"100.128.0.1", false},
		{"2606:4700:4700::1111", false},
		{"::ffff:8.8.8.8", false},
	} {
		if got := blocked(netip.MustParseAddr(tc.ip)); got != tc.want {
			t.Errorf("blocked(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// fakeNet is a dialer that sends every destination to the loopback
// server that stands in for it, and records what was dialed.
type fakeNet struct {
	mu     sync.Mutex
	routes map[string]string // host:port -> loopback address
	dialed []string
}

func (n *fakeNet) dial(ctx context.Context, network, address string) (net.Conn, error) {
	n.mu.Lock()
	n.dialed = append(n.dialed, address)
	to, ok := n.routes[address]
	n.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no route to %s", address)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, to)
}

type proxyTest struct {
	rules   string
	proxy   *Proxy
	url     *url.URL
	net     *fakeNet
	logs    []string
	tlsPool *tls.Config
	writes  int
}

// newProxyTest is a proxy under a rules file, in front of a plain HTTP
// server standing in for every host on port 80 and a TLS one for
// example.com:443 (httptest's certificate names example.com).
func newProxyTest(t *testing.T, rules Rules) *proxyTest {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "http://elsewhere.test/", http.StatusFound)
			return
		case "/headers":
			fmt.Fprintf(w, "auth=%q drop=%q keep=%q", r.Header.Get("Proxy-Authorization"), r.Header.Get("X-Drop"), r.Header.Get("X-Keep"))
			return
		}
		w.Header().Set("X-Up", "yes")
		fmt.Fprintf(w, "plain %s %s", r.Host, r.URL.Path)
	}))
	t.Cleanup(up.Close)
	tlsUp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "tls %s", r.Host)
	}))
	t.Cleanup(tlsUp.Close)
	pt := &proxyTest{rules: filepath.Join(t.TempDir(), "box", RulesFile)}
	pt.write(t, rules)
	pt.proxy = NewProxy(pt.rules, func(f string, a ...any) { pt.logs = append(pt.logs, fmt.Sprintf(f, a...)) })
	upAddr := up.Listener.Addr().String()
	pt.net = &fakeNet{routes: map[string]string{
		"example.com:443":      tlsUp.Listener.Addr().String(),
		"example.com:80":       upAddr,
		"proxy.golang.org:80":  upAddr,
		"golang.org:80":        upAddr,
		"evil.test:80":         upAddr,
		"a.example.com:80":     upAddr,
		"example.com:8080":     upAddr,
		"elsewhere.test:80":    upAddr,
		"raw.example.org:80":   upAddr,
		"proxy.golang.org:443": tlsUp.Listener.Addr().String(),
	}}
	pt.proxy.dial = pt.net.dial
	srv := httptest.NewServer(pt.proxy)
	t.Cleanup(srv.Close)
	pt.url, _ = url.Parse(srv.URL)
	pt.tlsPool = tlsUp.Client().Transport.(*http.Transport).TLSClientConfig
	return pt
}

// write writes the rules, making sure the file's time moves on, as two
// writes within one tick of a coarse clock might not.
func (pt *proxyTest) write(t *testing.T, r Rules) {
	t.Helper()
	if err := WriteRules(pt.rules, r); err != nil {
		t.Fatal(err)
	}
	pt.writes++
	later := time.Now().Add(time.Duration(pt.writes) * time.Second)
	os.Chtimes(pt.rules, later, later)
}

func (pt *proxyTest) client() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(pt.url), TLSClientConfig: pt.tlsPool},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 5 * time.Second,
	}
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestProxyModes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rules  Rules
		url    string
		status int
		body   string // a part of the body
	}{
		{"off refuses", Rules{Internet: InternetOff}, "http://example.com/x", 403, "the box has no internet"},
		{"off refuses https", Rules{Internet: InternetOff}, "https://example.com/", 0, "Forbidden"},
		{"on takes http", Rules{Internet: InternetOn}, "http://evil.test/x", 200, "plain evil.test /x"},
		{"on takes https", Rules{Internet: InternetOn}, "https://example.com/", 200, "tls example.com"},
		{"allow takes a listed host", Rules{Internet: InternetAllow, Allow: []string{"proxy.golang.org"}}, "http://proxy.golang.org/m", 200, "plain proxy.golang.org /m"},
		{"allow takes https to a listed host", Rules{Internet: InternetAllow, Allow: []string{"example.com"}}, "https://example.com/", 200, "tls example.com"},
		{"allow refuses another", Rules{Internet: InternetAllow, Allow: []string{"proxy.golang.org"}}, "http://evil.test/", 403, "dax box: evil.test is not allowed (box.allow; -box-internet)"},
		{"allow refuses https to another", Rules{Internet: InternetAllow, Allow: []string{"proxy.golang.org"}}, "https://example.com/", 0, "Forbidden"},
		{"wildcard takes below", Rules{Internet: InternetAllow, Allow: []string{"*.example.com"}}, "http://a.example.com/", 200, "plain a.example.com"},
		{"wildcard refuses the apex", Rules{Internet: InternetAllow, Allow: []string{"*.example.com"}}, "http://example.com/", 403, "example.com is not allowed"},
		{"a port other than 80 and 443", Rules{Internet: InternetOn}, "http://example.com:8080/", 403, "port 8080 is not allowed"},
		{"redirects not followed", Rules{Internet: InternetOn}, "http://example.com/redirect", 302, ""},
		{"upper case host", Rules{Internet: InternetAllow, Allow: []string{"proxy.golang.org"}}, "http://PROXY.Golang.ORG/", 200, "plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pt := newProxyTest(t, tc.rules)
			status, body := get(t, pt.client(), tc.url)
			if status != tc.status || !strings.Contains(body, tc.body) {
				t.Errorf("GET %s = %d %q, want %d and %q", tc.url, status, body, tc.status, tc.body)
			}
			if tc.status == 403 || tc.status == 0 {
				if len(pt.logs) != 1 || !strings.Contains(pt.logs[0], "refused") {
					t.Errorf("refusal logs = %q", pt.logs)
				}
				if len(pt.net.dialed) != 0 {
					t.Errorf("a refused request was dialed: %q", pt.net.dialed)
				}
			}
			if tc.status == 302 && len(pt.net.dialed) != 1 {
				t.Errorf("the redirect was followed: %q", pt.net.dialed)
			}
		})
	}
}

func TestProxyRefusalBodyOverConnect(t *testing.T) {
	pt := newProxyTest(t, Rules{Internet: InternetAllow, Allow: []string{"proxy.golang.org"}})
	conn, err := net.Dial("tcp", pt.url.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, _ := io.ReadAll(conn)
	if !strings.HasPrefix(string(b), "HTTP/1.1 403") || !strings.Contains(string(b), "example.com is not allowed (box.allow; -box-internet)") {
		t.Errorf("CONNECT refusal = %q", b)
	}
}

func TestProxyDropsHopHeaders(t *testing.T) {
	pt := newProxyTest(t, Rules{Internet: InternetOn})
	req, _ := http.NewRequest(http.MethodGet, "http://example.com/headers", nil)
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	req.Header.Set("Connection", "X-Drop")
	req.Header.Set("X-Drop", "1")
	req.Header.Set("X-Keep", "1")
	resp, err := pt.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if got := string(b); got != `auth="" drop="" keep="1"` {
		t.Errorf("upstream saw %s", got)
	}
}

func TestProxyReloadsRules(t *testing.T) {
	pt := newProxyTest(t, Rules{Internet: InternetOff})
	c := pt.client()
	for i, step := range []struct {
		rules  *Rules // nil removes the file
		status int
	}{
		{&Rules{Internet: InternetOff}, 403},
		{&Rules{Internet: InternetAllow, Allow: []string{"evil.test"}}, 200},
		{&Rules{Internet: InternetAllow, Allow: []string{"proxy.golang.org"}}, 403},
		{&Rules{Internet: InternetOn}, 200},
		{nil, 403},
	} {
		if step.rules == nil {
			os.Remove(pt.rules)
		} else {
			pt.write(t, *step.rules)
		}
		if status, body := get(t, c, "http://evil.test/"); status != step.status {
			t.Errorf("step %d: GET = %d %q, want %d", i, status, body, step.status)
		}
	}
	// A file that does not read is no internet.
	if err := os.WriteFile(pt.rules, []byte(`{"internet":"sideways"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	os.Chtimes(pt.rules, later, later)
	if status, _ := get(t, c, "http://evil.test/"); status != 403 {
		t.Errorf("bad rules: GET = %d, want 403", status)
	}
}

func TestProxyHealth(t *testing.T) {
	pt := newProxyTest(t, Rules{Internet: InternetOff})
	if err := CheckProxy(context.Background(), pt.url.Host); err != nil {
		t.Errorf("CheckProxy: %v", err)
	}
	status, _ := get(t, http.DefaultClient, pt.url.String()+"/other")
	if status != http.StatusBadRequest {
		t.Errorf("a request that is not a proxy's = %d", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := CheckProxy(ctx, "127.0.0.1:1"); err == nil {
		t.Error("CheckProxy of nothing succeeded")
	}
}

// The production dialer refuses this machine and private networks on
// the address a name resolves to, before it connects: no rule opens
// them, however the destination is named.
func TestProxyRefusesLocalAddresses(t *testing.T) {
	path := filepath.Join(t.TempDir(), RulesFile)
	if err := WriteRules(path, Rules{Internet: InternetOn}); err != nil {
		t.Fatal(err)
	}
	var logs []string
	p := NewProxy(path, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	srv := httptest.NewServer(p)
	defer srv.Close()
	pu, _ := url.Parse(srv.URL)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 5 * time.Second}
	for _, u := range []string{"http://127.0.0.1/", "http://localhost/", "http://[::1]/", "http://169.254.169.254/latest/meta-data/", "http://10.0.0.1/"} {
		status, body := get(t, c, u)
		if status != 403 || !strings.Contains(body, "not a public address") {
			t.Errorf("GET %s = %d %q, want 403 naming a public address", u, status, body)
		}
	}
	conn, err := net.Dial("tcp", pu.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT localhost:443 HTTP/1.1\r\nHost: localhost:443\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, _ := io.ReadAll(conn)
	if !strings.HasPrefix(string(b), "HTTP/1.1 403") {
		t.Errorf("CONNECT localhost:443 = %q", b)
	}
	if err := checkDial("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("checkDial of a public address: %v", err)
	}
}
