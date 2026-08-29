# file: testdata/golden/src/attack_fillbox.py
with open("/box/fill", "wb") as f:
    while True:
        f.write(b"\0" * (8 * 1024 * 1024))
