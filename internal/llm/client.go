package llm

import (
	"context"
	"fmt"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/anthropic"
	"github.com/tmc/langchaingo/llms/googleai"
	"github.com/tmc/langchaingo/llms/openai"

	"github.com/acksell/clank/internal/config"
)

// Message is a chat message for the LLM.
type Message struct {
	Role    string // "system", "user", "assistant"
	Content string
}

// Client wraps a langchaingo model for simple chat completion.
type Client struct {
	model    llms.Model
	provider string
}

// NewClient creates a Client from the given config.
func NewClient(cfg config.LLMConfig) (*Client, error) {
	apiKey := cfg.ResolveAPIKey()

	var model llms.Model
	var err error

	switch cfg.Provider {
	case config.ProviderOpenAI:
		opts := []openai.Option{openai.WithModel(cfg.Model)}
		if apiKey != "" {
			opts = append(opts, openai.WithToken(apiKey))
		}
		if cfg.BaseURL != "" {
			opts = append(opts, openai.WithBaseURL(cfg.BaseURL))
		}
		model, err = openai.New(opts...)

	case config.ProviderAnthropic:
		opts := []anthropic.Option{anthropic.WithModel(cfg.Model)}
		if apiKey != "" {
			opts = append(opts, anthropic.WithToken(apiKey))
		}
		if cfg.BaseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(cfg.BaseURL))
		}
		model, err = anthropic.New(opts...)

	case config.ProviderGemini:
		opts := []googleai.Option{googleai.WithDefaultModel(cfg.Model)}
		if apiKey != "" {
			opts = append(opts, googleai.WithAPIKey(apiKey))
		}
		model, err = googleai.New(context.Background(), opts...)

	default:
		return nil, fmt.Errorf("unsupported LLM provider: %q", cfg.Provider)
	}

	if err != nil {
		return nil, fmt.Errorf("create %s client: %w", cfg.Provider, err)
	}

	return &Client{model: model, provider: cfg.Provider}, nil
}

// ChatCompletion sends messages to the LLM and returns the response text.
func (c *Client) ChatCompletion(messages []Message) (string, error) {
	return c.ChatCompletionWithTemp(messages, 0.2)
}

// ChatCompletionWithTemp sends messages with a specific temperature.
func (c *Client) ChatCompletionWithTemp(messages []Message, temp float64) (string, error) {
	msgs := make([]llms.MessageContent, 0, len(messages))
	for _, m := range messages {
		var role llms.ChatMessageType
		switch m.Role {
		case "system":
			role = llms.ChatMessageTypeSystem
		case "user":
			role = llms.ChatMessageTypeHuman
		case "assistant":
			role = llms.ChatMessageTypeAI
		default:
			role = llms.ChatMessageTypeGeneric
		}
		msgs = append(msgs, llms.TextParts(role, m.Content))
	}

	resp, err := c.model.GenerateContent(
		context.Background(),
		msgs,
		llms.WithTemperature(temp),
	)
	if err != nil {
		return "", fmt.Errorf("LLM generate: %w", err)
	}

	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}
	return resp.Choices[0].Content, nil
}
