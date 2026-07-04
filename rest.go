package supabase_realtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// QueryTable performs a filtered REST query
func (c *Client) QueryTable(table string, dest any, filters map[string]any) (int, error) {
	if c == nil {
		return 0, fmt.Errorf("realtime client is nil")
	}

	url := fmt.Sprintf("%s/%s?select=*", c.RestUrl, table)

	first := true
	for k, v := range filters {
		if !first {
			url += "&"
		}
		first = false
		url += fmt.Sprintf("%s=eq.%v", k, v)
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("apikey", c.ApiKey)
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.ApiKey))
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to query table: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(bytes.NewReader(body)).Decode(dest); err != nil {
		return 0, fmt.Errorf("failed to decode response: %w", err)
	}

	return len(body), nil
}
