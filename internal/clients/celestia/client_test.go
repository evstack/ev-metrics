package celestia

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestGetBlockTimestamp_Authorization(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		wantHeader string
	}{
		{name: "with token", token: "tok", wantHeader: "Bearer tok"},
		{name: "without token", token: "", wantHeader: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotHeader string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeader = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"header":{"time":"2024-01-01T00:00:00Z"}}}`))
			}))
			defer srv.Close()

			c := &Client{url: srv.URL, token: tt.token, logger: zerolog.Nop()}
			ts, err := c.GetBlockTimestamp(context.Background(), 1)
			require.NoError(t, err)
			require.Equal(t, tt.wantHeader, gotHeader)
			require.Equal(t, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), ts.UTC())
		})
	}
}
