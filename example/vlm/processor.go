package vlm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ladydd/taskgate/internal/netsec"
)

// Request represents the VLM-specific input for a script generation task.
type Request struct {
	VLMBaseURL string   `json:"vlm_base_url"`
	VLMAPIKey  string   `json:"vlm_api_key"`
	VLMModel   string   `json:"vlm_model"`
	UserText   string   `json:"user_text"`
	ImageURLs  []string `json:"image_urls,omitempty"`
	Title      string   `json:"title,omitempty"`
}

// Processor implements service.TaskProcessor for VLM script generation.
type Processor struct {
	httpClient     *http.Client
	timeout        time.Duration
	promptFilePath string
}

// NewProcessor creates a new VLM Processor with the given timeout in seconds and prompt file path.
func NewProcessor(timeout int, promptFilePath string) *Processor {
	baseDialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid dial address %q: %w", addr, err)
		}

		ips, err := netsec.ResolvePublicIPs(ctx, host, nil)
		if err != nil {
			return nil, err
		}

		var lastErr error
		for _, ip := range ips {
			conn, err := baseDialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}

		return nil, fmt.Errorf("failed to connect to validated host %s: %w", host, lastErr)
	}

	return &Processor{
		httpClient: &http.Client{
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return fmt.Errorf("VLM API attempted redirect to %s, blocked for security", req.URL.String())
			},
		},
		timeout:        time.Duration(timeout) * time.Second,
		promptFilePath: promptFilePath,
	}
}

// Process implements service.TaskProcessor.
// It parses the raw input as a VLM Request, calls the VLM API, and returns the script as JSON output.
func (p *Processor) Process(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var req Request
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, fmt.Errorf("failed to parse VLM request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	chatReq := p.buildChatRequest(&req)

	body, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	endpoint := strings.TrimRight(req.VLMBaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.VLMAPIKey)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("VLM API call timed out after %v", p.timeout)
		}
		return nil, fmt.Errorf("VLM API call failed: %w", err)
	}
	defer resp.Body.Close()

	// Limit response body to 10 MB to prevent OOM from malicious/broken upstream.
	const maxResponseSize = 10 << 20
	limitedReader := io.LimitReader(resp.Body, maxResponseSize+1)
	respBody, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read VLM response body: %w", err)
	}
	if len(respBody) > maxResponseSize {
		return nil, fmt.Errorf("VLM response body exceeds %d bytes limit", maxResponseSize)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("VLM API returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	script, err := extractContent(respBody)
	if err != nil {
		return nil, err
	}

	// Wrap the script string as a JSON object.
	output, err := json.Marshal(map[string]string{"script": script})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal output: %w", err)
	}

	return output, nil
}

// --- OpenAI-compatible chat types ---

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type contentItem struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
}

type chatChoice struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
}

const defaultSystemPrompt = `You are a helpful assistant. Based on the user's input, generate a short, structured response in JSON format.`

func (p *Processor) loadSystemPrompt() string {
	data, err := os.ReadFile(p.promptFilePath)
	if err != nil {
		slog.Warn("failed to read prompt file, using default", "path", p.promptFilePath, "error", err)
		return defaultSystemPrompt
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		slog.Warn("prompt file is empty, using default", "path", p.promptFilePath)
		return defaultSystemPrompt
	}
	return content
}

func (p *Processor) buildChatRequest(req *Request) chatRequest {
	var userContent []contentItem

	for _, imgURL := range req.ImageURLs {
		userContent = append(userContent, contentItem{
			Type:     "image_url",
			ImageURL: &imageURL{URL: imgURL},
		})
	}

	text := req.UserText
	if req.Title != "" {
		text = "Product title: " + req.Title + "\n" + text
	}
	userContent = append(userContent, contentItem{
		Type: "text",
		Text: text,
	})

	return chatRequest{
		Model: req.VLMModel,
		Messages: []chatMessage{
			{Role: "system", Content: p.loadSystemPrompt()},
			{Role: "user", Content: userContent},
		},
	}
}

func extractContent(respBody []byte) (string, error) {
	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", fmt.Errorf("failed to parse VLM response JSON: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("VLM response contains no choices")
	}

	content := strings.TrimSpace(chatResp.Choices[0].Message.Content)
	if content == "" {
		return "", fmt.Errorf("VLM response message content is empty")
	}

	return content, nil
}
