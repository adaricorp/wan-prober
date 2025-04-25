package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/adaricorp/wan-prober/probe"
	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/ffhelp"
	"github.com/prometheus/common/version"
	"gopkg.in/yaml.v3"
)

const (
	binName = "wan_prober"
)

var (
	configFilePath    *string
	httpListenAddress *string
	logger            *slog.Logger
	logLevel          *string
	slogLevel         *slog.LevelVar = new(slog.LevelVar)

	probers = map[string]probe.ProbeFn{
		"http": probe.ProbeHTTP,
	}

	dnsCache           = sync.Map{}
	interfaceStatusMap = sync.Map{}
)

// Print program usage
func printUsage(fs ff.Flags) {
	fmt.Fprintf(os.Stderr, "%s\n", ffhelp.Flags(fs))
	os.Exit(1)
}

// Print program version
func printVersion() {
	fmt.Printf("%s v%s built on %s\n", binName, version.Version, version.BuildDate)
	os.Exit(0)
}

func init() {
	fs := ff.NewFlagSet(binName)
	displayVersion := fs.BoolLong("version", "Print version")
	configFilePath = fs.StringLong(
		"config-file",
		"wan-prober.yml",
		"Path to configuration file",
	)
	httpListenAddress = fs.StringLong(
		"http-listen-address",
		"localhost:8020",
		"Listen address for HTTP server",
	)
	logLevel = fs.StringEnumLong(
		"log-level",
		"Log level: debug, info, warn, error",
		"info",
		"debug",
		"error",
		"warn",
	)

	err := ff.Parse(fs, os.Args[1:],
		ff.WithEnvVarPrefix(strings.ToUpper(binName)),
		ff.WithEnvVarSplit(" "),
	)
	if err != nil {
		printUsage(fs)
	}

	if *displayVersion {
		printVersion()
	}

	switch *logLevel {
	case "debug":
		slogLevel.Set(slog.LevelDebug)
	case "info":
		slogLevel.Set(slog.LevelInfo)
	case "warn":
		slogLevel.Set(slog.LevelWarn)
	case "error":
		slogLevel.Set(slog.LevelError)
	}

	logger = slog.New(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: slogLevel,
		}),
	)
	slog.SetDefault(logger)
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	exitSignal := make(chan os.Signal, 1)
	signal.Notify(exitSignal, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-exitSignal
		cancel()
		os.Exit(0)
	}()

	configFile, err := os.ReadFile(*configFilePath)
	if err != nil {
		slog.Error(
			"Couldn't open configuration file",
			"config_file",
			*configFilePath,
			"error",
			err.Error(),
		)
		os.Exit(1)
	}

	config := Config{}
	if err := yaml.Unmarshal(configFile, &config); err != nil {
		slog.Error(
			"Couldn't parse configuration file",
			"config_file",
			*configFilePath,
			"error",
			err.Error(),
		)
		os.Exit(1)
	}

	if config.ProbeConfiguration.MinInterval == 0 {
		config.ProbeConfiguration.MinInterval = 30 * time.Second
	}

	if config.ProbeConfiguration.Timeout == 0 {
		config.ProbeConfiguration.Timeout = 5 * time.Second
	}

	if config.ProbeConfiguration.Attempts == 0 {
		config.ProbeConfiguration.Attempts = 3
	}

	if len(config.FallbackResolvers) == 0 {
		config.FallbackResolvers = []AddrPort{
			AddrPort{netip.MustParseAddrPort("8.8.8.8:53")},
			AddrPort{netip.MustParseAddrPort("[2001:4860:4860::8888]:53")},
			AddrPort{netip.MustParseAddrPort("1.1.1.1:53")},
			AddrPort{netip.MustParseAddrPort("[2606:4700:4700::1111]:53")},
		}
	}

	ifaces := []string{}
	for _, iface := range config.Interfaces {
		if slices.Contains(ifaces, iface.Name) {
			slog.Error(
				"Interface is defined more than once",
				"config_file",
				*configFilePath,
				"interface",
				iface.Name,
			)
			os.Exit(1)
		}
		ifaces = append(ifaces, iface.Name)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		resp := []InterfaceStatusResponse{}

		interfaceStatusMap.Range(func(key, val interface{}) bool {
			switch v := val.(type) {
			case InterfaceStatusResponse:
				resp = append(resp, v)
			}

			return true
		})

		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(resp); err != nil {
			logger.Error("Error writing HTTP response", "error", err.Error())
			http.Error(w, "Failed to render data", http.StatusInternalServerError)
		}
	})

	go func() {
		if err := http.ListenAndServe(*httpListenAddress, nil); err != nil {
			logger.Error("Error starting HTTP server", "error", err.Error())
			os.Exit(1)
		}
	}()

	channel := make(chan InterfaceStatus)

	for _, iface := range config.Interfaces {
		go probeInterface(ctx, channel, config, iface)
	}

	for status := range channel {
		now := time.Now().Unix()

		lastStatus, exists := interfaceStatusMap.Load(status.Name)
		if !exists {
			interfaceStatusMap.Store(
				status.Name,
				InterfaceStatusResponse{
					Name:       status.Name,
					Healthy:    status.Healthy,
					LastProbe:  now,
					LastChange: now,
				},
			)
		} else {
			switch v := lastStatus.(type) {
			case InterfaceStatusResponse:
				v.LastProbe = now

				if v.Healthy != status.Healthy {
					v.Healthy = status.Healthy
					v.LastChange = now
				}

				interfaceStatusMap.Store(
					status.Name,
					v,
				)
			}
		}
	}
}

func probeInterface(
	ctx context.Context,
	channel chan<- InterfaceStatus,
	config Config,
	iface Interface,
) {
	fallbackResolvers := []string{}
	for _, fallbackResolver := range config.FallbackResolvers {
		fallbackResolvers = append(fallbackResolvers, fallbackResolver.String())
	}

	probe_config := probe.Config{
		BindInterface:     iface.Name,
		FallbackResolvers: fallbackResolvers,
		Timeout:           config.ProbeConfiguration.Timeout,
		HTTP: probe.HTTPProbe{
			Method: "HEAD",
		},
	}

	if config.HostResolver != nil {
		probe_config.HostResolver = config.HostResolver.String()
	}

	for {
		healthy := false

		validTargets := len(config.Targets)
		unreachableTargets := 0

		logger.Info(
			"Checking interface health",
			"interface",
			iface.Name,
		)

		// Try probes in a random order
		for _, i := range rand.Perm(len(config.Targets)) {
			target := config.Targets[i]

			logger.Info(
				"Probing target",
				"interface",
				iface.Name,
				"target",
				target.Host,
				"type",
				target.Probe,
			)

			attempts := 0
			timeouts := 0
			errs := 0

			success := false
			for !success && attempts < config.ProbeConfiguration.Attempts {
				attempts += 1

				if prober, exists := probers[target.Probe]; exists {
					if err := prober(
						ctx,
						target.Host,
						probe_config,
						&dnsCache,
						logger,
					); err != nil {
						if errors.Is(err, probe.ErrProbeTimeout) {
							// Timeout while trying to probe target

							timeouts += 1

							logger.Warn(
								"Probe target is unreachable",
								"interface",
								iface.Name,
								"target",
								target.Host,
								"error",
								err.Error(),
							)
						} else if errors.Is(err, probe.ErrDNSResolutionImpossible) {
							// Treat all DNS resolution attempts failing
							// as a timeout, as it's likely the network
							// connection is unhealthy if host resolver
							// and fallback resolvers aren't answering

							timeouts += 1

							logger.Warn(
								"All DNS resolvers are unreachable",
								"interface",
								iface.Name,
								"target",
								target.Host,
							)
						} else if errors.Is(err, syscall.ENETDOWN) || errors.Is(err, syscall.ENETUNREACH) {
							// Kernel tells us network is not usable

							timeouts += 1

							logger.Warn(
								"Network is down or misconfigured",
								"interface",
								iface.Name,
								"target",
								target.Host,
								"error",
								err.Error(),
							)

							break
						} else if errors.Is(err, probe.ErrDNSNXDomain) {
							// NXDOMAIN is fatal so we don't need to make
							// any more attempts, we can't treat this
							// as a successful response as we don't know
							// who answered (host resolver or fallback)

							errs += 1

							logger.Warn(
								"Probe target doesn't exist",
								"interface",
								iface.Name,
								"target",
								target.Host,
								"error",
								err.Error(),
							)

							break
						} else {
							// Error during probe which could be unrelated
							// to the health of the network connection

							errs += 1

							logger.Error(
								"Error during probe",
								"interface",
								iface.Name,
								"target",
								target.Host,
								"error",
								err.Error(),
							)

							// Wait before trying again, in case this
							// is a temporary error which will clear
							timer := time.NewTimer(config.ProbeConfiguration.Timeout)
							select {
							case <-ctx.Done():
								timer.Stop()
							case <-timer.C:
							}
						}
					} else {
						success = true

						logger.Info(
							"Probe target is healthy",
							"interface",
							iface.Name,
							"target",
							target.Host,
						)
					}
				} else {
					logger.Error("Invalid prober type", "prober", target.Probe)
				}
			}

			if success {
				// At least one successful probe
				healthy = true
				break
			}

			if errs == attempts {
				// All attempts resulted in an error
				validTargets -= 1
			} else if timeouts == attempts-errs {
				// All valid attempts resulted in a timeout
				unreachableTargets += 1
			}
		}

		if !healthy {
			// If no probes were successful, there are some undefined cases
			// where we should declare interface healthy because we can't
			// determine if it is actually down

			if validTargets == 0 {
				logger.Info(
					"No valid targets",
					"interface",
					iface.Name,
					"targets",
					len(config.Targets),
					"valid",
					validTargets,
					"unreachable",
					unreachableTargets,
				)

				healthy = true

			} else if unreachableTargets < validTargets {
				logger.Info(
					"All valid targets are not unreachable",
					"interface",
					iface.Name,
					"targets",
					len(config.Targets),
					"valid",
					validTargets,
					"unreachable",
					unreachableTargets,
				)

				healthy = true
			} else {
				logger.Info(
					"All valid targets are unreachable",
					"interface",
					iface.Name,
					"targets",
					len(config.Targets),
					"valid",
					validTargets,
					"unreachable",
					unreachableTargets,
				)
			}
		}

		if healthy {
			logger.Info(
				"Interface is healthy",
				"interface",
				iface.Name,
			)
		} else {
			logger.Warn(
				"Interface is unhealthy",
				"interface",
				iface.Name,
			)
		}

		channel <- InterfaceStatus{
			Name:    iface.Name,
			Healthy: healthy,
		}

		jitter := time.Duration(rand.IntN(5000)) * time.Millisecond
		timer := time.NewTimer(config.ProbeConfiguration.MinInterval + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}
