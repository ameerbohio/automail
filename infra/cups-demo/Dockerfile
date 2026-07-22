# Demo/test CUPS server. NOT for the Proxmox deployment -- there, CUPS runs on
# the host with the real Canon queue (docs/cups-host-setup.md). This exists so
# the printer service's REAL `lp` call can be exercised on a machine with no
# printer attached: the printer image carries no /etc/cups, so `lp` honours
# CUPS_SERVER and will talk to this container over the network instead of the
# host socket.
FROM alpine:3.19
# samba-client supplies the smb:// backend, so PRINTER_DEVICE_URI can point at a
# printer shared from Windows and produce real paper without USB passthrough.
RUN apk add --no-cache cups cups-filters samba-client
COPY backend-tofile /usr/lib/cups/backend/tofile
# CUPS refuses to run a backend that is group- or world-writable; 0700 root-owned
# is what lets it run (as root) and write outside the spool directory.
RUN chmod 0700 /usr/lib/cups/backend/tofile && chown root:root /usr/lib/cups/backend/tofile
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
EXPOSE 631
ENTRYPOINT ["/entrypoint.sh"]
