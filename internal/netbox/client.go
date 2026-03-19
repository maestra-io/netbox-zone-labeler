package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const (
	maxRetries     = 3
	initialBackoff = 500 * time.Millisecond
)

var (
	errNotFound = errors.New("device not found in netbox")
	errNoRack   = errors.New("device has no rack assigned")
)

// IsNotFound reports whether the error indicates a device was not found in NetBox.
func IsNotFound(err error) bool {
	return errors.Is(err, errNotFound)
}

// IsNoRack reports whether the error indicates a device has no rack assigned.
func IsNoRack(err error) bool {
	return errors.Is(err, errNoRack)
}

type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

type deviceListResponse struct {
	Count   int      `json:"count"`
	Results []Device `json:"results"`
}

type Device struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Rack *Rack  `json:"rack"`
}

type Rack struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// GetDeviceRack returns the rack name for a device identified by hostname.
// It retries transient errors with exponential backoff but does not retry
// "not found" or "no rack" errors.
func (c *Client) GetDeviceRack(ctx context.Context, hostname string) (string, error) {
	var lastErr error
	for attempt := range maxRetries + 1 {
		if attempt > 0 {
			backoff := initialBackoff * time.Duration(1<<(attempt-1))
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoff):
			}
		}

		rack, err := c.getDeviceRack(ctx, hostname)
		if err == nil {
			return rack, nil
		}
		if errors.Is(err, errNotFound) || errors.Is(err, errNoRack) {
			return "", err
		}
		lastErr = err
	}
	return "", fmt.Errorf("after %d retries: %w", maxRetries, lastErr)
}

func (c *Client) getDeviceRack(ctx context.Context, hostname string) (string, error) {
	u, err := url.Parse(c.baseURL + "/api/dcim/devices/")
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	q := u.Query()
	q.Set("name", hostname)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request netbox: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("netbox returned status %d", resp.StatusCode)
	}

	var result deviceListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if result.Count == 0 {
		return "", fmt.Errorf("device %q: %w", hostname, errNotFound)
	}

	device := result.Results[0]
	if device.Rack == nil {
		return "", fmt.Errorf("device %q: %w", hostname, errNoRack)
	}

	return device.Rack.Name, nil
}
