#!/bin/bash
set -euo pipefail

# Route all egress through the filtering gateway. The sandbox network is internal, so a
# missing or broken route can only cut this container off — it can never widen access.
if [[ -n "${HOLE_GATEWAY_IP:-}" ]]; then
  if ! sudo ip route replace default via "${HOLE_GATEWAY_IP}" 2>/dev/null; then
    echo "WARNING: could not point the default route at the gateway (${HOLE_GATEWAY_IP}); network access may fail" >&2
  fi
fi

# Execute prestart hook scripts (if any are mounted). The markers let Hole extract one log per
# script from the container log; the first failing script ends the container with its code.
if [[ -d /tmp/prestart-scripts ]]; then
  for script in /tmp/prestart-scripts/*; do
    [[ -f "${script}" ]] || continue
    [[ "${script}" == *.gitkeep ]] && continue
    name="$(basename "${script}")"
    echo "::hole-hook-begin prestart ${name}::"
    set +e
    "${script}"
    rc=$?
    set -e
    echo "::hole-hook-end prestart ${name} exit=${rc}::"
    [[ "${rc}" -eq 0 ]] || exit "${rc}"
  done
fi

echo "Starting agent CLI..."
exec "$@"
