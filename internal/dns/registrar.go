package dns

import "context"

// Registrar manages DNS A-records for DBInstance VMs.
// Register is called once the VM's vpc-net IP is known; Deregister on deletion.
// Swap implementations via --dns-provider without changing the reconciler.
type Registrar interface {
	Register(ctx context.Context, hostname, ip string) error
	Deregister(ctx context.Context, hostname string) error
}
