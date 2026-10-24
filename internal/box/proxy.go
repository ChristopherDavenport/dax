package box

// This file is a box's egress: the rules a box's internet is held to,
// and the proxy its sidecar runs, the box's only way out.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The modes of a box's internet.
const (
	// InternetOff refuses every request: the box has no internet.
	InternetOff = "off"
	// InternetAllow takes requests to the hosts Rules.Allow names.
	InternetAllow = "allow"
	// InternetOn takes requests to any host.
	InternetOn = "on"
)

// ProxyPort is the port the sidecar's proxy listens on.
const ProxyPort = "3128"

// RulesFile is the rules file's name in a box's state directory, and
// RulesMount where the sidecar sees that directory.
const (
	RulesFile  = "egress.json"
	RulesMount = "/etc/dax-box"
)

// Rules are what a box's internet is held to. Whatever the mode, a
// destination that is this machine, a private network, a link-local
// address (a cloud's metadata service among them) or not a public
// unicast address is refused, and only ports 80 and 443 are reached.
type Rules struct {
	Internet string `json:"internet"`
	// Allow, in mode allow, are the hosts taken: a host name, or *.domain
	// for every name below domain (not domain itself).
	Allow []string `json:"allow,omitempty"`
}

// allows reports whether r takes host, a name or an address, lower case.
func (r Rules) allows(host string) bool {
	switch r.Internet {
	case InternetOn:
		return true
	case InternetAllow:
		for _, a := range r.Allow {
			a = strings.ToLower(a)
			if d, ok := strings.CutPrefix(a, "*."); ok {
				if strings.HasSuffix(host, "."+d) {
					return true
				}
			} else if host == a {
				return true
			}
		}
	}
	return false
}

// Same reports whether r and o take the same requests.
func (r Rules) Same(o Rules) bool {
	if r.Internet != o.Internet {
		return false
	}
	if r.Internet != InternetAllow {
		return true
	}
	return strings.Join(r.Allow, "\n") == strings.Join(o.Allow, "\n")
}

// String is r as a status line names it: off, on, or allow and its
// hosts.
func (r Rules) String() string {
	if r.Internet == InternetAllow {
		return "allow " + strings.Join(r.Allow, ",")
	}
	return r.Internet
}

// ReadRules reads the rules file at path. A file that is not there, or
// that does not parse, is no internet: the proxy fails closed.
func ReadRules(path string) (Rules, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Rules{Internet: InternetOff}, err
	}
	var r Rules
	if err := json.Unmarshal(b, &r); err != nil {
		return Rules{Internet: InternetOff}, fmt.Errorf("%s: %w", path, err)
	}
	switch r.Internet {
	case InternetOff, InternetOn, InternetAllow:
	default:
		return Rules{Internet: InternetOff}, fmt.Errorf("%s: internet %q: want off, on or allow", path, r.Internet)
	}
	return r, nil
}

// WriteRules writes r to path, through a file renamed into place, so
// the proxy never reads half of it and a directory mounted elsewhere
// sees the new file.
func WriteRules(path string, r Rules) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".egress-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// errBlocked is a destination no rule may open: not a public unicast
// address.
var errBlocked = errors.New("not a public address")

// blocked reports whether ip is one a box may never reach: loopback,
// private (RFC 1918, unique local), link-local, shared (CGNAT,
// 100.64.0.0/10), multicast, unspecified, or otherwise not global
// unicast.
func blocked(ip netip.Addr) bool {
	ip = ip.Unmap()
	return !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() ||
		ip.IsUnspecified() || !ip.IsGlobalUnicast() || cgnat.Contains(ip)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// checkDial is a net.Dialer's Control: it refuses a connection to an
// address blocked names, on the address dialed, after the name was
// resolved, so a name that resolves to this machine or a private
// network is refused however it was asked for.
func checkDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || blocked(ip) {
		return fmt.Errorf("%s: %w", host, errBlocked)
	}
	return nil
}

// Proxy is a box's egress proxy: HTTPS through CONNECT and HTTP through
// absolute-URI requests, to the hosts the rules file takes, on ports 80
// and 443. It reads the file again whenever it changes, so a session
// changes the box's internet without making anything again. It answers
// GET /healthz for the box's check.
type Proxy struct {
	rulesPath string
	logf      func(format string, args ...any)

	// dial and ports are fixed by NewProxy; the package's tests set
	// them, to reach servers on loopback.
	dial  func(ctx context.Context, network, address string) (net.Conn, error)
	ports map[string]bool
	tr    *http.Transport

	mu    sync.Mutex
	stamp string // the rules file's size and time when read
	rules Rules
}

// NewProxy is a Proxy under the rules file at rulesPath, logging each
// refusal through logf.
func NewProxy(rulesPath string, logf func(format string, args ...any)) *Proxy {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: checkDial}
	p := &Proxy{rulesPath: rulesPath, logf: logf, dial: d.DialContext, ports: map[string]bool{"80": true, "443": true}}
	p.tr = &http.Transport{
		Proxy:                 nil,
		DialContext:           func(ctx context.Context, network, addr string) (net.Conn, error) { return p.dial(ctx, network, addr) },
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: time.Minute,
	}
	return p
}

// current is the rules as the file says now.
func (p *Proxy) current() Rules {
	p.mu.Lock()
	defer p.mu.Unlock()
	stamp := "none"
	if fi, err := os.Stat(p.rulesPath); err == nil {
		stamp = fmt.Sprintf("%d-%d", fi.Size(), fi.ModTime().UnixNano())
	}
	if stamp != p.stamp || p.stamp == "" {
		r, err := ReadRules(p.rulesPath)
		if err != nil {
			p.logf("dax box proxy: rules: %v; no internet until they read", err)
		}
		p.stamp, p.rules = stamp, r
	}
	return p.rules
}

// hopHeaders are a connection's own, never forwarded.
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func dropHop(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, k := range strings.Split(f, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect && !r.URL.IsAbs() {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			io.WriteString(w, "ok\n")
			return
		}
		http.Error(w, "dax box: a proxy: send an absolute URL or CONNECT", http.StatusBadRequest)
		return
	}
	hostport := r.Host
	if r.Method != http.MethodConnect {
		if r.URL.Scheme != "http" {
			p.refuse(w, r.URL.Host, fmt.Sprintf("dax box: scheme %s: only http here, and https through CONNECT", r.URL.Scheme))
			return
		}
		hostport = r.URL.Host
		if r.URL.Port() == "" {
			hostport = net.JoinHostPort(r.URL.Hostname(), "80")
		}
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		http.Error(w, "dax box: "+err.Error(), http.StatusBadRequest)
		return
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	rules := p.current()
	switch {
	case rules.Internet == InternetOff:
		p.refuse(w, hostport, fmt.Sprintf("dax box: %s is not allowed: the box has no internet (box.internet; -box-internet)", host))
		return
	case !rules.allows(host):
		p.refuse(w, hostport, fmt.Sprintf("dax box: %s is not allowed (box.allow; -box-internet)", host))
		return
	case !p.ports[port]:
		p.refuse(w, hostport, fmt.Sprintf("dax box: port %s is not allowed: only 80 and 443", port))
		return
	}
	// The destination is the host as checked, lower case, whatever
	// spelling the client sent.
	dest := net.JoinHostPort(host, port)
	if r.Method == http.MethodConnect {
		p.tunnel(w, r, dest)
		return
	}
	p.forward(w, r, dest)
}

// refuse answers 403 with msg and logs it.
func (p *Proxy) refuse(w http.ResponseWriter, dest, msg string) {
	p.logf("dax box proxy: refused %s: %s", dest, msg)
	http.Error(w, msg, http.StatusForbidden)
}

// dialFailed answers a request whose destination could not be reached:
// 403 when it is an address no rule may open, else 502.
func (p *Proxy) dialFailed(w http.ResponseWriter, dest string, err error) {
	if errors.Is(err, errBlocked) {
		host, _, _ := net.SplitHostPort(dest)
		p.refuse(w, dest, fmt.Sprintf("dax box: %s is not allowed: it is not a public address (this machine, a private network or a link-local one)", host))
		return
	}
	http.Error(w, "dax box: "+err.Error(), http.StatusBadGateway)
}

func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request, dest string) {
	up, err := p.dial(r.Context(), "tcp", dest)
	if err != nil {
		p.dialFailed(w, dest, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "dax box: cannot tunnel", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := io.WriteString(down, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		up.Close()
		down.Close()
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		// What the client sent behind the CONNECT is already buffered.
		if n := buf.Reader.Buffered(); n > 0 {
			b, _ := buf.Reader.Peek(n)
			up.Write(b)
		}
		io.Copy(up, down)
		closeWrite(up)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(down, up)
		closeWrite(down)
		done <- struct{}{}
	}()
	<-done
	<-done
	up.Close()
	down.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, dest string) {
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Host = dest
	dropHop(out.Header)
	resp, err := p.tr.RoundTrip(out)
	if err != nil {
		p.dialFailed(w, dest, err)
		return
	}
	defer resp.Body.Close()
	dropHop(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// CheckProxy asks the proxy at addr whether it is up, retrying for a few
// seconds while it starts. It goes to addr directly, whatever proxy the
// environment names.
func CheckProxy(ctx context.Context, addr string) error {
	c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	var last error
	for i := 0; i < 25; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
		if err != nil {
			return err
		}
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("%s answered %s", addr, resp.Status)
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("the box's proxy at %s: %w", addr, last)
}

// ServeProxy serves p at addr until ctx ends.
func ServeProxy(ctx context.Context, addr string, p *Proxy) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
