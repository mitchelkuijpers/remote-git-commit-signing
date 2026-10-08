// Package client implements the Git gpg.ssh.program client (git-remote-sign).
//
// Git invokes a configured gpg.ssh.program positionally as
//
//	git-remote-sign -Y sign -n git -f <user.signingkey> <bufferfile>
//
// The client signs the buffer contents through the remote signer, validates the
// returned SSHSIG locally against the pinned public key, and only then writes
// <bufferfile>.sig. Any failure exits non-zero (and removes a partial .sig) so
// that Git aborts the commit instead of embedding an unverified signature.
package client

import (
	"errors"
	"fmt"
)

// invocation is the parsed form of Git's signing-program argv.
type invocation struct {
	// op is the positional operation from argv[2] ("sign", "verify", ...).
	op string
	// namespace is the -n value. Git always passes "git" for signing.
	namespace string
	// keyFile is the -f value (a public key path in this setup).
	keyFile string
	// bufferFile is the trailing positional argument: the file Git wants
	// signed, or (for verify operations) the signature file.
	bufferFile string
}

// parseInvocation parses argv in os.Args form (argv[0] is the program name).
//
// The operation is determined strictly by position — argv[1] must be "-Y" and
// argv[2] the operation — never by substring matching. The spike showed a glob
// like *sign* both matching "find-principals" and a temporary path, which
// corrupted delegation.
func parseInvocation(argv []string) (invocation, error) {
	if len(argv) < 3 {
		return invocation{}, fmt.Errorf("expected %q, got %q", "-Y <operation> ...", argv)
	}
	if argv[1] != "-Y" {
		return invocation{}, fmt.Errorf("unsupported signing-program invocation: argv[1] is %q, want -Y", argv[1])
	}

	inv := invocation{op: argv[2]}
	if inv.op != "sign" {
		// The caller reports unimplemented operations loudly.
		return inv, nil
	}

	args := argv[3:]
	for i := 0; i < len(args); {
		switch args[i] {
		case "-n", "-f":
			if i+1 >= len(args) {
				return inv, fmt.Errorf("option %s requires a value", args[i])
			}
			if args[i] == "-n" {
				inv.namespace = args[i+1]
			} else {
				inv.keyFile = args[i+1]
			}
			i += 2
		default:
			if inv.bufferFile != "" {
				return inv, fmt.Errorf("unexpected extra argument %q", args[i])
			}
			inv.bufferFile = args[i]
			i++
		}
	}

	if inv.namespace == "" {
		return inv, errors.New("missing required -n git namespace")
	}
	if inv.namespace != "git" {
		return inv, fmt.Errorf("unsupported signing namespace %q: only \"git\" is signed", inv.namespace)
	}
	if inv.keyFile == "" {
		return inv, errors.New("missing required -f <signing key>")
	}
	if inv.bufferFile == "" {
		return inv, errors.New("missing signing buffer file argument")
	}
	return inv, nil
}
