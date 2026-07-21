package host

// Env-borne provider credentials as a status signal. Spawned agent
// CLIs inherit this process's environment (claude via the SDK's
// os.Environ()+ExtraEnv build, `opencode serve` via plain exec), so a
// key exported into clank-host's environment authenticates sessions
// with no clank connection — operator-injected on sandboxes, shell
// exports on laptops. OpenCode applies the same rule itself: provider
// resolution enables any catalog provider whose declared env var is
// present, then stored api credentials merge over env (see
// packages/opencode/src/provider/provider.ts upstream). Status mirrors
// that: store wins, env fills the gap, on every host type.

// Env var names the spawned claude CLI consumes. Shared by
// AnthropicEnv (injection), setup-token scrubbing, and env-credential
// detection so the three can't drift. EnvAnthropicAuthToken is the
// bearer-token variant custom LLM gateways use (paired with a base
// URL); claude treats it as env-borne auth like the API key.
const (
	EnvClaudeCodeOAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"
	EnvAnthropicAPIKey      = "ANTHROPIC_API_KEY"
	EnvAnthropicAuthToken   = "ANTHROPIC_AUTH_TOKEN"
)

// providerEnvVars maps catalog provider IDs to the env var names that
// authenticate them. Any one present counts — matching opencode's
// rule, which its OpenCode-backend entries mirror from the provider
// `env` lists in the models.dev database opencode resolves against.
// Device-flow providers (Copilot) are deliberately absent: opencode
// would env-enable Copilot from a generic GITHUB_TOKEN, but such
// tokens rarely carry Copilot entitlement, and reporting connected
// would promise sessions that 401.
var providerEnvVars = map[string][]string{
	ProviderAnthropicClaudeCode: {EnvClaudeCodeOAuthToken},
	ProviderAnthropicAPI:        {EnvAnthropicAPIKey, EnvAnthropicAuthToken},
	"openai":                    {"OPENAI_API_KEY"},
	"google":                    {"GOOGLE_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY", "GEMINI_API_KEY"},
	"xai":                       {"XAI_API_KEY"},
	"groq":                      {"GROQ_API_KEY"},
	"deepseek":                  {"DEEPSEEK_API_KEY"},
	"mistral":                   {"MISTRAL_API_KEY"},
	"openrouter":                {"OPENROUTER_API_KEY"},
	"azure":                     {"AZURE_RESOURCE_NAME", "AZURE_API_KEY"},
	"cloudflare-workers-ai":     {"CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_API_KEY"},
	"cloudflare-ai-gateway":     {"CLOUDFLARE_API_TOKEN", "CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_GATEWAY_ID"},
}

// envCredentialPresent reports whether any of providerID's env vars is
// non-empty in this process's environment. Empty and unset are the
// same absence, matching opencode. Presence-only: the value is never
// persisted or logged.
func (a *AuthManager) envCredentialPresent(providerID string) bool {
	for _, name := range providerEnvVars[providerID] {
		if a.lookupEnv(name) != "" {
			return true
		}
	}
	return false
}
