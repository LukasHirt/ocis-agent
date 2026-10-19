package config

import (
	"fmt"
	"strconv"
	"strings"
)

// APIConfig holds the settings of the REST API.
type APIConfig struct {
	Rate            float64 // requests per second per user (and per client IP before auth)
	Burst           int     // token bucket size of the rate limiter
	UserDailyTokens int64   // operator cap and default of a user's daily LLM token budget
	StreamsPerUser  int     // open event streams per user (AGENT_API_STREAMS_PER_USER)
	StreamsTotal    int     // open event streams in the process (AGENT_API_STREAMS_TOTAL)
}

func loadAPI(getenv func(string) string) (APIConfig, error) {
	a := APIConfig{Rate: 20, Burst: 40, UserDailyTokens: 200000, StreamsPerUser: 3, StreamsTotal: 200}

	if raw := strings.TrimSpace(getenv("AGENT_API_RATE")); raw != "" {
		// plain decimal notation only: no sign, exponent, hex, NaN or Inf
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || strings.Trim(raw, "0123456789.") != "" || f < 0.5 || f > 1000 {
			return APIConfig{}, fmt.Errorf("AGENT_API_RATE must be a decimal number from 0.5 to 1000")
		}
		a.Rate = f
	}
	if raw := strings.TrimSpace(getenv("AGENT_API_BURST")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 5000 {
			return APIConfig{}, fmt.Errorf("AGENT_API_BURST must be an integer from 1 to 5000")
		}
		a.Burst = n
	}
	if raw := strings.TrimSpace(getenv("AGENT_USER_DAILY_TOKENS")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1000 || n > 1000000000 {
			return APIConfig{}, fmt.Errorf("AGENT_USER_DAILY_TOKENS must be an integer from 1000 to 1000000000")
		}
		a.UserDailyTokens = n
	}
	for _, c := range []struct {
		name     string
		dst      *int
		min, max int
	}{
		{"AGENT_API_STREAMS_PER_USER", &a.StreamsPerUser, 1, 10},
		{"AGENT_API_STREAMS_TOTAL", &a.StreamsTotal, 1, 5000},
	} {
		raw := strings.TrimSpace(getenv(c.name))
		if raw == "" {
			continue
		}
		// decimal digits only: no sign, exponent or hex
		n, err := strconv.Atoi(raw)
		if err != nil || strings.Trim(raw, "0123456789") != "" || n < c.min || n > c.max {
			return APIConfig{}, fmt.Errorf("%s must be an integer from %d to %d", c.name, c.min, c.max)
		}
		*c.dst = n
	}
	return a, nil
}
