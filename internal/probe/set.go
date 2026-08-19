package probe

import (
	"context"

	"github.com/eeegoloauq/lookout/internal/check"
	"github.com/eeegoloauq/lookout/internal/config"
	"github.com/eeegoloauq/lookout/internal/state"
)

// Set routes a check to the probe that implements its type.
type Set struct {
	HTTP   *HTTP
	DNS    *DNS
	Domain *Domain
}

// New returns the production set of probers.
func New() *Set {
	return &Set{
		HTTP:   NewHTTP(),
		DNS:    NewDNS(),
		Domain: NewDomain(),
	}
}

// Probe runs the check with the matching prober. An unknown type is a
// configuration bug that slipped the validator: treat it as down, never
// as success.
func (s *Set) Probe(ctx context.Context, c config.Check) check.Result {
	switch c.Type {
	case config.TypeDNS:
		return s.DNS.Probe(ctx, c)
	case config.TypeDomain:
		return s.Domain.Probe(ctx, c)
	default:
		return s.HTTP.Probe(ctx, c)
	}
}

// LoadRegistry restores the RDAP/WHOIS cache from durable state.
func (s *Set) LoadRegistry(cache state.RegistryCache) {
	if s.Domain != nil {
		s.Domain.LoadCache(cache)
	}
}

// Registry is the current RDAP/WHOIS cache, ready to persist.
func (s *Set) Registry() state.RegistryCache {
	if s.Domain == nil {
		return state.RegistryCache{}
	}
	return s.Domain.Cache()
}

// RegistryDirty reports whether the cache changed since the last save.
func (s *Set) RegistryDirty() bool {
	if s.Domain == nil {
		return false
	}
	return s.Domain.Dirty()
}

func (s *Set) ClearRegistryDirty() {
	if s.Domain != nil {
		s.Domain.ClearDirty()
	}
}
