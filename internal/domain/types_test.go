package domain

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkerResultUnmarshalAcceptsLegacyChangedFileStrings(t *testing.T) {
	var result WorkerResult
	err := json.Unmarshal([]byte(`{
  "status": "success",
  "changedFiles": ["internal/runner/runner.go"]
}`), &result)
	require.NoError(t, err)
	require.Len(t, result.ChangedFiles, 1)
	require.Equal(t, "internal/runner/runner.go", result.ChangedFiles[0].Path)
}

func TestWorkerResultUnmarshalAcceptsStructuredChangedFiles(t *testing.T) {
	var result WorkerResult
	err := json.Unmarshal([]byte(`{
  "status": "success",
  "changedFiles": [
    {
      "path": "internal/runner/runner.go",
      "line": 123,
      "symbols": ["applySuccess"],
      "newSymbols": ["queueForIntegration"],
      "description": "queue integration instead of tagging done"
    }
  ]
}`), &result)
	require.NoError(t, err)
	require.Len(t, result.ChangedFiles, 1)
	require.Equal(t, 123, result.ChangedFiles[0].Line)
	require.Equal(t, []string{"applySuccess"}, result.ChangedFiles[0].Symbols)
	require.Equal(t, []string{"queueForIntegration"}, result.ChangedFiles[0].NewSymbols)
}
