package server

import "strings"

type hostPolicy struct {
	allowAll         bool
	exact            map[string]struct{}
	wildcardSuffixes []string
}

func newHostPolicy(values []string) hostPolicy {
	policy := hostPolicy{exact: make(map[string]struct{}, len(values))}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		switch {
		case value == "*":
			policy.allowAll = true
		case strings.HasPrefix(value, "*."):
			policy.wildcardSuffixes = append(policy.wildcardSuffixes, strings.TrimPrefix(value, "*"))
		default:
			if host := normalizeHostname(value); host != "" {
				policy.exact[host] = struct{}{}
			}
		}
	}
	return policy
}

func (p hostPolicy) allows(hostport string) bool {
	host := normalizeHostname(hostport)
	if host == "" {
		return false
	}
	if p.allowAll {
		return true
	}
	if _, ok := p.exact[host]; ok {
		return true
	}
	for _, suffix := range p.wildcardSuffixes {
		if strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".") {
			return true
		}
	}
	return false
}

type corsPolicy struct {
	allowAll bool
	origins  []string
}

func newCORSPolicy(values []string) corsPolicy {
	policy := corsPolicy{origins: make([]string, 0, len(values))}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "*" {
			policy.allowAll = true
			continue
		}
		policy.origins = append(policy.origins, value)
	}
	return policy
}

func (p corsPolicy) allows(origin string) bool {
	if p.allowAll {
		return true
	}
	for _, allowed := range p.origins {
		if strings.EqualFold(allowed, origin) {
			return true
		}
	}
	return false
}
