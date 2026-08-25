-- Map API keys to external agent-identity registries (Okta for AI
-- Agents, Entra Agent ID): the subject claim is the registry principal.
ALTER TABLE api_keys ADD COLUMN subject TEXT NOT NULL DEFAULT '';
