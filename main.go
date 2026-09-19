package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/zhoudm1743/kimi-ssh/internal/mcp"
)

// maxFrameBytes caps a single JSON-RPC frame. Real frames are a few kilobytes;
// the ceiling only keeps a broken stream from growing without bound.
const maxFrameBytes = 8 << 20

func main() {
	if err := serve(); err != nil {
		fmt.Fprintf(os.Stderr, "kimi-ssh: %v\n", err)
		os.Exit(1)
	}
}

// serve runs the stdio transport: one JSON object per input line, one reply per
// request on stdout. Everything that is not a protocol frame goes to stderr so
// the client never has to skip noise on stdout.
func serve() error {
	server := mcp.NewServer()

	input := bufio.NewScanner(os.Stdin)
	input.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)

	output := bufio.NewWriter(os.Stdout)
	defer output.Flush()
	encoder := json.NewEncoder(output)

	for input.Scan() {
		frame := bytes.TrimSpace(input.Bytes())
		if len(frame) == 0 {
			continue
		}

		var request mcp.Request
		if err := json.Unmarshal(frame, &request); err != nil {
			// Nothing usable arrived, and a frame without an id has no reply
			// slot, so report it out of band and keep reading.
			fmt.Fprintf(os.Stderr, "kimi-ssh: dropping unparsable frame: %v\n", err)
			continue
		}

		if response := server.HandleRequest(request); response != nil {
			if err := encoder.Encode(response); err != nil {
				return fmt.Errorf("writing response: %w", err)
			}
			if err := output.Flush(); err != nil {
				return fmt.Errorf("flushing response: %w", err)
			}
		}

		if request.Method == "shutdown" {
			return nil
		}
	}

	return input.Err()
}
