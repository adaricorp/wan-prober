package main

import (
	"fmt"
	"net/netip"
	"time"
)

type Config struct {
	ProbeConfiguration ProbeConfiguration `yaml:"probe_config"`
	Interfaces         []Interface        `yaml:"interfaces"`
	Targets            []Target           `yaml:"targets"`
	HostResolver       *AddrPort          `yaml:"host_resolver"`
	FallbackResolvers  []AddrPort         `yaml:"fallback_resolvers"`
}

type ProbeConfiguration struct {
	MinInterval time.Duration `yaml:"min_interval"`
	Timeout     time.Duration `yaml:"timeout"`
	Attempts    int           `yaml:"attempts"`
}

type Interface struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

type Target struct {
	Host  string `yaml:"host"`
	Probe string `yaml:"probe"`
}

type AddrPort struct {
	netip.AddrPort
}

func (a *AddrPort) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	addrPort, err := netip.ParseAddrPort(s)
	if err != nil {
		return fmt.Errorf("Could not parse address port: %s", s)
	}
	*a = AddrPort{addrPort}
	return nil
}

type InterfaceStatus struct {
	Name    string
	Healthy bool
}

type InterfaceStatusResponse struct {
	Name       string `json:"name,"`
	Healthy    bool   `json:"healthy,"`
	LastProbe  int64  `json:"last_probe,"`
	LastChange int64  `json:"last_change,"`
}
