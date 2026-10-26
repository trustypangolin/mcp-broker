// Package scripts embeds helper scripts the binaries run, so they work without a checkout.
package scripts

import _ "embed"

// Firewall is firewall.ps1: creates, or with -Remove deletes, the inbound rules the broker
// needs to listen beyond loopback. It must run elevated.
//
//go:embed firewall.ps1
var Firewall string
