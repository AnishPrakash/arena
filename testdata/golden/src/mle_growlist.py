# file: testdata/golden/src/mle_growlist.py
# A gradual leak rather than one big allocation. If swap were enabled this would slowly
# thrash to a wall-clock timeout instead of producing a clean MLE.
buf = []
while True:
    buf.append(bytearray(4 * 1024 * 1024))
