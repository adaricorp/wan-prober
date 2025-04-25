package probe

import (
	"time"
)

type Config struct {
	BindInterface     string
	HostResolver      string
	FallbackResolvers []string
	Timeout           time.Duration
	HTTP              HTTPProbe
}

type HTTPProbe struct {
	Method string
}
