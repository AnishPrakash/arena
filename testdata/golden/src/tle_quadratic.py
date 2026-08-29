# Correct maximum-subarray, deliberately O(n^2).
# max-subarray is seeded with cpu_ms=1000; at n=200000 this is ~2e10 inner iterations
# in CPython, so RLIMIT_CPU fires on test 2 after tests 0 and 1 pass.
import sys

data = sys.stdin.read().split()
n = int(data[0])
a = list(map(int, data[1:1 + n]))

best = a[0]
for i in range(n):
    s = 0
    for j in range(i, n):
        s += a[j]
        if s > best:
            best = s
print(best)
