// Package aichat is a Go client for the ContextGrip AI Chat HTTP API.
//
// The authoritative contract is openapi.yaml at the repository root. The
// client depends only on the Go standard library.
package aichat

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
)

// Client calls the ContextGrip AI Chat API. Construct it with New; the zero
// value is not usable.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the *http.Client used for requests. By default the
// client uses http.DefaultClient.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.httpClient = h
		}
	}
}

// New returns a Client for the service at baseURL (for example
// "http://localhost:8080"). When token is non-empty it is sent as a bearer
// token on every request.
func New(baseURL, token string, opts ...Option) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: http.DefaultClient,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// APIError is a non-2xx JSON error response from the API, parsed from its
// {error, code} body. Use errors.As to recover it:
//
//	var apiErr *aichat.APIError
//	if errors.As(err, &apiErr) && apiErr.Code == "UNAUTHORIZED" { ... }
type APIError struct {
	// StatusCode is the HTTP status code of the response.
	StatusCode int
	// Code is the stable machine slug (for example "UNAUTHORIZED",
	// "NOT_FOUND"). It may be empty when the server omitted it.
	Code string
	// Message is the human-readable error message.
	Message string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("aichat: %s (status %d, code %s)", e.Message, e.StatusCode, e.Code)
	}
	return fmt.Sprintf("aichat: %s (status %d)", e.Message, e.StatusCode)
}

// Status returns service status for authenticated callers.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var out Status
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/status", nil, &out)
	return out, err
}

// Ask sends a one-shot question and blocks until the full answer is ready.
//
// Failed query execution is not an error: the returned AskResponse carries
// ResultError and an Answer explaining the failure.
func (c *Client) Ask(ctx context.Context, req AskRequest) (AskResponse, error) {
	var out AskResponse
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/ask", req, &out)
	return out, err
}

// ListConversations lists conversations, most recently updated first.
func (c *Client) ListConversations(ctx context.Context) ([]Conversation, error) {
	var out []Conversation
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/conversations", nil, &out)
	return out, err
}

// GetConversation fetches one conversation with its messages in order.
func (c *Client) GetConversation(ctx context.Context, id string) (ConversationDetail, error) {
	var out ConversationDetail
	if id == "" {
		return out, errors.New("aichat: conversation id must not be empty")
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/conversations/"+url.PathEscape(id), nil, &out)
	return out, err
}

// DeleteConversation deletes a conversation and its messages.
func (c *Client) DeleteConversation(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("aichat: conversation id must not be empty")
	}
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/conversations/"+url.PathEscape(id), nil, nil)
}

// ListTokens lists named API tokens. Admin: only the primary
// APP_ACCESS_TOKEN may call this.
func (c *Client) ListTokens(ctx context.Context) ([]TokenInfo, error) {
	var out []TokenInfo
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/tokens", nil, &out)
	return out, err
}

// CreateToken mints a named API token. The raw token value is returned once
// in CreatedToken.Token and stored server-side only as a hash. Admin: only
// the primary APP_ACCESS_TOKEN may call this.
func (c *Client) CreateToken(ctx context.Context, label string) (CreatedToken, error) {
	var out CreatedToken
	body := struct {
		Label string `json:"label"`
	}{Label: label}
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/tokens", body, &out)
	return out, err
}

// RevokeToken revokes a named API token. Admin: only the primary
// APP_ACCESS_TOKEN may call this.
func (c *Client) RevokeToken(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("aichat: token id must not be empty")
	}
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/tokens/"+url.PathEscape(id), nil, nil)
}

// newRequest builds a request with auth and content-type headers. body, when
// non-nil, is JSON-encoded.
func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// doJSON performs a request and decodes a JSON response into out when out is
// non-nil. Non-2xx responses are returned as *APIError.
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errorFromResponse(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("aichat: decoding %s %s response: %w", method, path, err)
	}
	return nil
}

// errorFromResponse turns a non-2xx response into an *APIError, parsing the
// {error, code} body when present.
func errorFromResponse(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	apiErr := &APIError{StatusCode: resp.StatusCode}
	var payload struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		apiErr.Message = payload.Error
		apiErr.Code = payload.Code
		return apiErr
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	apiErr.Message = msg
	return apiErr
}
