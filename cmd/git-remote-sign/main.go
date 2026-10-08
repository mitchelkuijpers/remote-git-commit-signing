// Command git-remote-sign is the Git signing-program client
// (gpg.ssh.program) that delegates signing to git-signer-server.
//
// Git invokes it positionally as
//
//	git-remote-sign -Y sign -n git -f <user.signingkey> <bufferfile>
//
// The client submits the buffer bytes to GIT_REMOTE_SIGNER_URL, verifies the
// returned SSHSIG against the pinned public key in
// GIT_REMOTE_SIGNER_PUBLIC_KEY, and only then writes <bufferfile>.sig. Any
// failure exits non-zero (and removes a partial .sig) so Git aborts the commit.
package main

import (
	"os"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/client"
)

func main() {
	os.Exit(client.Run(os.Args, os.Getenv, os.Stderr))
}
