package cosmos

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		target   string
		useTLS   bool
	}{
		{"http://h:9090", "h:9090", false},
		{"https://h", "h:443", true},
		{"https://h:443/path", "h:443", true},
		{"https://h:9443", "h:9443", true},
		{"h:9090", "h:9090", false},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			target, useTLS := parseEndpoint(tt.endpoint)
			require.Equal(t, tt.target, target)
			require.Equal(t, tt.useTLS, useTLS)
		})
	}
}
