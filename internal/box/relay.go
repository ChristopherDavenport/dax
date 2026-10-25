package box

// This file is the box host's relay: the session's executor protocol
// passed through to the executor in the box, a message at a time, but
// for the first, which says which box to make, the handshake's answers,
// which gain what the box host resolved, and the process methods of the
// session's MCP units' servers, which the box host answers itself.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

// Child is the executor the relay passes the session through to: its
// standard input and output, and its end.
type Child interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Wait() error
}

// Hosted is what the box a session asks for gives the relay: the
// executor in the box, what the box host tells the session it resolved
// (added under the capability key, as "box", to the executor's answer
// to the handshake), the session's MCP units by name, and how to start
// a unit's server.
type Hosted struct {
	Child Child
	Info  any
	Units []string
	// StartUnit starts unit's server, its standard error kept when
	// stderr; nil when the session has no units.
	StartUnit func(ctx context.Context, unit string, stderr bool) (Proc, error)
}

// Start makes the box r asks for and starts its executor.
type Start func(ctx context.Context, r Request) (Hosted, error)

// AdminPrefix begins the methods of the box host's own requests: a
// first message of one of these is answered by the box host, which
// then exits, in place of a session.
const AdminPrefix = "dax/box."

// The box host's own requests.
const (
	// MethodStatus asks for the project's box, its units and its
	// checkout.
	MethodStatus = AdminPrefix + "status"
	// MethodDown removes the project's box and its units; {"state": true}
	// their state directories too.
	MethodDown = AdminPrefix + "down"
	// MethodPull asks for a bundle of the project's head.
	MethodPull = AdminPrefix + "pull"
)

// Admin answers one of the box host's own requests, method with its
// params; it acts on the box host's one project and no other.
type Admin func(ctx context.Context, method string, params json.RawMessage) (any, error)

// RelayOption is an option of Relay's.
type RelayOption func(*relayOptions)

type relayOptions struct{ admin Admin }

// WithAdmin has Relay answer a first message whose method begins with
// AdminPrefix through a, and return: the session is that one request.
func WithAdmin(a Admin) RelayOption { return func(o *relayOptions) { o.admin = a } }

// maxFirstLine bounds the session's first message, its initialize.
const maxFirstLine = 1 << 20

// maxLine bounds every later message: a process write's base64 data,
// at most 1 MiB raw, and room.
const maxLine = 4 << 20

// clientCapabilitiesMeta is where server/discover carries the client's
// capabilities: in its parameters' _meta, under MCP's key.
const clientCapabilitiesMeta = "io.modelcontextprotocol/clientCapabilities"

// Relay reads the session's first message from in, its handshake:
// initialize, or the stateless server/discover a newer client tries
// first. It takes from it, under key in the client's experimental
// capabilities, the box the session asks for, has start make it and
// start its executor, and from then on passes each message, a line at
// a time, between in and the executor and the executor and out, as it
// came, but for two kinds: the executor's answers to the handshake
// (initialize and server/discover, each try) gain the box host's Info
// under key, as "box"; and a process method for a unit's server (an
// agentworkspace/process.start of ["dax-unit", NAME], and every later
// method on the id it gave) is the box host's, answered here, never sent on. A
// box start refuses is the first message's JSON-RPC error, with the
// reason, and so is every request after it until in ends (a client
// whose discover is refused tries initialize next, and must read why
// too); then it is Relay's error. Relay returns when the executor
// exits, with its error; when in ends, the executor's input is closed
// and Relay waits for it. Either way the units' servers end.
func Relay(ctx context.Context, in io.Reader, out io.Writer, key string, start Start, opts ...RelayOption) error {
	var o relayOptions
	for _, opt := range opts {
		opt(&o)
	}
	br := bufio.NewReaderSize(in, 64<<10)
	line, err := readLine(br, maxFirstLine)
	if err != nil {
		return fmt.Errorf("box host: the session's first message: %w", err)
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Capabilities *capabilities              `json:"capabilities"`
			Meta         map[string]json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return fmt.Errorf("box host: the session's first message is not JSON-RPC: %w", err)
	}
	if strings.HasPrefix(msg.Method, AdminPrefix) {
		return admin(ctx, out, o.admin, line, msg.ID, msg.Method)
	}
	var caps *capabilities
	switch msg.Method {
	case "initialize":
		caps = msg.Params.Capabilities
	case "server/discover":
		if raw := msg.Params.Meta[clientCapabilitiesMeta]; len(raw) > 0 {
			caps = new(capabilities)
			if err := json.Unmarshal(raw, caps); err != nil {
				return refuseAll(br, out, msg.ID, fmt.Errorf("box host: the session's capabilities: %w", err))
			}
		}
	}
	if (msg.Method != "initialize" && msg.Method != "server/discover") || len(msg.ID) == 0 {
		return refuseAll(br, out, msg.ID, fmt.Errorf("box host: the session's first message is %q, not initialize or server/discover", msg.Method))
	}
	var asked struct {
		Box *Request `json:"box"`
	}
	if caps != nil {
		if raw := caps.Experimental[key]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &asked); err != nil {
				return refuseAll(br, out, msg.ID, fmt.Errorf("box host: the session's box request: %w", err))
			}
		}
	}
	var r Request
	if asked.Box != nil {
		r = *asked.Box
	}
	h, err := start(ctx, r)
	if err != nil {
		return refuseAll(br, out, msg.ID, fmt.Errorf("box host: %w", err))
	}
	rl := &relay{out: out, key: key, units: h.Units, handshake: map[string]bool{idKey(msg.ID): true}, inflight: map[string]context.CancelFunc{}}
	if h.Info != nil {
		if rl.info, err = json.Marshal(h.Info); err != nil {
			return fmt.Errorf("box host: %w", err)
		}
	}
	start2 := h.StartUnit
	if start2 == nil {
		start2 = func(context.Context, string, bool) (Proc, error) { return nil, errors.New("this session has no units") }
	}
	rl.procs = newHostProcs(start2)
	defer rl.procs.close()
	child := h.Child
	stdin := child.Stdin()
	if _, err := stdin.Write(line); err != nil {
		stdin.Close()
		return errors.Join(fmt.Errorf("box host: the executor: %w", err), child.Wait())
	}
	go func() {
		// The rest of the session, as it comes; its end is the
		// executor's input's, and the units' servers'.
		rl.fromSession(br, stdin)
		stdin.Close()
		rl.procs.close()
	}()
	copyErr := rl.fromExecutor(child.Stdout())
	if err := child.Wait(); err != nil {
		return fmt.Errorf("box host: the executor: %w", err)
	}
	if copyErr != nil {
		return fmt.Errorf("box host: to the session: %w", copyErr)
	}
	return nil
}

// admin answers the box host's own request, line, through a, and
// returns: a request answers, or is refused, and nothing follows it. A
// refusal is the request's answer, which the asker prints; it is not
// the box host's error too, which would print it twice.
func admin(ctx context.Context, out io.Writer, a Admin, line []byte, id json.RawMessage, method string) error {
	w := plainWriter{out}
	if len(id) == 0 {
		return fmt.Errorf("box host: %s without an id", method)
	}
	if a == nil {
		refuse(w, id, fmt.Errorf("box host: %s: this box host takes no requests of its own", method))
		return nil
	}
	var m struct {
		Params json.RawMessage `json:"params"`
	}
	json.Unmarshal(line, &m)
	res, err := a(ctx, method, m.Params)
	if err != nil {
		refuse(w, id, fmt.Errorf("box host: %w", err))
		return nil
	}
	b, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", id, res})
	if err != nil {
		refuse(w, id, err)
		return err
	}
	return w.write(append(b, '\n'))
}

// relay is one session's relay once its box is made.
type relay struct {
	key   string
	info  json.RawMessage // what the handshake's answers gain, nil for nothing
	units []string
	procs *hostProcs

	outMu sync.Mutex // one message to the session at a time
	out   io.Writer

	mu        sync.Mutex
	handshake map[string]bool               // the ids of the handshake's tries
	inflight  map[string]context.CancelFunc // the box host's requests in flight, by id
}

// write sends one message, a whole line, to the session.
func (rl *relay) write(line []byte) error {
	rl.outMu.Lock()
	defer rl.outMu.Unlock()
	_, err := rl.out.Write(line)
	return err
}

// fromSession passes the session's messages to the executor, but those
// that are the box host's.
func (rl *relay) fromSession(br *bufio.Reader, stdin io.Writer) {
	for {
		line, err := readLine(br, maxLine)
		if err != nil {
			return
		}
		if rl.own(line) {
			continue
		}
		if _, err := stdin.Write(line); err != nil {
			return
		}
	}
}

// own reports whether line is the box host's to answer, and answers it
// if so: a process method of a unit's server, or the cancelling of one.
// It notes a handshake's try on the way.
func (rl *relay) own(line []byte) bool {
	if !bytes.Contains(line, []byte(`"method"`)) {
		return false
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(line, &m) != nil {
		return false
	}
	switch {
	case m.Method == "initialize" || m.Method == "server/discover":
		if len(m.ID) > 0 {
			rl.mu.Lock()
			rl.handshake[idKey(m.ID)] = true
			rl.mu.Unlock()
		}
		return false
	case m.Method == "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(m.Params, &p) != nil || len(p.RequestID) == 0 {
			return false
		}
		rl.mu.Lock()
		cancel := rl.inflight[idKey(p.RequestID)]
		rl.mu.Unlock()
		if cancel == nil {
			return false
		}
		cancel()
		return true
	case !strings.HasPrefix(m.Method, methodPrefix) || len(m.ID) == 0:
		return false
	case m.Method == methodStart:
		var p struct {
			Args []string `json:"args"`
		}
		if json.Unmarshal(m.Params, &p) != nil || len(p.Args) == 0 || p.Args[0] != UnitCommand {
			return false
		}
		if len(p.Args) != 2 || !slices.Contains(rl.units, p.Args[1]) {
			have := "none"
			if len(rl.units) > 0 {
				have = strings.Join(rl.units, ", ")
			}
			rl.answer(m.ID, nil, fmt.Errorf("%s %s: not one of this session's units (%s)", UnitCommand, strings.Join(p.Args[1:], " "), have))
			return true
		}
	default:
		var p struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(m.Params, &p) != nil || !isHostID(p.ID) {
			return false
		}
	}
	ctx, cancel := context.WithCancel(rl.procs.ctx)
	key := idKey(m.ID)
	rl.mu.Lock()
	rl.inflight[key] = cancel
	rl.mu.Unlock()
	go func() {
		defer func() {
			rl.mu.Lock()
			delete(rl.inflight, key)
			rl.mu.Unlock()
			cancel()
		}()
		res, err := rl.procs.serve(ctx, m.Method, m.Params)
		rl.answer(m.ID, res, err)
	}()
	return true
}

// answer sends the box host's answer to request id.
func (rl *relay) answer(id json.RawMessage, result any, err error) {
	if err != nil {
		refuse(rl, id, err)
		return
	}
	b, merr := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", id, result})
	if merr != nil {
		refuse(rl, id, merr)
		return
	}
	rl.write(append(b, '\n'))
}

// fromExecutor passes the executor's messages to the session, a line
// at a time, the handshake's answers with the box host's Info added.
func (rl *relay) fromExecutor(r io.Reader) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br, maxLine)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if werr := rl.write(rl.rewrite(line)); werr != nil {
			// The session is gone; the executor's end is its input's.
			io.Copy(io.Discard, br)
			return werr
		}
	}
}

// rewrite is line, or, when it answers a try of the handshake with a
// result, that result with "box" set to Info under the capability key.
func (rl *relay) rewrite(line []byte) []byte {
	rl.mu.Lock()
	pending := len(rl.handshake) > 0
	rl.mu.Unlock()
	if !pending || rl.info == nil || !bytes.Contains(line, []byte(`"result"`)) {
		return line
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(line, &m) != nil {
		return line
	}
	key := idKey(m["id"])
	rl.mu.Lock()
	ours := rl.handshake[key]
	delete(rl.handshake, key)
	rl.mu.Unlock()
	if !ours {
		return line
	}
	result, ok := setPath(m["result"], rl.info, "capabilities", "experimental", rl.key, "box")
	if !ok {
		return line
	}
	m["result"] = result
	b, err := json.Marshal(m)
	if err != nil {
		return line
	}
	return append(b, '\n')
}

// setPath is the JSON object raw with the member at path set to v, each
// object on the way there already; false when one is not.
func setPath(raw json.RawMessage, v json.RawMessage, path ...string) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, false
	}
	if len(path) == 1 {
		obj[path[0]] = v
	} else {
		inner, ok := setPath(obj[path[0]], v, path[1:]...)
		if !ok {
			return nil, false
		}
		obj[path[0]] = inner
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return b, true
}

// idKey is a JSON-RPC id as a map key: its JSON, without the space
// around it.
func idKey(id json.RawMessage) string { return string(bytes.TrimSpace(id)) }

// capabilities is the part of a client's capabilities the relay reads.
type capabilities struct {
	Experimental map[string]json.RawMessage `json:"experimental"`
}

// refuseAll answers id, and every request that follows on br until it
// ends, with err, and returns err. It answers no response and no
// notification.
func refuseAll(br *bufio.Reader, out io.Writer, id json.RawMessage, err error) error {
	w := plainWriter{out}
	refuse(w, id, err)
	for {
		line, rerr := readLine(br, maxLine)
		if rerr != nil {
			return err
		}
		var next struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		// A request, not a response or a notification.
		if json.Unmarshal(line, &next) == nil && next.Method != "" {
			refuse(w, next.ID, err)
		}
	}
}

// readLine is one newline-ended line of br, with its newline, at most
// limit bytes.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		part, err := br.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > limit {
			return nil, fmt.Errorf("longer than %d bytes", limit)
		}
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return nil, errors.New("it ends without a newline")
		default:
			return nil, err
		}
	}
}

// lineWriter sends one whole message.
type lineWriter interface{ write([]byte) error }

// plainWriter is a lineWriter over an io.Writer nothing else writes to.
type plainWriter struct{ w io.Writer }

func (p plainWriter) write(b []byte) error {
	_, err := p.w.Write(b)
	return err
}

// refuse answers the request id with err as a JSON-RPC error, so the
// session reads why rather than a closed pipe.
func refuse(out lineWriter, id json.RawMessage, err error) {
	if len(id) == 0 {
		return
	}
	b, merr := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{Code: -32603, Message: err.Error()}})
	if merr != nil {
		return
	}
	out.write(append(b, '\n'))
}
