package dns

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// NoOp logs the DNS record that must be added manually (e.g. in FortiGate).
// Replace with a real implementation when internal DNS is available.
type NoOp struct {
	Domain string
}

func (n *NoOp) Register(_ context.Context, hostname, ip string) error {
	log.Log.Info("DNS record needed — add manually", "hostname", hostname, "ip", ip, "domain", n.Domain)
	return nil
}

func (n *NoOp) Deregister(_ context.Context, hostname string) error {
	log.Log.Info("DNS record can be removed", "hostname", hostname)
	return nil
}
