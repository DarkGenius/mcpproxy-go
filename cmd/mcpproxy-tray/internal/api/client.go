//go:build darwin || windows

package api

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.uber.org/zap"

	"mcpproxy-go/internal/tray"
)

// Server represents a server from the API
type Server struct {
	Name        string `json:"name"`
	Connected   bool   `json:"connected"`
	Connecting  bool   `json:"connecting"`
	Enabled     bool   `json:"enabled"`
	Quarantined bool   `json:"quarantined"`
	Protocol    string `json:"protocol"`
	URL         string `json:"url"`
	Command     string `json:"command"`
	ToolCount   int    `json:"tool_count"`
	LastError   string `json:"last_error"`
}

// Tool represents a tool from the API
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Server      string                 `json:"server"`
	InputSchema map[string]interface{} `json:"input_schema,omitempty"`
}

// SearchResult represents a search result from the API
type SearchResult struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Server      string                 `json:"server"`
	Score       float64                `json:"score"`
	InputSchema map[string]interface{} `json:"input_schema,omitempty"`
}

// Response represents the standard API response format
type Response struct {
	Success bool                   `json:"success"`
	Data    map[string]interface{} `json:"data,omitempty"`
	Error   string                 `json:"error,omitempty"`
}

// StatusUpdate represents a status update from SSE
type StatusUpdate struct {
	Running       bool                   `json:"running"`
	ListenAddr    string                 `json:"listen_addr"`
	UpstreamStats map[string]interface{} `json:"upstream_stats"`
	Status        map[string]interface{} `json:"status"`
	Timestamp     int64                  `json:"timestamp"`
}

// Client provides access to the mcpproxy API
type Client struct {
	baseURL           string
	apiKey            string
	httpClient        *http.Client
	logger            *zap.SugaredLogger
	statusCh          chan StatusUpdate
	sseCancel         context.CancelFunc
	connectionStateCh chan tray.ConnectionState
}

// NewClient creates a new API client
func NewClient(baseURL string, logger *zap.SugaredLogger) *Client {
	// Create TLS config that trusts the local CA
	tlsConfig := createTLSConfig(logger)

	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				TLSClientConfig: tlsConfig,
			},
		},
		logger:            logger,
		statusCh:          make(chan StatusUpdate, 10),
		connectionStateCh: make(chan tray.ConnectionState, 8),
	}
}

// SetAPIKey sets the API key for authentication
func (c *Client) SetAPIKey(apiKey string) {
	c.apiKey = apiKey
}

// StartSSE starts the Server-Sent Events connection for real-time updates with enhanced retry logic
func (c *Client) StartSSE(ctx context.Context) error {
	c.logger.Info("Starting enhanced SSE connection for real-time updates")

	sseCtx, cancel := context.WithCancel(ctx)
	c.sseCancel = cancel

	go func() {
		defer close(c.statusCh)
		defer close(c.connectionStateCh)

		attemptCount := 0
		maxRetries := 10
		baseDelay := 2 * time.Second
		maxDelay := 30 * time.Second

		for {
			if sseCtx.Err() != nil {
				c.publishConnectionState(tray.ConnectionStateDisconnected)
				return
			}

			attemptCount++

			// Calculate exponential backoff delay
			minVal := attemptCount - 1
			if minVal > 4 {
				minVal = 4
			}
			if minVal < 0 {
				minVal = 0
			}
			backoffFactor := 1 << minVal
			delay := time.Duration(int64(baseDelay) * int64(backoffFactor))
			if delay > maxDelay {
				delay = maxDelay
			}

			if attemptCount > 1 {
				if c.logger != nil {
					c.logger.Info("SSE reconnection attempt",
						"attempt", attemptCount,
						"max_retries", maxRetries,
						"delay", delay,
						"base_url", c.baseURL)
				}

				// Wait before reconnecting (except first attempt)
				select {
				case <-sseCtx.Done():
					c.publishConnectionState(tray.ConnectionStateDisconnected)
					return
				case <-time.After(delay):
				}
			}

			// Check if we've exceeded max retries
			if attemptCount > maxRetries {
				if c.logger != nil {
					c.logger.Error("SSE connection failed after max retries",
						"attempts", attemptCount,
						"max_retries", maxRetries,
						"base_url", c.baseURL)
				}
				c.publishConnectionState(tray.ConnectionStateDisconnected)
				return
			}

			c.publishConnectionState(tray.ConnectionStateConnecting)

			if err := c.connectSSE(sseCtx); err != nil {
				if c.logger != nil {
					c.logger.Error("SSE connection error",
						"error", err,
						"attempt", attemptCount,
						"max_retries", maxRetries,
						"base_url", c.baseURL)
				}

				// Check if it's a context cancellation
				if sseCtx.Err() != nil {
					c.publishConnectionState(tray.ConnectionStateDisconnected)
					return
				}

				c.publishConnectionState(tray.ConnectionStateReconnecting)
				continue
			}

			// Successful connection - reset attempt count
			if attemptCount > 1 && c.logger != nil {
				c.logger.Info("SSE connection established successfully",
					"after_attempts", attemptCount,
					"base_url", c.baseURL)
			}
			attemptCount = 0
		}
	}()

	return nil
}

// StopSSE stops the SSE connection
func (c *Client) StopSSE() {
	if c.sseCancel != nil {
		c.sseCancel()
	}
}

// StatusChannel returns the channel for status updates
func (c *Client) StatusChannel() <-chan StatusUpdate {
	return c.statusCh
}

// ConnectionStateChannel exposes connectivity updates for tray consumers.
func (c *Client) ConnectionStateChannel() <-chan tray.ConnectionState {
	return c.connectionStateCh
}

// connectSSE establishes the SSE connection and processes events
func (c *Client) connectSSE(ctx context.Context) error {
	url := c.baseURL + "/events"
	if c.apiKey != "" {
		url += "?apikey=" + c.apiKey
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, http.NoBody)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("SSE connection failed with status: %d", resp.StatusCode)
	}

	c.publishConnectionState(tray.ConnectionStateConnected)

	scanner := bufio.NewScanner(resp.Body)
	var eventType string
	var data strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// End of event, process it
			if eventType != "" && data.Len() > 0 {
				c.processSSEEvent(eventType, data.String())
				eventType = ""
				data.Reset()
			}
		} else if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			dataLine := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data.Len() > 0 {
				data.WriteString("\n")
			}
			data.WriteString(dataLine)
		}
	}

	return scanner.Err()
}

// processSSEEvent processes incoming SSE events
func (c *Client) processSSEEvent(eventType, data string) {
	if eventType == "status" {
		var statusUpdate StatusUpdate
		if err := json.Unmarshal([]byte(data), &statusUpdate); err != nil {
			if c.logger != nil {
				c.logger.Error("Failed to parse SSE status data", "error", err)
			}
			return
		}

		// Send to status channel (non-blocking)
		select {
		case c.statusCh <- statusUpdate:
		default:
			// Channel full, skip this update
		}
	}
}

// publishConnectionState attempts to deliver a connection state update without blocking the SSE loop.
func (c *Client) publishConnectionState(state tray.ConnectionState) {
	select {
	case c.connectionStateCh <- state:
	default:
		if c.logger != nil {
			c.logger.Debug("Dropping connection state update", "state", state)
		}
	}
}

// GetServers fetches the list of servers from the API
func (c *Client) GetServers() ([]Server, error) {
	resp, err := c.makeRequest("GET", "/api/v1/servers", nil)
	if err != nil {
		return nil, err
	}

	if !resp.Success {
		return nil, fmt.Errorf("API error: %s", resp.Error)
	}

	servers, ok := resp.Data["servers"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected response format")
	}

	var result []Server
	for _, serverData := range servers {
		serverMap, ok := serverData.(map[string]interface{})
		if !ok {
			continue
		}

		server := Server{
			Name:        getString(serverMap, "name"),
			Connected:   getBool(serverMap, "connected"),
			Connecting:  getBool(serverMap, "connecting"),
			Enabled:     getBool(serverMap, "enabled"),
			Quarantined: getBool(serverMap, "quarantined"),
			Protocol:    getString(serverMap, "protocol"),
			URL:         getString(serverMap, "url"),
			Command:     getString(serverMap, "command"),
			ToolCount:   getInt(serverMap, "tool_count"),
			LastError:   getString(serverMap, "last_error"),
		}
		result = append(result, server)
	}

	return result, nil
}

// EnableServer enables or disables a server
func (c *Client) EnableServer(serverName string, enabled bool) error {
	var endpoint string
	if enabled {
		endpoint = fmt.Sprintf("/api/v1/servers/%s/enable", serverName)
	} else {
		endpoint = fmt.Sprintf("/api/v1/servers/%s/disable", serverName)
	}

	resp, err := c.makeRequest("POST", endpoint, nil)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("API error: %s", resp.Error)
	}

	return nil
}

// RestartServer restarts a server
func (c *Client) RestartServer(serverName string) error {
	endpoint := fmt.Sprintf("/api/v1/servers/%s/restart", serverName)

	resp, err := c.makeRequest("POST", endpoint, nil)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("API error: %s", resp.Error)
	}

	return nil
}

// TriggerOAuthLogin triggers OAuth login for a server
func (c *Client) TriggerOAuthLogin(serverName string) error {
	endpoint := fmt.Sprintf("/api/v1/servers/%s/login", serverName)

	resp, err := c.makeRequest("POST", endpoint, nil)
	if err != nil {
		return err
	}

	if !resp.Success {
		return fmt.Errorf("API error: %s", resp.Error)
	}

	return nil
}

// GetServerTools gets tools for a specific server
func (c *Client) GetServerTools(serverName string) ([]Tool, error) {
	endpoint := fmt.Sprintf("/api/v1/servers/%s/tools", serverName)

	resp, err := c.makeRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}

	if !resp.Success {
		return nil, fmt.Errorf("API error: %s", resp.Error)
	}

	tools, ok := resp.Data["tools"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected response format")
	}

	var result []Tool
	for _, toolData := range tools {
		toolMap, ok := toolData.(map[string]interface{})
		if !ok {
			continue
		}

		tool := Tool{
			Name:        getString(toolMap, "name"),
			Description: getString(toolMap, "description"),
			Server:      getString(toolMap, "server"),
		}

		if schema, ok := toolMap["input_schema"].(map[string]interface{}); ok {
			tool.InputSchema = schema
		}

		result = append(result, tool)
	}

	return result, nil
}

// SearchTools searches for tools
func (c *Client) SearchTools(query string, limit int) ([]SearchResult, error) {
	endpoint := fmt.Sprintf("/api/v1/index/search?q=%s&limit=%d", query, limit)

	resp, err := c.makeRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}

	if !resp.Success {
		return nil, fmt.Errorf("API error: %s", resp.Error)
	}

	results, ok := resp.Data["results"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected response format")
	}

	var searchResults []SearchResult
	for _, resultData := range results {
		resultMap, ok := resultData.(map[string]interface{})
		if !ok {
			continue
		}

		result := SearchResult{
			Name:        getString(resultMap, "name"),
			Description: getString(resultMap, "description"),
			Server:      getString(resultMap, "server"),
			Score:       getFloat64(resultMap, "score"),
		}

		if schema, ok := resultMap["input_schema"].(map[string]interface{}); ok {
			result.InputSchema = schema
		}

		searchResults = append(searchResults, result)
	}

	return searchResults, nil
}

// OpenWebUI opens the web control panel in the default browser
func (c *Client) OpenWebUI() error {
    url := c.baseURL + "/ui/"
    if c.apiKey != "" {
        url += "?apikey=" + c.apiKey
    }
    c.logger.Info("Opening web control panel", "url", c.baseURL+"/ui/")
    switch runtime.GOOS {
    case "darwin":
        return exec.Command("open", url).Run()
    case "windows":
        // Try rundll32 first
        if err := exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Run(); err == nil {
            return nil
        }
        // Fallback to cmd start
        return exec.Command("cmd", "/c", "start", "", url).Run()
    default:
        return fmt.Errorf("unsupported OS for OpenWebUI: %s", runtime.GOOS)
    }
}

// makeRequest makes an HTTP request to the API with enhanced error handling and retry logic
func (c *Client) makeRequest(method, path string, _ interface{}) (*Response, error) {
	url := c.baseURL + path
	maxRetries := 3
	baseDelay := 1 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequest(method, url, http.NoBody)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "mcpproxy-tray/1.0")

		// Add API key header if available
		if c.apiKey != "" {
			req.Header.Set("X-API-Key", c.apiKey)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if attempt < maxRetries {
				delay := time.Duration(attempt) * baseDelay
				if c.logger != nil {
					c.logger.Debug("Request failed, retrying",
						"attempt", attempt,
						"max_retries", maxRetries,
						"delay", delay,
						"error", err)
				}
				time.Sleep(delay)
				continue
			}
			return nil, fmt.Errorf("request failed after %d attempts: %w", maxRetries, err)
		}

		// Process response with proper cleanup
		result, shouldContinue, err := c.processResponse(resp, attempt, maxRetries, baseDelay, path)
		if err != nil {
			return nil, err
		}
		if shouldContinue {
			continue
		}
		return result, nil
	}

	return nil, fmt.Errorf("unexpected error in request retry loop")
}

// processResponse handles response processing with proper cleanup
func (c *Client) processResponse(resp *http.Response, attempt, maxRetries int, baseDelay time.Duration, path string) (*Response, bool, error) {
	defer resp.Body.Close()

	// Handle specific HTTP status codes
	switch resp.StatusCode {
	case 401:
		return nil, false, fmt.Errorf("authentication failed: invalid or missing API key")
	case 403:
		return nil, false, fmt.Errorf("authorization failed: insufficient permissions")
	case 404:
		return nil, false, fmt.Errorf("endpoint not found: %s", path)
	case 429:
		// Rate limited - retry with exponential backoff
		if attempt < maxRetries {
			delay := time.Duration(attempt*attempt) * baseDelay
			if c.logger != nil {
				c.logger.Warn("Rate limited, retrying",
					"attempt", attempt,
					"delay", delay,
					"status", resp.StatusCode)
			}
			time.Sleep(delay)
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("rate limited after %d attempts", maxRetries)
	case 500, 502, 503, 504:
		// Server errors - retry
		if attempt < maxRetries {
			delay := time.Duration(attempt) * baseDelay
			if c.logger != nil {
				c.logger.Warn("Server error, retrying",
					"attempt", attempt,
					"status", resp.StatusCode,
					"delay", delay)
			}
			time.Sleep(delay)
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("server error after %d attempts: status %d", maxRetries, resp.StatusCode)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("API call failed with status %d", resp.StatusCode)
	}

	var apiResp Response
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, false, fmt.Errorf("failed to decode response: %w", err)
	}

	return &apiResp, false, nil
}

// Helper functions to safely extract values from maps
func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getBool(m map[string]interface{}, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

func getInt(m map[string]interface{}, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}

func getFloat64(m map[string]interface{}, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0.0
}

// createTLSConfig creates a TLS config that trusts the local mcpproxy CA
func createTLSConfig(logger *zap.SugaredLogger) *tls.Config {
	// Start with system cert pool
	rootCAs, err := x509.SystemCertPool()
	if err != nil {
		if logger != nil {
			logger.Warn("Failed to load system cert pool, creating empty pool", "error", err)
		}
		rootCAs = x509.NewCertPool()
	}

	// Try to load the local mcpproxy CA certificate
	caPath := getLocalCAPath()
	if caPath != "" {
		if caCert, err := os.ReadFile(caPath); err == nil {
			if rootCAs.AppendCertsFromPEM(caCert) {
				if logger != nil {
					logger.Debug("Successfully loaded local mcpproxy CA certificate", "ca_path", caPath)
				}
			} else {
				if logger != nil {
					logger.Warn("Failed to parse local mcpproxy CA certificate", "ca_path", caPath)
				}
			}
		} else {
			if logger != nil {
				logger.Debug("Local mcpproxy CA certificate not found, will use system certs only", "ca_path", caPath)
			}
		}
	}

	return &tls.Config{
		RootCAs:            rootCAs,
		InsecureSkipVerify: false, // Keep verification enabled for security
		MinVersion:         tls.VersionTLS12,
	}
}

// getLocalCAPath returns the path to the local mcpproxy CA certificate
func getLocalCAPath() string {
	// Check environment variable first
	if customCertsDir := os.Getenv("MCPPROXY_CERTS_DIR"); customCertsDir != "" {
		return filepath.Join(customCertsDir, "ca.pem")
	}

	// Use default location
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(homeDir, ".mcpproxy", "certs", "ca.pem")
}
