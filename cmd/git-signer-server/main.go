// Command git-signer-server is the remote commit-signing server.
//
// This is a skeleton: the HTTP API, key loading, and authentication land in
// later tickets. Today it only establishes the build boundary.
package main

import (
	"log/slog"
	"os"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	slog.Info("git-signer-server: not implemented yet")
}
