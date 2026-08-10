# Host-CUPS checks shared by the two PRINT=host bring-ups: the Compose demo
# (scripts/demo/up.sh) and the Kubernetes printer (scripts/k8s/lib-printer.sh).
#
# SOURCE THIS, DO NOT EXECUTE IT.
#
# Why shared rather than copied: both checks exist because of the same
# hard-won observation -- a broken print path does NOT fail the bring-up. The
# stack comes up healthy and then every job fails one at a time, which is
# miserable to debug and looks like an application bug. A second copy of that
# reasoning would drift, and the copy that drifts is the one that stops
# catching the case it was written for.
#
# Both functions print their own diagnosis to stderr and RETURN non-zero rather
# than exiting, because the two callers have different fail() conventions and
# different cleanup obligations (the Kubernetes caller must tear its printer
# container back down; the Compose caller must tear the whole stack down).

# cups_preflight_host QUEUE
#
# Assert the HOST has a cupsd, and that it has the named queue, BEFORE anything
# slow or destructive starts. Applies only to PRINT=host -- the containerised
# CUPS of infra/compose/demo-print.yml needs none of this.
cups_preflight_host() {
	local queue="$1"

	# Ordered first on purpose: Docker AUTO-CREATES a bind-mount source as a
	# DIRECTORY when it does not exist, so a host that has never run cupsd
	# ends up with a directory sitting exactly where the socket belongs. The
	# generic "no socket" message below would then be actively misleading --
	# the path exists, it is just the wrong kind of thing, and no amount of
	# starting cups will fix it while the directory is in the way.
	if [ -d /run/cups/cups.sock ]; then
		echo "✗ /run/cups/cups.sock is a DIRECTORY, not a socket" >&2
		echo "  Docker auto-created it because no cupsd was running when a container mounted it." >&2
		echo "  → sudo rmdir /run/cups/cups.sock && sudo systemctl start cups" >&2
		return 1
	fi

	if [ ! -S /run/cups/cups.sock ]; then
		echo "✗ no CUPS socket at /run/cups/cups.sock -- there is no print server on this host" >&2
		echo "  → sudo apt install -y cups cups-client && sudo systemctl start cups   (docs/cups-host-setup.md)" >&2
		echo "    or use PRINT=1 for the containerised CUPS (Compose demo only)" >&2
		return 1
	fi

	# lpstat is the host's own client. If it is absent we cannot check the
	# queue from here -- but the socket exists, and the definitive check runs
	# from inside the container anyway, so this is a skip and not a failure.
	command -v lpstat >/dev/null 2>&1 || return 0

	if ! lpstat -p "$queue" >/dev/null 2>&1; then
		echo "✗ cupsd is running but has no queue named '${queue}'" >&2
		echo "  → run \`lpstat -p -d\` to see the real queue name, then re-run with PRINTER_NAME=<name>" >&2
		return 1
	fi
}

# cups_verify_container QUEUE COMPOSE_CMD...
#
# The definitive check, and it runs AFTER the container is up: a queue existing
# on the host does not prove the printer CONTAINER can reach it, and that hop
# -- the socket mount -- is exactly what breaks. Asking lp's own client inside
# the container tests the real path a job will take, which is the only thing
# that settles it.
#
# Retries because the container has to finish starting first; 30s is generous
# for a process that only has to exec a client binary.
cups_verify_container() {
	local queue="$1"
	shift
	local i
	for i in $(seq 1 30); do
		if "$@" exec -T printer lpstat -p "$queue" >/dev/null 2>&1; then
			echo "   printer container can reach CUPS queue '${queue}'"
			return 0
		fi
		sleep 1
	done
	echo "✗ the printer CONTAINER cannot see queue '${queue}'" >&2
	echo "  every job would fail instead of printing. What lp sees inside the container:" >&2
	"$@" exec -T printer lpstat -p 2>&1 | head -5 >&2 || true
	return 1
}
