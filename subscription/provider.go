package subscription

import "net/http"

// ProviderConfig binds one native model adapter to the selected profile and
// credential. OAuth endpoints are provider-owned, never a caller BaseURL.
// HTTPClient is the model-only client; OAuth exchange uses a separate client.
type ProviderConfig struct {
	Binding         Binding
	Model           string
	ThinkingType    string
	ReasoningEffort string
	HTTPClient      *http.Client
}
