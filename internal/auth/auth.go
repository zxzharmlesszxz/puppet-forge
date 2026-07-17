package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type TeamConfig struct {
	Team          string   `json:"team"`
	ReadTokens    []string `json:"read_tokens"`
	PublishTokens []string `json:"publish_tokens"`
	AdminTokens   []string `json:"-"`
	PublishOwners []string `json:"extra_publish_spaces,omitempty"`
	OIDCEmails    []string `json:"oidc_emails"`
	OIDCSubjects  []string `json:"oidc_subjects"`
	OIDCDomains   []string `json:"oidc_domains"`
	OIDCGroups    []string `json:"oidc_groups"`

	OIDCTeamAdminEmails []string `json:"oidc_team_admin_emails"`
	OIDCTeamAdminGroups []string `json:"oidc_team_admin_groups"`

	OIDCAdminEmails   []string `json:"oidc_admin_emails"`
	OIDCAdminSubjects []string `json:"oidc_admin_subjects"`
	OIDCAdminGroups   []string `json:"oidc_admin_groups"`
}

func (cfg *TeamConfig) UnmarshalJSON(data []byte) error {
	type teamConfigAlias TeamConfig
	var aux struct {
		teamConfigAlias
		LegacyPublishOwners []string `json:"publish_owners"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*cfg = TeamConfig(aux.teamConfigAlias)
	if len(cfg.PublishOwners) == 0 && len(aux.LegacyPublishOwners) > 0 {
		cfg.PublishOwners = aux.LegacyPublishOwners
	}
	return nil
}

type Principal struct {
	Team          string
	CanRead       bool
	CanPublish    bool
	CanManageTeam bool
	CanAdmin      bool
	PublishOwners map[string]struct{}
	ManagedTeams  map[string]struct{}
}

func (p Principal) CanDeleteOwner(owner string) bool {
	if p.CanAdmin {
		return true
	}
	if !p.CanManageTeam {
		return false
	}
	if len(p.ManagedTeams) > 0 {
		_, ok := p.ManagedTeams[owner]
		return ok
	}
	return p.Team == owner
}

type Authorizer struct {
	enabled      bool
	tokens       map[string]Principal
	oidcEmails   map[string]Principal
	oidcSubjects map[string]Principal
	oidcDomains  map[string]Principal
	oidcGroups   map[string]Principal
}

type contextKey string

const principalKey contextKey = "principal"

func NewAuthorizer(configs []TeamConfig) (*Authorizer, error) {
	tokenMap := make(map[string]Principal)
	oidcEmails := make(map[string]Principal)
	oidcSubjects := make(map[string]Principal)
	oidcDomains := make(map[string]Principal)
	oidcGroups := make(map[string]Principal)

	for _, cfg := range configs {
		if strings.TrimSpace(cfg.Team) == "" {
			return nil, errors.New("team is required in access config")
		}
		if err := validateTeamOIDCGroupRoles(cfg); err != nil {
			return nil, err
		}

		ownerSet := make(map[string]struct{}, len(cfg.PublishOwners)+1)
		ownerSet[cfg.Team] = struct{}{}
		owners := cfg.PublishOwners
		for _, owner := range owners {
			if owner == "" {
				continue
			}
			ownerSet[owner] = struct{}{}
		}

		for _, token := range cfg.ReadTokens {
			if token == "" {
				continue
			}
			tokenMap[token] = Principal{
				Team:          cfg.Team,
				CanRead:       true,
				CanPublish:    false,
				PublishOwners: ownerSet,
			}
		}

		for _, token := range cfg.PublishTokens {
			if token == "" {
				continue
			}
			tokenMap[token] = Principal{
				Team:          cfg.Team,
				CanRead:       true,
				CanPublish:    true,
				CanAdmin:      false,
				PublishOwners: ownerSet,
			}
		}

		for _, token := range cfg.AdminTokens {
			if token == "" {
				continue
			}
			tokenMap[token] = Principal{
				Team:          cfg.Team,
				CanRead:       true,
				CanPublish:    false,
				CanAdmin:      true,
				PublishOwners: ownerSet,
			}
		}

		webPrincipal := Principal{
			Team:          cfg.Team,
			CanRead:       true,
			CanPublish:    true,
			CanAdmin:      false,
			PublishOwners: ownerSet,
		}
		for _, email := range cfg.OIDCEmails {
			normalized := normalizeEmail(email)
			if normalized == "" {
				continue
			}
			if existing, exists := oidcEmails[normalized]; exists {
				if existing.CanAdmin {
					continue
				}
				return nil, fmt.Errorf("OIDC email %q is configured for both %q and %q", normalized, existing.Team, cfg.Team)
			}
			oidcEmails[normalized] = webPrincipal
		}
		for _, subject := range cfg.OIDCSubjects {
			subject = strings.TrimSpace(subject)
			if subject == "" {
				continue
			}
			if existing, exists := oidcSubjects[subject]; exists {
				if existing.CanAdmin {
					continue
				}
				return nil, fmt.Errorf("OIDC subject %q is configured for both %q and %q", subject, existing.Team, cfg.Team)
			}
			oidcSubjects[subject] = webPrincipal
		}
		for _, domain := range cfg.OIDCDomains {
			normalized := normalizeDomain(domain)
			if normalized == "" {
				continue
			}
			if existing, exists := oidcDomains[normalized]; exists {
				if existing.CanAdmin {
					continue
				}
				return nil, fmt.Errorf("OIDC domain %q is configured for both %q and %q", normalized, existing.Team, cfg.Team)
			}
			oidcDomains[normalized] = webPrincipal
		}
		for _, group := range cfg.OIDCGroups {
			normalized := normalizeGroup(group)
			if normalized == "" {
				continue
			}
			if existing, exists := oidcGroups[normalized]; exists {
				if existing.CanAdmin {
					continue
				}
				return nil, fmt.Errorf("OIDC group %q is configured for both %q and %q", normalized, existing.Team, cfg.Team)
			}
			oidcGroups[normalized] = webPrincipal
		}

		teamAdminPrincipal := Principal{
			Team:          cfg.Team,
			CanRead:       true,
			CanPublish:    true,
			CanManageTeam: true,
			CanAdmin:      false,
			PublishOwners: ownerSet,
			ManagedTeams:  map[string]struct{}{cfg.Team: {}},
		}
		for _, email := range cfg.OIDCTeamAdminEmails {
			normalized := normalizeEmail(email)
			if normalized == "" {
				continue
			}
			if existing, exists := oidcEmails[normalized]; exists {
				if existing.CanAdmin {
					continue
				}
				merged, ok := mergeTeamAdminPrincipals(existing, teamAdminPrincipal)
				if !ok {
					return nil, fmt.Errorf("OIDC email %q is configured for both %q and %q", normalized, existing.Team, cfg.Team)
				}
				oidcEmails[normalized] = merged
				continue
			}
			oidcEmails[normalized] = teamAdminPrincipal
		}
		for _, group := range cfg.OIDCTeamAdminGroups {
			normalized := normalizeGroup(group)
			if normalized == "" {
				continue
			}
			if existing, exists := oidcGroups[normalized]; exists {
				if existing.CanAdmin {
					continue
				}
				merged, ok := mergeTeamAdminPrincipals(existing, teamAdminPrincipal)
				if !ok {
					return nil, fmt.Errorf("OIDC group %q is configured for both %q and %q", normalized, existing.Team, cfg.Team)
				}
				oidcGroups[normalized] = merged
				continue
			}
			oidcGroups[normalized] = teamAdminPrincipal
		}

		adminPrincipal := Principal{
			Team:          cfg.Team,
			CanRead:       true,
			CanPublish:    false,
			CanAdmin:      true,
			PublishOwners: ownerSet,
		}
		for _, email := range cfg.OIDCAdminEmails {
			normalized := normalizeEmail(email)
			if normalized == "" {
				continue
			}
			oidcEmails[normalized] = adminPrincipal
		}
		for _, subject := range cfg.OIDCAdminSubjects {
			subject = strings.TrimSpace(subject)
			if subject == "" {
				continue
			}
			oidcSubjects[subject] = adminPrincipal
		}
		for _, group := range cfg.OIDCAdminGroups {
			normalized := normalizeGroup(group)
			if normalized == "" {
				continue
			}
			oidcGroups[normalized] = adminPrincipal
		}
	}

	return &Authorizer{
		enabled:      len(tokenMap) > 0 || len(oidcEmails) > 0 || len(oidcSubjects) > 0 || len(oidcDomains) > 0 || len(oidcGroups) > 0,
		tokens:       tokenMap,
		oidcEmails:   oidcEmails,
		oidcSubjects: oidcSubjects,
		oidcDomains:  oidcDomains,
		oidcGroups:   oidcGroups,
	}, nil
}

func validateTeamOIDCGroupRoles(cfg TeamConfig) error {
	publishGroups := make(map[string]struct{}, len(cfg.OIDCGroups))
	for _, group := range cfg.OIDCGroups {
		normalized := normalizeGroup(group)
		if normalized == "" {
			continue
		}
		publishGroups[normalized] = struct{}{}
	}
	for _, group := range cfg.OIDCTeamAdminGroups {
		normalized := normalizeGroup(group)
		if normalized == "" {
			continue
		}
		if _, exists := publishGroups[normalized]; exists {
			return fmt.Errorf("OIDC group %q cannot be configured as both publish group and team admin group for team %q", normalized, cfg.Team)
		}
	}
	return nil
}

func (a *Authorizer) Enabled() bool {
	return a != nil && a.enabled
}

func (a *Authorizer) AuthenticateToken(token string) (Principal, bool) {
	if a == nil || !a.enabled || strings.TrimSpace(token) == "" {
		return Principal{}, false
	}

	principal, ok := a.tokens[token]
	return principal, ok
}

func (a *Authorizer) AuthenticateOIDC(email, subject string, groups []string) (Principal, bool) {
	if a == nil || !a.enabled {
		return Principal{}, false
	}

	var first Principal
	var found bool
	addCandidate := func(principal Principal) {
		if principal.CanAdmin {
			first = principal
			found = true
			return
		}
		if principal.CanManageTeam && found && first.CanManageTeam {
			first, _ = mergeTeamAdminPrincipals(first, principal)
			return
		}
		if principal.CanManageTeam && (!found || !first.CanAdmin) {
			first = principal
			found = true
			return
		}
		if !found {
			first = principal
			found = true
		}
	}

	for _, group := range groups {
		if principal, ok := a.oidcGroups[normalizeGroup(group)]; ok {
			addCandidate(principal)
		}
	}

	if subject = strings.TrimSpace(subject); subject != "" {
		if principal, ok := a.oidcSubjects[subject]; ok {
			addCandidate(principal)
		}
	}

	normalizedEmail := normalizeEmail(email)
	if normalizedEmail != "" {
		if principal, ok := a.oidcEmails[normalizedEmail]; ok {
			addCandidate(principal)
		}
		if domain := EmailDomain(normalizedEmail); domain != "" {
			if principal, ok := a.oidcDomains[domain]; ok {
				addCandidate(principal)
			}
		}
	}

	return first, found
}

func mergeTeamAdminPrincipals(left, right Principal) (Principal, bool) {
	if !left.CanManageTeam || !right.CanManageTeam || left.CanAdmin || right.CanAdmin {
		return Principal{}, false
	}

	merged := left
	merged.CanRead = left.CanRead || right.CanRead
	merged.CanPublish = left.CanPublish || right.CanPublish
	merged.PublishOwners = mergeStringSets(left.PublishOwners, right.PublishOwners)
	merged.ManagedTeams = mergeStringSets(left.ManagedTeams, right.ManagedTeams)
	if len(merged.ManagedTeams) == 0 {
		merged.ManagedTeams = map[string]struct{}{}
		if strings.TrimSpace(left.Team) != "" {
			merged.ManagedTeams[left.Team] = struct{}{}
		}
		if strings.TrimSpace(right.Team) != "" {
			merged.ManagedTeams[right.Team] = struct{}{}
		}
	}
	return merged, true
}

func mergeStringSets(left, right map[string]struct{}) map[string]struct{} {
	merged := make(map[string]struct{}, len(left)+len(right))
	for value := range left {
		merged[value] = struct{}{}
	}
	for value := range right {
		merged[value] = struct{}{}
	}
	return merged
}

func (a *Authorizer) RequireRead(w http.ResponseWriter, req *http.Request) (Principal, bool) {
	if a == nil || !a.enabled {
		return Principal{}, true
	}

	principal, ok := a.authenticate(req)
	if !ok || !principal.CanRead {
		writeAuthError(w, http.StatusUnauthorized, "read access token required")
		return Principal{}, false
	}

	return principal, true
}

func (a *Authorizer) RequirePublish(w http.ResponseWriter, req *http.Request, owner string) (Principal, bool) {
	if a == nil || !a.enabled {
		return Principal{}, true
	}

	principal, ok := a.RequirePublishAny(w, req)
	if !ok {
		return Principal{}, false
	}

	if _, allowed := principal.PublishOwners[owner]; !allowed {
		writeAuthError(w, http.StatusForbidden, "token is not allowed to publish to this space")
		return Principal{}, false
	}

	return principal, true
}

func (a *Authorizer) RequirePublishAny(w http.ResponseWriter, req *http.Request) (Principal, bool) {
	if a == nil || !a.enabled {
		return Principal{}, true
	}

	principal, ok := a.authenticate(req)
	if !ok || !principal.CanPublish {
		writeAuthError(w, http.StatusUnauthorized, "publish access token required")
		return Principal{}, false
	}

	return principal, true
}

func (a *Authorizer) RequireDelete(w http.ResponseWriter, req *http.Request, owner string) (Principal, bool) {
	if a == nil || !a.enabled {
		return Principal{}, true
	}

	principal, ok := a.authenticate(req)
	if !ok {
		writeAuthError(w, http.StatusUnauthorized, "admin access token required")
		return Principal{}, false
	}

	if principal.CanAdmin {
		return principal, true
	}
	if principal.CanDeleteOwner(owner) {
		return principal, true
	}
	if principal.CanManageTeam {
		writeAuthError(w, http.StatusForbidden, "team admin is not allowed to delete in this publish space")
		return Principal{}, false
	}

	writeAuthError(w, http.StatusForbidden, "admin or team admin access required")
	return Principal{}, false
}

func (a *Authorizer) authenticate(req *http.Request) (Principal, bool) {
	token := bearerToken(req.Header.Get("Authorization"))
	if token == "" {
		return Principal{}, false
	}

	principal, ok := a.AuthenticateToken(token)
	if !ok {
		return Principal{}, false
	}

	*req = *req.WithContext(context.WithValue(req.Context(), principalKey, principal))
	return principal, true
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalKey).(Principal)
	return principal, ok
}

func bearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func normalizeDomain(domain string) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	return strings.TrimPrefix(domain, "@")
}

func normalizeGroup(group string) string {
	return strings.ToLower(strings.TrimSpace(group))
}

func EmailDomain(email string) string {
	_, domain, ok := strings.Cut(email, "@")
	if !ok {
		return ""
	}
	return normalizeDomain(domain)
}

func AccessConfigsWithRuntimeAdmin(configs []TeamConfig, adminToken string) []TeamConfig {
	if adminToken == "" {
		return configs
	}
	next := make([]TeamConfig, 0, len(configs)+1)
	next = append(next, configs...)
	next = append(next, TeamConfig{
		Team:        "bootstrap-admin",
		AdminTokens: []string{adminToken},
	})
	return next
}

func writeAuthError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]string{"error": message})
	_, _ = w.Write(b)
}
