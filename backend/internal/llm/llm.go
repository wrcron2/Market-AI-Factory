// Package llm provides direct HTTP access to the LLM providers the dashboard
// supports (Anthropic API, Groq cloud, NVIDIA-hosted GLM, local Ollama). It
// mirrors the routing the Ask AI panel uses so any feature can offer the same
// model dropdown — cloud and local — without shelling out to external CLIs.
package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// Client routes chat completions to Anthropic, Groq, or Ollama based on the
// UI model value.
type Client struct {
	http *http.Client
}

// New returns a Client with a timeout generous enough for local CPU Ollama
// inference (~10 min/call on the Oracle box).
func New() *Client {
	return &Client{http: &http.Client{Timeout: 20 * time.Minute}}
}

// Model values accepted from the dashboard dropdowns.
const (
	ModelClaudeSonnet    = "claude-sonnet"     // Anthropic API
	ModelDeepSeekGroq    = "deepseek-r1"       // Groq cloud, Ollama fallback
	ModelQwenGroq        = "qwen3"             // Groq cloud, Ollama fallback
	ModelDeepSeekLocal   = "deepseek-r1-local" // Ollama on host
	ModelQwenLocal       = "qwen3-local"       // Ollama on host
	ModelGLM             = "glm-5.2"           // NVIDIA-hosted z-ai/glm-5.2, OpenAI-compatible API
)

// KnownModels lists every model value Call accepts, for error messages.
var KnownModels = []string{
	ModelClaudeSonnet, ModelDeepSeekGroq, ModelQwenGroq, ModelDeepSeekLocal, ModelQwenLocal, ModelGLM,
}

// Call sends system+user to the provider behind uiModel and returns the reply
// plus a human-readable provider tag for logging (e.g. "Groq llama-3.3-70b").
// Groq- and Claude-backed models fall back to local Ollama when the key is
// missing or the call fails, matching Ask AI behavior.
func (c *Client) Call(uiModel, system, user string) (reply, provider string, err error) {
	switch uiModel {
	case ModelClaudeSonnet:
		if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
			reply, err = c.callClaude(system, user)
			if err == nil {
				return reply, "Anthropic claude-sonnet-4-6", nil
			}
		}
		reply, err = c.callOllama("deepseek-r1:7b", system, user)
		return reply, "Ollama deepseek-r1:7b (fallback)", err

	case ModelDeepSeekGroq, ModelQwenGroq:
		if key := os.Getenv("GROQ_API_KEY"); key != "" {
			reply, err = c.callGroq(key, "llama-3.3-70b-versatile", system, user)
			if err == nil {
				return reply, "Groq llama-3.3-70b", nil
			}
		}
		ollamaModel := "deepseek-r1:7b"
		if uiModel == ModelQwenGroq {
			ollamaModel = "qwen3:4b"
		}
		reply, err = c.callOllama(ollamaModel, system, user)
		return reply, "Ollama " + ollamaModel + " (fallback)", err

	case ModelDeepSeekLocal:
		reply, err = c.callOllama("deepseek-r1:7b", system, user)
		return reply, "Ollama deepseek-r1:7b", err

	case ModelQwenLocal:
		reply, err = c.callOllama("qwen3:4b", system, user)
		return reply, "Ollama qwen3:4b", err

	case ModelGLM:
		key := os.Getenv("NVIDIA_API_KEY")
		if key == "" {
			return "", "", fmt.Errorf("NVIDIA_API_KEY not set — configure it in .env to use GLM-5.2")
		}
		reply, err = c.callNvidia(key, "z-ai/glm-5.2", system, user)
		return reply, "NVIDIA z-ai/glm-5.2", err

	default:
		return "", "", fmt.Errorf("unknown model %q (expected one of: %s)", uiModel, strings.Join(KnownModels, ", "))
	}
}

func (c *Client) callClaude(system, user string) (string, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return "", fmt.Errorf("ANTHROPIC_API_KEY not set — configure it in .env to use Claude")
	}

	body := map[string]any{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 4096,
		"system":     system,
		"messages":   []map[string]string{{"role": "user", "content": user}},
	}
	b, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("claude request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("claude API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse claude response: %w", err)
	}
	if len(result.Content) == 0 {
		return "", fmt.Errorf("empty claude response")
	}
	return result.Content[0].Text, nil
}

func (c *Client) callGroq(apiKey, model, system, user string) (string, error) {
	body := map[string]any{
		"model":      model,
		"max_tokens": 4096,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	b, _ := json.Marshal(body)

	var respBody []byte
	var status int
	// Groq free tier rate-limits aggressively; back off and retry on 429
	// instead of failing the whole agent run.
	for attempt := 0; attempt < 4; attempt++ {
		req, _ := http.NewRequest("POST", "https://api.groq.com/openai/v1/chat/completions", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err := c.http.Do(req)
		if err != nil {
			return "", fmt.Errorf("groq request failed: %w", err)
		}
		respBody, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		status = resp.StatusCode

		if status != http.StatusTooManyRequests {
			break
		}
		wait := 15 * time.Second
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, perr := time.ParseDuration(ra + "s"); perr == nil && secs < 90*time.Second {
				wait = secs + time.Second
			}
		}
		time.Sleep(wait)
	}
	if status != 200 {
		return "", fmt.Errorf("groq API error %d: %s", status, string(respBody))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse groq response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("empty groq response")
	}
	return result.Choices[0].Message.Content, nil
}

// callNvidia hits NVIDIA's OpenAI-compatible chat completions endpoint
// (integrate.api.nvidia.com) — same request/response shape as Groq.
func (c *Client) callNvidia(apiKey, model, system, user string) (string, error) {
	body := map[string]any{
		"model":      model,
		"max_tokens": 4096,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	b, _ := json.Marshal(body)

	req, _ := http.NewRequest("POST", "https://integrate.api.nvidia.com/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("nvidia request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("nvidia API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse nvidia response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("empty nvidia response")
	}
	return result.Choices[0].Message.Content, nil
}

func (c *Client) callOllama(model, system, user string) (string, error) {
	ollamaHost := os.Getenv("OLLAMA_HOST")
	if ollamaHost == "" {
		ollamaHost = "http://127.0.0.1:11434"
	}
	ollamaHost = strings.TrimRight(ollamaHost, "/")

	body := map[string]any{
		"model":  model,
		"stream": false,
		"options": map[string]any{
			"num_ctx": 8192,
		},
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	b, _ := json.Marshal(body)

	resp, err := c.http.Post(ollamaHost+"/api/chat", "application/json", bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("ollama error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("failed to parse ollama response: %w", err)
	}
	return result.Message.Content, nil
}

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// StripThink removes <think>…</think> reasoning blocks that DeepSeek R1 and
// Qwen3 emit before their answer.
func StripThink(s string) string {
	return strings.TrimSpace(thinkRe.ReplaceAllString(s, ""))
}

// ExtractJSONArray returns the first top-level JSON array found in s, for
// parsing structured answers out of chatty model output.
func ExtractJSONArray(s string) (string, error) {
	s = StripThink(s)
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start == -1 || end == -1 || end < start {
		return "", fmt.Errorf("no JSON array found in model output")
	}
	return s[start : end+1], nil
}
