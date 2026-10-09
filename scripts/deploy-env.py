#!/usr/bin/env python3
"""Prepare only missing fresh-install secrets, without evaluating shell input."""
import base64
import os
import pathlib
import secrets
import subprocess
import sys

root = pathlib.Path(__file__).resolve().parent.parent
path = root / '.env'

def values(text):
    result = {}
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith('#') or '=' not in line:
            continue
        key, value = line.split('=', 1)
        result[key.strip()] = value.strip().strip('\"\'')
    return result

if sys.argv[1:] == ['ports']:
    env = values(path.read_text())
    for key, fallback in [('NOFX_FRONTEND_PORT', '3000'), ('NOFX_BACKEND_PORT', '8080')]:
        port = env.get(key, fallback)
        if not port.isdigit() or not 1 <= int(port) <= 65535:
            sys.exit('Invalid service port in .env')
        print(port)
    sys.exit()

verify = sys.argv[1:] == ['verify']
if verify and not path.exists():
    sys.exit('Update requires the original .env; refusing to initialize a new installation.')

regenerate = sys.argv[1:] == ['regenerate']
text = path.read_text() if path.exists() else (root / '.env.example').read_text()
env = values(text)
keys = ['JWT_SECRET', 'DATA_ENCRYPTION_KEY', 'RSA_PRIVATE_KEY']
missing = [key for key in keys if not env.get(key) or 'your-' in env[key] or 'YOUR_KEY_HERE' in env[key]]
if verify and missing:
    sys.exit('Update requires all original keys; refusing to regenerate credentials.')
if regenerate:
    missing = keys
# Losing keys is not recoverable by generating different ones.
if missing and not regenerate and (root / 'data').exists() and any((root / 'data').iterdir()):
    sys.exit('Existing data found but encryption/auth keys are missing. Restore the original .env; refusing to generate replacements.')
for key in missing:
    value = subprocess.check_output(['openssl', 'genrsa', '2048'], stderr=subprocess.DEVNULL).decode().strip().replace('\n', '\\n') if key == 'RSA_PRIVATE_KEY' else (base64.b64encode(secrets.token_bytes(32)).decode() if key == 'DATA_ENCRYPTION_KEY' else secrets.token_urlsafe(32))
    lines = [line for line in text.splitlines() if not line.strip().startswith(key + '=')]
    lines.append(key + '=' + value)
    text = '\n'.join(lines) + '\n'
# Preserve all user-provided SMTP/DB settings and unknown fields verbatim.
os.umask(0o077)
if not path.exists() or missing:
    temp = path.with_name('.env.install.tmp')
    with open(temp, 'w') as f:
        f.write(text)
        f.flush()
        os.fsync(f.fileno())
    os.replace(temp, path)
path.chmod(0o600)
(root / 'data').mkdir(mode=0o700, exist_ok=True)
(root / 'data').chmod(0o700)
