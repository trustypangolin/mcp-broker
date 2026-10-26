# Creates (or with -Remove, deletes) the inbound firewall rules mcp-broker needs when it
# listens beyond loopback. Run elevated. The tray app will do the same thing for you.
#
#   LAN      TCP <port>, Private profile only: closed on Public networks (cafe, hotel).
#   WSL      TCP <port> on the WSL virtual adapter, from the WSL NAT range only.
#   wslc     TCP <port> from link-local addresses: wslc containers reach the host as
#            host.wslc.internal, a 169.254.x.x address.
#
# Every rule still leaves the broker's own checks in place: allowed_sources, agent token,
# and the agent's match conditions.
param(
    [int]$Port = 8765,
    [switch]$Remove
)

$ErrorActionPreference = 'Stop'
$group = 'mcp-broker'

Get-NetFirewallRule -Group $group -ErrorAction SilentlyContinue | Remove-NetFirewallRule
if ($Remove) {
    Write-Output "Removed mcp-broker firewall rules."
    return
}

$common = @{ Group = $group; Direction = 'Inbound'; Action = 'Allow'; Protocol = 'TCP'; LocalPort = $Port }

New-NetFirewallRule @common -DisplayName "mcp-broker LAN (Private, TCP $Port)" -Profile Private | Out-Null

$wsl = Get-NetAdapter -IncludeHidden -ErrorAction SilentlyContinue | Where-Object Name -like 'vEthernet (WSL*' | Select-Object -ExpandProperty Name
if ($wsl) {
    New-NetFirewallRule @common -DisplayName "mcp-broker WSL (TCP $Port)" -Profile Any -InterfaceAlias $wsl -RemoteAddress 172.16.0.0/12 | Out-Null
} else {
    Write-Warning "No WSL virtual adapter found; skipping the WSL rule. Re-run after starting WSL."
}

New-NetFirewallRule @common -DisplayName "mcp-broker wslc (link-local, TCP $Port)" -Profile Any -RemoteAddress 169.254.0.0/16 | Out-Null

Get-NetFirewallRule -Group $group | Select-Object DisplayName, Profile, Enabled | Format-Table -AutoSize
