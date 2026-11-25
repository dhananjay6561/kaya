#!/usr/bin/env bash
# Warns when this machine sends bigger packets than the internet link carries (issue #64).
set -euo pipefail

host="${HOST:-1.1.1.1}"
headers=28

case "$(uname -s)" in
Darwin)
	iface=$(route -n get "$host" 2>/dev/null | awk '/interface:/ {print $2}')
	mtu=$(ifconfig "$iface" 2>/dev/null | awk '/mtu/ {print $NF; exit}')
	fits() { ping -D -c 1 -t 2 -s "$1" "$host" >/dev/null 2>&1; }
	;;
Linux)
	iface=$(ip route get "$host" 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i + 1); exit}}')
	mtu=$(cat "/sys/class/net/$iface/mtu" 2>/dev/null || true)
	fits() { ping -M "do" -c 1 -W 2 -s "$1" "$host" >/dev/null 2>&1; }
	;;
*)
	echo "check-network: only macOS and Linux are supported" >&2
	exit 2
	;;
esac

if [ -z "${iface:-}" ] || [ -z "${mtu:-}" ]; then
	echo "check-network: could not find the interface that reaches $host" >&2
	exit 2
fi

probe() { fits "$1" || fits "$1"; }

if ! probe 0; then
	echo "check-network: $host does not answer ping, so the path MTU is unknown (try HOST=<another address>)" >&2
	exit 2
fi

low=0
high=$((mtu - headers))
if probe "$high"; then
	echo "ok: $iface sends packets of up to $mtu bytes and the link carries them"
	exit 0
fi
while [ $((high - low)) -gt 1 ]; do
	mid=$(((low + high) / 2))
	if probe "$mid"; then low=$mid; else high=$mid; fi
done
path=$((low + headers))

echo "problem: $iface sends packets of up to $mtu bytes, but the link to $host carries only $path."
echo "Large WhatsApp replies stall and the gateway keeps reconnecting. Fix it in one of two ways:"
echo "  - on the router, turn on TCP MSS clamping (fixes every device and container), or"
if [ "$(uname -s)" = Darwin ]; then
	echo "  - on this Mac: sudo networksetup -setMTU $iface $path"
else
	echo "  - on this machine: set the connection's MTU to $path in NetworkManager"
	echo "    (nmcli connection modify <connection> <ethernet or wifi>.mtu $path), and give Docker's"
	echo "    networks the same MTU, since containers here do not use the host's (see the README)."
fi
exit 1
