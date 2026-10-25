#!/bin/bash
set -euo pipefail

# The container starts with net.ipv4.ip_forward=1 already set (compose `sysctls`; /proc/sys is
# read-only in an unprivileged container, so it cannot be enabled from here instead), which makes
# this container a router before anything below has run. Drop forwarded traffic first, so the
# window between container start and the real ruleset is closed — and so an abort in any check
# below leaves the sandbox with no route rather than an unfiltered one.
#
# The real ruleset re-creates this table in a single nft transaction, so replacing it opens no gap.
nft -f - <<'EOF'
table inet hole
delete table inet hole
table inet hole {
    chain forward {
        type filter hook forward priority filter; policy drop;
    }
}
EOF

# shellcheck source=hole-gateway-lib.sh
source /usr/local/bin/hole-gateway-lib.sh

# Generated per run by internal/network and mounted read-only — unless a runtime change
# (`hole modify`) stored a newer configuration in this container, which then survives a restart
# of the container (`restart: on-failure`).
source_dir=/etc/hole
if runtime_config_present; then
  source_dir="${HOLE_RUNTIME_DIR}"
  echo "Restoring the configuration of the last runtime change"
fi
COREFILE_TEMPLATE="${source_dir}/Corefile"
DNSMASQ_CONF="${source_dir}/dnsmasq.conf"
NFT_TEMPLATE="${source_dir}/nftables.rules"

# The sandbox subnet is passed in so interfaces can be identified by address instead of by
# name — compose does not guarantee eth0/eth1 ordering across the two networks.
: "${HOLE_SANDBOX_SUBNET:?HOLE_SANDBOX_SUBNET must be set}"

# Filtering depends on dnsmasq recording answered addresses in nftables sets. Without that
# option the sandbox would resolve names it could never connect to, so refuse to start.
# The output is captured first rather than piped into grep: under `pipefail`, grep -q exiting
# on the first match can leave dnsmasq killed by SIGPIPE, which would fail this check against
# a build that does support nftset.
dnsmasq_version="$(dnsmasq --version 2>&1)"
if ! grep -q '[^-]nftset' <<<"${dnsmasq_version}"; then
  echo "ERROR: this dnsmasq build has no nftset support; the gateway cannot enforce network rules" >&2
  head -2 <<<"${dnsmasq_version}" >&2
  exit 1
fi

# Read from /etc/hosts, never through getent (see hole-gateway-lib.sh).
host_gateway_ip="$(host_gateway_address)"

# A host gateway zone exists exactly when the generated Corefile carries the placeholder, so the
# entrypoint needs no extra signal from compose — and cannot drift out of sync with the policy.
if grep -q '{HOST_GATEWAY_IP}' "${COREFILE_TEMPLATE}"; then
  if ! usable_host_gateway "${host_gateway_ip}"; then
    echo "ERROR: no usable IPv4 host gateway address in /etc/hosts (found '${host_gateway_ip:-none}');" >&2
    echo "       network.hostGatewayDomains cannot work on this container runtime" >&2
    exit 1
  fi
elif [[ -z "${host_gateway_ip}" ]]; then
  # Never leave the placeholder in the rules: an unresolved host gateway must not turn into
  # a syntactically valid rule pointing somewhere unintended.
  host_gateway_ip="127.0.0.1"
fi

subnet_prefix="$(echo "${HOLE_SANDBOX_SUBNET}" | cut -d/ -f1 | cut -d. -f1-3)"

sandbox_if=""
internet_if=""
while read -r iface addr; do
  [[ "${iface}" == "lo" ]] && continue
  if [[ "$(echo "${addr}" | cut -d. -f1-3)" == "${subnet_prefix}" ]]; then
    sandbox_if="${iface}"
  else
    internet_if="${iface}"
  fi
done < <(ip -o -4 addr show | awk '{split($4, a, "/"); print $2, a[1]}')

if [[ -z "${sandbox_if}" || -z "${internet_if}" ]]; then
  echo "ERROR: could not identify sandbox (${sandbox_if:-none}) and internet (${internet_if:-none}) interfaces" >&2
  ip -o -4 addr show >&2
  exit 1
fi

echo "Gateway interfaces: sandbox=${sandbox_if} internet=${internet_if}, host gateway=${host_gateway_ip}"

mkdir -p "${HOLE_RUN_DIR}"
substitute_host_gateway "${COREFILE_TEMPLATE}" "${HOLE_RUN_DIR}/Corefile" "${host_gateway_ip}"
cp "${DNSMASQ_CONF}" "${HOLE_RUN_DIR}/dnsmasq.conf"
sed -e "s/{HOST_GATEWAY_IP}/${host_gateway_ip}/g" \
    -e "s/{SANDBOX_IF}/${sandbox_if}/g" \
    -e "s/{INTERNET_IF}/${internet_if}/g" \
    "${NFT_TEMPLATE}" > /tmp/hole/nftables.rules

nft -f /tmp/hole/nftables.rules

# dnsmasq resolves only what CoreDNS already approved and records the answers in nftables sets.
# It runs under a supervising loop so a runtime change can restart it with a new configuration
# (it cannot reload nftset lines), and so a crashed dnsmasq comes back. The kernel sets survive
# the restart; the short sleep keeps a dnsmasq that dies at once from spinning.
(
  set +e
  while :; do
    dnsmasq --conf-file="${HOLE_RUN_DIR}/dnsmasq.conf" --keep-in-foreground &
    echo "$!" > "${HOLE_DNSMASQ_PID}"
    wait "$!"
    sleep 0.2
  done
) &

exec coredns -conf "${HOLE_RUN_DIR}/Corefile"
