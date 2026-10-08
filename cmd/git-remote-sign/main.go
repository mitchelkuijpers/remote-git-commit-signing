// Command git-remote-sign is the Git signing-program client
// (gpg.ssh.program) that delegates signing to git-signer-server.
//
// This is a skeleton: the Git argument protocol and server calls land in later
// tickets. Today it only establishes the build boundary.
package main

import (
	"log/slog"
	"os"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	slog.Info("git-remote-sign: not implemented yet")
}
