package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// verifyOperations are the Git gpg.ssh.program operations that are not signing
// requests. Git uses the configured SSH program for verification too, and
// expects the real ssh-keygen to perform them; the client only intercepts
// signing. The set is matched exactly (positionally derived argv[2]), never by
// substring, because "find-principals" and temp *paths* both match "*sign*".
var verifyOperations = map[string]bool{
	"verify":           true,
	"find-principals":  true,
	"check-novalidate": true,
}

// isPassthroughOperation reports whether op must be delegated verbatim to the
// system ssh-keygen rather than signed.
func isPassthroughOperation(op string) bool {
	return verifyOperations[op]
}

// runPassthrough executes the real system ssh-keygen with argv's original
// arguments (argv[0] is the program name, so the child receives argv[1:]
// verbatim) and the supplied stdio, and returns ssh-keygen's exit code
// unchanged.
//
// No argument is rewritten: in verify mode -f is the gpg.ssh.allowedSignersFile
// (not a key), and -I/-s/-Overify-time must reach ssh-keygen exactly as Git
// passed them. Nothing runs through a shell: the original argument slice is
// handed to exec, which kills the child if ctx is canceled.
//
// This path deliberately does not use internal/keyutil: it forwards Git's argv
// and the child's exit code verbatim and applies no timeout, so sharing the
// signer's exec shape would change its semantics rather than remove duplication.
func runPassthrough(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		fmt.Fprintf(stderr, "git-remote-sign: ssh-keygen not found in PATH: %v\n", err)
		return 1
	}

	cmd := exec.CommandContext(ctx, keygen, argv[1:]...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Propagate ssh-keygen's status exactly (0 is impossible here).
			return exitErr.ExitCode()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			fmt.Fprintf(stderr, "git-remote-sign: ssh-keygen %s: %v\n", argv[2], ctxErr)
			return 1
		}
		fmt.Fprintf(stderr, "git-remote-sign: exec ssh-keygen: %v\n", err)
		return 1
	}
	return 0
}
