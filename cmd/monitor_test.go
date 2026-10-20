package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewLogger(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		var buf bytes.Buffer
		logger, err := newLogger("json", false, &buf)
		require.NoError(t, err)

		logger.Info().Msg("hello")

		var entry map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
		require.Equal(t, "info", entry["level"])
		require.Equal(t, "hello", entry["message"])
		require.Contains(t, entry, "time")
	})

	t.Run("console", func(t *testing.T) {
		var buf bytes.Buffer
		logger, err := newLogger("console", false, &buf)
		require.NoError(t, err)

		logger.Info().Msg("hello")

		require.Contains(t, buf.String(), "hello")
		require.False(t, json.Valid(buf.Bytes()))
	})

	t.Run("invalid format", func(t *testing.T) {
		_, err := newLogger("xml", false, &bytes.Buffer{})
		require.Error(t, err)
	})

	t.Run("verbose enables debug", func(t *testing.T) {
		var buf bytes.Buffer
		logger, err := newLogger("json", false, &buf)
		require.NoError(t, err)
		logger.Debug().Msg("hidden")
		require.Empty(t, buf.String())

		logger, err = newLogger("json", true, &buf)
		require.NoError(t, err)
		logger.Debug().Msg("shown")
		require.Contains(t, buf.String(), `"message":"shown"`)
	})
}
