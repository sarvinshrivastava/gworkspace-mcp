package policy

// Bearer → agent.
//
// The legacy bearer (vault GWORKSPACE_MCP_TOKEN, what Open WebUI already sends)
// is compared in memory and is never a row in `tokens`. Rotating it is a vault
// edit and a restart with no database step, and the dashboard cannot revoke
// the break-glass path by accident.

type AgentContext struct {
	Slug   string
	Scoped bool
	Grants []Grant
	Scopes []Scope
}

type Authenticator func(bearer string) *AgentContext

func legacyMatches(provided, legacy string) bool {
	if legacy == "" {
		return false
	}
	return SafeEqual(provided, legacy)
}

// LoadContext resolves a slug to its full context, or nil for an archived or
// unknown agent.
func LoadContext(store *PolicyStore, slug string) (*AgentContext, error) {
	agent, err := store.GetAgent(slug)
	if err != nil {
		return nil, err
	}
	if agent == nil || agent.ArchivedAt != nil {
		return nil, nil
	}
	grants, err := store.GetGrants(slug)
	if err != nil {
		return nil, err
	}
	scopes, err := store.GetScopes(slug)
	if err != nil {
		return nil, err
	}
	return &AgentContext{Slug: slug, Scoped: agent.Scoped, Grants: grants, Scopes: scopes}, nil
}

// MakeAuthenticator matches makeAuthenticator: legacy bearer → default agent;
// otherwise hash lookup over live tokens.
//
// Unlike the TS closure, DB errors are surfaced on an error channel the
// gateway turns into 503 {"error":"policy unavailable"} — the TS authenticator
// lets the lookup throw and the gateway catches it; same observable contract.
func MakeAuthenticator(store *PolicyStore, legacyToken func() string) func(bearer string) (*AgentContext, error) {
	return func(bearer string) (*AgentContext, error) {
		if bearer == "" {
			return nil, nil
		}
		if legacyMatches(bearer, legacyToken()) {
			return LoadContext(store, DefaultAgent)
		}
		slug, err := store.AgentForTokenHash(HashToken(bearer))
		if err != nil {
			return nil, err
		}
		if slug == "" {
			return nil, nil
		}
		return LoadContext(store, slug)
	}
}

// MakeDegradedAuthenticator is used only when the database never existed and
// cannot be created. Nothing can have been restricted yet, so the legacy
// bearer gets exactly what a first boot would have seeded; minted tokens
// cannot exist and are refused.
func MakeDegradedAuthenticator(legacyToken func() string) Authenticator {
	return func(bearer string) *AgentContext {
		if bearer == "" || !legacyMatches(bearer, legacyToken()) {
			return nil
		}
		return &AgentContext{
			Slug:   DefaultAgent,
			Scoped: false,
			Grants: DefaultGrants(),
			Scopes: []Scope{},
		}
	}
}
