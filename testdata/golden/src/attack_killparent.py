# file: testdata/golden/src/attack_killparent.py
import os, signal
os.kill(1, signal.SIGKILL)   # try to kill boxrun and escape measurement
print("KILLED THE SUPERVISOR - SANDBOX IS BROKEN")
