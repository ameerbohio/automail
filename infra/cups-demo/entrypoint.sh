#!/bin/sh
# Brings up cupsd with one queue named $PRINTER_NAME, pointed at $PRINTER_DEVICE_URI.
#
# Default device is the capture backend, so the demo needs no printer at all.
# Point PRINTER_DEVICE_URI at a real device to get paper, e.g. a printer shared
# from Windows:  smb://user:pass@HOST/Canon_MF240
set -e
mkdir -p /out && chown -R lp:lp /out

cat > /etc/cups/cupsd.conf <<CONF
LogLevel ${CUPS_LOG_LEVEL:-warn}
Listen 0.0.0.0:631
DefaultEncryption Never
# cupsd 2.4 validates the HTTP Host: header and rejects anything it does not
# recognise as itself -- a container reached by its compose service name is
# rejected with 'invalid Host: field'. Worse, the CLIENT reports that as
# "add '/version=1.1' to server name", which sends you chasing an IPP version
# problem that does not exist. ServerAlias * is what makes network clients work.
ServerAlias *
<Location />
  Order allow,deny
  Allow all
</Location>
<Location /admin>
  Order allow,deny
  Allow all
</Location>
CONF
grep -q '^FileDevice' /etc/cups/cups-files.conf || echo 'FileDevice Yes' >> /etc/cups/cups-files.conf

cupsd
for i in $(seq 1 30); do lpstat -r >/dev/null 2>&1 && break; sleep 1; done

lpadmin -p "${PRINTER_NAME:-Canon_MF240}" -E \
        -v "${PRINTER_DEVICE_URI:-tofile:/out/last-print.pdf}" -m raw
cupsenable "${PRINTER_NAME:-Canon_MF240}" || true
cupsaccept "${PRINTER_NAME:-Canon_MF240}" || true

echo "cups-demo: queue '${PRINTER_NAME:-Canon_MF240}' -> ${PRINTER_DEVICE_URI:-tofile:/out/last-print.pdf}"
lpstat -p
exec tail -f /var/log/cups/error_log
