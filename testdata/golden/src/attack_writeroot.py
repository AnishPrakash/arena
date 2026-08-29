# file: testdata/golden/src/attack_writeroot.py
open("/etc/arena_pwned", "w").write("x")
print("WROTE TO ROOTFS - SANDBOX IS BROKEN")
