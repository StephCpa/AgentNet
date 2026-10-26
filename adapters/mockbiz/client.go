package mockbiz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/agentnet/agentnet/services/execution"
)

// Client is only for the loopback faultlab server. Production targets require
// authenticated endpoint bindings and their own business-evidence validation.
type Client struct {
	URL  string
	HTTP *http.Client
}

func (c Client) Send(ctx context.Context, r execution.Request) (execution.Observation, error) {
	return c.call(ctx, "/send", r)
}
func (c Client) Query(ctx context.Context, r execution.Request) (execution.Observation, error) {
	return c.call(ctx, "/query", r)
}
func (c Client) call(ctx context.Context, path string, r execution.Request) (execution.Observation, error) {
	var o execution.Observation
	b, err := json.Marshal(r)
	if err != nil {
		return o, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+path, bytes.NewReader(b))
	if err != nil {
		return o, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return o, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return o, fmt.Errorf("mockbiz HTTP status %d", response.StatusCode)
	}
	b, err = io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return o, err
	}
	if len(b) > 1<<20 {
		return o, execution.ErrInvalid
	}
	err = json.Unmarshal(b, &o)
	return o, err
}
