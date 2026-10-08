// Package keyutil holds the one shape shared by every place this project
// shells out to ssh-keygen: a context-bounded subprocess with captured stderr
// and consistent timeout-vs-failure disambiguation.
//
// It exists so the signer backend, the derived-public-key helper, and the
// client's local signature verifier agree on how ssh-keygen is run. The client's
// verification passthrough deliberately does not use it: that path must forward
// Git's argv and the child's exit code verbatim, with no timeout, so it is a
// different operation rather than a second copy of this one.
package keyutil

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"time"
)

// RunKeygen runs keygen with args under a context bounded by timeout. stdin and
// dir are optional (nil for no input, "" for the current directory). It returns
// the captured stdout and stderr.
//
// On failure the returned error is the context error when the deadline (or
// cancellation) caused the failure, and the exec error otherwise, so callers
// can report a timeout distinctly from an ssh-keygen failure. Stderr is returned
// either way so the caller can append its own context.
func RunKeygen(ctx context.Context, timeout time.Duration, keygen string, args []string, stdin io.Reader, dir string) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var outBuf, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, keygen, args...)
	cmd.Stdin = stdin
	cmd.Dir = dir
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return outBuf.Bytes(), errBuf.Bytes(), ctxErr
		}
		return outBuf.Bytes(), errBuf.Bytes(), err
	}
	return outBuf.Bytes(), errBuf.Bytes(), nil
}
