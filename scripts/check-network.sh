#!/usr/bin/env bash
# Warns when this machine sends bigger packets than the internet link carries (issue #64).
set -euo pipefail

host="${HOST:-1.1.1.1}"
network="${NETWORK:-kaya_default}"
headers=28
largest=65507

if ! [[ "$host" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; then
	echo "check-network: HOST must be an IPv4 address, not \"$host\"" >&2
	exit 2
fi

case "$(uname -s)" in
Darwin)
	iface=$(route -n get "$host" 2>/dev/null | awk '/interface:/ {print $2}' || true)
	mtu=$(ifconfig "$iface" 2>/dev/null | awk '/mtu/ {print $NF; exit}' || true)
	largest=8184
	fits() { ping -D -c 1 -t 2 -s "$1" "$host" >/dev/null 2>&1; }
	;;
Linux)
	iface=$(ip route get "$host" 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i + 1); exit}}' || true)
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
[ "$high" -gt "$largest" ] && high=$largest
if probe "$high"; then
	path=$mtu
else
	while [ $((high - low)) -gt 1 ]; do
		mid=$(((low + high) / 2))
		if probe "$mid"; then low=$mid; else high=$mid; fi
	done
	path=$((low + headers))
fi

docker_mtu=""
if [ "$(uname -s)" = Linux ] && command -v docker >/dev/null 2>&1; then
	if docker network inspect "$network" >/dev/null 2>&1; then
		docker_mtu=$(docker network inspect "$network" -f '{{index .Options "com.docker.network.driver.mtu"}}' 2>/dev/null || true)
	else
		docker_mtu=$(grep -o '"com.docker.network.driver.mtu"[[:space:]]*:[[:space:]]*"[0-9]*"' /etc/docker/daemon.json 2>/dev/null |
			grep -o '[0-9]*"$' | tr -d '"' || true)
	fi
	docker_mtu=${docker_mtu:-1500}
fi

if [ "$mtu" -le "$path" ] && { [ -z "$docker_mtu" ] || [ "$docker_mtu" -le "$path" ]; }; then
	echo "ok: $iface sends packets of up to $mtu bytes and the link carries them${docker_mtu:+ (Docker network $network: $docker_mtu)}"
	exit 0
fi

if [ "$mtu" -gt "$path" ]; then
	echo "problem: $iface sends packets of up to $mtu bytes, but the link to $host carries only $path."
fi
if [ -n "$docker_mtu" ] && [ "$docker_mtu" -gt "$path" ]; then
	echo "problem: containers on Docker network $network send packets of up to $docker_mtu bytes, but $iface carries only $path."
fi
echo "Large WhatsApp replies stall and the gateway keeps reconnecting. To fix it:"
echo "  - on the router, turn on TCP MSS clamping (fixes every device and container), or"
if [ "$mtu" -gt "$path" ]; then
	case "$iface" in
	utun* | tun* | wg* | ipsec*)
		echo "  - $iface is a VPN tunnel: lower the MTU in the VPN client, or on the network interface under it."
		;;
	*)
		if [ "$(uname -s)" = Darwin ]; then
			echo "  - on this Mac: sudo networksetup -setMTU $iface $path"
		else
			echo "  - on this machine: nmcli connection modify <connection> <ethernet or wifi>.mtu $path,"
			echo "    then nmcli connection up <connection>"
		fi
		;;
	esac
fi
if [ -n "$docker_mtu" ] && [ "$docker_mtu" -gt "$path" ]; then
	echo "  - for the containers: set Docker's MTU to $path in /etc/docker/daemon.json, restart Docker,"
	echo "    then make down && make up (see the README)."
fi
exit 1
