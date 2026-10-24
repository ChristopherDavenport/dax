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

// Relay reads the session's initialize from in, takes from it, under
// key in the client's experimental capabilities, the box the session
// asks for, has start make it and start its executor, and from then on
// copies in to the executor and the executor to out, byte for byte: no
// message is rewritten, reordered or answered here but the first. A
// box start refuses is the initialize's JSON-RPC error, with the
// reason, and Relay's error. Relay returns when the executor exits,
// with its error; when in ends, the executor's input is closed and
// Relay waits for it.
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
			Capabilities struct {
				Experimental map[string]json.RawMessage `json:"experimental"`
			} `json:"capabilities"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return fmt.Errorf("box host: the session's first message is not JSON-RPC: %w", err)
	}
	if msg.Method != "initialize" || len(msg.ID) == 0 {
		err := fmt.Errorf("box host: the session's first message is %q, not initialize", msg.Method)
		refuse(out, msg.ID, err)
		return err
	}
	var asked struct {
		Box *Request `json:"box"`
	}
	if raw := msg.Params.Capabilities.Experimental[key]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &asked); err != nil {
			err = fmt.Errorf("box host: the session's box request: %w", err)
			refuse(out, msg.ID, err)
			return err
		}
	}
	var r Request
	if asked.Box != nil {
		r = *asked.Box
	}
	child, err := start(ctx, r)
	if err != nil {
		err = fmt.Errorf("box host: %w", err)
		refuse(out, msg.ID, err)
		return err
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
