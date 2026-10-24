package box

// This file is the box host's relay: the session's executor protocol
// passed through to the executor in the box, unchanged but for the
// first message, which says which box to make.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Child is the executor the relay passes the session through to: its
// standard input and output, and its end.
type Child interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Wait() error
}

// Start makes the box r asks for and starts its executor.
type Start func(ctx context.Context, r Request) (Child, error)

// maxFirstLine bounds the session's first message, its initialize.
const maxFirstLine = 1 << 20

// clientCapabilitiesMeta is where server/discover carries the client's
// capabilities: in its parameters' _meta, under MCP's key.
const clientCapabilitiesMeta = "io.modelcontextprotocol/clientCapabilities"

// Relay reads the session's first message from in, its handshake:
// initialize, or the stateless server/discover a newer client tries
// first. It takes from it, under key in the client's experimental
// capabilities, the box the session asks for, has start make it and
// start its executor, and from then on copies in to the executor and
// the executor to out, byte for byte: no message is rewritten,
// reordered or answered here but the first. A box start refuses is the
// first message's JSON-RPC error, with the reason, and so is every
// request after it until in ends (a client whose discover is refused
// tries initialize next, and must read why too); then it is Relay's
// error. Relay returns when the executor exits, with its error; when
// in ends, the executor's input is closed and Relay waits for it.
func Relay(ctx context.Context, in io.Reader, out io.Writer, key string, start Start) error {
	br := bufio.NewReaderSize(in, 64<<10)
	line, err := readLine(br)
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
	child, err := start(ctx, r)
	if err != nil {
		return refuseAll(br, out, msg.ID, fmt.Errorf("box host: %w", err))
	}
	stdin := child.Stdin()
	if _, err := stdin.Write(line); err != nil {
		stdin.Close()
		return errors.Join(fmt.Errorf("box host: the executor: %w", err), child.Wait())
	}
	go func() {
		// The rest of the session, as it comes; its end is the
		// executor's input's.
		io.Copy(stdin, br)
		stdin.Close()
	}()
	_, copyErr := io.Copy(out, child.Stdout())
	if err := child.Wait(); err != nil {
		return fmt.Errorf("box host: the executor: %w", err)
	}
	if copyErr != nil {
		return fmt.Errorf("box host: to the session: %w", copyErr)
	}
	return nil
}

// capabilities is the part of a client's capabilities the relay reads.
type capabilities struct {
	Experimental map[string]json.RawMessage `json:"experimental"`
}

// refuseAll answers id, and every request that follows on br until it
// ends, with err, and returns err. It answers no response and no
// notification.
func refuseAll(br *bufio.Reader, out io.Writer, id json.RawMessage, err error) error {
	refuse(out, id, err)
	for {
		line, rerr := readLine(br)
		if rerr != nil {
			return err
		}
		var next struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		// A request, not a response or a notification.
		if json.Unmarshal(line, &next) == nil && next.Method != "" {
			refuse(out, next.ID, err)
		}
	}
}

// readLine is one newline-ended line of br, with its newline, at most
// maxFirstLine bytes.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := br.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > maxFirstLine {
			return nil, fmt.Errorf("longer than %d bytes", maxFirstLine)
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

// refuse answers the request id with err as a JSON-RPC error, so the
// session reads why rather than a closed pipe.
func refuse(out io.Writer, id json.RawMessage, err error) {
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
	out.Write(append(b, '\n'))
}
