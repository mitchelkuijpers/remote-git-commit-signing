package server

import (
	"strings"
	"testing"
)

// TestClientFileArchitecturesAreDispatchable guards the two tables the
// bootstrap's architecture dispatch is derived from against drifting apart:
// clientFiles owns the set of served artifacts and clientMachines owns the
// uname -m spellings that select them, so adding an architecture is a two-table
// edit. clientArchCases silently drops a whitelisted linux binary whose suffix
// has no clientMachines entry, which would leave the signer serving a binary no
// bootstrap arm can ever select; this test turns that silent gap into a failure
// at test time. It is package-internal because both tables are unexported and
// keeping the invariant visible only to tests beats widening the package API.
func TestClientFileArchitecturesAreDispatchable(t *testing.T) {
	for name := range clientFiles {
		arch, ok := strings.CutPrefix(name, clientBinaryLinuxPrefix)
		if !ok {
			continue
		}
		if _, ok := clientMachines[arch]; !ok {
			t.Errorf("whitelisted client binary %q has no clientMachines spelling: add %q to clientMachines so the bootstrap can dispatch it", name, arch)
		}
	}
}
