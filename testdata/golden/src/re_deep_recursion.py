# file: testdata/golden/src/re_deep_recursion.py
import sys
sys.setrecursionlimit(10**7)
def f(n): return f(n + 1)
f(0)
