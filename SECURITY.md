# Security Policy

## No authentication by design

`sos-server` has **no authentication**. Anyone who can reach the server can,
without any login:

- list, open and close serial ports;
- write arbitrary bytes to any connected serial port;
- read all serial output;
- read, edit and delete the shared command history;
- change the server configuration and use the stored WebDAV credentials.

This is intentional: the threat model is a **trusted local network**. The
server is meant to run on your own machine or LAN, where everyone with
network access is trusted.

## Do not expose the server to the internet

- **Never** expose `sos-server` directly on a public IP or forward its port
  through your router.
- If you do not need remote access, bind it to localhost only:
  `sos-server -address 127.0.0.1`.
- If you do need remote access, put it behind a reverse proxy with
  authentication (or a VPN / Tailscale-style private network) — do not rely
  on the server itself for access control.

One mitigation is built in: the WebSocket upgrader only accepts `Origin`
headers whose host matches the server's own host, so a malicious website
cannot drive your serial ports from a browser tab. That stops cross-site
WebSocket hijacking; it is not a substitute for keeping the server off the
public internet.

## Reporting a vulnerability

Please report security issues through GitHub's **private vulnerability
reporting** on this repository (Security tab → "Report a vulnerability").
Do not open a public issue for a vulnerability. We will respond and
coordinate a fix before any public disclosure.
