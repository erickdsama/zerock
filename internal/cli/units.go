package cli

import "fmt"

// The systemd units live here as templates rather than as files to copy, so a
// single binary is everything the server needs.

// serverUnit renders the unit for the zerock server.
func serverUnit(binary string) string {
	return fmt.Sprintf(serverUnitTemplate, binary)
}

// tunnelUnit renders the templated unit for a long-lived agent. One instance per
// tunnel: zerock-tunnel@api-x reads /etc/zerock/tunnels/api-x.env.
func tunnelUnit(binary string) string {
	return fmt.Sprintf(tunnelUnitTemplate, binary)
}

const serverUnitTemplate = `# Managed by 'zerock service install'. Edits are overwritten on reinstall.
[Unit]
Description=zerock tunnel server
After=network-online.target
Wants=network-online.target
# Give up after repeated rapid failures instead of restarting forever. An
# unattended crash loop can hammer an external service, such as an ACME CA,
# hard enough to trip a rate limit that then blocks recovery for hours.
StartLimitIntervalSec=600
StartLimitBurst=5

[Service]
Type=simple
ExecStart=%s serve --config /etc/zerock/zerock.yaml --log-format json
Restart=always
RestartSec=10s

# Run unprivileged, but keep the one capability needed to bind 80 and 443.
DynamicUser=yes
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

# DynamicUser gives /var/lib/zerock to this service and nobody else. The token
# database and the certificate cache both live there.
StateDirectory=zerock
StateDirectoryMode=0700

ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
NoNewPrivileges=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictRealtime=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service

# Keeps the DNS credential out of the config file, and so out of backups.
EnvironmentFile=-/etc/zerock/zerock.env

[Install]
WantedBy=multi-user.target
`

const tunnelUnitTemplate = `# Managed by 'zerock service tunnel'. Edits are overwritten on reinstall.
#
# One instance per tunnel. zerock-tunnel@api-x reads its settings from
# /etc/zerock/tunnels/api-x.env, where ZEROCK_ARGS carries the tunnel itself:
#
#   ZEROCK_ARGS=http 3000 --sub api-x --quiet
#   ZEROCK_ARGS=tcp 5432 --sub db --quiet
[Unit]
Description=zerock tunnel (%%i)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# The token lives in this file rather than on the command line, where ps would
# expose it to every user on the box.
EnvironmentFile=/etc/zerock/tunnels/%%i.env
ExecStart=%s $ZEROCK_ARGS
Restart=always
RestartSec=5s

DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6
RestrictNamespaces=yes
LockPersonality=yes
RestrictRealtime=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service

[Install]
WantedBy=multi-user.target
`

// dnsEnvBody seeds the credential file that keeps the DNS token out of the
// config, naming what the chosen provider's token actually needs.
func dnsEnvBody(provider string) string {
	return fmt.Sprintf(`# Read by the zerock service. Keep this file at mode 600.
#
# Needed for the wildcard certificate, which can only be issued over DNS-01.
# %s
ZEROCK_DNS_API_TOKEN=
`, dnsTokenHint(provider))
}
