// Package okteto is a small client for the parts of the Okteto API that the
// cleanup needs.
package okteto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/okteto/users-cleanup/app/cleanup"
)

// ErrNotAdmin is returned when the token does not belong to an Okteto admin.
var ErrNotAdmin = errors.New("the Okteto token of this pod does not belong to an admin: deploy this service in a namespace owned by an Okteto admin")

// Client talks to an Okteto instance.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a client for the Okteto instance at baseURL.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

// ListUsers returns every user of the Okteto instance. It requires an admin token.
func (c *Client) ListUsers(ctx context.Context) ([]cleanup.User, error) {
	var users []cleanup.User
	if err := c.do(ctx, http.MethodGet, "/api/v0/users", nil, &users); err != nil {
		return nil, err
	}
	return users, nil
}

// DeleteUser deletes a user and, with it, every namespace the user owns.
func (c *Client) DeleteUser(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v0/users/"+url.PathEscape(id), nil, nil)
}

// SetPersistent marks a namespace as persistent ("Keep awake"), or removes the mark.
// A persistent namespace is never put to sleep, nor deleted, by the Okteto garbage collector.
func (c *Client) SetPersistent(ctx context.Context, namespace string, persistent bool) error {
	mutation := "disablePersistentSpace"
	if persistent {
		mutation = "enablePersistentSpace"
	}
	body := map[string]any{
		"query":     fmt.Sprintf("mutation($space: String!) { %s(space: $space) { id } }", mutation),
		"variables": map[string]string{"space": namespace},
	}

	var result struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.do(ctx, http.MethodPost, "/graphql", body, &result); err != nil {
		return err
	}
	if len(result.Errors) > 0 {
		return errors.New(result.Errors[0].Message)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling the Okteto API: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("reading the Okteto API response: %w", err)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if out == nil || len(payload) == 0 {
			return nil
		}
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("decoding the Okteto API response: %w", err)
		}
		return nil
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("the Okteto API rejected the token of this pod (401)")
	case resp.StatusCode == http.StatusForbidden:
		return ErrNotAdmin
	default:
		var apiError struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(payload, &apiError) == nil && apiError.Message != "" {
			return fmt.Errorf("%s (HTTP %d)", apiError.Message, resp.StatusCode)
		}
		return fmt.Errorf("the Okteto API returned HTTP %d", resp.StatusCode)
	}
}
