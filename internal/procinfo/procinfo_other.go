//go:build !windows

package procinfo

import "net/netip"

// The broker's process matching is for agents on a Windows host. Other platforms identify
// agents by token and source network only.

func connOwner(netip.Addr, uint16, netip.Addr, uint16) (int, error) { return 0, ErrUnsupported }

func chain(int) ([]Proc, error) { return nil, ErrUnsupported }
