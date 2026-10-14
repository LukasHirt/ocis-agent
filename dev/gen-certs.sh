#!/bin/sh
# Generates a self-signed certificate for https://localhost:<port> (dev only).
set -eu
dir="$(cd "$(dirname "$0")" && pwd)/certs"
mkdir -p "$dir"
cfg="$(mktemp)"
trap 'rm -f "$cfg"' EXIT
cat > "$cfg" <<'EOF'
[req]
distinguished_name = dn
x509_extensions = v3
prompt = no
[dn]
CN = localhost
[v3]
subjectAltName = DNS:localhost,IP:127.0.0.1
EOF
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout "$dir/localhost.key" -out "$dir/localhost.crt" -config "$cfg"
echo "wrote $dir/localhost.crt and localhost.key"
