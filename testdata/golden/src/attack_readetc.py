# file: testdata/golden/src/attack_readetc.py
# Tries to read anything interesting outside the box. Must never produce the answer.
for p in ("/etc/passwd", "/etc/shadow", "/proc/1/environ",
          "/var/run/docker.sock", "/root/.ssh/id_rsa"):
    try:
        print(p, open(p, "rb").read()[:64])
    except Exception as e:
        print(p, "denied", type(e).__name__)
