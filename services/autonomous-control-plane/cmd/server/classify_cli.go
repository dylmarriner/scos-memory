package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// classifyStream reads one RouteRequest JSON object per line and writes the
// RouteDecision buildRoute produces for it. A malformed line or a request
// buildRoute rejects is an error: the caller is measuring the router and
// must not receive a partial answer set.
func classifyStream(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	enc := json.NewEncoder(out)
	line := 0
	for scanner.Scan() {
		line++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		var req RouteRequest
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			return fmt.Errorf("line %d: invalid route request: %w", line, err)
		}
		decision, err := buildRoute(req)
		if err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		if err := enc.Encode(decision); err != nil {
			return err
		}
	}
	return scanner.Err()
}
