package failover

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

type HTTPHealthChecker struct {
	client *http.Client
}

func NewHTTPHealthChecker(timeout time.Duration) *HTTPHealthChecker {
	return &HTTPHealthChecker{client: &http.Client{Timeout: timeout}}
}

func (c *HTTPHealthChecker) Healthy(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected health status: %d", resp.StatusCode)
	}
	return nil
}
