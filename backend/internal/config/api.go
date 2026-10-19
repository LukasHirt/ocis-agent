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
}

func loadAPI(getenv func(string) string) (APIConfig, error) {
	a := APIConfig{Rate: 20, Burst: 40, UserDailyTokens: 200000}

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
	return a, nil
}
