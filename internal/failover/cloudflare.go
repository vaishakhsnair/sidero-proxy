package failover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type CloudflareClient struct {
	apiToken string
	baseURL  string
	client   *http.Client
}

func NewCloudflareClient(apiToken string) *CloudflareClient {
	baseURL := os.Getenv("API_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.cloudflare.com/client/v4"
	}
	return &CloudflareClient{
		apiToken: apiToken,
		baseURL:  baseURL,
		client:   &http.Client{},
	}
}

type cfResponse[T any] struct {
	Success bool `json:"success"`
	Result  T    `json:"result"`
}

type cfRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
}

func (c *CloudflareClient) ListARecords(ctx context.Context, zoneID, name string) ([]DNSRecord, error) {
	values := url.Values{}
	values.Set("type", "A")
	values.Set("name", name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/zones/%s/dns_records?%s", c.baseURL, zoneID, values.Encode()), nil)
	if err != nil {
		return nil, err
	}
	var resp cfResponse[[]cfRecord]
	if err := c.do(req, &resp); err != nil {
		return nil, err
	}
	records := make([]DNSRecord, 0, len(resp.Result))
	for _, record := range resp.Result {
		records = append(records, DNSRecord{
			ID:      record.ID,
			Name:    record.Name,
			Content: record.Content,
			TTL:     record.TTL,
			Type:    record.Type,
		})
	}
	return records, nil
}

func (c *CloudflareClient) DeleteRecord(ctx context.Context, zoneID, recordID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("%s/zones/%s/dns_records/%s", c.baseURL, zoneID, recordID), nil)
	if err != nil {
		return err
	}
	var resp cfResponse[cfRecord]
	return c.do(req, &resp)
}

func (c *CloudflareClient) CreateARecord(ctx context.Context, zoneID, name, content string, ttl int) error {
	body, err := json.Marshal(map[string]any{
		"type":    "A",
		"name":    name,
		"content": content,
		"ttl":     ttl,
		"proxied": false,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/zones/%s/dns_records", c.baseURL, zoneID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	var resp cfResponse[cfRecord]
	return c.do(req, &resp)
}

func (c *CloudflareClient) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("cloudflare api %s %s failed: %s", req.Method, req.URL, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode cloudflare response: %w", err)
	}
	return nil
}
