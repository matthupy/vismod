package azure

import "time"

// options is the decoded, typed adapter.options block. Each adapter owns the
// decoding of its provider-opaque Options map (spec §D.2). All fields are
// non-secret — secrets come only via the Secret accessor.
type options struct {
	endpoint     string
	authMode     string
	apiVersion   string
	rps          float64
	maxRetries   int
	retryBackoff time.Duration
}

// decodeOptions reads known keys defensively from the opaque map. Unknown keys
// are ignored; malformed values fall back to zero (the Factory then applies
// defaults). viper delivers numbers as int/float64 and durations as strings.
func decodeOptions(m map[string]any) options {
	var o options
	o.endpoint = asString(m["endpoint"])
	o.authMode = asString(m["auth_mode"])
	o.apiVersion = asString(m["api_version"])
	o.rps = asFloat(m["rps"])
	o.maxRetries = asInt(m["max_retries"])
	if d, err := time.ParseDuration(asString(m["retry_backoff"])); err == nil {
		o.retryBackoff = d
	}
	return o
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}
