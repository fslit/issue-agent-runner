package main

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"issue-agent-runner/internal/config"
)

func TestCallbackAuthorizedAllowsWhenSecretUnset(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "/callbacks/cnb", nil)
	require.NoError(t, err)

	require.True(t, callbackAuthorized(config.CallbackConfig{}, req))
}

func TestCallbackAuthorizedChecksConfiguredHeader(t *testing.T) {
	cfg := config.CallbackConfig{
		Secret:       "secret-value",
		SecretHeader: "X-Issue-Agent-Token",
	}
	req, err := http.NewRequest(http.MethodPost, "/callbacks/cnb", nil)
	require.NoError(t, err)

	require.False(t, callbackAuthorized(cfg, req))

	req.Header.Set("X-Issue-Agent-Token", "secret-value")
	require.True(t, callbackAuthorized(cfg, req))
}
