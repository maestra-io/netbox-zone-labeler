// Package netbox is the small read-only NetBox API client the labeler needs:
// it resolves a Kubernetes node name to the NetBox rack of the device, or of
// the host device when the node is a virtual machine.
package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTimeout    = 10 * time.Second
	defaultMaxRetries = 3
	defaultBackoff    = 500 * time.Millisecond
)

var (
	// ErrNoZone is returned when NetBox cannot yield a rack for the host, for
	// any of the permanent reasons below (the wrapped error says which). The
	// caller treats it as a miss to retry on its next full pass, not as an
	// error.
	ErrNoZone = errors.New("no zone in netbox")

	// ErrAmbiguous is returned when more than one device or VM carries the
	// name; NetBox names are unique only per site/tenant, and guessing would
	// label the node with the wrong rack.
	ErrAmbiguous = errors.New("ambiguous name in netbox")

	errNotFound  = errors.New("device not found")
	errNoRack    = errors.New("device has no rack")
	errNoHost    = errors.New("VM has no host device")
	errRetryable = errors.New("retryable error")
)

// RackLookup is what the labeler needs from NetBox.
type RackLookup interface {
	LookupRack(ctx context.Context, hostname string) (string, error)
}

// Client is a NetBox REST API client.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
	maxRetries int
	backoff    time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithTimeout sets the per-request HTTP timeout (default 10s).
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = d }
}

// WithRetry sets how many times a transient failure is retried and the
// initial backoff, which doubles on every attempt (default 3 and 500ms).
func WithRetry(maxRetries int, initialBackoff time.Duration) Option {
	return func(c *Client) {
		c.maxRetries = maxRetries
		c.backoff = initialBackoff
	}
}

// NewClient creates a NetBox API client for baseURL authenticating with token.
func NewClient(baseURL, token string, opts ...Option) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: &http.Client{Timeout: defaultTimeout},
		maxRetries: defaultMaxRetries,
		backoff:    defaultBackoff,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

type namedRef struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type device struct {
	ID   int       `json:"id"`
	Name string    `json:"name"`
	Rack *namedRef `json:"rack"`
}

type virtualMachine struct {
	ID     int       `json:"id"`
	Name   string    `json:"name"`
	Device *namedRef `json:"device"`
}

type listResponse[T any] struct {
	Count   int `json:"count"`
	Results []T `json:"results"`
}

// Ping checks that NetBox answers authenticated requests.
func (c *Client) Ping(ctx context.Context) error {
	var status map[string]any
	return c.get(ctx, "/api/status/", nil, &status)
}

// LookupRack returns the rack name for hostname. It looks in dcim/devices
// first; when the name is not a device it falls back to
// virtualization/virtual-machines and resolves the rack through the VM's host
// device. Transient failures (network errors, 5xx, 429) are retried with
// exponential backoff; permanent ones (ErrNoZone, ErrAmbiguous, other 4xx,
// malformed responses) are returned at once.
func (c *Client) LookupRack(ctx context.Context, hostname string) (string, error) {
	var lastErr error
	for attempt := range c.maxRetries + 1 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(c.backoff * time.Duration(1<<(attempt-1))):
			}
		}
		rack, err := c.lookupRack(ctx, hostname)
		if err == nil {
			return rack, nil
		}
		if !errors.Is(err, errRetryable) {
			return "", err
		}
		lastErr = err
	}
	return "", fmt.Errorf("after %d retries: %w", c.maxRetries, lastErr)
}

func (c *Client) lookupRack(ctx context.Context, hostname string) (string, error) {
	rack, err := c.deviceRack(ctx, hostname)
	if err == nil || !errors.Is(err, errNotFound) {
		return rack, err
	}
	return c.vmRack(ctx, hostname)
}

func (c *Client) deviceRack(ctx context.Context, hostname string) (string, error) {
	var resp listResponse[device]
	if err := c.get(ctx, "/api/dcim/devices/", url.Values{"name": {hostname}, "limit": {"2"}}, &resp); err != nil {
		return "", err
	}
	switch {
	case len(resp.Results) == 0:
		return "", fmt.Errorf("device %q: %w: %w", hostname, ErrNoZone, errNotFound)
	case len(resp.Results) > 1:
		return "", fmt.Errorf("device %q: %w: %d matches", hostname, ErrAmbiguous, resp.Count)
	case resp.Results[0].Rack == nil:
		return "", fmt.Errorf("device %q: %w: %w", hostname, ErrNoZone, errNoRack)
	}
	return resp.Results[0].Rack.Name, nil
}

func (c *Client) vmRack(ctx context.Context, hostname string) (string, error) {
	var resp listResponse[virtualMachine]
	if err := c.get(ctx, "/api/virtualization/virtual-machines/", url.Values{"name": {hostname}, "limit": {"2"}}, &resp); err != nil {
		return "", err
	}
	switch {
	case len(resp.Results) == 0:
		return "", fmt.Errorf("VM %q: %w: %w", hostname, ErrNoZone, errNotFound)
	case len(resp.Results) > 1:
		return "", fmt.Errorf("VM %q: %w: %d matches", hostname, ErrAmbiguous, resp.Count)
	case resp.Results[0].Device == nil:
		return "", fmt.Errorf("VM %q: %w: %w", hostname, ErrNoZone, errNoHost)
	}

	host := resp.Results[0].Device
	var dev device
	if err := c.get(ctx, fmt.Sprintf("/api/dcim/devices/%d/", host.ID), nil, &dev); err != nil {
		return "", err
	}
	if dev.Rack == nil {
		return "", fmt.Errorf("VM %q host %q: %w: %w", hostname, host.Name, ErrNoZone, errNoRack)
	}
	return dev.Rack.Name, nil
}

// get performs an authenticated GET of path (with optional query) and decodes
// the JSON body into out. 5xx and 429 are retryable, any other non-200 is
// permanent.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request netbox: %w: %w", errRetryable, err)
	}
	defer func() {
		// Drain so the keep-alive connection is reusable.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("netbox %s: status %d: %w", path, resp.StatusCode, errRetryable)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("netbox %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode netbox %s: %w", path, err)
	}
	return nil
}
