package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"

	"github.com/pkg/errors"
	"github.com/prometheus/common/version"
)

var (
	userAgent = fmt.Sprintf("Adari WAN prober/%s", version.Version)
)

func ProbeHTTP(
	ctx context.Context,
	target string,
	config Config,
	dnsCache *sync.Map,
	logger *slog.Logger,
) error {
	httpConfig := config.HTTP

	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "http://" + target
	}

	targetURL, err := url.Parse(target)
	if err != nil {
		return errors.Wrapf(err, "Could not parse target URL")
	}

	if targetURL.Hostname()[len(targetURL.Hostname())-1] != '.' {
		// Make hostname fully qualified to prevent lookups with search domain
		if targetURL.Port() != "" {
			targetURL.Host = targetURL.Hostname() + ".:" + targetURL.Port()
		} else {
			targetURL.Host = targetURL.Hostname() + "."
		}
	}

	bindToDevice := func(network, address string, c syscall.RawConn) error {
		if config.BindInterface == "" {
			return nil
		}

		var errSock error
		err := c.Control((func(fd uintptr) {
			errSock = syscall.SetsockoptString(
				int(fd),
				syscall.SOL_SOCKET,
				syscall.SO_BINDTODEVICE,
				config.BindInterface,
			)
			return
		}))
		if err != nil {
			return err
		}
		return errSock
	}

	resolverDialer := net.Dialer{
		Control: bindToDevice,
	}

	hostResolver := net.DefaultResolver
	if config.HostResolver != "" {
		hostResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				return resolverDialer.DialContext(ctx, "udp", config.HostResolver)
			},
		}
	}

	fallbackResolverMap := map[string]*net.Resolver{}
	for _, resolver := range config.FallbackResolvers {
		fallbackResolverMap[resolver] = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				return resolverDialer.DialContext(ctx, "udp", resolver)
			},
		}
	}

	timeout, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()

	workingHostResolver := false

	addrs, err := hostResolver.LookupIPAddr(timeout, targetURL.Hostname())
	if err != nil {
		var dnsError *net.DNSError
		if errors.As(err, &dnsError) && !dnsError.IsTimeout && dnsError.IsNotFound {
			// Host resolver returned NXDOMAIN, don't need to keep trying
			return ErrDNSNXDomain
		}
		logger.Warn(
			"Unable to resolve target with host DNS resolver",
			"interface",
			config.BindInterface,
			"target",
			target,
			"error",
			err.Error(),
		)

		servFails := 0
		fallbackSuccess := false
		for _, i := range rand.Perm(len(config.FallbackResolvers)) {
			cache, exists := dnsCache.Load(target)
			if exists {
				logger.Info(
					"Cache hit for target in internal DNS cache",
					"interface",
					config.BindInterface,
					"target",
					target,
				)

				switch v := cache.(type) {
				case []net.IPAddr:
					addrs = v
				}

				fallbackSuccess = true
				break
			} else {
				logger.Warn(
					"Cache miss for target in internal DNS cache",
					"interface",
					config.BindInterface,
					"target",
					target,
				)
			}

			var err error

			fallbackResolver := fallbackResolverMap[config.FallbackResolvers[i]]

			timeout, cancel := context.WithTimeout(ctx, config.Timeout)
			defer cancel()

			addrs, err = fallbackResolver.LookupIPAddr(timeout, targetURL.Hostname())
			if err != nil {
				var dnsError *net.DNSError
				if errors.As(err, &dnsError) && !dnsError.IsTimeout {
					if dnsError.IsNotFound {
						// Fallback resolver returned NXDOMAIN, don't need to keep trying
						return ErrDNSNXDomain
					}

					if dnsError.IsTemporary && dnsError.Err == ErrDNSServerMisbehaving.Error() {
						// Fallback resolver returned a SERVFAIL
						servFails += 1
					}
				}
				logger.Error(
					"Error resolving target with fallback DNS resolver",
					"interface",
					config.BindInterface,
					"resolver",
					config.FallbackResolvers[i],
					"target",
					target,
					"error",
					err.Error(),
				)
			} else {
				logger.Error(
					"Resolved target with fallback DNS resolver",
					"interface",
					config.BindInterface,
					"resolver",
					config.FallbackResolvers[i],
					"target",
					target,
				)

				fallbackSuccess = true
				break
			}
		}

		if !fallbackSuccess {
			if servFails >= 1 {
				// We didn't get a successful response,
				// but did receive an error response
				// which probably means the network has connectivity
				return ErrDNSFallbackServFail
			}
			return ErrDNSResolutionImpossible
		}
	} else {
		workingHostResolver = true
	}

	if len(addrs) == 0 {
		return errors.New("No addresses found for hostname")
	}

	dnsCache.Store(target, addrs)

	httpDialer := net.Dialer{
		Timeout:   config.Timeout,
		DualStack: true,
		Control:   bindToDevice,
	}

	transport := &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !workingHostResolver {
				// When host resolver isn't working, we enter a degraded mode
				// where we dial a random IPv4 address from our internal DNS cache
				// or from fallback DNS resolver
				_, port, err := net.SplitHostPort(addr)
				if err != nil {
					logger.Error("Failed to split address", "addr", addr)
				} else {
					// Find a random ipv4 address from address list and use it
					for _, i := range rand.Perm(len(addrs)) {
						ip := addrs[i].IP
						if ip.To4() != nil {
							addr = net.JoinHostPort(ip.String(), port)
							logger.Info(
								"Overriding IP address for probe",
								"interface",
								config.BindInterface,
								"target",
								target,
								"addr",
								addr,
							)
							break
						}
					}
				}
			}

			return httpDialer.DialContext(ctx, network, addr)
		},
	}

	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Don't follow redirects
			return http.ErrUseLastResponse
		},
	}

	if config.Timeout > 0 {
		client.Timeout = config.Timeout
	}

	if httpConfig.Method == "" {
		httpConfig.Method = "GET"
	}

	request, err := http.NewRequest(httpConfig.Method, targetURL.String(), nil)
	if err != nil {
		return errors.Wrapf(err, "Error creating request")
	}
	request = request.WithContext(ctx)

	request.Header.Set("User-Agent", userAgent)

	_, err = client.Do(request)
	if err != nil {
		logger.Info(
			"Error making HTTP request",
			"interface",
			config.BindInterface,
			"target",
			target,
			"error",
			err.Error(),
		)

		if errors.Is(err, context.DeadlineExceeded) {
			return ErrProbeTimeout
		}

		return err
	}

	return nil
}
