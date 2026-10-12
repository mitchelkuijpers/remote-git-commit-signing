// Command devproxy is a local-demo helper, not a product binary.
//
// DEVELOPMENT ONLY. This shim is a stand-in for exe.dev platform plumbing: it
// stamps the client-identity header (internal/wire's wire.SourceVMHeader) on
// every request so the local demo and loopback deployments can exercise the
// signer's authorization chain. It MUST NEVER be deployed in front of a
// production signer. Doing so would make the platform-vouched identity
// forgeable by any client that can reach the proxy, defeating exactly the
// trust boundary the signer's authorization depends on.
//
// In production, exe.dev's authenticated peer proxy presents the verified
// source-VM identity to git-signer-server as the wire.SourceVMHeader header;
// a client cannot set it itself. This shim plays that part for the Milestone 1
// local demo: it reverse-proxies to git-signer-server and stamps the header on
// ingress so the server's authorization chain accepts the request.
//
// Usage:
//
//	go run ./scripts/devproxy -listen 127.0.0.1:0 \
//	    -upstream http://127.0.0.1:8000 -vm local-demo-vm
//
// It prints "listening <addr>" on stdout once bound, so a script can discover
// the chosen port.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/wire"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "address to listen on")
	upstream := flag.String("upstream", "http://127.0.0.1:8000", "git-signer-server URL")
	vm := flag.String("vm", "local-demo-vm", "source-VM identity to stamp on requests")
	flag.Parse()

	target, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("devproxy: parse upstream: %v", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		director(r)
		r.Header.Set(wire.SourceVMHeader, *vm)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("devproxy: listen: %v", err)
	}
	os.Stdout.WriteString("listening " + ln.Addr().String() + "\n")
	log.Fatalf("devproxy: %v", http.Serve(ln, proxy))
}
