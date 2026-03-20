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
	errNotFound  = errors.New("device not found in netbox")
	errNoRack    = errors.New("device has no rack assigned")
	errRetryable = errors.New("retryable error")
	errNoHost    = errors.New("VM has no host device assigned")
)

// IsNotFound reports whether the error indicates a device was not found in NetBox.
func IsNotFound(err error) bool {
	return errors.Is(err, errNotFound)
}

// IsNoRack reports whether the error indicates a device has no rack assigned.
func IsNoRack(err error) bool {
	return errors.Is(err, errNoRack)
}

// IsNoHost reports whether the error indicates a VM has no host device assigned.
func IsNoHost(err error) bool {
	return errors.Is(err, errNoHost)
}

// IsRetryable reports whether the error is transient and worth retrying.
func IsRetryable(err error) bool {
	return errors.Is(err, errRetryable)
}

type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient creates a NetBox API client with the given base URL and auth token.
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

type vmListResponse struct {
	Count   int  `json:"count"`
	Results []VM `json:"results"`
}

type VM struct {
	ID     int     `json:"id"`
	Name   string  `json:"name"`
	Device *VMHost `json:"device"`
}

type VMHost struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// GetDeviceRack returns the rack name for a device identified by hostname.
// It first searches in dcim/devices. If the device is not found, it falls back
// to virtualization/virtual-machines and resolves the rack via the VM's host device.
// It retries only transient errors (network errors, 5xx, 429) with exponential
// backoff. Permanent errors (not found, no rack, 4xx, decode errors) are
// returned immediately.
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

		rack, err := c.getRack(ctx, hostname)
		if err == nil {
			return rack, nil
		}
		if !errors.Is(err, errRetryable) {
			return "", err
		}
		lastErr = err
	}
	return "", fmt.Errorf("after %d retries: %w", maxRetries, lastErr)
}

// getRack tries to find the rack first as a device, then as a VM.
func (c *Client) getRack(ctx context.Context, hostname string) (string, error) {
	rack, err := c.getDeviceRack(ctx, hostname)
	if err == nil {
		return rack, nil
	}
	if !errors.Is(err, errNotFound) {
		return "", err
	}

	return c.getVMRack(ctx, hostname)
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
		// Network/transport errors are retryable
		return "", fmt.Errorf("request netbox: %w: %w", errRetryable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		// 5xx and 429 are retryable
		return "", fmt.Errorf("netbox returned status %d: %w", resp.StatusCode, errRetryable)
	}
	if resp.StatusCode != http.StatusOK {
		// Other non-200 (401, 403, 404, etc.) are permanent
		return "", fmt.Errorf("netbox returned status %d", resp.StatusCode)
	}

	var result deviceListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		// Decode errors are permanent (bad response shape)
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(result.Results) == 0 {
		return "", fmt.Errorf("device %q: %w", hostname, errNotFound)
	}

	device := result.Results[0]
	if device.Rack == nil {
		return "", fmt.Errorf("device %q: %w", hostname, errNoRack)
	}

	return device.Rack.Name, nil
}

// getVMRack looks up a virtual machine by name, then resolves the rack
// from its host device.
func (c *Client) getVMRack(ctx context.Context, hostname string) (string, error) {
	deviceID, err := c.getVMHostDeviceID(ctx, hostname)
	if err != nil {
		return "", err
	}

	return c.getDeviceRackByID(ctx, deviceID)
}

// getVMHostDeviceID searches for a VM by name and returns the ID of its host device.
func (c *Client) getVMHostDeviceID(ctx context.Context, hostname string) (int, error) {
	u, err := url.Parse(c.baseURL + "/api/virtualization/virtual-machines/")
	if err != nil {
		return 0, fmt.Errorf("parse url: %w", err)
	}
	q := u.Query()
	q.Set("name", hostname)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request netbox: %w: %w", errRetryable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return 0, fmt.Errorf("netbox returned status %d: %w", resp.StatusCode, errRetryable)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("netbox returned status %d", resp.StatusCode)
	}

	var result vmListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode response: %w", err)
	}

	if len(result.Results) == 0 {
		return 0, fmt.Errorf("VM %q: %w", hostname, errNotFound)
	}

	vm := result.Results[0]
	if vm.Device == nil {
		return 0, fmt.Errorf("VM %q: %w", hostname, errNoHost)
	}

	return vm.Device.ID, nil
}

// getDeviceRackByID fetches a device by ID and returns its rack name.
func (c *Client) getDeviceRackByID(ctx context.Context, deviceID int) (string, error) {
	u := fmt.Sprintf("%s/api/dcim/devices/%d/", c.baseURL, deviceID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request netbox: %w: %w", errRetryable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("netbox returned status %d: %w", resp.StatusCode, errRetryable)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("netbox returned status %d", resp.StatusCode)
	}

	var device Device
	if err := json.NewDecoder(resp.Body).Decode(&device); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if device.Rack == nil {
		return "", fmt.Errorf("device %d: %w", deviceID, errNoRack)
	}

	return device.Rack.Name, nil
}
