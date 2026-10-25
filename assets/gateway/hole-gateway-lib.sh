# Shared by entrypoint.sh and hole-gateway-apply; sourced, never executed.
# shellcheck disable=SC2034  # the variables are used by the scripts that source this file

# Rendered configuration the running services read.
HOLE_RUN_DIR=/tmp/hole
# Configuration a runtime change (`hole modify`) left in this container's own writable layer. A
# symlink to the newest runtime.<id> directory, swapped atomically; it lives exactly as long as the
# container, so a restart of this container keeps runtime changes and a new sandbox never sees them.
HOLE_STATE_DIR=/var/lib/hole
HOLE_RUNTIME_DIR="${HOLE_STATE_DIR}/runtime"
# Written by the dnsmasq supervisor for every dnsmasq it starts.
HOLE_DNSMASQ_PID=/run/hole-dnsmasq.pid

# The address the runtime injects for `host-gateway` lives in /etc/hosts, and it is read from there
# rather than through getent: a runtime that injects both an IPv4 and an IPv6 entry (OrbStack) makes
# glibc answer with the IPv6 one, and an IPv4-only filter is then left with nothing.
# `host.containers.internal` is Podman's built-in name for the same address.
host_gateway_address() {
  awk '$1 !~ /:/ {
      for (i = 2; i <= NF; i++)
        if ($i == "host.internal" || $i == "host.containers.internal") { print $1; exit }
    }' /etc/hosts
}

# Empty, loopback and the unspecified address all mean "no host gateway"; substituting any of
# them would point the configured names at the gateway container itself.
usable_host_gateway() {
  case "$1" in
    ""|127.*|0.0.0.0) return 1 ;;
  esac
  return 0
}

# A host gateway zone exists exactly when a generated file carries the placeholder.
needs_host_gateway() {
  grep -q '{HOST_GATEWAY_IP}' "$1"
}

# substitute_host_gateway <in> <out> <address>
substitute_host_gateway() {
  sed -e "s/{HOST_GATEWAY_IP}/$3/g" "$1" > "$2"
}

# runtime_config_present reports whether a runtime change left a complete configuration behind.
runtime_config_present() {
  [[ -f "${HOLE_RUNTIME_DIR}/Corefile" && -f "${HOLE_RUNTIME_DIR}/dnsmasq.conf" \
    && -f "${HOLE_RUNTIME_DIR}/nftables.rules" ]]
}
