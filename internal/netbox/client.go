package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

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
func (c *Client) GetDeviceRack(ctx context.Context, hostname string) (string, error) {
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
		return "", fmt.Errorf("device %q not found in netbox", hostname)
	}

	device := result.Results[0]
	if device.Rack == nil {
		return "", fmt.Errorf("device %q has no rack assigned", hostname)
	}

	return device.Rack.Name, nil
}
