package celestia

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestGetBlockTimestamp_AuthorizationHeader(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		wantHeader string
		wantSet    bool
	}{
		{name: "token configured", token: "secret", wantHeader: "Bearer secret", wantSet: true},
		{name: "no token", token: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotHeader []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeader = r.Header.Values("Authorization")
				_, _ = w.Write([]byte(`{"result":{"header":{"time":"2024-01-02T03:04:05Z"}}}`))
			}))
			defer srv.Close()

			c := &Client{logger: zerolog.Nop(), url: srv.URL, token: tt.token}
			_, err := c.GetBlockTimestamp(context.Background(), 1)
			require.NoError(t, err)

			if tt.wantSet {
				require.Equal(t, []string{tt.wantHeader}, gotHeader)
			} else {
				require.Empty(t, gotHeader)
			}
		})
	}
}
