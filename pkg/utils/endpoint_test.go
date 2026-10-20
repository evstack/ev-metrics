package utils

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedactEndpoint(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://host/v2/KEY?x=1", "https://host"},
		{"https://user:pass@host:8545/path", "https://host:8545"},
		{"http://host:9090", "http://host:9090"},
		{"host:9090", "host:9090"},
		{"host:9090/v2/KEY?x=1", "host:9090"},
		{"user:pass@host:9090", "host:9090"},
		{"https://host#frag", "https://host"},
		{"https://SECRET_KEY%zz@/path", InvalidEndpoint},
		{"https:///v2/SECRET_KEY", InvalidEndpoint},
		{"://SECRET_KEY", InvalidEndpoint},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := RedactEndpoint(tt.in)
			require.Equal(t, tt.want, got)
			require.NotContains(t, got, "SECRET_KEY")
		})
	}
}

func TestRedactEndpoints(t *testing.T) {
	got := RedactEndpoints([]string{"https://gw/a/KEY1", "https://gw/b/KEY2", "https://other/x", "https://gw/c/KEY3"})
	require.Equal(t, []string{"https://gw", "https://gw#1", "https://other", "https://gw#3"}, got)
}

func TestRedactError(t *testing.T) {
	raw := "https://user:pass@host:8545/v2/SECRET_KEY?token=SECRET_TOK"
	err := fmt.Errorf("failed to connect to %s: %w", raw, fs.ErrNotExist)
	err = fmt.Errorf("dial target user:pass@host:8545 path /v2/SECRET_KEY: %w", err)

	red := RedactError(err, raw)
	msg := red.Error()
	for _, secret := range []string{"pass", "SECRET_KEY", "SECRET_TOK"} {
		require.False(t, strings.Contains(msg, secret), "%q leaked in %q", secret, msg)
	}
	require.Contains(t, msg, "https://host:8545")

	require.ErrorIs(t, red, fs.ErrNotExist)
	var pe *fs.PathError
	pathErr := &fs.PathError{Op: "open", Path: raw, Err: fs.ErrNotExist}
	require.True(t, errors.As(RedactError(pathErr, raw), &pe))
	require.NotContains(t, RedactError(pathErr, raw).Error(), "SECRET_KEY")

	require.NoError(t, RedactError(nil, raw))
}
