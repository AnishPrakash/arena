# file: testdata/golden/src/tle_sleep.py
# Burns zero CPU. RLIMIT_CPU can never fire for this; only the wall-clock watchdog can.
import time
time.sleep(600)
