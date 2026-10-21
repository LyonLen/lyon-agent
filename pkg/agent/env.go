package agent

import (
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// lyon's own environment names (#54). Claude Code owns ANTHROPIC_MODEL,
// ANTHROPIC_BASE_URL, ANTHROPIC_AUTH_TOKEN and friends, and a shell that
// exports them for Claude Code must not silently redirect lyon to the same
// gateway. Every lyon lookup reads its LYON_* name first and the ANTHROPIC_*
// name second, so both can coexist and LYON_* always wins; the ANTHROPIC_*
// names stay supported unchanged for scripts that already set them.
const (
	envBaseURL   = "LYON_BASE_URL"
	envAuthToken = "LYON_AUTH_TOKEN"
	envAPIKey    = "LYON_API_KEY"
	envModel     = "LYON_MODEL"
)

// envFirst returns the first of keys set to a non-empty value.
func envFirst(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// endpointBaseURL is the API endpoint: LYON_BASE_URL, else ANTHROPIC_BASE_URL.
func endpointBaseURL() string { return envFirst(envBaseURL, "ANTHROPIC_BASE_URL") }

// endpointOptions returns the explicit client options for the endpoint (#54).
// With no LYON_* variable set it returns nil and the SDK's own credential
// chain (ANTHROPIC_*, profile, federation) runs untouched — the pre-#54
// behavior. A LYON_* override is resolved here instead:
//
//   - LYON_BASE_URL alone: passed as WithBaseURL, which lands after the SDK's
//     default and wins; credentials still come from the SDK chain.
//   - LYON_API_KEY / LYON_AUTH_TOKEN: the SDK autoload is switched off, since
//     it would add a second credential header from ANTHROPIC_* (X-Api-Key
//     next to Authorization: Bearer). One header, from the LYON_* variable.
func endpointOptions() []option.RequestOption {
	base := endpointBaseURL()
	key := envFirst(envAPIKey, "ANTHROPIC_API_KEY")
	token := envFirst(envAuthToken, "ANTHROPIC_AUTH_TOKEN")
	var opts []option.RequestOption
	if envFirst(envAPIKey, envAuthToken) == "" {
		// No credential override: leave the chain alone.
		if envFirst(envBaseURL) == "" {
			return nil
		}
	} else {
		// Same precedence the SDK itself uses: an API key wins over an auth
		// token, and exactly one credential header is sent.
		opts = append(opts, option.WithoutEnvironmentDefaults())
		if key != "" {
			opts = append(opts, option.WithAPIKey(key))
		} else if token != "" {
			opts = append(opts, option.WithAuthToken(token))
		}
	}
	if base != "" {
		opts = append(opts, option.WithBaseURL(base))
	}
	return opts
}
