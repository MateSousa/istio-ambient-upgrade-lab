#!/usr/bin/env python3
# Regenerate the SCRAM-SHA-256 verifier literals used across the data slice.
#
# The SAME verifier string is used in three places and MUST match byte-for-byte:
#   - demo/data/postgres.yaml       : CREATE ROLE demo_app ... PASSWORD '<verifier>'
#   - demo/data/pgbouncer-config.yaml: userlist.txt  "demo_app" "<verifier>"
#   - the plaintext password ('demo_app_pw') is what pgbouncer (outbound) and
#     app-a authenticate with; PgBouncer/Postgres verify it against the verifier.
#
# Salts are fixed here so the literals are reproducible for the lab. A real
# deployment uses random per-credential salts. Run:  python3 scripts/gen-scram.py
import base64
import hashlib
import hmac

ITERATIONS = 4096


def scram_verifier(password: str, salt: bytes, iterations: int = ITERATIONS) -> str:
    # RFC 5802 / PostgreSQL SCRAM-SHA-256 verifier construction.
    salted = hashlib.pbkdf2_hmac("sha256", password.encode("utf-8"), salt, iterations)
    client_key = hmac.new(salted, b"Client Key", hashlib.sha256).digest()
    stored_key = hashlib.sha256(client_key).digest()
    server_key = hmac.new(salted, b"Server Key", hashlib.sha256).digest()
    return "SCRAM-SHA-256${it}:{salt}${sk}:{svk}".format(
        it=iterations,
        salt=base64.b64encode(salt).decode(),
        sk=base64.b64encode(stored_key).decode(),
        svk=base64.b64encode(server_key).decode(),
    )


CREDENTIALS = [
    ("demo_app", "demo_app_pw", b"ambientlabdemoapp"),
    ("pgbouncer", "pgbouncer_admin_pw", b"ambientlabpgbadmin"),
]

if __name__ == "__main__":
    for user, password, salt in CREDENTIALS:
        print(f'"{user}" "{scram_verifier(password, salt)}"')
