package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/agentnet/agentnet/internal/canonical"
	"os"
)

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		raw := scanner.Bytes()
		b, err := canonical.Bytes(raw)
		result := map[string]any{"ok": err == nil}
		if err == nil {
			digest, _ := canonical.Digest(raw)
			result["canonical"] = string(b)
			result["digest"] = digest
		}
		out, _ := json.Marshal(result)
		fmt.Println(string(out))
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
