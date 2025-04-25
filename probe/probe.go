package probe

import (
	"context"
	"log/slog"
	"sync"

	"github.com/pkg/errors"
)

type ProbeFn func(ctx context.Context, target string, config Config, dnsCache *sync.Map, logger *slog.Logger) error

var (
	ErrProbeTimeout = errors.New("timeout waiting for probe target to respond")

	ErrDNSNXDomain             = errors.New("DNS resolver responded with NXDOMAIN")
	ErrDNSFallbackServFail     = errors.New("fallback DNS resolver responded with SERVFAIL")
	ErrDNSResolutionImpossible = errors.New("all DNS resolvers are unreachable")
	ErrDNSServerMisbehaving    = errors.New("server misbehaving")
)
